package gcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// hierarchyFixture builds a multi-project fixture whose SearchAllResources
// hierarchy fields (project / folders / organization, exactly as
// ResourceSearchResult returns them) populate the container nodes and
// containment edges. It exercises the real Ingest path through FakeLister, so
// the assertions cover the hierarchy builder as fed by production ingestion.
//
// Topology intended:
//
//	organization: organizations/1010
//	├── folder folders/2020
//	│   ├── project projects/alpha     (from instance/disk/bucket assets)
//	│   └── project projects/beta      (from one bucket)
//	└── (folder folders/3030, a second folder directly under the org)
//	    └── project projects/alpha shares folders/3030 too
//
// Locations: web-0 and pd-0 are in zone us-central1-a (region us-central1);
// both buckets are in the US multi-region (canonical "us"). So the region tier
// adds projects/alpha/regions/us-central1, projects/alpha/regions/us and
// projects/beta/regions/us.
//
// Every resource's hierarchy fields come straight from the search result:
// no Cloud Resource Manager call is involved.
func hierarchyFixture() *FakeLister {
	return &FakeLister{Assets: []*RawAsset{
		{
			Name:         "//compute.googleapis.com/projects/alpha/zones/us-central1-a/instances/web-0",
			AssetType:    TypeInstance,
			Project:      "projects/alpha",
			Folders:      []string{"folders/2020", "folders/3030"},
			Organization: "organizations/1010",
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
			Name:         "//compute.googleapis.com/projects/alpha/zones/us-central1-a/disks/pd-0",
			AssetType:    TypeDisk,
			Project:      "projects/alpha",
			Folders:      []string{"folders/2020"},
			Organization: "organizations/1010",
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
			Name:         "//storage.googleapis.com/alpha-data",
			AssetType:    TypeBucket,
			Project:      "projects/alpha",
			Folders:      []string{"folders/2020"},
			Organization: "organizations/1010",
			Resource: &RawResource{
				Version:  "v1",
				Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
				Location: "US",
				Data: json.RawMessage(`{
					"name": "alpha-data",
					"storageClass": "STANDARD",
					"location": "US"
				}`),
			},
		},
		{
			Name:         "//storage.googleapis.com/beta-data",
			AssetType:    TypeBucket,
			Project:      "projects/beta",
			Folders:      []string{"folders/2020"},
			Organization: "organizations/1010",
			Resource: &RawResource{
				Version:  "v1",
				Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
				Location: "US",
				Data: json.RawMessage(`{
					"name": "beta-data",
					"storageClass": "NEARLINE",
					"location": "US"
				}`),
			},
		},
	}}
}

// ingestHierarchyFixture runs hierarchyFixture through the real offline
// provider Ingest and returns the frozen graph.
func ingestHierarchyFixture(t *testing.T) (*graph.Graph, *Provider) {
	t.Helper()
	lister := hierarchyFixture()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := New(context.Background(), WithOffline(), WithLogger(log), WithLister(lister))
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

// TestHierarchy_BuiltFromSearchResultFields is the primary regression test for
// the resource-hierarchy feature. It ingests a multi-project fixture whose
// assets carry project/folders/organization hierarchy fields and asserts:
//
//   - container nodes exist for the organization, both folders, both
//     projects and the three region nodes (and only those — no duplicate
//     container per token);
//   - the containment edges link each resource to its region, each region to
//     its project, each project to its folder(s), and each folder to the
//     organization;
//   - the "N resources" scan count excludes containers;
//   - no container node reaches rule evaluation.
func TestHierarchy_BuiltFromSearchResultFields(t *testing.T) {
	gr, p := ingestHierarchyFixture(t)
	defer p.Close()

	// 1. Container nodes exist, one per distinct token, with the exact
	// expected kinds and no duplicates.
	wantContainers := []struct {
		id   graph.Ref
		kind graph.ResourceKind
	}{
		{"organizations/1010", graph.KindOrganization},
		{"folders/2020", graph.KindFolder},
		{"folders/3030", graph.KindFolder},
		{"projects/alpha", graph.KindProject},
		{"projects/beta", graph.KindProject},
		// Region tier: web-0 and pd-0 flatten from zone us-central1-a to
		// region us-central1; both buckets are the US multi-region ("us").
		{"projects/alpha/regions/us-central1", graph.KindRegion},
		{"projects/alpha/regions/us", graph.KindRegion},
		{"projects/beta/regions/us", graph.KindRegion},
	}
	for _, w := range wantContainers {
		n, ok := gr.Node(w.id)
		if !ok {
			t.Errorf("container node %s (%s) missing", w.id, w.kind)
			continue
		}
		if n.Kind != w.kind {
			t.Errorf("container %s: Kind = %s, want %s", w.id, n.Kind, w.kind)
		}
		if !n.Container() {
			t.Errorf("container %s: Container() must be true", w.id)
		}
	}

	// 2. Containment edges. Direction is "contained -> container"
	// ("dependent -> dependency"), the same convention as every other edge:
	// out from a resource reaches its region; out from a region reaches its
	// project; out from a project reaches its folder; out from a folder
	// reaches the org. Assert the exact set.
	type edgeTriple struct{ from, to graph.Ref }
	wantEdges := []edgeTriple{
		// resource -> region
		{"//compute.googleapis.com/projects/alpha/zones/us-central1-a/instances/web-0", "projects/alpha/regions/us-central1"},
		{"//compute.googleapis.com/projects/alpha/zones/us-central1-a/disks/pd-0", "projects/alpha/regions/us-central1"},
		{"//storage.googleapis.com/alpha-data", "projects/alpha/regions/us"},
		{"//storage.googleapis.com/beta-data", "projects/beta/regions/us"},
		// region -> project
		{"projects/alpha/regions/us-central1", "projects/alpha"},
		{"projects/alpha/regions/us", "projects/alpha"},
		{"projects/beta/regions/us", "projects/beta"},
		// project -> folder
		{"projects/alpha", "folders/2020"},
		{"projects/alpha", "folders/3030"},
		{"projects/beta", "folders/2020"},
		// folder -> organization
		{"folders/2020", "organizations/1010"},
		{"folders/3030", "organizations/1010"},
	}
	haveEdges := map[edgeTriple]bool{}
	gr.Nodes(func(n *graph.Node) bool {
		for _, e := range gr.Out(n.ID) {
			if e.Kind == graph.EdgeContains {
				haveEdges[edgeTriple{e.From, e.To}] = true
			}
		}
		return true
	})
	for _, w := range wantEdges {
		if !haveEdges[w] {
			t.Errorf("missing contains edge %s -> %s", w.from, w.to)
		}
	}
	// The leaf -> project edge of the pre-region tier must be GONE: it is
	// replaced by leaf -> region plus region -> project, so the chain stays
	// linear.
	for _, leaf := range []graph.Ref{
		"//compute.googleapis.com/projects/alpha/zones/us-central1-a/instances/web-0",
		"//storage.googleapis.com/alpha-data",
	} {
		if haveEdges[edgeTriple{leaf, "projects/alpha"}] {
			t.Errorf("contains edge %s -> projects/alpha must NOT exist (replaced by leaf -> region -> project)", leaf)
		}
	}

	// 3. Rollup: from any resource, walking out along contains reaches the
	// organization through the region tier. This is the property the feature
	// exists to provide.
	assertReachesOrg(t, gr, "//storage.googleapis.com/beta-data", "organizations/1010")
	assertReachesOrg(t, gr, "//compute.googleapis.com/projects/alpha/zones/us-central1-a/instances/web-0", "organizations/1010")
	// And the region node itself sits on the walk.
	assertReachesOrg(t, gr, "//compute.googleapis.com/projects/alpha/zones/us-central1-a/instances/web-0", "projects/alpha/regions/us-central1")

	// 4. The scan counts real resources only: 4 leaf resources, and the
	// container nodes — including the three region nodes — do not inflate the
	// count.
	if got := gr.ResourceNodeCount(); got != 4 {
		t.Errorf("ResourceNodeCount = %d, want 4 (containers must not inflate the count)", got)
	}
	if got, want := gr.NodeCount(), 4+len(wantContainers); got != want {
		t.Errorf("NodeCount = %d, want %d = 4 leaves + %d containers (incl. regions)", got, want, len(wantContainers))
	}

	// 5. No container node reaches rule evaluation.
	assertNoContainerReachesRules(t, gr)
}

// unmodeledAssetType is a CAI asset type with no normalizer registered. It is
// deliberately NOT one of the constants in assettypes.go: the defect this
// guards against fires exactly when a rule declares an asset type for which no
// normalizer exists, so the type must be unrecognized by the Normalize map.
const unmodeledAssetType = "compute.googleapis.com/ForwardingRule"

// unmodeledHierarchyFixture is a project whose ONLY assets are an asset type
// with no normalizer registered (compute.googleapis.com/ForwardingRule — a
// currently unmodeled type that Normalize maps to (nil, nil)). It also sits
// under a folder and an organization, so before the defect fix the container
// nodes (project / folder / organization) would be created with no normalized
// leaf under them.
func unmodeledHierarchyFixture() *FakeLister {
	return &FakeLister{Assets: []*RawAsset{
		{
			Name: "//compute.googleapis.com/projects/alpha/regions/us-central1/forwardingRules/fr-0",
			// A genuine asset type that Normalize does NOT model: it has no
			// normalizer, so Normalize returns (nil, nil). This is the exact
			// situation a rule that declares a brand-new asset type would hit.
			AssetType:    unmodeledAssetType,
			Project:      "projects/alpha",
			Folders:      []string{"folders/2020"},
			Organization: "organizations/1010",
			Resource: &RawResource{
				Version:  "v1",
				Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
				Location: "us-central1",
				Data: json.RawMessage(`{
					"name": "fr-0",
					"IPAddress": "10.0.0.1",
					"loadBalancingScheme": "EXTERNAL"
				}`),
			},
		},
	}}
}

// TestHierarchy_UnmappedAssetTypeBuildsNoContainers is the regression test for
// the ingestion defect where buildHierarchy created project/folder/organization
// container nodes even when Normalize returned nil for an unmapped asset type.
// A project whose only assets are unmodelled types must produce NO container
// nodes: container nodes only make sense when they have a normalized leaf
// resource beneath them. This is also Fixture R6 — the region builder must
// skip when leaf == nil, so an unmapped asset produces zero region nodes too.
func TestHierarchy_UnmappedAssetTypeBuildsNoContainers(t *testing.T) {
	lister := unmodeledHierarchyFixture()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := New(context.Background(), WithOffline(), WithLogger(log), WithLister(lister))
	if err != nil {
		t.Fatalf("offline gcp.New: %v", err)
	}
	defer p.Close()

	// Request the unmapped type explicitly. Passing nil would fall back to
	// SupportedAssetTypes, and the lister's asset-type filter would drop the fixture
	// before Normalize ever ran — the graph would be empty because nothing was ingested,
	// not because the guard works, and the test would pass with the guard removed.
	gr, err := p.Ingest(context.Background(), cloud.Scope{},
		[]string{"compute.googleapis.com/ForwardingRule"})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// The asset type is unmapped, so Normalize must have returned nil and no
	// leaf node was added. Sanity-check that assumption first.
	if gr.ResourceNodeCount() != 0 {
		t.Fatalf("ResourceNodeCount = %d, want 0: the unmapped asset must normalize to nil", gr.ResourceNodeCount())
	}

	// No container nodes anywhere: the project, folder, organization AND
	// region must NOT exist because no normalized leaf hangs beneath them.
	for _, token := range []graph.Ref{
		"projects/alpha",
		"folders/2020",
		"organizations/1010",
		"projects/alpha/regions/us-central1",
	} {
		if n, ok := gr.Node(token); ok {
			t.Errorf("container node %s (%s) must NOT exist for an unmapped asset type", token, n.Kind)
		}
	}

	// The graph must be entirely empty of nodes and edges.
	if gr.NodeCount() != 0 {
		t.Errorf("NodeCount = %d, want 0 (no leaves, no containers)", gr.NodeCount())
	}
	if gr.EdgeCount() != 0 {
		t.Errorf("EdgeCount = %d, want 0 (no containment edges without a leaf)", gr.EdgeCount())
	}

	// No container node can possibly reach rule evaluation when there are none.
	assertNoContainerReachesRules(t, gr)
}

// assertReachesOrg walks out from an asset ref along contains edges until it
// reaches the target token, failing if it cannot.
func assertReachesOrg(t *testing.T, g *graph.Graph, from, target graph.Ref) {
	t.Helper()
	seen := map[graph.Ref]bool{}
	stack := []graph.Ref{from}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == target {
			return // reached
		}
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
	t.Errorf("no contains path from %s to %s", from, target)
}

// assertNoContainerReachesRules asserts that the graph's rule evaluation
// entry points never surface a container node. Rules enter only through
// ByKind with a leaf kind; iterating every leaf kind must yield zero container
// nodes.
func assertNoContainerReachesRules(t *testing.T, g *graph.Graph) {
	t.Helper()
	leafKinds := []graph.ResourceKind{
		graph.KindInstance, graph.KindDisk, graph.KindSnapshot, graph.KindImage,
		graph.KindBucket, graph.KindAddress, graph.KindForwardRule, graph.KindNetwork,
		graph.KindSubnetwork, graph.KindUnknown,
	}
	for _, kind := range leafKinds {
		g.ByKind(kind, func(n *graph.Node) bool {
			if n.Container() {
				t.Errorf("rule evaluation reached container node %s (kind %s) through ByKind(%s)", n.ID, n.Kind, kind)
			}
			return true
		})
	}
}
