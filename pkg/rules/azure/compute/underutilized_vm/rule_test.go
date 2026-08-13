// Package underutilized_vm tests exercise the underutilized_vm rule against
// synthetic graph nodes shaped exactly like the Azure normalizer's output
// (pkg/cloud/azure/normalize.go): a VM node with the attributes
// NormalizeVM writes and the shape attributes the Resource SKUs sizer hydrates,
// plus a CPU metric value the Azure Monitor enrichment pass stores.
//
// The discipline mirrors the AWS and GCP rule tests: every firing case asserts
// the EXACT monthly waste figure, every skip path asserts the SPECIFIC SkipCode
// (never merely "nothing fired"), and each price-driven branch uses a fake
// pricer that resolves only the VM sizes under test so an unrelated lookup
// cannot mask a broken predicate.
package underutilized_vm

import (
	"context"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	azurerules "github.com/TypeOneLabs/tellury/pkg/rules/azure"
)

// ── test fakes ─────────────────────────────────────────────────────────────

// testPricer implements both pricing.Pricer and vmPricer. It prices exactly
// the VM sizes in the map; every other lookup misses with pricing.ErrNoPrice,
// so an unrelated pricing dimension can never mask a broken predicate.
type testPricer struct {
	prices map[string]float64 // "size/os" -> USD hourly rate
}

func (tp testPricer) VMPrice(_ context.Context, _ string, size, osType string) (float64, error) {
	key := size + "/" + osType
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

// testSizer implements pricing.Sizer and pricing.RegionalSizer over a fixed
// family ladder. findCandidate only calls LadderInRegion, but the other
// methods are implemented anyway so the type satisfies both interfaces
// completely.
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

func (s testSizer) SpecInRegion(machineType, region string) (pricing.MachineSpec, bool) {
	return s.Spec(machineType)
}

func (s testSizer) LadderInRegion(family, region string) []pricing.MachineSpec {
	return s.ladder
}

// ── helpers ────────────────────────────────────────────────────────────────

// launched is the wall-clock launch instant every VM fixture anchors to.
var launched = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// buildNode returns a fully valid firing fixture: a running, standalone,
// regular Linux D4as_v5 VM, created 30 days ago, with p95 CPU at 10% (deep
// overprovision). D4as_v5 has a smaller in-family sibling (D2as_v5), so the
// primary fixture exercises the rightsize branch. Individual tests mutate
// specific attributes to force a desired skip branch.
func buildNode(now time.Time) *graph.Node {
	n := &graph.Node{
		ID:        graph.Ref("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-1"),
		Kind:      graph.KindInstance,
		Name:      "vm-1",
		Provider:  "azure",
		Service:   "microsoft.compute",
		AssetType: azurerules.TypeVM,
		Project:   "sub",
		Location:  "swedencentral",
	}
	n.SetAttr(azurerules.AttrVMSize, "Standard_D4as_v5")
	n.SetAttr(azurerules.AttrPowerState, PowerStateRunning)
	n.SetAttr(azurerules.AttrPriority, "")
	n.SetAttr(azurerules.AttrOSType, "Linux")
	n.SetAttr(azurerules.AttrTimeCreated, launched.Format(time.RFC3339))
	n.SetAttr(azurerules.AttrVMSSID, "")
	n.SetAttr(azurerules.AttrVCpuCount, 4.0)
	n.SetAttr(azurerules.AttrMemoryGiB, 16.0)
	n.SetAttr(azurerules.AttrMachineFamily, "standardDasv5Family")
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

// dasv5Pricer prices the Dasv5 sizes used in the rightsize tests. Published
// On-Demand Linux rates (swedencentral, as of the recorded fixture):
//
//	Standard_D2as_v5 — $0.092/hr
//	Standard_D4as_v5 — $0.184/hr
func dasv5Pricer() testPricer {
	return testPricer{prices: map[string]float64{
		"Standard_D2as_v5/Linux": 0.092,
		"Standard_D4as_v5/Linux": 0.184,
	}}
}

// dasv5Ladder returns a Sizer with the standardDasv5Family ladder:
// D2as_v5 (2 vCPU) → D4as_v5 (4 vCPU) → D8as_v5 (8 vCPU), sorted ascending.
// D4as_v5 CAN rightsize to D2as_v5; D2as_v5 cannot rightsize and is the
// correct stop_delete case for this family.
func dasv5Ladder() testSizer {
	return testSizer{ladder: []pricing.MachineSpec{
		{Name: "Standard_D2as_v5", Family: "standardDasv5Family", VCPU: 2, MemoryGiB: 8},
		{Name: "Standard_D4as_v5", Family: "standardDasv5Family", VCPU: 4, MemoryGiB: 16},
		{Name: "Standard_D8as_v5", Family: "standardDasv5Family", VCPU: 8, MemoryGiB: 32},
	}}
}

// ── firing cases ───────────────────────────────────────────────────────────

// TestEval_RightsizeBranch_Fires is the rule's PRIMARY detection path. A
// D4as_v5 (4 vCPU) at 10% p95 CPU finds the smallest in-family shape whose
// projected p95 stays at or below TargetCPUUtil. The ladder is D2as_v5
// (2 vCPU) → D4as_v5 (4 vCPU) → D8as_v5 (8 vCPU) ascending. The first
// qualifying candidate is D2as_v5 with projected utilization
// 0.10 * 4 / 2 = 0.20 (20%), well under 60%.
//
// Waste = $0.184*730 - $0.092*730 = $134.32 - $67.16 = $67.16.
func TestEval_RightsizeBranch_Fires(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	findings, skips := runEval(t, []*graph.Node{n}, dasv5Pricer(), dasv5Ladder(), now)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding from the rightsize branch, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}

	currentMonthly := 0.184 * pricing.HoursPerMonth   // $134.32
	candidateMonthly := 0.092 * pricing.HoursPerMonth // $67.16
	wantWaste := pricing.Round2(currentMonthly - candidateMonthly)
	if f.MonthlyWasteUSD != wantWaste {
		t.Errorf("MonthlyWasteUSD = %v, want %v (current $0.184*730 - candidate $0.092*730)",
			f.MonthlyWasteUSD, wantWaste)
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
	if byKey["recommended_machine_type"] != "Standard_D2as_v5" {
		t.Errorf("evidence recommended_machine_type = %q, want Standard_D2as_v5", byKey["recommended_machine_type"])
	}
	if byKey["p95_cpu"] != "10.00%" {
		t.Errorf("evidence p95_cpu = %q, want 10.00%%", byKey["p95_cpu"])
	}
	if byKey["vm_size"] != "Standard_D4as_v5" {
		t.Errorf("evidence vm_size = %q, want Standard_D4as_v5", byKey["vm_size"])
	}
}

// TestEval_StopDeleteBranch_Fires pins the stop/delete fallback branch with
// EXACT numbers. When no smaller shape qualifies, the rule reports the FULL
// current monthly cost ($0.184 * 730 = $134.32) at confidence 0.5, with
// no_smaller_size=true evidence.
func TestEval_StopDeleteBranch_Fires(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)

	// No Sizer → no candidate → stop_delete branch.
	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"Standard_D4as_v5/Linux": 0.184}},
		nil, now)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding from the stop_delete branch, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}

	wantWaste := pricing.Round2(0.184 * pricing.HoursPerMonth)
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

// TestEval_RightsizeBranch_DoesNotCrossFamilies asserts that findCandidate
// only considers shapes in the SAME family. When the Sizer returns an empty
// ladder for the family, the stop_delete branch fires — crossing families
// (Dasv5 → Easv5) changes performance characteristics the tool cannot reason
// about.
func TestEval_RightsizeBranch_DoesNotCrossFamilies(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)

	// A Sizer that returns an empty ladder: no shapes in the family.
	sizer := testSizer{ladder: nil}
	pr := testPricer{prices: map[string]float64{"Standard_D4as_v5/Linux": 0.184}}

	findings, skips := runEval(t, []*graph.Node{n}, pr, sizer, now)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding from the stop_delete branch (no cross-family candidate), got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
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

// TestEval_D2as_v5_StopDeleteBranch documents the real Azure family shape:
// the Dasv5 family starts at 2 vCPUs, so Standard_D2as_v5 has no smaller
// in-family sibling and can only ever be a stop_delete candidate. This is
// correct behaviour, not a bug — the same is true of AWS t3 below xlarge.
func TestEval_D2as_v5_StopDeleteBranch(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	n.SetAttr(azurerules.AttrVMSize, "Standard_D2as_v5")
	n.SetAttr(azurerules.AttrVCpuCount, 2.0)
	n.SetAttr(azurerules.AttrMemoryGiB, 8.0)

	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"Standard_D2as_v5/Linux": 0.092}},
		dasv5Ladder(), now)

	if len(findings) != 1 {
		t.Fatalf("want 1 stop_delete finding for D2as_v5, got %d (%+v)", len(findings), findings)
	}
	if findings[0].Confidence != 0.5 {
		t.Errorf("Confidence = %v, want 0.5 for D2as_v5 stop_delete", findings[0].Confidence)
	}
	if findings[0].MonthlyWasteUSD != pricing.Round2(0.092*pricing.HoursPerMonth) {
		t.Errorf("MonthlyWasteUSD = %v, want %v", findings[0].MonthlyWasteUSD, pricing.Round2(0.092*pricing.HoursPerMonth))
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a stop_delete finding, got %+v", skips)
	}
}

// TestEval_CPUPercentageFractionConversion pins the percentage-to-fraction
// conversion: the graph stores CPU utilization as a FRACTION (0–1), never as a
// percentage (0–100). If the enrichment layer failed to divide by 100, a 10%
// utilized VM would store 10.0 and the overprovision guard would compute
// 1 - 10.0 = -9.0, which is NOT > 0.40, and the node would be skipped.
func TestEval_CPUPercentageFractionConversion(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"Standard_D4as_v5/Linux": 0.184}},
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
			name: "missing vmss attr",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, azurerules.AttrVMSSID)
			},
			want: rules.SkipMissingAttr,
		},
		{
			name: "missing vm_size",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, azurerules.AttrVMSize)
			},
			want: rules.SkipUnknownMachineType,
		},
		{
			name: "missing vcpu_count",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, azurerules.AttrVCpuCount)
			},
			want: rules.SkipUnknownMachineType,
		},
		{
			name: "missing memory_gib",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, azurerules.AttrMemoryGiB)
			},
			want: rules.SkipUnknownMachineType,
		},
		{
			name: "missing machine_family",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, azurerules.AttrMachineFamily)
			},
			want: rules.SkipUnknownMachineType,
		},
		{
			name: "missing power state",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, azurerules.AttrPowerState)
			},
			want: rules.SkipMissingAttr,
		},
		{
			name: "not running",
			mutate: func(n *graph.Node) {
				n.SetAttr(azurerules.AttrPowerState, "PowerState/deallocated")
			},
			want: rules.SkipNotRunning,
		},
		{
			name: "missing priority",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, azurerules.AttrPriority)
			},
			want: rules.SkipMissingAttr,
		},
		{
			name: "spot vm",
			mutate: func(n *graph.Node) {
				n.SetAttr(azurerules.AttrPriority, "Spot")
			},
			want: rules.SkipSpot,
		},
		{
			name: "missing os type",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, azurerules.AttrOSType)
			},
			want: rules.SkipMissingAttr,
		},
		{
			name: "missing time_created",
			mutate: func(n *graph.Node) {
				delete(n.Attrs, azurerules.AttrTimeCreated)
			},
			want: rules.SkipMissingAttr,
		},
		{
			name: "unparseable time_created",
			mutate: func(n *graph.Node) {
				n.SetAttr(azurerules.AttrTimeCreated, "not-a-timestamp")
			},
			want: rules.SkipMissingAttr,
		},
		{
			name: "too young",
			mutate: func(n *graph.Node) {
				n.SetAttr(azurerules.AttrTimeCreated, now.Add(-2*24*time.Hour).Format(time.RFC3339))
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := buildNode(now)
			tc.mutate(n)
			findings, skips := runEval(t, []*graph.Node{n},
				testPricer{prices: map[string]float64{"Standard_D4as_v5/Linux": 0.184}},
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

// TestEval_SkipScaleSetMember asserts that a VM carrying a non-empty
// virtual_machine_scale_set_id is skipped with the distinct managed_by_vmss
// reason, and produces NO finding. A scale-set VM cannot be resized
// individually.
func TestEval_SkipScaleSetMember(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	n.SetAttr(azurerules.AttrVMSSID,
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss")

	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"Standard_D4as_v5/Linux": 0.184}},
		nil, now)

	if len(findings) != 0 {
		t.Fatalf("want 0 findings for a scale-set member, got %d (%+v)", len(findings), findings)
	}
	if got := skips[skipManagedByVMSS]; got != 1 {
		t.Fatalf("managed_by_vmss recorded %d times, want exactly 1 (this VM is a scale-set member)", got)
	}
	for code, count := range skips {
		if code != skipManagedByVMSS {
			t.Errorf("unexpected skip reason %q recorded %d times; a scale-set member must be skipped solely by managed_by_vmss", code, count)
		}
	}
}

// TestEval_StandaloneVM_EvaluatesNormally asserts that a VM with an EMPTY
// virtual_machine_scale_set_id still evaluates normally.
func TestEval_StandaloneVM_EvaluatesNormally(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	n.SetAttr(azurerules.AttrVMSSID, "")

	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"Standard_D4as_v5/Linux": 0.184}},
		nil, now)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding for a standalone VM, got %d (%+v)", len(findings), findings)
	}
	if f := findings[0]; f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	if got := skips[skipManagedByVMSS]; got != 0 {
		t.Errorf("managed_by_vmss recorded %d times for a standalone VM; want 0", got)
	}
}

// TestEval_NoPrice_Skips covers the price gate: the rule must never assume $0
// for a VM whose size/OS/region cannot be priced (Invariant I4).
func TestEval_NoPrice_Skips(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{}}, // nothing priced
		nil, now)

	if len(findings) != 0 {
		t.Fatalf("VM with no price must not fire at $0, got %+v", findings)
	}
	if skips[rules.SkipNoPrice] != 1 {
		t.Errorf("want SkipNoPrice recorded once, got %+v", skips)
	}
}

// TestEval_BelowMinWaste_Skips covers the noise floor: a VM whose computed
// waste falls under $1.00/month must skip as below_min_waste.
func TestEval_BelowMinWaste_Skips(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	// $0.001/hr → $0.73/month, below the $1.00 floor.
	findings, skips := runEval(t, []*graph.Node{n},
		testPricer{prices: map[string]float64{"Standard_D4as_v5/Linux": 0.001}},
		nil, now)

	if len(findings) != 0 {
		t.Fatalf("sub-noise-floor VM must not fire, got %+v", findings)
	}
	if skips[rules.SkipBelowMinWaste] != 1 {
		t.Errorf("want SkipBelowMinWaste recorded once, got %+v", skips)
	}
}

// TestEval_SkipsAndFindingsDisjoint guards the invariant that a node either
// produces a finding or records a skip, never both.
func TestEval_SkipsAndFindingsDisjoint(t *testing.T) {
	now := launched.Add(30 * 24 * time.Hour)
	n := buildNode(now)
	findings, skips := runEval(t, []*graph.Node{n},
		dasv5Pricer(), dasv5Ladder(), now)

	if len(findings) != 1 {
		t.Fatalf("expected the firing fixture to produce exactly one finding, got %d", len(findings))
	}
	if len(skips) != 0 {
		t.Errorf("a firing VM must record zero skips (skips and findings are disjoint), got %+v", skips)
	}
}

// ── meta ───────────────────────────────────────────────────────────────────

// TestMeta_IDIsStable is the stability guard.
func TestMeta_IDIsStable(t *testing.T) {
	if got := (rule{}).Meta().ID; got != ID {
		t.Errorf("Meta().ID = %q, want %q", got, ID)
	}
	if got := (rule{}).Meta().Provider; got != "azure" {
		t.Errorf("Meta().Provider = %q, want azure", got)
	}
	if got := (rule{}).Meta().Service; got != "compute" {
		t.Errorf("Meta().Service = %q, want compute", got)
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
		t.Fatalf("RequiredAssetTypes = %v, want exactly [azure.compute.vm]", m.RequiredAssetTypes)
	}
	if m.RequiredAssetTypes[0] != azurerules.TypeVM {
		t.Errorf("RequiredAssetTypes[0] = %q, want %q", m.RequiredAssetTypes[0], azurerules.TypeVM)
	}
}
