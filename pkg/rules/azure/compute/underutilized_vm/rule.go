// Package underutilized_vm implements the "underutilized_vm" FinOps rule: a
// running, non-spot Azure VM whose p95 CPU utilization is low enough that a
// smaller size in the same family (or stop/delete) would suffice.
//
// It is the Azure mirror of the AWS underutilized_instance rule and the GCP
// underutilized_instance rule: the same guard structure with typed skip codes,
// the same two mutually exclusive cost branches (rightsize vs. stop_delete),
// and the same thresholds. The Azure counterpart of the AWS Auto Scaling group
// guard and the GCP managed instance group guard is the Virtual Machine Scale
// Set member guard, detected from virtual_machine_scale_set_id.
package underutilized_vm

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	azurerules "github.com/TypeOneLabs/tellury/pkg/rules/azure"
)

// ID is the stable rule identifier. It is distinct from the AWS and GCP rule
// IDs because the rule registry keys on Meta().ID across all providers, and
// two rules may not share an ID.
const ID = "underutilized_vm"

// Thresholds verbatim from the AWS/GCP rule specs. They are unchanged because
// Azure's hourly PT1H platform metric produces the same 7-day / 168-sample
// geometry as CloudWatch and Cloud Monitoring.
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

// PowerStateRunning is the Azure Resource Graph power state code for a running
// VM. The normalizer reads it from
// properties.extended.instanceView.powerState.code.
const PowerStateRunning = "PowerState/running"

// skipManagedByVMSS is the skip code for a VM that belongs to a Virtual
// Machine Scale Set. A scale-set VM cannot be resized individually, so the
// recommendation is unactionable. This is the Azure equivalent of the AWS
// rule's managed_by_asg and the GCP rule's SkipManagedByMIG.
const skipManagedByVMSS rules.SkipCode = "managed_by_vmss"

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "azure",
		Service:  "compute",
		Title:    "Virtual machine is significantly overprovisioned for its CPU load",
		Description: "A running, non-spot Azure VM whose p95 CPU utilization " +
			"leaves more than 40% headroom versus a smaller size in the same " +
			"family. Rightsizing (or stopping) reclaims the delta. A VM in a " +
			"Virtual Machine Scale Set is excluded because its size is owned by " +
			"the scale set, not by the VM itself.",
		Severity:           rules.SeverityHigh,
		RequiredAssetTypes: []string{azurerules.TypeVM},
		RequiredMetrics:    []string{metrics.KeyCPUUtilizationP95},
		Remediation:        "az vm resize --ids ID --size CANDIDATE",
		Origin:             "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindInstance }

// Guards are evaluated in order; the first to return false decides the
// SkipCode recorded for the node. Every guard has a distinct skip code so
// --explain-skips separates them. In particular, a missing attribute is never
// reported as a business fact: power_state_present and running are different
// diagnoses, as are priority_present and not_spot, and vmss_attr_present and
// not_scale_set_member.
func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		// P0: VMSS membership attribute present. The normalizer writes
		// virtual_machine_scale_set_id unconditionally ("" for a standalone
		// VM), so an absent attribute means the payload was not parsed.
		{Name: "vmss_attr_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				_, ok := n.Str(azurerules.AttrVMSSID)
				return ok
			}},

		// P1: not a Virtual Machine Scale Set member. A scale-set VM cannot be
		// resized individually, so the recommendation is unactionable.
		{Name: "not_scale_set_member", SkipCode: skipManagedByVMSS,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, _ := n.Str(azurerules.AttrVMSSID)
				return v == ""
			}},

		// P2: shape known — vm_size, vCPU, memory and family all present and
		// valid. Four guards share SkipUnknownMachineType, each independently
		// named so --explain-skips can show which field was missing.
		{Name: "vm_size_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(azurerules.AttrVMSize)
				return ok && v != ""
			}},
		{Name: "vcpu_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Num(azurerules.AttrVCpuCount)
				return ok && v >= 1
			}},
		{Name: "memory_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Num(azurerules.AttrMemoryGiB)
				return ok && v > 0
			}},
		{Name: "family_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(azurerules.AttrMachineFamily)
				return ok && v != ""
			}},

		// P3: power state present. A missing power_state_code is an unparsed
		// payload, not "not running".
		{Name: "power_state_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(azurerules.AttrPowerState)
				return ok && v != ""
			}},
		{Name: "running", SkipCode: rules.SkipNotRunning,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				s, _ := n.Str(azurerules.AttrPowerState)
				return s == PowerStateRunning
			}},

		// P4: priority present. The normalizer writes priority unconditionally
		// ("" for a regular VM, "Spot" for a spot VM), so absence means the
		// payload was not parsed, never "regular".
		{Name: "priority_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				_, ok := n.Str(azurerules.AttrPriority)
				return ok
			}},
		{Name: "not_spot", SkipCode: rules.SkipSpot,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				priority, _ := n.Str(azurerules.AttrPriority)
				return priority != "Spot"
			}},

		// P5: OS type present. It is the discriminator between Linux and
		// Windows VM pricing rows, so a missing value is not priceable.
		{Name: "os_type_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(azurerules.AttrOSType)
				return ok && v != ""
			}},

		// P6: old enough. The parse check and the age gate are separate guards
		// so a missing/unparseable time_created keeps its distinct
		// missing-attribute code (SkipMissingAttr) instead of collapsing onto
		// too_young.
		{Name: "time_created_parseable", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				createdAt, ok := n.Time(azurerules.AttrTimeCreated)
				if !ok {
					return false
				}
				nc.Set("created_at", createdAt)
				return true
			}},
		{Name: "old_enough", SkipCode: rules.SkipTooYoung,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				createdAt, _ := nc.Get("created_at")
				created := createdAt.(time.Time)
				days := p.Now.Sub(created).Hours() / 24
				if days < MinAgeDays {
					return false
				}
				nc.Set("age_days", days)
				return true
			}},

		// P7: CPU data present. The metric gate stashes the p95 value and
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

		// P8: overprovisioned. Ratio uses the current shape's headroom:
		// a p95 CPU utilization of p95 on vcpu vCPUs; overprovision
		// ratio = 1 - p95 (fraction of the current shape sitting idle).
		{Name: "overprovisioned", SkipCode: rules.SkipBelowOverprovision,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				p95, _ := nc.Get("p95_cpu")
				overprovisionRatio := 1 - p95.(float64)
				return overprovisionRatio > MinOverprovisionRatio
			}},
	}
}

// vmPricer is the subset of the Azure price catalog the rule needs to price a
// VM. The live CatalogPricer and StaticPricer both implement this via their
// VMPrice method; test fakes implement it directly.
type vmPricer interface {
	VMPrice(ctx context.Context, region, size, osType string) (float64, error)
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	// All shape attrs are guaranteed present and valid by the guards.
	vmSize, _ := n.Str(azurerules.AttrVMSize)
	family, _ := n.Str(azurerules.AttrMachineFamily)
	vcpu, _ := n.Num(azurerules.AttrVCpuCount)
	osType, _ := n.Str(azurerules.AttrOSType)
	region := pricing.RegionOf(n.Location)

	vp, ok := p.Price.(vmPricer)
	if !ok {
		return nil, fmt.Errorf("underutilized_vm: pricer does not support Azure VM pricing")
	}

	// P9: current shape monthly cost.
	currentMonthly, err := vmMonthlyCost(ctx, vp, region, vmSize, osType)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never $0 (Invariant I4)
	}

	p95, _ := nc.Get("p95_cpu")
	cpu := p95.(float64)

	// P10: candidate exists. Two mutually exclusive cost branches:
	//
	//   rightsize — a smaller in-family shape exists: the delta between the
	//       current and candidate monthly cost, confidence 0.8.
	//
	//   stop_delete — no smaller shape exists: the whole current monthly
	//       cost is reclaimable by stopping the VM, confidence 0.5.
	//       This is a Finding, NOT a skip: skips and findings are disjoint
	//       sets, so no skip code is ever recorded here; the no-candidate
	//       fact is surfaced as evidence instead.
	candidate, candidateMonthly, hasCandidate := findCandidate(ctx, vp, p, family, vcpu, cpu, region, osType)

	var (
		monthlyWaste  float64
		confidence    float64
		recMachine    string
		recMonthly    float64
		noSmallerSize bool
		label         string
	)
	if hasCandidate {
		monthlyWaste = currentMonthly - candidateMonthly
		confidence = 0.8
		recMachine = candidate.Name
		recMonthly = candidateMonthly
		label = "rightsize"
	} else {
		noSmallerSize = true
		monthlyWaste = currentMonthly
		confidence = 0.5
		recMachine = "stop/delete"
		recMonthly = 0
		label = "stop_delete"
	}

	// Stash evidence inputs. ExtraEvidence has no Pass to ask the pricer,
	// so the monthly figures are computed here and stashed.
	nc.Set("currency", rules.CurrencyOf(p))
	nc.Set("recommended_machine_type", recMachine)
	nc.Set("current_monthly", currentMonthly)
	nc.Set("recommended_monthly", recMonthly)
	nc.Set("no_smaller_size", noSmallerSize)

	return []rules.CostBranch{{
		Waste:      pricing.Round2(monthlyWaste),
		Confidence: round2(confidence),
		Label:      label,
	}}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

// EvidenceKeys returns nil: every evidence entry is formatted with a non-%v
// layout (p95_cpu as %.2f%%, the monthly figures as $%.2f) or produced from a
// computed value, so ExtraEvidence renders the whole list.
func (rule) EvidenceKeys() []string { return nil }

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	vmSize, _ := n.Str(azurerules.AttrVMSize)
	p95, _ := nc.Get("p95_cpu")
	samples, _ := nc.Get("cpu_samples")
	recMachine, _ := nc.Get("recommended_machine_type")
	currentMonthly, _ := nc.Get("current_monthly")
	recMonthly, _ := nc.Get("recommended_monthly")

	ev := []rules.Evidence{
		{Key: "p95_cpu", Value: fmt.Sprintf("%.2f%%", p95.(float64)*100)},
		{Key: "vm_size", Value: vmSize},
		{Key: "recommended_machine_type", Value: recMachine.(string)},
		rules.EvMoneyIn("current_monthly", curStr, currentMonthly.(float64), 2),
		rules.EvMoneyIn("recommended_monthly", curStr, recMonthly.(float64), 2),
		{Key: "samples", Value: fmt.Sprintf("%d", samples.(int))},
	}
	if noSmaller, _ := nc.Get("no_smaller_size"); noSmaller.(bool) {
		ev = append(ev, rules.Evidence{Key: "no_smaller_size", Value: "true"})
	}
	return ev
}

// vmMonthlyCost returns the monthly cost for one Azure VM size by calling
// VMPrice and multiplying by HoursPerMonth. A price lookup failure is returned
// as an error; the caller (Cost) returns it so the engine records SkipNoPrice —
// never a $0 assumption (Invariant I4).
func vmMonthlyCost(ctx context.Context, vp vmPricer, region, size, osType string) (float64, error) {
	hourly, err := vp.VMPrice(ctx, region, size, osType)
	if err != nil {
		return 0, err
	}
	return hourly * pricing.HoursPerMonth, nil
}

// findCandidate walks the region-specific family ladder (ascending by VCPU)
// and returns the smallest shape whose projected p95 CPU utilization stays at
// or below TargetCPUUtil, per the cost model: candidate vCPU count C satisfies
// p95CPU * currentVCPU / C <= TargetCPUUtil.
//
// It does NOT cross families: only shapes from the same authoritative family
// are considered. When the Sizer implements pricing.RegionalSizer, the ladder
// is taken from LadderInRegion so a VM is never recommended a size that is not
// available in its region. If the Sizer is only a plain pricing.Sizer, the
// provider-neutral Ladder is used as a fallback.
func findCandidate(ctx context.Context, vp vmPricer, p *rules.Pass, family string, currentVCPU, p95CPU float64, region, osType string) (pricing.MachineSpec, float64, bool) {
	if p.Sizer == nil {
		return pricing.MachineSpec{}, 0, false
	}

	var ladder []pricing.MachineSpec
	if rs, ok := p.Sizer.(pricing.RegionalSizer); ok {
		ladder = rs.LadderInRegion(family, region)
	} else {
		ladder = p.Sizer.Ladder(family)
	}

	for _, cand := range ladder {
		if cand.VCPU < MinCandidateVCPU || cand.MemoryGiB < MinCandidateMemGiB {
			continue
		}
		if cand.VCPU >= currentVCPU {
			continue // must be strictly smaller to be a rightsizing win
		}
		projected := p95CPU * currentVCPU / cand.VCPU
		if projected <= TargetCPUUtil {
			monthly, err := vmMonthlyCost(ctx, vp, region, cand.Name, osType)
			if err != nil {
				continue
			}
			return cand, monthly, true
		}
	}
	return pricing.MachineSpec{}, 0, false
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
