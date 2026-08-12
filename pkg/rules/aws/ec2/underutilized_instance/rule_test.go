// Package underutilized_instance_test exercises the underutilized_instance
// rule against synthetic graph nodes shaped exactly like the AWS normalizer's
// output (pkg/cloud/aws/normalize.go): an instance node with the attributes
// the DescribeInstances + DescribeInstanceTypes path writes, and a CPU metric
// value the CloudWatch enrichment pass stores.
//
// The discipline mirrors the GCP rule tests: every firing case asserts the
// EXACT monthly waste figure, every skip path asserts the SPECIFIC SkipCode
// (never merely "nothing fired"), and each price-driven branch uses a fake
// Pricer that resolves only the instance types under test so an unrelated
// lookup cannot mask a broken predicate.
package underutilized_instance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	awsrules "github.com/TypeOneLabs/tellury/pkg/rules/aws"
)

// ── test fakes ─────────────────────────────────────────────────────────────

// testPricer implements both pricing.Pricer and instancePricer. It prices
// exactly the instance types in the map; every other lookup misses with
// ErrNoPrice, so an unrelated pricing dimension can never mask a broken
// predicate.
type testPricer struct {
	prices map[string]float64 // "instanceType/os" -> USD hourly rate
}

func (tp testPricer) InstancePrice(_ context.Context, region, instanceType, operatingSystem string) (float64, error) {
	key := instanceType + "/" + operatingSystem
	if price, ok := tp.prices[key]; ok {
		return price, nil
	}
	return 0, pricing.ErrNoPrice
}

func (tp testPricer) UnitPrice(_ pricing.Kind, _, _, _ string) (float64, string, error) {
	return 0, "", pricing.ErrNoPrice
}

func (tp testPricer) MonthlyCost(_ pricing.Item) (float64, error) {
	return 0, pricing.ErrNoPrice
}

// testSizer implements pricing.Sizer over a fixed family ladder. findCandidate
// only calls Ladder, but Spec/Family are implemented anyway so the type
// satisfies the interface completely.
type testSizer struct {
	ladder []pricing.MachineSpec
}

func (s testSizer) Spec(machineType string) (pricing.MachineSpec, bool) {
	for _, spec := range s.ladder {
		if spec.Name == machineType {
			return spec, true
		}
	}
	return pricing.MachineSpec{}, false
}

func (s testSizer) Family(machineType string) string {
	for _, spec := range s.ladder {
		if spec.Name == machineType {
			return spec.Family
		}
	}
	return ""
}

func (s testSizer) Ladder(family string) []pricing.MachineSpec { return s.ladder }

// ── helpers ────────────────────────────────────────────────────────────────

// launched is the wall-clock launch instant every instance fixture anchors to.
var launched = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// buildNode returns a fully valid firing fixture: a running, on-demand
// c6i.xlarge instance, launched 30 days ago, with p95 CPU at 10% (deep
// overprovision). Uses c6i (not t3) because t3 shapes share the same vCPU
// count, and findCandidate requires a strictly smaller vCPU count.
// Individual tests mutate specific attributes to force a desired skip branch.
func buildNode(now time.Time) *graph.Node {
	n := &graph.Node{
		ID:       graph.Ref("accounts/123456789012/regions/us-east-1/instances/i-0cafe"),
		Kind:     graph.KindInstance,
		Name:     "i-0cafe",
		Provider: "aws",
		Project:  "123456789012",
		Location: "us-east-1a",
	}
	n.SetAttr(awsrules.AttrInstanceType, "c6i.xlarge")
	n.SetAttr(awsrules.AttrState, "running")
	n.SetAttr(awsrules.AttrVCpuCount, 4.0)
	n.SetAttr(awsrules.AttrMemoryGiB, 8.0)
	n.SetAttr(awsrules.AttrMachineFamily, "c6i")
	n.SetAttr(awsrules.AttrLaunchTime, launched.Format(time.RFC3339))
	n.SetAttr(awsrules.AttrPlatform, "")
	n.SetAttr(awsrules.AttrLifecycle, "")
	n.SetAttr(awsrules.AttrProvisioningModel, awsrules.ProvisioningStandard)
	n.SetAttr(awsrules.AttrArchitecture, "x86_64")
	n.SetAttr(awsrules.AttrTenancy, "default")
	n.SetAttr(awsrules.AttrInstanceID, "i-0cafe")
	n.Metrics = map[string]graph.MetricValue{
		metrics.KeyCPUUtilizationP95: {
			Value:      0.10, // 10% p95 CPU → overprovision ratio 0.90 > 0.40
			Samples:    200,
			Coverage:   0.9,
			WindowDays: 7,
		},
	}
	return n
}

// runEval builds a graph from the given nodes, freezes it, and runs the rule
// through the real adapter (rules.AdaptNodeRule), returning findings and the
// skip tally. now defaults to launched+30d when zero.
func runEval(t *testing.T, nodes []*graph.Node, pricer pricing.Pricer, sizer pricing.Sizer, now time.Time) ([]rules.Finding, map[rules.SkipCode]int) {
	t.Helper()
	if now.IsZero() {
		now = launched.Add(30 * 24 * time.Hour)
	}
	g := graph.New()
	for _, n := range nodes {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: pricer,
		Sizer: sizer,
		Now:   now,
		Skip: func(ruleID string, id graph.Ref, code rules.SkipCode) {
			skipCounts[code]++
		},
	}
	findings, err := rules.AdaptNodeRule(rule{}).Eval(context.Background(), p)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	return findings, skipCounts
}

// c6iPricer prices the c6i family instance types used in tests.
// Published On-Demand Linux rates (us-east-1, as of pricing data):
//
//	c6i.large   — $0.085/hr
//	c6i.xlarge  — $0.17/hr
func c6iPricer() testPricer {
	return testPricer{prices: map[string]float64{
		"c6i.large/Linux":  0.085,
		"c6i.xlarge/Linux": 0.17,
	}}
}

// c6iLadder returns a Sizer with the c6i family ladder: c6i.large (2 vCPU)
// → c6i.xlarge (4 vCPU) → c6i.2xlarge (8 vCPU), sorted ascending by VCPU.
func c6iLadder() testSizer {
	return testSizer{ladder: []pricing.MachineSpec{
		{Name: "c6i.large", Family: "c6i", VCPU: 2, MemoryGiB: 4},
		{Name: "c6i.xlarge", Family: "c6i", VCPU: 4, MemoryGiB: 8},
		{Name: "c6i.2xlarge", Family: "c6i", VCPU: 8, MemoryGiB: 16},
	}}
}

// ── firing cases ───────────────────────────────────────────────────────────

// TestEval_RightsizeBranch_Fires is the rule's PRIMARY detection path. A
// c6i.xlarge (4 vCPU) at 10% p95 CPU finds the smallest in-family shape whose
// projected p95 stays at or below TargetCPUUtil. The ladder is c6i.large
// (2 vCPU) → c6i.xlarge (4 vCPU) → c6i.2xlarge (8 vCPU) ascending.
// The first qualifying candidate is c6i.large with projected utilization
// 0.10 * 4 / 2 = 0.20 (20%), well under 60%.
//
// Waste = $0.17*730 - $0.085*730 = $124.10 - $62.05 = $62.05.
func TestEval_RightsizeBranch_Fires(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	findings, skips := runEval(t, []*graph.Node{n}, c6iPricer(), c6iLadder(), now)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding from the rightsize branch, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}

	currentMonthly := 0.17 * pricing.HoursPerMonth    // $124.10
	candidateMonthly := 0.085 * pricing.HoursPerMonth // $62.05
	wantWaste := pricing.Round2(currentMonthly - candidateMonthly)
	if f.MonthlyWasteUSD != wantWaste {
		t.Errorf("MonthlyWasteUSD = %v, want %v (current $%.3f*730 - candidate $%.3f*730)",
			f.MonthlyWasteUSD, wantWaste, 0.17, 0.085)
	}
	if f.Confidence != 0.8 {
		t.Errorf("Confidence = %v, want 0.8 for the rightsize branch", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a rightsize finding, got %+v", skips)
	}

	// Evidence: the candidate must be the smallest qualifying shape.
	byKey := map[string]string{}
	for _, e := range f.Evidence {
		byKey[e.Key] = e.Value
	}
	if byKey["recommended_machine_type"] != "c6i.large" {
		t.Errorf("evidence recommended_machine_type = %q, want c6i.large", byKey["recommended_machine_type"])
	}
	if byKey["p95_cpu"] != "10.00%" {
		t.Errorf("evidence p95_cpu = %q, want 10.00%%", byKey["p95_cpu"])
	}
	if byKey["instance_type"] != "c6i.xlarge" {
		t.Errorf("evidence instance_type = %q, want c6i.xlarge", byKey["instance_type"])
	}
}

// TestEval_RightsizeBranch_DoesNotCrossFamilies asserts that findCandidate
// only considers shapes in the SAME family. When the Sizer returns an empty
// ladder for the family, the stop_delete branch fires — crossing families
// (c6i → m5) changes performance characteristics the tool cannot reason about.
func TestEval_RightsizeBranch_DoesNotCrossFamilies(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)

	// A Sizer that returns an empty ladder: no shapes in the family.
	sizer := testSizer{ladder: nil}
	pr := testPricer{prices: map[string]float64{"c6i.xlarge/Linux": 0.17}}

	findings, skips := runEval(t, []*graph.Node{n}, pr, sizer, now)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding from the stop_delete branch (no cross-family candidate), got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	// Stop/delete: full current cost at confidence 0.5.
	if f.Confidence != 0.5 {
		t.Errorf("Confidence = %v, want 0.5 for the stop_delete branch", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a stop_delete finding, got %+v", skips)
	}

	byKey := map[string]string{}
	for _, e := range f.Evidence {
		byKey[e.Key] = e.Value
	}
	if byKey["no_smaller_size"] != "true" {
		t.Errorf("evidence no_smaller_size = %q, want true", byKey["no_smaller_size"])
	}
	if byKey["recommended_machine_type"] != "stop/delete" {
		t.Errorf("evidence recommended_machine_type = %q, want stop/delete", byKey["recommended_machine_type"])
	}
}

// TestEval_StopDeleteBranch_Fires pins the stop/delete fallback branch with
// EXACT numbers. When no smaller shape qualifies, the rule reports the FULL
// current monthly cost ($0.17 * 730 = $124.10) at confidence 0.5, with
// no_smaller_size=true evidence.
func TestEval_StopDeleteBranch_Fires(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)

	// No Sizer → no candidate → stop_delete branch.
	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"c6i.xlarge/Linux": 0.17}},
		nil, now)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding from the stop_delete branch, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}

	wantWaste := pricing.Round2(0.17 * pricing.HoursPerMonth)
	if f.MonthlyWasteUSD != wantWaste {
		t.Errorf("MonthlyWasteUSD = %v, want %v (FULL current cost when no smaller shape qualifies)",
			f.MonthlyWasteUSD, wantWaste)
	}
	if f.Confidence != 0.5 {
		t.Errorf("Confidence = %v, want 0.5 for the stop/delete branch", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a stop_delete finding, got %+v", skips)
	}

	found := false
	for _, ev := range f.Evidence {
		if ev.Key == "no_smaller_size" && ev.Value == "true" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected evidence no_smaller_size=true on the stop/delete finding, got %+v", f.Evidence)
	}
}

// TestEval_NoSmallerSize_NoDoubleCounting is the regression test for the
// known defect: when no smaller instance type exists, the rule must emit a
// Finding recommending stop/delete WITHOUT also recording a SkipNode call
// for the same resource.
func TestEval_NoSmallerSize_NoDoubleCounting(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)

	skipCounts := map[rules.SkipCode]int{}
	g := graph.New()
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	p := &rules.Pass{
		Graph: g,
		Price: testPricer{prices: map[string]float64{"c6i.xlarge/Linux": 0.17}},
		Sizer: nil, // no Sizer → stop_delete
		Now:   now,
		Skip: func(ruleID string, id graph.Ref, code rules.SkipCode) {
			skipCounts[code]++
		},
	}

	findings, err := rules.AdaptNodeRule(rule{}).Eval(context.Background(), p)
	if err != nil {
		t.Fatalf("Eval returned error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}

	if got := skipCounts[rules.SkipNoSmallerSize]; got != 0 {
		t.Errorf("SkipNoSmallerSize recorded %d times for a resource that also produced a finding; want 0 (skips and findings must be disjoint)", got)
	}

	found := false
	for _, ev := range findings[0].Evidence {
		if ev.Key == "no_smaller_size" && ev.Value == "true" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected evidence {no_smaller_size: true} on the finding, got %+v", findings[0].Evidence)
	}
}

// TestEval_CPUPercentageFractionConversion pins the percentage-to-fraction
// conversion: the graph stores CPU utilization as a FRACTION (0–1), never as
// a percentage (0–100). If the enrichment layer failed to divide by 100, a
// 10% utilized instance would store 10.0, which after clamping to [0,1] would
// become 1.0 — the instance appears 100% utilized and the overprovision guard
// would skip it.
func TestEval_CPUPercentageFractionConversion(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	// p95_cpu = 0.10 (fraction, 10%). If the rule mistakenly expects a
	// percentage (10.0), the overprovision check would compute 1 - 10.0 = -9.0
	// which is NOT > 0.40, and the node would be skipped.
	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"c6i.xlarge/Linux": 0.17}},
		nil, now)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding (CPU at 0.10 fraction = 10%%), got %d; "+
			"if the rule expected a percentage (10.0) instead of a fraction, "+
			"the overprovision guard would skip this node. skips=%+v", len(findings), skips)
	}
	for _, ev := range findings[0].Evidence {
		if ev.Key == "p95_cpu" && ev.Value != "10.00%" {
			t.Errorf("evidence p95_cpu = %q, want 10.00%% (fraction 0.10 * 100)", ev.Value)
		}
	}
}

// ── guard skip paths ───────────────────────────────────────────────────────

// TestEval_GuardSkipCodes pins every predicate that the firing fixture must
// be able to FAIL, by mutating ONE attribute of the otherwise-valid buildNode
// and asserting the exact SkipCode.
func TestEval_GuardSkipCodes(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)

	cases := []struct {
		name   string
		mutate func(n *graph.Node)
		want   rules.SkipCode
	}{
		{
			name: "not running",
			mutate: func(n *graph.Node) {
				n.SetAttr(awsrules.AttrState, "stopped")
			},
			want: rules.SkipNotRunning,
		},
		{
			name: "spot instance",
			mutate: func(n *graph.Node) {
				n.SetAttr(awsrules.AttrLifecycle, awsrules.LifecycleSpot)
				n.SetAttr(awsrules.AttrProvisioningModel, awsrules.ProvisioningSpot)
			},
			want: rules.SkipSpot,
		},
		{
			name: "missing instance_type",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, awsrules.AttrInstanceType)
			},
			want: rules.SkipUnknownMachineType,
		},
		{
			name: "missing vcpu_count",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, awsrules.AttrVCpuCount)
			},
			want: rules.SkipUnknownMachineType,
		},
		{
			name: "missing memory_gib",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, awsrules.AttrMemoryGiB)
			},
			want: rules.SkipUnknownMachineType,
		},
		{
			name: "missing machine_family",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, awsrules.AttrMachineFamily)
			},
			want: rules.SkipUnknownMachineType,
		},
		{
			name: "missing launch_time",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, awsrules.AttrLaunchTime)
			},
			want: rules.SkipMissingAttr,
		},
		{
			name: "too young",
			mutate: func(n *graph.Node) {
				n.SetAttr(awsrules.AttrLaunchTime, now.Add(-2*24*time.Hour).Format(time.RFC3339))
			},
			want: rules.SkipTooYoung,
		},
		{
			name: "no cpu metric",
			mutate: func(n *graph.Node) {
				n.Metrics = nil
			},
			want: rules.SkipNoMetric,
		},
		{
			name: "below overprovision threshold",
			mutate: func(n *graph.Node) {
				n.Metrics = map[string]graph.MetricValue{
					metrics.KeyCPUUtilizationP95: {
						Value:      0.90, // ratio 1-0.90=0.10 <= 0.40 → NOT overprovisioned
						Samples:    200,
						Coverage:   0.9,
						WindowDays: 7,
					},
				}
			},
			want: rules.SkipBelowOverprovision,
		},
		{
			name: "low cpu coverage",
			mutate: func(n *graph.Node) {
				n.Metrics = map[string]graph.MetricValue{
					metrics.KeyCPUUtilizationP95: {
						Value:      0.10,
						Samples:    200,
						Coverage:   0.30, // below MinCoverageReq 0.50
						WindowDays: 7,
					},
				}
			},
			want: rules.SkipNoMetric,
		},
		{
			name: "low cpu samples",
			mutate: func(n *graph.Node) {
				n.Metrics = map[string]graph.MetricValue{
					metrics.KeyCPUUtilizationP95: {
						Value:      0.10,
						Samples:    100, // below MinSamplesReq 168
						Coverage:   0.9,
						WindowDays: 7,
					},
				}
			},
			want: rules.SkipNoMetric,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := buildNode(now)
			tc.mutate(n)
			findings, skips := runEval(t, []*graph.Node{n},
				testPricer{prices: map[string]float64{"c6i.xlarge/Linux": 0.17}},
				nil, now)

			if len(findings) != 0 {
				t.Fatalf("want 0 findings, got %d (%+v)", len(findings), findings)
			}
			if got := skips[tc.want]; got != 1 {
				t.Errorf("SkipCode %q recorded %d times, want exactly 1; skips=%+v", tc.want, got, skips)
			}
		})
	}
}

// TestEval_SkipASGMember asserts that an instance carrying the
// aws:autoscaling:groupName tag is skipped with the distinct managed_by_asg
// reason, and produces NO finding.
func TestEval_SkipASGMember(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	n.Labels = map[string]string{"aws:autoscaling:groupName": "web-asg"}

	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"c6i.xlarge/Linux": 0.17}},
		nil, now)

	if len(findings) != 0 {
		t.Fatalf("want 0 findings for an ASG member, got %d (%+v)", len(findings), findings)
	}
	if got := skips[skipManagedByASG]; got != 1 {
		t.Fatalf("managed_by_asg recorded %d times, want exactly 1 (this instance is an ASG member)", got)
	}
	for code, count := range skips {
		if code != skipManagedByASG {
			t.Errorf("unexpected skip reason %q recorded %d times; an ASG member must be skipped solely by managed_by_asg", code, count)
		}
	}
}

// TestEval_NonASG_EvaluatesNormally asserts that an instance with NO
// aws:autoscaling:groupName tag still evaluates normally.
func TestEval_NonASG_EvaluatesNormally(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	n.Labels = map[string]string{"Environment": "production"}

	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"c6i.xlarge/Linux": 0.17}},
		nil, now)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding for a non-ASG instance, got %d (%+v)", len(findings), findings)
	}
	if f := findings[0]; f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	if got := skips[skipManagedByASG]; got != 0 {
		t.Errorf("managed_by_asg recorded %d times for a non-ASG instance; want 0", got)
	}
}

// TestEval_NoPrice_Skips covers the price gate: the rule must never assume $0
// for an instance whose type/OS/region cannot be priced (Invariant I4).
func TestEval_NoPrice_Skips(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{}}, // nothing priced
		nil, now)

	if len(findings) != 0 {
		t.Fatalf("instance with no price must not fire at $0, got %+v", findings)
	}
	if skips[rules.SkipNoPrice] != 1 {
		t.Errorf("want SkipNoPrice recorded once, got %+v", skips)
	}
}

// TestEval_BelowMinWaste_Skips covers the noise floor: an instance whose
// computed waste falls under $1.00/month must skip as below_min_waste.
func TestEval_BelowMinWaste_Skips(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	// Use a tiny rate: $0.001/hr → $0.73/month, below the $1.00 floor.
	n.SetAttr(awsrules.AttrInstanceType, "t4g.nano")
	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"t4g.nano/Linux": 0.001}},
		nil, now)

	if len(findings) != 0 {
		t.Fatalf("sub-noise-floor instance must not fire, got %+v", findings)
	}
	if skips[rules.SkipBelowMinWaste] != 1 {
		t.Errorf("want SkipBelowMinWaste recorded once, got %+v", skips)
	}
}

// TestEval_TelluryExemptLabel_Skips is the P0 short-circuit.
func TestEval_TelluryExemptLabel_Skips(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	n.Labels = map[string]string{"tellury-exempt": "true"}
	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"c6i.xlarge/Linux": 0.17}},
		nil, now)

	if len(findings) != 0 {
		t.Fatalf("exempt instance must not fire, got %+v", findings)
	}
	if skips[rules.SkipExemptLabel] != 1 {
		t.Errorf("want SkipExemptLabel recorded once, got %+v", skips)
	}
}

// TestEval_SkipsAndFindingsDisjoint guards the invariant that a node either
// produces a finding or records a skip, never both.
func TestEval_SkipsAndFindingsDisjoint(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"c6i.xlarge/Linux": 0.17}},
		nil, now)

	if len(findings) != 1 {
		t.Fatalf("expected the firing fixture to produce exactly one finding, got %d", len(findings))
	}
	if len(skips) != 0 {
		t.Errorf("a firing instance must record zero skips (skips and findings are disjoint), got %+v", skips)
	}
}

// TestEval_EvidenceRendering verifies the evidence output for a stop_delete
// finding.
func TestEval_EvidenceRendering(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	findings, _ := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"c6i.xlarge/Linux": 0.17}},
		nil, now)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(findings))
	}

	f := findings[0]
	byKey := map[string]string{}
	for _, e := range f.Evidence {
		byKey[e.Key] = e.Value
	}

	checks := []struct{ key, want string }{
		{"p95_cpu", "10.00%"},
		{"instance_type", "c6i.xlarge"},
		{"recommended_machine_type", "stop/delete"},
		{"no_smaller_size", "true"},
	}
	for _, c := range checks {
		if got := byKey[c.key]; got != c.want {
			t.Errorf("evidence %s = %q, want %q", c.key, got, c.want)
		}
	}

	if !strings.HasPrefix(byKey["current_monthly"], "$") {
		t.Errorf("evidence current_monthly = %q, want $-prefixed", byKey["current_monthly"])
	}
	if byKey["recommended_monthly"] != "$0.00" {
		t.Errorf("evidence recommended_monthly = %q, want $0.00", byKey["recommended_monthly"])
	}
	if _, ok := byKey["samples"]; !ok {
		t.Errorf("evidence missing samples key")
	}
}

// TestEval_OSForPlatform tests the platform-to-OS mapping.
func TestEval_OSForPlatform(t *testing.T) {
	cases := []struct {
		platform string
		wantOS   string
	}{
		{"", "Linux"},
		{"linux/unix", "Linux"},
		{"windows", "Windows"},
		{"rhel", "RHEL"},
		{"suse", "SUSE"},
		{"Windows", "Windows"},
		{"RHEL", "RHEL"},
	}
	for _, tc := range cases {
		got := awsOSForPlatform(tc.platform)
		if got != tc.wantOS {
			t.Errorf("awsOSForPlatform(%q) = %q, want %q", tc.platform, got, tc.wantOS)
		}
	}
}

// TestEval_ConfidenceRounding checks round2.
func TestEval_ConfidenceRounding(t *testing.T) {
	if got := round2(0.80); got != 0.80 {
		t.Errorf("round2(0.80) = %v, want 0.80", got)
	}
	if got := round2(0.50); got != 0.50 {
		t.Errorf("round2(0.50) = %v, want 0.50", got)
	}
	if got := round2(0.855); got != 0.86 {
		t.Errorf("round2(0.855) = %v, want 0.86", got)
	}
}

// TestMeta_IDIsStable is the stability guard.
func TestMeta_IDIsStable(t *testing.T) {
	if got := (rule{}).Meta().ID; got != ID {
		t.Errorf("Meta().ID = %q, want %q", got, ID)
	}
	if got := (rule{}).Meta().Provider; got != "aws" {
		t.Errorf("Meta().Provider = %q, want aws", got)
	}
	if got := (rule{}).Meta().Service; got != "ec2" {
		t.Errorf("Meta().Service = %q, want ec2", got)
	}
}

// TestMeta_RequiredMetrics declares cpu_utilization_p95.
func TestMeta_RequiredMetrics(t *testing.T) {
	m := (rule{}).Meta()
	if len(m.RequiredMetrics) != 1 {
		t.Fatalf("RequiredMetrics = %v, want exactly [cpu_utilization_p95]", m.RequiredMetrics)
	}
	if m.RequiredMetrics[0] != metrics.KeyCPUUtilizationP95 {
		t.Errorf("RequiredMetrics[0] = %q, want %q", m.RequiredMetrics[0], metrics.KeyCPUUtilizationP95)
	}
}

// TestMeta_RequiredAssetTypes is the stability guard for the asset type.
func TestMeta_RequiredAssetTypes(t *testing.T) {
	m := (rule{}).Meta()
	if len(m.RequiredAssetTypes) != 1 {
		t.Fatalf("RequiredAssetTypes = %v, want exactly [aws.ec2.instance]", m.RequiredAssetTypes)
	}
	if m.RequiredAssetTypes[0] != awsrules.TypeInstance {
		t.Errorf("RequiredAssetTypes[0] = %q, want %q", m.RequiredAssetTypes[0], awsrules.TypeInstance)
	}
}
