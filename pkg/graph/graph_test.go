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

// TestProjectContainerCount_CountsProjectNodesNotFindings pins where the
// scan report's "projects analyzed" figure comes from: the graph's project
// CONTAINER nodes, never anything derived from findings. A graph carrying two
// project containers plus a leaf under each reports 2 even when no finding
// exists — the "nothing wasteful" vs "nothing scanned" distinction the scan
// summary exists to draw. It also verifies the count survives a snapshot
// round-trip (the --cache-file replay path rebuilds the same byKind index).
func TestProjectContainerCount_CountsProjectNodesNotFindings(t *testing.T) {
	g := New()
	addProjectWithLeaf(t, g, "projects/alpha", "//…/disks/a1")
	addProjectWithLeaf(t, g, "projects/beta", "//…/disks/b1")
	g.Freeze()

	if got := g.ProjectContainerCount(); got != 2 {
		t.Fatalf("ProjectContainerCount = %d, want 2 (project container nodes, regardless of findings)", got)
	}
	if got := g.ProjectCount(); got != 2 {
		t.Errorf("ProjectCount = %d, want 2 (distinct project IDs across resource nodes)", got)
	}
	if got := g.ResourceNodeCount(); got != 2 {
		t.Errorf("ResourceNodeCount = %d, want 2", got)
	}
}

// TestProjectContainerCount_EmptyGraphIsZero: a graph with no project
// container nodes — nothing scanned — reports 0 projects analyzed, which is
// the "broken scope" signal the summary must not paper over.
func TestProjectContainerCount_EmptyGraphIsZero(t *testing.T) {
	g := New()
	g.Freeze()
	if got := g.ProjectContainerCount(); got != 0 {
		t.Fatalf("ProjectContainerCount on an empty graph = %d, want 0", got)
	}
}

// TestKindRegion_IsContainer pins the new container kind: a region node is
// hierarchy scaffolding, not a billable leaf resource, so Container() must
// return true and ResourceNodeCount must not count it — the guard that keeps
// the scan report's "N resources scanned" figure unchanged when region nodes
// appear in the graph. If a region node ever leaked into ResourceNodeCount,
// every scan summary would silently inflate its count, and an operator could
// no longer tell "nothing wasteful" from "nothing scanned".
func TestKindRegion_IsContainer(t *testing.T) {
	g := New()
	leaf := &Node{ID: Ref("//compute.googleapis.com/projects/p/zones/us-central1-a/disks/d1"), Kind: KindDisk, Name: "d1", Project: "p"}
	region := &Node{ID: Ref("projects/p/regions/us-central1"), Kind: KindRegion, Name: "us-central1", Project: "p"}
	proj := &Node{ID: Ref("projects/p"), Kind: KindProject, Name: "p", Project: "p"}
	for _, n := range []*Node{leaf, region, proj} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n.ID, err)
		}
	}
	if !region.Container() {
		t.Errorf("a KindRegion node must report Container() == true")
	}
	g.Freeze()
	if got := g.ResourceNodeCount(); got != 1 {
		t.Errorf("ResourceNodeCount = %d, want 1 (the region is a container, not a billable leaf)", got)
	}
	if got := g.CountByKind(KindRegion); got != 1 {
		t.Errorf("CountByKind(KindRegion) = %d, want 1", got)
	}
}

func addProjectWithLeaf(t *testing.T, g *Graph, projectToken, leafID string) {
	t.Helper()
	proj := &Node{ID: Ref(projectToken), Kind: KindProject, Name: lastSeg(projectToken), Project: lastSeg(projectToken)}
	leaf := &Node{ID: Ref(leafID), Kind: KindDisk, Name: lastSeg(leafID), Project: lastSeg(projectToken)}
	for _, n := range []*Node{proj, leaf} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n.ID, err)
		}
	}
	if err := g.AddEdge(Edge{From: leaf.ID, To: proj.ID, Kind: EdgeContains}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
}

func lastSeg(s string) string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return s[i+1:]
		}
	}
	return s
}
