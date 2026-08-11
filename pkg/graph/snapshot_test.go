package graph

import (
	"bytes"
	"testing"
)

// TestSnapshotVersion_Is3 pins the version the region-container feature
// bumped to: a v3 snapshot carries region nodes and region containment edges
// that v2 predates. The --cache-file replay path compares a loaded
// snapshot's Version against this constant to decide whether the provider's
// region reconstruction (gcp.Provider.MigrateV2ToV3) must run before the
// graph is used.
func TestSnapshotVersion_Is3(t *testing.T) {
	if SnapshotVersion != 3 {
		t.Fatalf("SnapshotVersion = %d, want 3 (region container nodes are a structural change, not additive fields)", SnapshotVersion)
	}
}

// TestSnapshot_RoundTripsRegionContainers pins that the region tier survives
// a write/load round trip — the exact path --cache-file and the artifact
// directory use: a v3 snapshot carries KindRegion nodes and the
// resource -> region -> project edges, and the loaded graph still counts the
// same number of billable resources (ResourceNodeCount excludes regions).
func TestSnapshot_RoundTripsRegionContainers(t *testing.T) {
	g := New()
	leaf := &Node{ID: Ref("//compute.googleapis.com/projects/p/zones/us-central1-a/disks/d1"), Kind: KindDisk, Name: "d1", Project: "p", Location: "us-central1"}
	region := &Node{ID: Ref("projects/p/regions/us-central1"), Kind: KindRegion, Name: "us-central1", Project: "p"}
	proj := &Node{ID: Ref("projects/p"), Kind: KindProject, Name: "p", Project: "p"}
	for _, n := range []*Node{leaf, region, proj} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n.ID, err)
		}
	}
	if err := g.AddEdge(Edge{From: leaf.ID, To: region.ID, Kind: EdgeContains}); err != nil {
		t.Fatalf("AddEdge leaf->region: %v", err)
	}
	if err := g.AddEdge(Edge{From: region.ID, To: proj.ID, Kind: EdgeContains}); err != nil {
		t.Fatalf("AddEdge region->project: %v", err)
	}
	g.Freeze()

	var buf bytes.Buffer
	if err := g.WriteSnapshot(&buf, "gcp", "projects/p"); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	got, snap, err := LoadSnapshot(&buf)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap.Version != 3 {
		t.Errorf("snapshot version = %d, want 3", snap.Version)
	}
	if got.CountByKind(KindRegion) != 1 {
		t.Errorf("replayed CountByKind(KindRegion) = %d, want 1", got.CountByKind(KindRegion))
	}
	if got.NodeCount() != 3 {
		t.Errorf("replayed NodeCount = %d, want 3", got.NodeCount())
	}
	if got.ResourceNodeCount() != 1 {
		t.Errorf("replayed ResourceNodeCount = %d, want 1 (region is a container)", got.ResourceNodeCount())
	}
	if got.EdgeCount() != 2 {
		t.Errorf("replayed EdgeCount = %d, want 2 (resource->region, region->project)", got.EdgeCount())
	}
}
