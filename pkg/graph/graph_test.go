package graph

import (
	"fmt"
	"sync"
	"testing"
)

// TestSetMetric_ConcurrentWritesAreSafe is the regression test for the
// known defect: SetMetric had no synchronization, while the Graph doc
// comment claimed the graph was safe for concurrent use after Freeze().
// Metric enrichment is about to be parallelized across nodes, so this must
// hold under the race detector (`go test -race`).
func TestSetMetric_ConcurrentWritesAreSafe(t *testing.T) {
	g := New()
	const nodeCount = 64
	ids := make([]Ref, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		id := Ref(fmt.Sprintf("node-%d", i))
		if err := g.AddNode(&Node{ID: id, Kind: KindInstance, Name: string(id)}); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		ids = append(ids, id)
	}
	g.Freeze()

	const writersPerNode = 8
	var wg sync.WaitGroup
	for _, id := range ids {
		for w := 0; w < writersPerNode; w++ {
			wg.Add(1)
			go func(id Ref, w int) {
				defer wg.Done()
				g.SetMetric(id, "cpu_utilization_p95", MetricValue{
					Value:   float64(w),
					Samples: w + 1,
				})
			}(id, w)
		}
	}
	wg.Wait()

	// Every node must have received exactly one (racily-decided, but
	// non-corrupted) value for the key - the assertion here is really just
	// "this ran clean under -race and left the map in a valid state".
	for _, id := range ids {
		n, ok := g.Node(id)
		if !ok {
			t.Fatalf("node %s missing after concurrent SetMetric", id)
		}
		mv, ok := n.Metrics["cpu_utilization_p95"]
		if !ok {
			t.Fatalf("node %s has no metric value after concurrent SetMetric", id)
		}
		if mv.Samples < 1 || mv.Samples > writersPerNode {
			t.Fatalf("node %s has out-of-range Samples=%d, map likely corrupted", id, mv.Samples)
		}
	}
}

// TestSetMetric_UnknownNodeIsNoop keeps the pre-existing contract explicit:
// SetMetric on a Ref that was never added must be a safe no-op, not a panic.
func TestSetMetric_UnknownNodeIsNoop(t *testing.T) {
	g := New()
	g.Freeze()
	g.SetMetric(Ref("does-not-exist"), "k", MetricValue{Value: 1, Samples: 1})
}
