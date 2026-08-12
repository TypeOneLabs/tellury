// Package underutilized_instance implements the "underutilized_instance"
// FinOps rule: a running, on-demand EC2 instance whose p95 CPU utilization is
// low enough that a smaller instance type (or stop/delete) would suffice.
//
// It is the AWS mirror of the GCP underutilized_instance rule: the same guard
// structure with typed skip codes, the same two mutually exclusive cost
// branches (rightsize vs. stop_delete), and the same thresholds. The AWS
// counterpart of the GCP MIG guard is the Auto Scaling group member guard,
// detected via the aws:autoscaling:groupName tag.
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
	awsrules "github.com/TypeOneLabs/tellury/pkg/rules/aws"
)

// ID is the stable rule identifier. It is distinct from the GCP rule's
// "underutilized_instance" because the rule registry keys on Meta().ID across
// all providers, and two rules may not share an ID.
const ID = "underutilized_ec2"

// Thresholds verbatim from the GCP rule spec §7.1. They are sound starting
// points; none differ from the GCP rule because the same overprovision,
// target-utilization, sample-count, coverage, age, and noise-floor invariants
// hold for AWS EC2 instances.
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

// skipManagedByASG is the skip code for an instance that belongs to an
// Auto Scaling group. An ASG owns its members' size and count; recommending
// a resize for one member is advice an operator cannot act on, and the
// group's own sizing is a separate concern. This is the AWS equivalent of
// the GCP rule's SkipManagedByMIG.
const skipManagedByASG rules.SkipCode = "managed_by_asg"

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "aws",
		Service:  "ec2",
		Title:    "Instance is significantly overprovisioned for its CPU load",
		Description: "A running, on-demand instance whose p95 CPU utilization " +
			"leaves more than 40% headroom versus a smaller instance type in " +
			"the same family. Rightsizing (or stopping) reclaims the delta.",
		Severity:           rules.SeverityHigh,
		RequiredAssetTypes: []string{awsrules.TypeInstance},
		RequiredMetrics:    []string{metrics.KeyCPUUtilizationP95},
		Remediation:        "aws ec2 modify-instance-attribute --instance-id ID --instance-type \"{\\\"Value\\\": \\\"CANDIDATE\\\"}\"",
		Origin:             "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindInstance }

// Guards are evaluated in order; the first to return false decides the
// SkipCode recorded for the node. Every guard has a distinct skip code so
// --explain-skips separates them.
func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		// P0: not an Auto Scaling group member. An ASG member carries the
		// aws:autoscaling:groupName tag, written to Labels by the normalizer
		// from the instance's Tags. The same logic as the GCP MIG guard: a
		// group owns its members' size; recommending a resize for a single
		// member is unactionable noise. Distinct skip code so
		// --explain-skips shows these separately.
		{Name: "not_asg_member", SkipCode: skipManagedByASG,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				_, ok := n.Label("aws:autoscaling:groupName")
				return !ok
			}},

		// P1: running. The presence check is a SEPARATE guard from the value
		// check, mirroring the GCP rule's status_present. Collapsing them
		// reports "instance is not running" for an instance whose state
		// attribute simply failed to parse — a diagnosis the operator would
		// act on incorrectly.
		{Name: "state_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(awsrules.AttrState)
				return ok && v != ""
			}},
		// EC2 InstanceState.Name is lowercase ("running"), unlike GCP's
		// uppercase "RUNNING".
		{Name: "running", SkipCode: rules.SkipNotRunning,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				s, _ := n.Str(awsrules.AttrState)
				return s == "running"
			}},

		// P2: not spot. InstanceLifecycle empty means on-demand; "spot"
		// means the instance already pays a steeply discounted rate.
		{Name: "not_spot", SkipCode: rules.SkipSpot,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				lifecycle, _ := n.Str(awsrules.AttrLifecycle)
				return lifecycle != awsrules.LifecycleSpot
			}},

		// P3: shape known — instance_type, vCPU, memory, and family all
		// present and valid. Four guards share SkipUnknownMachineType,
		// each independently named so --explain-skips can show which field
		// was missing.
		{Name: "instance_type_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(awsrules.AttrInstanceType)
				return ok && v != ""
			}},
		{Name: "vcpu_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Num(awsrules.AttrVCpuCount)
				return ok && v >= 1
			}},
		{Name: "memory_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Num(awsrules.AttrMemoryGiB)
				return ok && v > 0
			}},
		{Name: "family_valid", SkipCode: rules.SkipUnknownMachineType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(awsrules.AttrMachineFamily)
				return ok && v != ""
			}},

		// P4: old enough. The parse check and the age gate are separate
		// guards so a missing/unparseable launch_time keeps its distinct
		// missing-attribute code (SkipMissingAttr) instead of collapsing
		// onto too_young.
		{Name: "launch_time_parseable", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				ts, ok := n.Str(awsrules.AttrLaunchTime)
				if !ok {
					return false
				}
				_, err := time.Parse(time.RFC3339, ts)
				return err == nil
			}},
		{Name: "old_enough", SkipCode: rules.SkipTooYoung,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				ts, _ := n.Str(awsrules.AttrLaunchTime)
				createdAt, _ := time.Parse(time.RFC3339, ts)
				return p.Now.Sub(createdAt).Hours()/24 >= MinAgeDays
			}},

		// P5: CPU data present. The metric gate stashes the p95 value and
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

		// P6: overprovisioned. Ratio uses the current shape's headroom:
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

// instancePricer is the subset of *aws.CatalogPricer the rule needs to price
// an EC2 instance. The live CatalogPricer implements this via its
// InstancePrice method; test fakes implement it directly.
type instancePricer interface {
	InstancePrice(ctx context.Context, region, instanceType, operatingSystem string) (float64, error)
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	// All shape attrs are guaranteed present and valid by the guards.
	instanceType, _ := n.Str(awsrules.AttrInstanceType)
	family, _ := n.Str(awsrules.AttrMachineFamily)
	vcpu, _ := n.Num(awsrules.AttrVCpuCount)
	platform, _ := n.Str(awsrules.AttrPlatform)
	os := awsOSForPlatform(platform)
	region := pricing.RegionOf(n.Location)

	ip, ok := p.Price.(instancePricer)
	if !ok {
		return nil, fmt.Errorf("underutilized_instance: pricer does not support instance pricing")
	}

	// P7: current shape monthly cost.
	currentMonthly, err := instanceMonthlyCost(ctx, ip, region, instanceType, os)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never $0 (Invariant I4)
	}

	p95, _ := nc.Get("p95_cpu")
	cpu := p95.(float64)

	// P8: candidate exists. Two mutually exclusive cost branches:
	//
	//   rightsize — a smaller in-family shape exists: the delta between the
	//       current and candidate monthly cost, confidence 0.8.
	//
	//   stop_delete — no smaller shape exists: the whole current monthly
	//       cost is reclaimable by stopping the instance, confidence 0.5.
	//       This is a Finding, NOT a skip: skips and findings are disjoint
	//       sets, so no skip code is ever recorded here; the no-candidate
	//       fact is surfaced as evidence instead.
	candidate, candidateMonthly, hasCandidate := findCandidate(ctx, p, ip, family, vcpu, cpu, region, os)

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
// layout (p95_cpu as %.2f%%, the monthly figures as $%.2f) or produced from
// a computed value, so ExtraEvidence renders the whole list.
func (rule) EvidenceKeys() []string { return nil }

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	instanceType, _ := n.Str(awsrules.AttrInstanceType)
	p95, _ := nc.Get("p95_cpu")
	samples, _ := nc.Get("cpu_samples")
	recMachine, _ := nc.Get("recommended_machine_type")
	currentMonthly, _ := nc.Get("current_monthly")
	recMonthly, _ := nc.Get("recommended_monthly")

	ev := []rules.Evidence{
		{Key: "p95_cpu", Value: fmt.Sprintf("%.2f%%", p95.(float64)*100)},
		{Key: "instance_type", Value: instanceType},
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

// instanceMonthlyCost returns the monthly cost for an EC2 instance type by
// calling InstancePrice and multiplying by HoursPerMonth. A price lookup
// failure is returned as an error; the caller (Cost) returns it so the engine
// records SkipNoPrice — never a $0 assumption (Invariant I4).
func instanceMonthlyCost(ctx context.Context, ip instancePricer, region, instanceType, os string) (float64, error) {
	hourly, err := ip.InstancePrice(ctx, region, instanceType, os)
	if err != nil {
		return 0, err
	}
	return hourly * pricing.HoursPerMonth, nil
}

// findCandidate walks the family ladder (ascending by VCPU) and returns the
// smallest shape whose projected p95 CPU utilization stays at or below
// TargetCPUUtil, per the cost model: candidate vCPU count C satisfies
// p95CPU * currentVCPU / C <= TargetCPUUtil.
//
// It does NOT cross families (t3 -> m5 changes performance characteristics
// the tool cannot reason about), and does NOT recommend burstable-to-fixed or
// fixed-to-burstable moves. Only shapes from p.Sizer.Ladder(family) are
// considered — the same family as the current instance type.
func findCandidate(ctx context.Context, p *rules.Pass, ip instancePricer, family string, currentVCPU, p95CPU float64, region, os string) (pricing.MachineSpec, float64, bool) {
	if p.Sizer == nil {
		return pricing.MachineSpec{}, 0, false
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
			monthly, err := instanceMonthlyCost(ctx, ip, region, cand.Name, os)
			if err != nil {
				continue
			}
			return cand, monthly, true
		}
	}
	return pricing.MachineSpec{}, 0, false
}

// awsOSForPlatform maps an EC2 instance's Platform attribute (from
// DescribeInstances) to the operatingSystem value the AWS Price List API's
// GetProducts filter expects. It mirrors pricing/aws.OSForPlatform so the
// rule does not import a cloud pricing package.
//
//	"" or "linux/unix"  → "Linux"
//	"windows"           → "Windows"
//	"rhel"              → "RHEL"
//	"suse"              → "SUSE"
func awsOSForPlatform(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "", "linux/unix":
		return "Linux"
	case "windows":
		return "Windows"
	case "rhel":
		return "RHEL"
	case "suse":
		return "SUSE"
	default:
		return platform
	}
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
