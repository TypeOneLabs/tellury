package gcp

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fixtures and helpers for the region-container tier (Fixtures R1–R8)
// ─────────────────────────────────────────────────────────────────────────────

// regionR1Assets is Fixture R1: one project (alpha) with an instance and a
// disk in zone us-central1-a — both flatten to region us-central1 — and a
// bucket in the EU multi-region. It is the canonical region-tier fixture the
// other fixtures extend.
func regionR1Assets() []*RawAsset {
	return []*RawAsset{
		{
			Name:      "//compute.googleapis.com/projects/alpha/zones/us-central1-a/instances/web-0",
			AssetType: TypeInstance,
			Project:   "projects/alpha",
			Resource: &RawResource{
				Version:  "v1",
				Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
				Location: "us-central1-a",
				Data: json.RawMessage(`{
					"name": "web-0",
					"machineType": "projects/alpha/zones/us-central1-a/machineTypes/n2-standard-2",
					"status": "RUNNING"
				}`),
			},
		},
		{
			Name:      "//compute.googleapis.com/projects/alpha/zones/us-central1-a/disks/pd-0",
			AssetType: TypeDisk,
			Project:   "projects/alpha",
			Resource: &RawResource{
				Version:  "v1",
				Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
				Location: "us-central1-a",
				Data: json.RawMessage(`{
					"name": "pd-0",
					"sizeGb": 100,
					"status": "READY"
				}`),
			},
		},
		{
			Name:      "//storage.googleapis.com/alpha-data",
			AssetType: TypeBucket,
			Project:   "projects/alpha",
			Resource: &RawResource{
				Version:  "v1",
				Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
				Location: "EU",
				Data: json.RawMessage(`{
					"name": "alpha-data",
					"storageClass": "STANDARD",
					"location": "EU",
					"locationType": "multi-region"
				}`),
			},
		},
	}
}

// ingestAssets runs the given raw assets through the real offline provider
// Ingest — the same path production ingestion uses — and returns the frozen
// graph plus the provider (the caller closes it).
func ingestAssets(t *testing.T, assets []*RawAsset) (*graph.Graph, *Provider) {
	t.Helper()
	p, err := New(context.Background(), WithOffline(), WithLogger(newTestLogger()), WithLister(&FakeLister{Assets: assets}))
	if err != nil {
		t.Fatalf("offline gcp.New: %v", err)
	}
	gr, err := p.Ingest(context.Background(), cloud.Scope{}, nil)
	if err != nil {
		p.Close()
		t.Fatalf("Ingest: %v", err)
	}
	return gr, p
}

// ingestV2Shape builds the graph a v2 snapshot would have contained for the
// given assets: the same leaves and project container nodes, but the OLD
// leaf -> project containment edge and NO region tier at all. It mirrors the
// pre-region buildHierarchy edge set exactly, which is what MigrateV2ToV3
// must reconstruct from.
func ingestV2Shape(t *testing.T, assets []*RawAsset) *graph.Graph {
	t.Helper()
	g := graph.New()
	for _, a := range assets {
		n, err := Normalize(a, nil)
		if err != nil {
			t.Fatalf("Normalize(%s): %v", a.Name, err)
		}
		if n == nil {
			continue
		}
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n.ID, err)
		}
		projectToken := a.Project
		projectID := projectIDFrom(projectToken)
		if projectID == "" {
			projectID = n.Project
			projectToken = "projects/" + projectID
		}
		if projectID == "" {
			continue
		}
		pn := hierarchyNode(graph.KindProject, projectToken, projectID)
		if err := g.AddNode(pn); err != nil {
			t.Fatalf("AddNode(%s): %v", pn.ID, err)
		}
		// The pre-region edge set: leaf -> project, no regions.
		if err := g.AddEdge(graph.Edge{From: n.ID, To: pn.ID, Kind: graph.EdgeContains}); err != nil {
			t.Fatalf("AddEdge(%s -> %s): %v", n.ID, pn.ID, err)
		}
	}
	g.Freeze()
	return g
}

// containsTargets returns the ordered set of To endpoints of a node's
// EdgeContains out-edges.
func containsTargets(g *graph.Graph, from graph.Ref) []graph.Ref {
	var out []graph.Ref
	for _, e := range g.Out(from) {
		if e.Kind == graph.EdgeContains {
			out = append(out, e.To)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// regionNodeIDs returns the set of region node refs in the graph.
func regionNodeIDs(g *graph.Graph) map[graph.Ref]bool {
	ids := map[graph.Ref]bool{}
	g.Nodes(func(n *graph.Node) bool {
		if n.Kind == graph.KindRegion {
			ids[n.ID] = true
		}
		return true
	})
	return ids
}

// reachableContainers walks out from ref along contains edges and returns the
// set of every container reached (the ref itself excluded unless reachable).
func reachableContainers(g *graph.Graph, from graph.Ref) map[graph.Ref]bool {
	seen := map[graph.Ref]bool{}
	stack := []graph.Ref{from}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		for _, e := range g.Out(cur) {
			if e.Kind == graph.EdgeContains {
				stack = append(stack, e.To)
			}
		}
	}
	delete(seen, from)
	return seen
}

// topologyKey serializes a graph's nodes and edges deterministically so two
// graphs can be compared for structural equality (used by the idempotency and
// no-op-on-v3 tests).
func topologyKey(g *graph.Graph) string {
	var nodes []string
	g.Nodes(func(n *graph.Node) bool {
		nodes = append(nodes, string(n.ID)+":"+string(n.Kind))
		return true
	})
	sort.Strings(nodes)
	var edges []string
	g.Nodes(func(n *graph.Node) bool {
		for _, e := range g.Out(n.ID) {
			edges = append(edges, string(e.From)+"->"+string(e.To)+":"+string(e.Kind))
		}
		return true
	})
	sort.Strings(edges)
	return "nodes=" + join(nodes, ",") + "|edges=" + join(edges, ",")
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Fixture R1 — basic region tier
// ─────────────────────────────────────────────────────────────────────────────

// TestRegionTier_Basic asserts the full region tier for one project whose
// resources span two billing geographies: an instance and a disk in
// us-central1-a (both flatten to region us-central1) and a bucket in EU
// (canonical "eu"). The containment chain is resource -> region -> project,
// the leaf -> project edge is replaced (not supplemented), and the counts an
// operator reads are unchanged.
func TestRegionTier_Basic(t *testing.T) {
	gr, p := ingestAssets(t, regionR1Assets())
	defer p.Close()

	// Region nodes, one per (project, location), with the canonical location
	// as Name and the owning project on Project.
	for id, want := range map[graph.Ref]struct{ name, project string }{
		"projects/alpha/regions/us-central1": {"us-central1", "alpha"},
		"projects/alpha/regions/eu":          {"eu", "alpha"},
	} {
		n, ok := gr.Node(id)
		if !ok {
			t.Errorf("region node %s missing", id)
			continue
		}
		if n.Kind != graph.KindRegion || !n.Container() {
			t.Errorf("region %s: Kind=%s Container()=%v; want KindRegion container", id, n.Kind, n.Container())
		}
		if n.Name != want.name {
			t.Errorf("region %s: Name=%q, want %q", id, n.Name, want.name)
		}
		if n.Project != want.project {
			t.Errorf("region %s: Project=%q, want %q", id, n.Project, want.project)
		}
	}
	if got := gr.CountByKind(graph.KindRegion); got != 2 {
		t.Errorf("CountByKind(KindRegion) = %d, want 2", got)
	}

	// Resource -> region edges: two into us-central1 (instance + disk), one
	// into eu (bucket).
	wantLeafEdges := map[graph.Ref][]graph.Ref{
		"//compute.googleapis.com/projects/alpha/zones/us-central1-a/instances/web-0": {"projects/alpha/regions/us-central1"},
		"//compute.googleapis.com/projects/alpha/zones/us-central1-a/disks/pd-0":      {"projects/alpha/regions/us-central1"},
		"//storage.googleapis.com/alpha-data":                                          {"projects/alpha/regions/eu"},
	}
	for leaf, want := range wantLeafEdges {
		if got := containsTargets(gr, leaf); !reflect.DeepEqual(got, want) {
			t.Errorf("contains out of %s = %v, want %v", leaf, got, want)
		}
	}

	// Region -> project edges: two distinct regions, both to projects/alpha.
	wantRegionEdges := map[graph.Ref][]graph.Ref{
		"projects/alpha/regions/us-central1": {"projects/alpha"},
		"projects/alpha/regions/eu":          {"projects/alpha"},
	}
	for region, want := range wantRegionEdges {
		if got := containsTargets(gr, region); !reflect.DeepEqual(got, want) {
			t.Errorf("contains out of region %s = %v, want %v", region, got, want)
		}
	}

	// The pre-region leaf -> project edge is replaced, not supplemented.
	for _, leaf := range []graph.Ref{
		"//compute.googleapis.com/projects/alpha/zones/us-central1-a/instances/web-0",
		"//storage.googleapis.com/alpha-data",
	} {
		for _, to := range containsTargets(gr, leaf) {
			if to == "projects/alpha" {
				t.Errorf("leaf %s must not point directly at projects/alpha (replaced by leaf -> region -> project)", leaf)
			}
		}
	}

	// Counts: ResourceNodeCount is unchanged (3 billable leaves, regions are
	// containers); NodeCount is 3 leaves + 1 project + 2 regions = 6.
	if got := gr.ResourceNodeCount(); got != 3 {
		t.Errorf("ResourceNodeCount = %d, want 3 (regions are containers and must not inflate it)", got)
	}
	if got := gr.NodeCount(); got != 6 {
		t.Errorf("NodeCount = %d, want 6 = 3 leaves + 1 project + 2 regions", got)
	}

	// Walking out from the instance along contains reaches the region, then
	// the project.
	if got := containsTargets(gr, "//compute.googleapis.com/projects/alpha/zones/us-central1-a/instances/web-0"); !reflect.DeepEqual(got, []graph.Ref{"projects/alpha/regions/us-central1"}) {
		t.Errorf("walk from instance must reach its region first, got %v", got)
	}
	reach := reachableContainers(gr, "//compute.googleapis.com/projects/alpha/zones/us-central1-a/instances/web-0")
	for _, want := range []graph.Ref{"projects/alpha/regions/us-central1", "projects/alpha"} {
		if !reach[want] {
			t.Errorf("walk from instance must reach %s (got %v)", want, reach)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fixture R2 — zone flattening
// ─────────────────────────────────────────────────────────────────────────────

// TestRegionTier_ZoneFlattening asserts that a zone location is flattened to
// its parent region: a disk in us-central1-a carries Location "us-central1",
// there is no "us-central1-a" region node, and the disk points at the
// us-central1 region node.
func TestRegionTier_ZoneFlattening(t *testing.T) {
	assets := []*RawAsset{
		{
			Name:      "//compute.googleapis.com/projects/alpha/zones/us-central1-a/disks/pd-zonal",
			AssetType: TypeDisk,
			Project:   "projects/alpha",
			Resource: &RawResource{
				Version:  "v1",
				Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
				Location: "us-central1-a",
				Data: json.RawMessage(`{
					"name": "pd-zonal",
					"sizeGb": 200,
					"zone": "projects/alpha/zones/us-central1-a",
					"status": "READY"
				}`),
			},
		},
	}
	gr, p := ingestAssets(t, assets)
	defer p.Close()

	n, ok := gr.Node("//compute.googleapis.com/projects/alpha/zones/us-central1-a/disks/pd-zonal")
	if !ok {
		t.Fatalf("disk node missing")
	}
	if n.Location != "us-central1" {
		t.Errorf("disk Location = %q, want the flattened region \"us-central1\"", n.Location)
	}
	if _, ok := gr.Node("projects/alpha/regions/us-central1-a"); ok {
		t.Errorf("a zone must not get its own region node (us-central1-a)")
	}
	region, ok := gr.Node("projects/alpha/regions/us-central1")
	if !ok {
		t.Fatalf("region node projects/alpha/regions/us-central1 missing")
	}
	if region.Kind != graph.KindRegion {
		t.Errorf("region node kind = %s, want %s", region.Kind, graph.KindRegion)
	}
	if got := containsTargets(gr, n.ID); !reflect.DeepEqual(got, []graph.Ref{region.ID}) {
		t.Errorf("disk contains out = %v, want the us-central1 region", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fixture R3 — case collision
// ─────────────────────────────────────────────────────────────────────────────

// TestRegionTier_CaseCollision asserts that two resources whose raw locations
// differ only in case ("EU" vs "eu") resolve to the SAME canonical location
// and the SAME region node — without canonicalisation they would collide into
// two nodes that an operator would have to mentally recombine.
func TestRegionTier_CaseCollision(t *testing.T) {
	assets := []*RawAsset{
		{
			Name:      "//storage.googleapis.com/alpha-eu-bucket",
			AssetType: TypeBucket,
			Project:   "projects/alpha",
			Resource: &RawResource{
				Version:  "v1",
				Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
				Location: "EU",
				Data: json.RawMessage(`{
					"name": "alpha-eu-bucket",
					"storageClass": "STANDARD",
					"location": "EU"
				}`),
			},
		},
		{
			Name:      "//compute.googleapis.com/projects/alpha/global/snapshots/snap-eu",
			AssetType: TypeSnapshot,
			Project:   "projects/alpha",
			Resource: &RawResource{
				Version:  "v1",
				Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
				Location: "eu",
				Data: json.RawMessage(`{
					"name": "snap-eu",
					"diskSizeGb": 100,
					"storageBytes": 30,
					"status": "READY"
				}`),
			},
		},
	}
	gr, p := ingestAssets(t, assets)
	defer p.Close()

	for _, id := range []graph.Ref{
		"//storage.googleapis.com/alpha-eu-bucket",
		"//compute.googleapis.com/projects/alpha/global/snapshots/snap-eu",
	} {
		n, ok := gr.Node(id)
		if !ok {
			t.Fatalf("node %s missing", id)
		}
		if n.Location != "eu" {
			t.Errorf("node %s Location = %q, want the canonical \"eu\"", id, n.Location)
		}
	}

	// Exactly one eu region node, and both resources point at it.
	regions := regionNodeIDs(gr)
	if len(regions) != 1 || !regions["projects/alpha/regions/eu"] {
		t.Fatalf("region nodes = %v, want exactly projects/alpha/regions/eu", regions)
	}
	for _, id := range []graph.Ref{
		"//storage.googleapis.com/alpha-eu-bucket",
		"//compute.googleapis.com/projects/alpha/global/snapshots/snap-eu",
	} {
		if got := containsTargets(gr, id); !reflect.DeepEqual(got, []graph.Ref{"projects/alpha/regions/eu"}) {
			t.Errorf("resource %s must point at the single eu region node, got %v", id, got)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fixture R4 — global
// ─────────────────────────────────────────────────────────────────────────────

// TestRegionTier_Global asserts that a resource with location "global" (an
// external reserved IP address) attaches to a per-project "global" region
// node: projects/alpha/regions/global, with the region -> project edge
// intact. global is NOT a scan-global node because that would break project
// attribution.
func TestRegionTier_Global(t *testing.T) {
	assets := []*RawAsset{
		{
			Name:      "//compute.googleapis.com/projects/alpha/regions/global/addresses/ext-ip",
			AssetType: TypeAddress,
			Project:   "projects/alpha",
			Resource: &RawResource{
				Version:  "v1",
				Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
				Location: "global",
				Data: json.RawMessage(`{
					"name": "ext-ip",
					"addressType": "EXTERNAL",
					"status": "RESERVED",
					"address": "8.8.8.8",
					"users": []
				}`),
			},
		},
	}
	gr, p := ingestAssets(t, assets)
	defer p.Close()

	region, ok := gr.Node("projects/alpha/regions/global")
	if !ok {
		t.Fatalf("region node projects/alpha/regions/global missing")
	}
	if region.Name != "global" || region.Project != "alpha" {
		t.Errorf("global region: Name=%q Project=%q, want global/alpha", region.Name, region.Project)
	}
	addr := graph.Ref("//compute.googleapis.com/projects/alpha/regions/global/addresses/ext-ip")
	if got := containsTargets(gr, addr); !reflect.DeepEqual(got, []graph.Ref{"projects/alpha/regions/global"}) {
		t.Errorf("address must point at its global region node, got %v", got)
	}
	if got := containsTargets(gr, region.ID); !reflect.DeepEqual(got, []graph.Ref{"projects/alpha"}) {
		t.Errorf("global region must point at projects/alpha, got %v", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fixture R5 — multi-project region isolation
// ─────────────────────────────────────────────────────────────────────────────

// TestRegionTier_MultiProjectIsolation asserts that region nodes are
// PER-PROJECT: two projects each hosting a disk in us-central1 get two
// distinct us-central1 nodes, each disk points at its own project's node, and
// each region points at its own project. This is what keeps the containment
// chain linear — a shared "us-central1" node would have no unambiguous
// out-edge to a project.
func TestRegionTier_MultiProjectIsolation(t *testing.T) {
	mkDisk := func(project, disk string) *RawAsset {
		return &RawAsset{
			Name:      "//compute.googleapis.com/projects/" + project + "/zones/us-central1-a/disks/" + disk,
			AssetType: TypeDisk,
			Project:   "projects/" + project,
			Resource: &RawResource{
				Version:  "v1",
				Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
				Location: "us-central1-a",
				Data: json.RawMessage(`{"name":"` + disk + `","sizeGb":100,"status":"READY"}`),
			},
		}
	}
	gr, p := ingestAssets(t, []*RawAsset{mkDisk("alpha", "pd-a"), mkDisk("beta", "pd-b")})
	defer p.Close()

	regions := regionNodeIDs(gr)
	if len(regions) != 2 || !regions["projects/alpha/regions/us-central1"] || !regions["projects/beta/regions/us-central1"] {
		t.Fatalf("region nodes = %v, want exactly the two per-project us-central1 nodes", regions)
	}

	for _, leaf := range []struct{ disk, region graph.Ref }{
		{"//compute.googleapis.com/projects/alpha/zones/us-central1-a/disks/pd-a", "projects/alpha/regions/us-central1"},
		{"//compute.googleapis.com/projects/beta/zones/us-central1-a/disks/pd-b", "projects/beta/regions/us-central1"},
	} {
		if got := containsTargets(gr, leaf.disk); !reflect.DeepEqual(got, []graph.Ref{leaf.region}) {
			t.Errorf("disk %s must point at its own project's region node, got %v", leaf.disk, got)
		}
	}
	if got := containsTargets(gr, "projects/alpha/regions/us-central1"); !reflect.DeepEqual(got, []graph.Ref{"projects/alpha"}) {
		t.Errorf("alpha region must point at projects/alpha, got %v", got)
	}
	if got := containsTargets(gr, "projects/beta/regions/us-central1"); !reflect.DeepEqual(got, []graph.Ref{"projects/beta"}) {
		t.Errorf("beta region must point at projects/beta, got %v", got)
	}

	if got := gr.ProjectContainerCount(); got != 2 {
		t.Errorf("ProjectContainerCount = %d, want 2 (unchanged by the region tier)", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fixture R8 — ResourceNodeCount unchanged
// ─────────────────────────────────────────────────────────────────────────────

// TestRegionTier_ResourceCountUnchanged is Fixture R8: the "N resources
// scanned" figure must equal the number of billable leaf nodes no matter how
// many region nodes the graph carries. A scan that reported "100 resources"
// before this feature must still report exactly 100 after. Region nodes are
// containers, so ResourceNodeCount excludes them structurally.
func TestRegionTier_ResourceCountUnchanged(t *testing.T) {
	// R1 carries 3 leaves and 2 region nodes; R5 carries 2 leaves and 2
	// region nodes. In both, ResourceNodeCount must equal the leaf count.
	cases := []struct {
		name         string
		assets       []*RawAsset
		leaves, want int
	}{
		{"r1", regionR1Assets(), 3, 2},
		{"r5", []*RawAsset{
			regionR1Assets()[0],
			{
				Name:      "//storage.googleapis.com/beta-data",
				AssetType: TypeBucket,
				Project:   "projects/beta",
				Resource: &RawResource{
					Version:  "v1",
					Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
					Location: "EU",
					Data:     json.RawMessage(`{"name":"beta-data","storageClass":"STANDARD","location":"EU"}`),
				},
			},
		}, 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gr, p := ingestAssets(t, tc.assets)
			defer p.Close()
			if got := gr.ResourceNodeCount(); got != tc.leaves {
				t.Errorf("ResourceNodeCount = %d, want %d (regions must never inflate the scanned-resource figure)", got, tc.leaves)
			}
			if got := gr.CountByKind(graph.KindRegion); got != tc.want {
				t.Errorf("CountByKind(KindRegion) = %d, want %d (sanity: the fixture really has regions)", got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fixture R7 — migration from a v2 snapshot
// ─────────────────────────────────────────────────────────────────────────────

// TestMigrateV2ToV3_ReconstructsRegions is Fixture R7: a v2-shaped graph (no
// region nodes, old leaf -> project edges) is run through MigrateV2ToV3 and
// must come out with the same region tier as a fresh v3 ingestion of the same
// data. The R1 assets include a GCS bucket whose leaf.Project is the parent
// project NUMBER while its project container is "projects/alpha" — the
// migration must resolve the project token from the containment edge, not
// from "projects/" + leaf.Project, or the bucket's region would dangle.
func TestMigrateV2ToV3_ReconstructsRegions(t *testing.T) {
	assets := regionR1Assets()
	p, err := New(context.Background(), WithOffline(), WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("offline gcp.New: %v", err)
	}
	defer p.Close()

	v2 := ingestV2Shape(t, assets)
	if v2.CountByKind(graph.KindRegion) != 0 {
		t.Fatalf("v2-shaped graph must start with zero region nodes")
	}
	// Sanity: the bucket leaf really carries the NUMBER project (the real CAI
	// shape), which is the trap the containment-edge resolution must defuse.
	if n, ok := v2.Node("//storage.googleapis.com/alpha-data"); !ok || n.Project != "34968801978" {
		t.Fatalf("bucket leaf Project = %q; want the parent number 34968801978 (the real CAI shape)", n.Project)
	}

	if err := p.MigrateV2ToV3(v2); err != nil {
		t.Fatalf("MigrateV2ToV3: %v", err)
	}

	fresh, pf := ingestAssets(t, assets)
	defer pf.Close()

	assertSameRegionTier(t, v2, fresh)

	// The migrated graph is additive: it keeps the historical leaf -> project
	// edge (a v2 snapshot already carried it) alongside the reconstructed
	// region chain. That extra path changes no reachability and no count.
	if got := v2.ResourceNodeCount(); got != fresh.ResourceNodeCount() {
		t.Errorf("migrated ResourceNodeCount = %d, fresh = %d (must agree)", got, fresh.ResourceNodeCount())
	}
}

// assertSameRegionTier compares a migrated graph against a fresh v3
// ingestion: identical region node set, identical resource -> region and
// region -> project edges, and identical containment reachability from every
// leaf. The migrated graph may carry extra edges (the historical leaf ->
// project edge), so the comparison is on the region tier's reachability, not
// a byte-for-byte edge-set equality.
func assertSameRegionTier(t *testing.T, migrated, fresh *graph.Graph) {
	t.Helper()

	if !reflect.DeepEqual(regionNodeIDs(migrated), regionNodeIDs(fresh)) {
		t.Errorf("region node sets differ: migrated=%v fresh=%v", regionNodeIDs(migrated), regionNodeIDs(fresh))
	}

	// Compare, for every leaf, the set of REGION nodes it points at.
	migLeaves := map[graph.Ref][]graph.Ref{}
	freshLeaves := map[graph.Ref][]graph.Ref{}
	fresh.Nodes(func(n *graph.Node) bool {
		if n.Container() {
			return true
		}
		var regions []graph.Ref
		for _, to := range containsTargets(fresh, n.ID) {
			if n, ok := fresh.Node(to); ok && n.Kind == graph.KindRegion {
				regions = append(regions, to)
			}
		}
		freshLeaves[n.ID] = regions
		return true
	})
	migrated.Nodes(func(n *graph.Node) bool {
		if n.Container() {
			return true
		}
		var regions []graph.Ref
		for _, to := range containsTargets(migrated, n.ID) {
			if n, ok := migrated.Node(to); ok && n.Kind == graph.KindRegion {
				regions = append(regions, to)
			}
		}
		migLeaves[n.ID] = regions
		return true
	})
	if !reflect.DeepEqual(migLeaves, freshLeaves) {
		t.Errorf("resource -> region edges differ: migrated=%v fresh=%v", migLeaves, freshLeaves)
	}

	// Region -> project edges.
	migRegionTo := map[graph.Ref][]graph.Ref{}
	freshRegionTo := map[graph.Ref][]graph.Ref{}
	for r := range regionNodeIDs(fresh) {
		freshRegionTo[r] = containsTargets(fresh, r)
	}
	for r := range regionNodeIDs(migrated) {
		migRegionTo[r] = containsTargets(migrated, r)
	}
	if !reflect.DeepEqual(migRegionTo, freshRegionTo) {
		t.Errorf("region -> project edges differ: migrated=%v fresh=%v", migRegionTo, freshRegionTo)
	}

	// Containment reachability from every leaf must be identical (walking out
	// reaches the same project/folder/org set either way).
	for leaf := range freshLeaves {
		if !reflect.DeepEqual(reachableContainers(migrated, leaf), reachableContainers(fresh, leaf)) {
			t.Errorf("reachability from %s differs: migrated=%v fresh=%v",
				leaf, reachableContainers(migrated, leaf), reachableContainers(fresh, leaf))
		}
	}
}

// TestMigrateV2ToV3_Idempotent asserts that calling MigrateV2ToV3 twice on the
// same graph produces the same graph: the second call sees region nodes
// already present and returns immediately, so a double migration (e.g. a
// replay path that re-checks the version) is harmless.
func TestMigrateV2ToV3_Idempotent(t *testing.T) {
	assets := regionR1Assets()
	p, err := New(context.Background(), WithOffline(), WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("offline gcp.New: %v", err)
	}
	defer p.Close()

	g := ingestV2Shape(t, assets)
	if err := p.MigrateV2ToV3(g); err != nil {
		t.Fatalf("first MigrateV2ToV3: %v", err)
	}
	first := topologyKey(g)

	if err := p.MigrateV2ToV3(g); err != nil {
		t.Fatalf("second MigrateV2ToV3: %v", err)
	}
	if got := topologyKey(g); got != first {
		t.Errorf("second migration changed the graph:\n first: %s\nsecond: %s", first, got)
	}
}

// TestMigrateV2ToV3_NoOpOnV3 asserts that a graph that already carries region
// nodes (a v3 snapshot loaded directly) is returned untouched — the replay
// path must not re-run reconstruction on current snapshots.
func TestMigrateV2ToV3_NoOpOnV3(t *testing.T) {
	assets := regionR1Assets()
	p, err := New(context.Background(), WithOffline(), WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("offline gcp.New: %v", err)
	}
	defer p.Close()

	g, pv3 := ingestAssets(t, assets)
	defer pv3.Close()

	before := topologyKey(g)
	if err := p.MigrateV2ToV3(g); err != nil {
		t.Fatalf("MigrateV2ToV3 on a v3 graph: %v", err)
	}
	if got := topologyKey(g); got != before {
		t.Errorf("MigrateV2ToV3 modified a graph that already has regions:\n before: %s\n after: %s", before, got)
	}
}

// TestMigrateV2ToV3_CanonicalisesLeafLocation: migrating a v2 snapshot must
// rewrite each leaf's own Location, not only build a region node from it.
//
// A v2 snapshot stores whatever spelling the service returned — Cloud Storage
// reports "EUROPE-WEST4" and "US" where Compute reports "europe-west4" and
// "us". A finding copies Location verbatim, and the waste-by-region chart
// groups on it. Migrating the region node while leaving the leaf raw puts a
// replayed snapshot and a fresh scan of the SAME bucket into two different
// rows for one real region.
//
// Money is unaffected either way, because every rule prices through
// pricing.RegionOf which re-canonicalises — which is precisely why this would
// go unnoticed without a test.
func TestMigrateV2ToV3_CanonicalisesLeafLocation(t *testing.T) {
	g := graph.New()
	proj := &graph.Node{ID: graph.Ref("projects/p"), Kind: graph.KindProject, Name: "p"}
	if err := g.AddNode(proj); err != nil {
		t.Fatalf("AddNode(project): %v", err)
	}
	// Raw GCS spelling, exactly as it appears in the v2 snapshots on disk.
	bucket := &graph.Node{
		ID: graph.Ref("//storage.googleapis.com/b1"), Kind: graph.KindBucket,
		Name: "b1", Project: "p", Location: "EUROPE-WEST4",
	}
	if err := g.AddNode(bucket); err != nil {
		t.Fatalf("AddNode(bucket): %v", err)
	}
	if err := g.AddEdge(graph.Edge{From: proj.ID, To: bucket.ID, Kind: graph.EdgeContains}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.Freeze()

	prov, err := New(context.Background(), WithOffline(), WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("offline gcp.New: %v", err)
	}
	defer prov.Close()
	if err := prov.MigrateV2ToV3(g); err != nil {
		t.Fatalf("MigrateV2ToV3: %v", err)
	}

	if got := bucket.Location; got != "europe-west4" {
		t.Errorf("leaf Location = %q after migration, want %q: a replayed snapshot would "+
			"label its finding differently from a fresh scan of the same bucket, "+
			"splitting one region across two rows of the region chart", got, "europe-west4")
	}
}
