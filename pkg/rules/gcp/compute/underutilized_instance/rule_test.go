package underutilized_instance

import (
	"context"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
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
