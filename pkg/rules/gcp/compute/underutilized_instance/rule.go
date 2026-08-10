// Package underutilized_instance implements the "underutilized_instance"
// FinOps rule: a running, non-spot instance whose p95 CPU utilization is low
// enough that a smaller machine type (or stop/delete) would suffice.
package underutilized_instance

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	gcprules "github.com/TypeOneLabs/tellury/pkg/rules/gcp"
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

// migCreatedByMarker is the substring a managed instance group's `created-by`
// metadata item names when it points at an instanceGroupManagers resource:
//
//	"projects/<p>/zones/<z>/instanceGroupManagers/<name>"
//
// The marker match is substring-based on the resource-type segment because the
// CREATED_BY metadata value is a creator self-link whose exact path prefix
// differs by API version and zone vs. region placement; what is stable across
// every spelling is that it RESOLVES to an instanceGroupManagers resource.
const migCreatedByMarker = "instanceGroupManagers"

func init() { rules.RegisterNode(rule{}) }

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

func (rule) Kind() graph.ResourceKind { return graph.KindInstance }

func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		// P0.5: managed instance group member. GCP marks a MIG member with
		// the `created-by` instance metadata item, whose value is a creator
		// self-link that resolves to an instanceGroupManagers resource. A MIG
		// owns its members' size and count; recommending a resize for one
		// member is advice an operator cannot act on, and the group's own
		// sizing is a separate concern with its own rules later. Distinct
		// skip reason so `--explain-skips` shows these separately from other
		// skips ("12 instances skipped: managed by a MIG").
		{Name: "not_mig_member", SkipCode: rules.SkipManagedByMIG,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				createdBy, ok := n.Str(gcprules.AttrCreatedBy)
				return !ok || !strings.Contains(createdBy, migCreatedByMarker)
			}},

		// P1: shape valid. Four checks share SkipUnknownMachineType, each
		// independently named; status is its own code.
		{Name: "machine_type_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str("machine_type")
				return ok && v != ""
			}},
		{Name: "vcpu_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Num("vcpu_count")
				return ok && v >= 1
			}},
		{Name: "memory_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Num("memory_gib")
				return ok && v > 0
			}},
		{Name: "family_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str("machine_family")
				return ok && v != ""
			}},
		{Name: "status_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str("status")
				return ok && v != ""
			}},

		// P2: running.
		{Name: "running", SkipCode: rules.SkipNotRunning,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				s, _ := n.Str("status")
				return s == "RUNNING"
			}},

		// P3: not spot. provisioning_model absent defaults to STANDARD (the
		// normalizer writes it unconditionally, but the defaulting read
		// preserves the pre-refactor semantics for hand-built fixtures).
		{Name: "not_spot", SkipCode: rules.SkipSpot,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				provisioningModel, _ := n.Str("provisioning_model")
				if provisioningModel == "" {
					provisioningModel = "STANDARD"
				}
				preemptible, _ := n.Bool("preemptible")
				return provisioningModel != "SPOT" && !preemptible
			}},

		// P4: no accelerators.
		{Name: "no_accelerators", SkipCode: rules.SkipAccelerator,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				// ABSENCE MEANS ZERO: accelerator_count is written
				// unconditionally by the normalizer; a missing key means
				// "no accelerators", never an unknown to skip on.
				accelCount, _ := n.Num("accelerator_count")
				return accelCount == 0
			}},

		// P5: old enough. The parse check and the age gate are separate
		// guards so a missing/unparseable creation_timestamp keeps its
		// distinct missing-attribute code (SkipMissingAttr) instead of
		// collapsing onto too_young.
		{Name: "creation_timestamp_parseable", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				ts, ok := n.Str("creation_timestamp")
				if !ok {
					return false
				}
				_, err := time.Parse(time.RFC3339, ts)
				return err == nil
			}},
		{Name: "old_enough", SkipCode: rules.SkipTooYoung,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				ts, _ := n.Str("creation_timestamp")
				createdAt, _ := time.Parse(time.RFC3339, ts)
				return p.Now.Sub(createdAt).Hours()/24 >= MinAgeDays
			}},

		// P6: CPU data present. The metric gate stashes the p95 value and
		// sample count for Cost and ExtraEvidence.
		{Name: "cpu_data_present", SkipCode: rules.SkipNoMetric,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				cpu, ok := n.MetricOK(metrics.KeyCPUUtilizationP95, MinSamplesReq, MinCoverageReq)
				if !ok {
					return false
				}
				nc.Set("p95_cpu", cpu.Value)
				nc.Set("cpu_samples", cpu.Samples)
				return true
			}},

		// P7: overprovisioned. Ratio uses the *current* shape's headroom:
		// a p95 CPU utilization of p95 on `vcpu` vCPUs; overprovision
		// ratio = 1 - p95 (fraction of the current shape sitting idle).
		{Name: "overprovisioned", SkipCode: rules.SkipBelowOverprovision,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				p95, _ := nc.Get("p95_cpu")
				overprovisionRatio := 1 - p95.(float64)
				return overprovisionRatio > MinOverprovisionRatio
			}},
	}
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	// All four shape attrs are guaranteed present and valid by the guards.
	machineType, _ := n.Str("machine_type")
	family, _ := n.Str("machine_family")
	vcpu, _ := n.Num("vcpu_count")
	memGiB, _ := n.Num("memory_gib")

	// P8: current priceable.
	current, err := priceShape(p, machineType, family, vcpu, memGiB)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never a $0 assumption (Invariant I4)
	}

	p95, _ := nc.Get("p95_cpu")
	cpu := p95.(float64)

	// P9: candidate exists. Two mutually exclusive cost branches (§7):
	//
	//   rightsize — a smaller in-family shape exists: the delta between the
	//       current and candidate monthly cost, confidence 0.8.
	//
	//   stop_delete — no smaller shape exists: the whole current monthly
	//       cost is reclaimable by stopping the instance, confidence 0.5.
	//       This is a Finding, NOT a skip: skips and findings are disjoint
	//       sets, so no SkipNoSmallerSize is ever recorded here; the
	//       no-candidate fact is surfaced as evidence instead.
	candidate, cand, ok := findCandidate(p, family, vcpu, cpu)

	var (
		monthlyWaste  float64
		confidence    float64
		recMachine    string
		recMonthly    float64
		noSmallerSize bool
		label         string
	)
	if ok {
		monthlyWaste = current.cost - cand.cost
		confidence = 0.8
		recMachine = candidate.Name
		recMonthly = cand.cost
		label = "rightsize"
	} else {
		noSmallerSize = true
		monthlyWaste = current.cost
		confidence = 0.5
		recMachine = "stop/delete"
		recMonthly = 0
		label = "stop_delete"
	}

	// Stash evidence inputs; price-source entries are rendered here because
	// ExtraEvidence has no Pass to reach the pricer.
	// Stashed here because ExtraEvidence has no Pass to ask the pricer.
	nc.Set("currency", rules.CurrencyOf(p))
	nc.Set("recommended_machine_type", recMachine)
	nc.Set("current_monthly", current.cost)
	nc.Set("recommended_monthly", recMonthly)
	nc.Set("no_smaller_size", noSmallerSize)
	nc.Set("current_price_source", rules.PriceEvidenceFor("current_price_source", p.Price, current.comps...))
	if ok {
		nc.Set("recommended_price_source", rules.PriceEvidenceFor("recommended_price_source", p.Price, cand.comps...))
	}

	return []rules.CostBranch{{
		Waste:      monthlyWaste,
		Confidence: round2(confidence),
		Label:      label,
	}}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

// EvidenceKeys returns nil: every evidence entry is formatted with a non-%v
// layout (p95_cpu as %.2f%%, the monthly figures as $%.2f) or produced from
// a price lookup, so ExtraEvidence renders the whole list to keep the
// pre-refactor keys, values and order byte-for-byte.
func (rule) EvidenceKeys() []string { return nil }

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	machineType, _ := n.Str("machine_type")
	p95, _ := nc.Get("p95_cpu")
	samples, _ := nc.Get("cpu_samples")
	recMachine, _ := nc.Get("recommended_machine_type")
	currentMonthly, _ := nc.Get("current_monthly")
	recMonthly, _ := nc.Get("recommended_monthly")

	ev := []rules.Evidence{
		{Key: "p95_cpu", Value: fmt.Sprintf("%.2f%%", p95.(float64)*100)},
		{Key: "machine_type", Value: machineType},
		{Key: "recommended_machine_type", Value: recMachine.(string)},
		rules.EvMoneyIn("current_monthly", curStr, currentMonthly.(float64), 2),
		rules.EvMoneyIn("recommended_monthly", curStr, recMonthly.(float64), 2),
		{Key: "samples", Value: fmt.Sprintf("%d", samples.(int))},
	}
	if v, ok := nc.Get("current_price_source"); ok {
		ev = append(ev, v.([]rules.Evidence)...)
	}
	if v, ok := nc.Get("recommended_price_source"); ok {
		ev = append(ev, v.([]rules.Evidence)...)
	}
	if noSmaller, _ := nc.Get("no_smaller_size"); noSmaller.(bool) {
		ev = append(ev, rules.Evidence{Key: "no_smaller_size", Value: "true"})
	}
	return ev
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
