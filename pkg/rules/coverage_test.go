package rules

import (
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// TestMetricsBlocked_FixtureIsHonest is the regression test for the offense
// where a `--fixture` replay (raw Cloud Asset Inventory JSON, no metric series)
// silently looked like "no waste" for the metric-dependent rules. The returned
// set must name every metric-requiring rule that the graph cannot evaluate.
func TestMetricsBlocked_FixtureIsHonest(t *testing.T) {
	// A graph with metric-dependent nodes but NO metrics at all — exactly the
	// shape a raw CAI fixture produces, since fixtures carry no series.
	g := graph.New()
	g.AddNode(&graph.Node{ID: "//compute.../instances/i1", Kind: graph.KindInstance})
	g.AddNode(&graph.Node{ID: "//storage.../b1", Kind: graph.KindBucket})
	g.Freeze()

	// Two metric-requiring rules and one topology-only rule. Only the metric
	// ones should be reported blocked.
	metricRule := RuleFunc{M: Meta{ID: "underutilized_instance", RequiredMetrics: []string{"cpu_utilization_p95"}}}
	metricRule2 := RuleFunc{M: Meta{ID: "no_lifecycle_policy", RequiredMetrics: []string{"bucket_total_bytes_mean"}}}
	topoRule := RuleFunc{M: Meta{ID: "detached_disk"}}

	blocked := MetricsBlocked([]Rule{metricRule, metricRule2, topoRule}, g)
	if len(blocked) != 2 {
		t.Fatalf("blocked = %v; want the 2 metric-requiring rules (topology rule must NOT be listed)", blocked)
	}
	if blocked[0] != "no_lifecycle_policy" || blocked[1] != "underutilized_instance" {
		t.Fatalf("blocked unsorted or wrong = %v; want [no_lifecycle_policy underutilized_instance]", blocked)
	}
}

// TestMetricsBlocked_CachedSnapshotUnblocks verifies the flip side: a graph
// replayed from a cache file carries the serialized Metrics map, so the same
// rules are NOT blocked — the cached-snapshot path is full fidelity.
func TestMetricsBlocked_CachedSnapshotUnblocks(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:   "//compute.../instances/i1",
		Kind: graph.KindInstance,
		Metrics: map[string]graph.MetricValue{
			"cpu_utilization_p95": {Value: 0.1, Samples: 200, Coverage: 0.9},
		},
	})
	g.Freeze()

	metricRule := RuleFunc{M: Meta{ID: "underutilized_instance", RequiredMetrics: []string{"cpu_utilization_p95"}}}

	blocked := MetricsBlocked([]Rule{metricRule}, g)
	if len(blocked) != 0 {
		t.Fatalf("cached-snapshot replay must NOT report the rule blocked; got %v", blocked)
	}
}

// TestMetricsBlocked_SampleZeroIsAbsent guards against treating a metric entry
// with Samples==0 (a placeholder a cache may carry) as data: SampleLess is
// absent, per invariant I5, and must keep the rule blocked.
func TestMetricsBlocked_SampleZeroIsAbsent(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:   "//compute.../instances/i1",
		Kind: graph.KindInstance,
		Metrics: map[string]graph.MetricValue{
			"cpu_utilization_p95": {Value: 0.1, Samples: 0}, // no real data
		},
	})
	g.Freeze()

	metricRule := RuleFunc{M: Meta{ID: "underutilized_instance", RequiredMetrics: []string{"cpu_utilization_p95"}}}

	blocked := MetricsBlocked([]Rule{metricRule}, g)
	if len(blocked) != 1 {
		t.Fatalf("a Samples==0 metric entry must count as absent; blocked=%v", blocked)
	}
}
