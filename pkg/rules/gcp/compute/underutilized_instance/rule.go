// Package underutilized_instance implements the "underutilized_instance"
// FinOps rule: a running, non-spot instance whose p95 CPU utilization is low
// enough that a smaller machine type (or stop/delete) would suffice.
package underutilized_instance

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// ID is the stable rule identifier.
const ID = "underutilized_instance"

// Constants verbatim from the rule spec §7.1.
const (
	MinOverprovisionRatio = 0.40 // >40% headroom, strictly greater
	TargetCPUUtil         = 0.60 // p95 CPU target on the recommended shape
	MinSamplesReq         = 168  // 7 days of hourly points
	MinCoverageReq        = 0.50
	MinAgeDays            = 7.0
	MinMonthlyWasteUSD    = 1.00

	MinCandidateVCPU   = 1.0
	MinCandidateMemGiB = 1.0
)

func init() { rules.Register(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "gcp",
		Service:  "compute",
		Title:    "Instance is significantly overprovisioned for its CPU load",
		Description: "A running, non-spot instance whose p95 CPU utilization " +
			"leaves more than 40% headroom versus a smaller machine type in " +
			"the same family. Rightsizing (or stopping) reclaims the delta.",
		Severity:           rules.SeverityHigh,
		RequiredAssetTypes: []string{"compute.googleapis.com/Instance"},
		RequiredMetrics:    []string{metrics.KeyCPUUtilizationP95},
		Remediation:        "gcloud compute instances set-machine-type NAME --machine-type CANDIDATE --zone ZONE",
		Origin:             "native",
	}
}

// priceableShape bundles the summed monthly cost of a machine shape with the
// pricing components that contributed to it, so a Finding can attach one
// price-evidence entry per contributing component rather than collapsing
// the provenance of a compound (CPU + RAM) cost onto a single dominant leg.
type priceableShape struct {
	cost  float64
	comps []rules.PricedComponent
}

func (rule) Eval(ctx context.Context, p *rules.Pass) ([]rules.Finding, error) {
	var out []rules.Finding

	p.Graph.ByKind(graph.KindInstance, func(n *graph.Node) bool {
		if ctx.Err() != nil {
			return false
		}

		// P0: exemption label.
		if n.Labels["tellury-exempt"] == "true" {
			p.SkipNode(ID, n.ID, rules.SkipExemptLabel)
			return true
		}

		// P1: shape valid.
		machineType, ok := n.Str("machine_type")
		if !ok || machineType == "" {
			p.SkipNode(ID, n.ID, rules.SkipUnknownMachineType)
			return true
		}
		vcpu, ok := n.Num("vcpu_count")
		if !ok || vcpu < 1 {
			p.SkipNode(ID, n.ID, rules.SkipUnknownMachineType)
			return true
		}
		memGiB, ok := n.Num("memory_gib")
		if !ok || memGiB <= 0 {
			p.SkipNode(ID, n.ID, rules.SkipUnknownMachineType)
			return true
		}
		family, ok := n.Str("machine_family")
		if !ok || family == "" {
			p.SkipNode(ID, n.ID, rules.SkipUnknownMachineType)
			return true
		}
		status, ok := n.Str("status")
		if !ok || status == "" {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}

		// P2: running.
		if status != "RUNNING" {
			p.SkipNode(ID, n.ID, rules.SkipNotRunning)
			return true
		}

		// P3: not spot.
		provisioningModel, _ := n.Str("provisioning_model")
		if provisioningModel == "" {
			provisioningModel = "STANDARD"
		}
		preemptible, _ := n.Bool("preemptible")
		if provisioningModel == "SPOT" || preemptible {
			p.SkipNode(ID, n.ID, rules.SkipSpot)
			return true
		}

		// P4: no accelerators.
		accelCount, _ := n.Num("accelerator_count")
		if accelCount > 0 {
			p.SkipNode(ID, n.ID, rules.SkipAccelerator)
			return true
		}

		// P5: old enough.
		creationTS, ok := n.Str("creation_timestamp")
		if !ok {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}
		createdAt, err := time.Parse(time.RFC3339, creationTS)
		if err != nil {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}
		ageDays := p.Now.Sub(createdAt).Hours() / 24
		if ageDays < MinAgeDays {
			p.SkipNode(ID, n.ID, rules.SkipTooYoung)
			return true
		}

		// P6: CPU data present.
		cpu, ok := n.MetricOK(metrics.KeyCPUUtilizationP95, MinSamplesReq, MinCoverageReq)
		if !ok {
			p.SkipNode(ID, n.ID, rules.SkipNoMetric)
			return true
		}

		// P7: overprovisioned. Ratio uses the *current* shape's headroom:
		// a p95 CPU utilization of cpu.Value on `vcpu` vCPUs; overprovision
		// ratio = 1 - cpu.Value (fraction of the current shape sitting idle).
		overprovisionRatio := 1 - cpu.Value
		if overprovisionRatio <= MinOverprovisionRatio {
			p.SkipNode(ID, n.ID, rules.SkipBelowOverprovision)
			return true
		}

		// P8: current priceable.
		current, err := priceShape(p, machineType, family, vcpu, memGiB)
		if err != nil {
			p.SkipNode(ID, n.ID, rules.SkipNoPrice)
			return true
		}

		// P9: candidate exists.
		candidate, cand, ok := findCandidate(p, family, vcpu, cpu.Value)
		var (
			monthlyWaste  float64
			confidence    float64
			recMachine    string
			recMonthly    float64
			noSmallerSize bool
			recComps      []rules.PricedComponent
		)
		if ok {
			monthlyWaste = current.cost - cand.cost
			confidence = 0.8
			recMachine = candidate.Name
			recMonthly = cand.cost
			recComps = cand.comps
		} else {
			// No smaller size exists in-family: recommend stop/delete.
			//
			// This is NOT a skip: we still have a Finding to report (the
			// instance is overprovisioned and reclaimable via stop/delete),
			// so SkipNode must not be called here - skips and findings are
			// disjoint sets, and --explain-skips tallies would otherwise
			// double-count this resource. The fact is surfaced as evidence
			// on the Finding instead.
			noSmallerSize = true
			monthlyWaste = current.cost
			confidence = 0.5
			recMachine = "stop/delete"
			recMonthly = 0
		}

		if monthlyWaste < MinMonthlyWasteUSD {
			p.SkipNode(ID, n.ID, rules.SkipBelowMinWaste)
			return true
		}

		ev := []rules.Evidence{
			{Key: "p95_cpu", Value: fmt.Sprintf("%.2f%%", cpu.Value*100)},
			{Key: "machine_type", Value: machineType},
			{Key: "recommended_machine_type", Value: recMachine},
			{Key: "current_monthly", Value: fmt.Sprintf("$%.2f", current.cost)},
			{Key: "recommended_monthly", Value: fmt.Sprintf("$%.2f", recMonthly)},
			{Key: "samples", Value: fmt.Sprintf("%d", cpu.Samples)},
		}
		// One price-source evidence entry per component that contributed a
		// nonzero amount: a live answer for one leg and an embedded answer
		// for another must both be visible on the Finding, never collapsed
		// onto a single dominant source.
		ev = append(ev, rules.PriceEvidenceFor("current_price_source", p.Price, current.comps...)...)
		if ok {
			ev = append(ev, rules.PriceEvidenceFor("recommended_price_source", p.Price, recComps...)...)
		}
		if noSmallerSize {
			ev = append(ev, rules.Evidence{Key: "no_smaller_size", Value: "true"})
		}

		out = append(out, rules.Finding{
			RuleID:          ID,
			ResourceID:      n.ID,
			Resource:        n.Display(),
			Kind:            n.Kind,
			Project:         n.Project,
			Location:        n.Location,
			MonthlyWasteUSD: monthlyWaste,
			Confidence:      round2(confidence),
			Evidence:        ev,
		})
		return true
	})
	return out, nil
}

// priceShape prices the given machine shape and returns the summed monthly
// cost together with the pricing components that contributed a nonzero amount
// to it. The components come from the same decision path that priced the
// shape, so they always match the lookups actually made.
func priceShape(p *rules.Pass, machineType, family string, vcpu, memGiB float64) (priceableShape, error) {
	cost, comps, err := instanceMonthlyCost(p, machineType, family, vcpu, memGiB)
	if err != nil {
		return priceableShape{}, err
	}
	return priceableShape{cost: cost, comps: comps}, nil
}

// instanceMonthlyCost prices the current shape: catalog machine types use
// KindVMInstance; custom shapes use KindVMCustomCPU + KindVMCustomRAM. It
// returns the summed cost and a PricedComponent per component that actually
// contributed a nonzero dollar amount, so a Finding can attach one
// price-source evidence entry per contributing leg instead of collapsing the
// provenance of a composite CPU+RAM price onto a single dominant component.
func instanceMonthlyCost(p *rules.Pass, machineType, family string, vcpu, memGiB float64) (float64, []rules.PricedComponent, error) {
	region := pricing.RegionOf("") // instance cost table is region-flat ("default") in v1
	if unit, resolvedRegion, err := p.Price.UnitPrice(pricing.KindVMInstance, "gcp", machineType, region); err == nil {
		return unit * pricing.HoursPerMonth, []rules.PricedComponent{
			{Kind: pricing.KindVMInstance, SKU: machineType, Region: resolvedRegion, Key: "instance"},
		}, nil
	}
	cpuUnit, cpuRegion, err := p.Price.UnitPrice(pricing.KindVMCustomCPU, "gcp", family, region)
	if err != nil {
		return 0, nil, err
	}
	ramUnit, ramRegion, err := p.Price.UnitPrice(pricing.KindVMCustomRAM, "gcp", family, region)
	if err != nil {
		return 0, nil, err
	}

	cost := (vcpu*cpuUnit + memGiB*ramUnit) * pricing.HoursPerMonth

	var comps []rules.PricedComponent
	if vcpu*cpuUnit != 0 {
		comps = append(comps, rules.PricedComponent{Kind: pricing.KindVMCustomCPU, SKU: family, Region: cpuRegion, Key: "cpu"})
	}
	if memGiB*ramUnit != 0 {
		comps = append(comps, rules.PricedComponent{Kind: pricing.KindVMCustomRAM, SKU: family, Region: ramRegion, Key: "ram"})
	}
	if len(comps) == 0 {
		// Both legs contributed nothing yet the cost resolved; this cannot
		// normally happen, but be safe rather than emit an empty provenance.
		comps = []rules.PricedComponent{
			{Kind: pricing.KindVMCustomCPU, SKU: family, Region: cpuRegion, Key: "cpu"},
			{Kind: pricing.KindVMCustomRAM, SKU: family, Region: ramRegion, Key: "ram"},
		}
	}
	return cost, comps, nil
}

// findCandidate walks the family ladder (ascending) and returns the smallest
// size that keeps the projected p95 CPU utilization at or below
// TargetCPUUtil, per §7 cost model: candidate vCPU count C satisfies
// p95CPU * currentVCPU / C <= TargetCPUUtil.
func findCandidate(p *rules.Pass, family string, currentVCPU, p95CPU float64) (pricing.MachineSpec, priceableShape, bool) {
	if p.Sizer == nil {
		return pricing.MachineSpec{}, priceableShape{}, false
	}
	ladder := p.Sizer.Ladder(family)
	for _, cand := range ladder {
		if cand.VCPU < MinCandidateVCPU || cand.MemoryGiB < MinCandidateMemGiB {
			continue
		}
		if cand.VCPU >= currentVCPU {
			continue // must be strictly smaller to be a rightsizing win
		}
		projected := p95CPU * currentVCPU / cand.VCPU
		if projected <= TargetCPUUtil {
			shape, err := priceShape(p, cand.Name, cand.Family, cand.VCPU, cand.MemoryGiB)
			if err != nil {
				continue
			}
			return cand, shape, true
		}
	}
	return pricing.MachineSpec{}, priceableShape{}, false
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
