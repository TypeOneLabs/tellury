package underutilized_instance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	gcprules "github.com/TypeOneLabs/tellury/pkg/rules/gcp"
)

// fakePricer prices exactly one catalog machine type so instanceMonthlyCost
// succeeds via the KindVMInstance branch; every other lookup misses,
// matching ErrNoPrice semantics.
type fakePricer struct {
	sku  string
	unit float64
}

func (f fakePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if kind == pricing.KindVMInstance && sku == f.sku {
		return f.unit, region, nil
	}
	return 0, "", pricing.ErrNoPrice
}

func (f fakePricer) MonthlyCost(it pricing.Item) (float64, error) {
	unit, _, err := f.UnitPrice(it.Kind, it.Provider, it.SKU, it.Region)
	if err != nil {
		return 0, err
	}
	return unit * it.Quantity, nil
}

// multiSKUPrice resolves KindVMInstance for exactly the machine types in the
// map; every other lookup misses with ErrNoPrice, so an unrelated pricing
// dimension can never mask a broken predicate. The rightsize branch needs
// BOTH the current shape and the candidate shape priced in one Pass.
type multiSKUPrice map[string]float64 // machine type -> USD unit price

func (m multiSKUPrice) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if kind == pricing.KindVMInstance {
		if unit, ok := m[sku]; ok {
			return unit, region, nil
		}
	}
	return 0, "", pricing.ErrNoPrice
}

func (m multiSKUPrice) MonthlyCost(it pricing.Item) (float64, error) {
	unit, _, err := m.UnitPrice(it.Kind, it.Provider, it.SKU, it.Region)
	if err != nil {
		return 0, err
	}
	return unit * it.Quantity, nil
}

// fakeSizer implements pricing.Sizer over a fixed family ladder. findCandidate
// only calls Ladder, but Spec/Family are implemented anyway so the type
// satisfies the interface completely and behaves like a real catalog.
type fakeSizer struct {
	ladder []pricing.MachineSpec
}

func (f fakeSizer) Spec(machineType string) (pricing.MachineSpec, bool) {
	for _, s := range f.ladder {
		if s.Name == machineType {
			return s, true
		}
	}
	return pricing.MachineSpec{}, false
}

func (f fakeSizer) Family(machineType string) string {
	for _, s := range f.ladder {
		if s.Name == machineType {
			return s.Family
		}
	}
	return ""
}

func (f fakeSizer) Ladder(family string) []pricing.MachineSpec { return f.ladder }

// buildNode constructs a single RUNNING, non-spot, non-accelerated instance
// old enough and with metric coverage good enough to clear every predicate
// up through P8, leaving only the "candidate exists?" branch (P9) to decide
// the outcome.
func buildNode(now time.Time) *graph.Node {
	n := &graph.Node{
		ID:       graph.Ref("//compute.googleapis.com/projects/p/zones/z/instances/i1"),
		Kind:     graph.KindInstance,
		Name:     "i1",
		Project:  "p",
		Location: "us-central1-a",
	}
	n.SetAttr("machine_type", "n1-standard-4")
	n.SetAttr("vcpu_count", 4.0)
	n.SetAttr("memory_gib", 15.0)
	n.SetAttr("machine_family", "n1-standard")
	n.SetAttr("status", "RUNNING")
	n.SetAttr("creation_timestamp", now.Add(-30*24*time.Hour).Format(time.RFC3339))
	n.Metrics = map[string]graph.MetricValue{
		metrics.KeyCPUUtilizationP95: {
			Value:      0.10, // deep overprovision: ratio 0.90 > 0.40 threshold
			Samples:    200,
			Coverage:   0.9,
			WindowDays: 7,
		},
	}
	return n
}

// TestEval_NoSmallerSize_NoDoubleCounting is the regression test for the
// known defect: when no smaller machine type exists, the rule must emit a
// Finding recommending stop/delete WITHOUT also recording a SkipNode call
// for the same resource. Skips and findings are documented as disjoint sets;
// double-counting broke `--explain-skips` accounting.
func TestEval_NoSmallerSize_NoDoubleCounting(t *testing.T) {
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	g := graph.New()
	n := buildNode(now)
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: fakePricer{sku: "n1-standard-4", unit: 0.5}, // -> $365/mo, well above MinMonthlyWasteUSD
		Sizer: nil,                                         // no Sizer => findCandidate always reports "no candidate"
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
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}

	// The defect: SkipNode(SkipNoSmallerSize) was called on the very same
	// node that also produced a Finding. Assert it is never recorded at all.
	if got := skipCounts[rules.SkipNoSmallerSize]; got != 0 {
		t.Errorf("SkipNoSmallerSize recorded %d times for a resource that also produced a finding; want 0 (skips and findings must be disjoint)", got)
	}

	// The fact itself must not be lost - it belongs on the Finding as
	// evidence now.
	found := false
	for _, ev := range f.Evidence {
		if ev.Key == "no_smaller_size" && ev.Value == "true" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected evidence {no_smaller_size: true} on the finding, got %+v", f.Evidence)
	}
	if f.MonthlyWasteUSD <= 0 {
		t.Errorf("expected positive MonthlyWasteUSD, got %v", f.MonthlyWasteUSD)
	}
}

// TestEval_StopDeleteBranch_Fires pins the stop/delete fallback branch with
// EXACT numbers: when no smaller shape qualifies, the rule reports the FULL
// current monthly cost (0.5 * HoursPerMonth = $365.00) at confidence 0.5,
// with no_smaller_size=true evidence. A mutation that shifts the fallback to
// a partial delta or a different confidence fails this test.
func TestEval_StopDeleteBranch_Fires(t *testing.T) {
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	g := graph.New()
	n := buildNode(now)
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: fakePricer{sku: "n1-standard-4", unit: 0.5},
		Sizer: fakeSizer{ladder: nil}, // no smaller shape => stop/delete branch
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
		t.Fatalf("want 1 finding from the stop/delete branch, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	wantWaste := round2(0.5 * pricing.HoursPerMonth)
	if f.MonthlyWasteUSD != wantWaste {
		t.Errorf("MonthlyWasteUSD = %v, want %v (FULL current cost when no smaller shape qualifies)", f.MonthlyWasteUSD, wantWaste)
	}
	if f.Confidence != 0.5 {
		t.Errorf("Confidence = %v, want 0.5 for the stop/delete branch", f.Confidence)
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
	if len(skipCounts) != 0 {
		t.Errorf("expected zero skips for a stop/delete finding, got %+v", skipCounts)
	}
}

// TestEval_RightsizeBranch_Fires is the rule's PRIMARY detection path: with
// a real Sizer supplying a family ladder, a deep-overprovisioned
// n1-standard-4 (p95 10%) finds the smallest in-family shape whose projected
// p95 stays at or below TargetCPUUtil (n1-standard-1, projected p95 40%).
// The rightsize branch fires with waste = current.cost - candidate.cost and
// confidence 0.8, and the candidate (plus its monthly cost) appears as
// evidence. This test fails if findCandidate is broken to always return "no
// candidate" (e.g. the ladder is emptied): the stop/delete branch would fire
// instead, with full cost and confidence 0.5.
func TestEval_RightsizeBranch_Fires(t *testing.T) {
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	g := graph.New()
	n := buildNode(now)
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	sizer := fakeSizer{ladder: []pricing.MachineSpec{
		{Name: "n1-standard-1", Family: "n1-standard", VCPU: 1, MemoryGiB: 3.75},
		{Name: "n1-standard-2", Family: "n1-standard", VCPU: 2, MemoryGiB: 7.5},
		{Name: "n1-standard-4", Family: "n1-standard", VCPU: 4, MemoryGiB: 15},
	}}

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: multiSKUPrice(map[string]float64{
			"n1-standard-1": 0.125,
			"n1-standard-2": 0.25,
			"n1-standard-4": 0.5,
		}),
		Sizer: sizer,
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
		t.Fatalf("want 1 finding from the rightsize branch, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}

	currentMonthly := 0.5 * pricing.HoursPerMonth
	candidateMonthly := 0.125 * pricing.HoursPerMonth
	wantWaste := round2(currentMonthly - candidateMonthly)
	if f.MonthlyWasteUSD != wantWaste {
		t.Errorf("MonthlyWasteUSD = %v, want %v (current $%.2f - candidate $%.2f)", f.MonthlyWasteUSD, wantWaste, currentMonthly, candidateMonthly)
	}
	if f.Confidence != 0.8 {
		t.Errorf("Confidence = %v, want 0.8 for the rightsize branch", f.Confidence)
	}
	if len(skipCounts) != 0 {
		t.Errorf("expected zero skips for a rightsize finding, got %+v", skipCounts)
	}

	// The candidate must be the smallest qualifying shape and appear as
	// evidence with its monthly cost.
	foundRec, foundCur, foundRecMonthly := false, false, false
	for _, ev := range f.Evidence {
		if ev.Key == "recommended_machine_type" && ev.Value == "n1-standard-1" {
			foundRec = true
		}
		if ev.Key == "current_monthly" && ev.Value == fmt.Sprintf("$%.2f", round2(currentMonthly)) {
			foundCur = true
		}
		if ev.Key == "recommended_monthly" && ev.Value == fmt.Sprintf("$%.2f", round2(candidateMonthly)) {
			foundRecMonthly = true
		}
	}
	if !foundRec {
		t.Errorf("expected evidence recommended_machine_type=n1-standard-1, got %+v", f.Evidence)
	}
	if !foundCur {
		t.Errorf("expected evidence current_monthly=$%.2f, got %+v", round2(currentMonthly), f.Evidence)
	}
	if !foundRecMonthly {
		t.Errorf("expected evidence recommended_monthly=$%.2f, got %+v", round2(candidateMonthly), f.Evidence)
	}
}

// TestEval_GuardSkipCodes pins every predicate that the firing fixture must
// be able to FAIL, by mutating ONE attribute of the otherwise-valid
// buildNode and asserting the exact SkipCode. Before these existed, six
// guards could be deleted outright without any test failing — running,
// not_spot, no_accelerators, old_enough, cpu_data_present and
// overprovisioned were never forced to fail. Each subtest is the regression
// test for one guard: deleting the guard's check (or breaking its threshold)
// makes the case below produce a finding instead of the asserted skip.
func TestEval_GuardSkipCodes(t *testing.T) {
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		mutate func(n *graph.Node)
		want   rules.SkipCode
	}{
		{
			name: "not running",
			mutate: func(n *graph.Node) {
				n.SetAttr("status", "STOPPED")
			},
			want: rules.SkipNotRunning,
		},
		{
			name: "spot instance",
			mutate: func(n *graph.Node) {
				n.SetAttr("provisioning_model", "SPOT")
			},
			want: rules.SkipSpot,
		},
		{
			name: "accelerator present",
			mutate: func(n *graph.Node) {
				n.SetAttr("accelerator_count", 1.0)
			},
			want: rules.SkipAccelerator,
		},
		{
			name: "too young",
			mutate: func(n *graph.Node) {
				n.SetAttr("creation_timestamp", now.Add(-2*24*time.Hour).Format(time.RFC3339))
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
						Value:      0.90, // ratio 1-0.90=0.10 <= 0.40 threshold => NOT overprovisioned
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
			g := graph.New()
			n := buildNode(now)
			tc.mutate(n)
			if err := g.AddNode(n); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
			g.Freeze()

			skipCounts := map[rules.SkipCode]int{}
			p := &rules.Pass{
				Graph: g,
				Price: fakePricer{sku: "n1-standard-4", unit: 0.5},
				Sizer: nil,
				Now:   now,
				Skip: func(ruleID string, id graph.Ref, code rules.SkipCode) {
					skipCounts[code]++
				},
			}
			findings, err := rules.AdaptNodeRule(rule{}).Eval(context.Background(), p)
			if err != nil {
				t.Fatalf("Eval returned error: %v", err)
			}
			if len(findings) != 0 {
				t.Fatalf("want 0 findings, got %d (%+v)", len(findings), findings)
			}
			if got := skipCounts[tc.want]; got != 1 {
				t.Errorf("SkipCode %q recorded %d times, want exactly 1; skips=%+v", tc.want, got, skipCounts)
			}
		})
	}
}

// TestEval_SkipMIGMember asserts that an instance whose `created-by` metadata
// item resolves to an instanceGroupManagers resource (i.e. a managed instance
// group member) is skipped with the distinct SkipManagedByMIG reason, and
// produces NO finding. A MIG owns its members' size and count; recommending a
// resize for one member is advice an operator cannot act on.
func TestEval_SkipMIGMember(t *testing.T) {
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	g := graph.New()
	n := buildNode(now)
	// Real CAI shape for a MIG member's created-by metadata: a creator
	// self-link that resolves to an instanceGroupManagers resource.
	n.SetAttr(gcprules.AttrCreatedBy,
		"projects/p/zones/us-central1-a/instanceGroupManagers/web-mig")
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: fakePricer{sku: "n1-standard-4", unit: 0.5},
		Sizer: nil,
		Now:   now,
		Skip: func(ruleID string, id graph.Ref, code rules.SkipCode) {
			skipCounts[code]++
		},
	}

	findings, err := rules.AdaptNodeRule(rule{}).Eval(context.Background(), p)
	if err != nil {
		t.Fatalf("Eval returned error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("want 0 findings for a MIG member, got %d (%+v)", len(findings), findings)
	}
	if got := skipCounts[rules.SkipManagedByMIG]; got != 1 {
		t.Fatalf("SkipManagedByMIG recorded %d times, want exactly 1 (this instance is a MIG member)", got)
	}
	// No other reason may fire for a clean MIG member.
	for code, count := range skipCounts {
		if code != rules.SkipManagedByMIG {
			t.Errorf("unexpected skip reason %q recorded %d times; a MIG member must be skipped solely by SkipManagedByMIG", code, count)
		}
	}
}

// TestEval_NonMIG_EvaluatesNormally asserts that an instance with NO
// `created-by` MIG marker still evaluates normally: it is NOT skipped by
// SkipManagedByMIG and, with no Sizer, reaches the no-candidate stop/delete
// branch and emits a Finding (the same behavior as a plain instance).
func TestEval_NonMIG_EvaluatesNormally(t *testing.T) {
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	g := graph.New()
	n := buildNode(now)
	// No created-by MIG marker: this is a standalone instance. For good
	// measure an unrelated created-by value must NOT trigger the skip.
	n.SetAttr(gcprules.AttrCreatedBy,
		"projects/p/zones/us-central1-a/instances/web-0")
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: fakePricer{sku: "n1-standard-4", unit: 0.5},
		Sizer: nil, // no Sizer => no candidate => stop/delete finding
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
		t.Fatalf("want 1 finding for a non-MIG instance, got %d (%+v)", len(findings), findings)
	}
	if f := findings[0]; f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	if got := skipCounts[rules.SkipManagedByMIG]; got != 0 {
		t.Errorf("SkipManagedByMIG recorded %d times for a non-MIG instance; want 0", got)
	}
}
