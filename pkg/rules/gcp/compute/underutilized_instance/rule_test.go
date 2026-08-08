package underutilized_instance

import (
	"context"
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

	findings, err := rule{}.Eval(context.Background(), p)
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

	findings, err := rule{}.Eval(context.Background(), p)
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

	findings, err := rule{}.Eval(context.Background(), p)
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
