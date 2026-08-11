package gcp

import (
	"encoding/json"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// TestLocationRegion_LocationNodeWrapper pins the second caller of the single
// canonicaliser: the graph location node. Unlike RegionOf it keeps the empty
// input empty — a locationless asset has no region to claim — and delegates
// every non-empty location straight to pricing.CanonicalRegion, so the graph
// node and the price can never disagree about where a resource lives.
func TestLocationRegion_LocationNodeWrapper(t *testing.T) {
	if got := locationRegion(""); got != "" {
		t.Errorf("locationRegion(\"\") = %q, want \"\" (a location node keeps empty empty)", got)
	}
	if got := locationRegion("us-central1-a"); got != "us-central1" {
		t.Errorf("locationRegion(\"us-central1-a\") = %q, want \"us-central1\" (GCP zone flattened)", got)
	}
	if got := locationRegion("US"); got != "us" {
		t.Errorf("locationRegion(\"US\") = %q, want \"us\" (multi-region lowercased)", got)
	}
	if got := locationRegion("us-east-1"); got != "us-east-1" {
		t.Errorf("locationRegion(\"us-east-1\") = %q, want \"us-east-1\" (AWS region kept)", got)
	}
}

// TestNormalizeDisk_LocationIsCanonicalRegion asserts that the graph node's
// Location is set through the single canonicaliser, not stored raw: a disk in
// zone "us-central1-a" carries Location "us-central1", the same region token
// the pricer resolves. This is what makes the location node a real caller of
// CanonicalRegion rather than a second, drift-prone answer.
func TestNormalizeDisk_LocationIsCanonicalRegion(t *testing.T) {
	a := &RawAsset{
		Name:      "//compute.googleapis.com/projects/p/zones/us-central1-a/disks/d1",
		AssetType: TypeDisk,
		Resource: &RawResource{
			Version:  "v1",
			Parent:   "//cloudresourcemanager.googleapis.com/projects/p",
			Location: "us-central1-a",
			Data: json.RawMessage(`{
				"name": "d1",
				"sizeGb": 100,
				"zone": "projects/p/zones/us-central1-a"
			}`),
		},
	}

	n, err := Normalize(a, nil)
	if err != nil {
		t.Fatalf("Normalize(disk): %v", err)
	}
	if n == nil {
		t.Fatalf("Normalize(disk) returned nil")
	}
	if n.Location != "us-central1" {
		t.Errorf("node Location = %q, want the canonical region \"us-central1\" (raw zone must be flattened)", n.Location)
	}
}

// TestNormalizeBucket_LocationLowercased asserts the GCS bucket normalizer also
// routes its location through the canonicaliser: a multi-region bucket whose
// payload says "US" resolves to the same lowercase token ("us") the pricer
// uses, never the raw uppercase spelling.
func TestNormalizeBucket_LocationLowercased(t *testing.T) {
	a := &RawAsset{
		Name:      "//storage.googleapis.com/alpha-data",
		AssetType: TypeBucket,
		Resource: &RawResource{
			Version:  "v1",
			Location: "US",
			Data: json.RawMessage(`{
				"name": "alpha-data",
				"storageClass": "STANDARD",
				"location": "US",
				"locationType": "multi-region"
			}`),
		},
	}

	n, err := Normalize(a, nil)
	if err != nil {
		t.Fatalf("Normalize(bucket): %v", err)
	}
	if n == nil {
		t.Fatalf("Normalize(bucket) returned nil")
	}
	if n.Location != "us" {
		t.Errorf("node Location = %q, want the canonical lowercase \"us\"", n.Location)
	}
}

// TestNormalizeBucket_ProjectFromParentFallback is the regression test for the
// "empty project on GCS buckets" bug. Real CAI output names a GCS bucket
// WITHOUT any "projects/" segment:
//
//	//storage.googleapis.com/bucket-data
//
// The only place the owning project appears is the resource envelope's
// `parent`, which Cloud Asset Inventory renders as a project NUMBER in the
// cloudresourcemanager namespace:
//
//	//cloudresourcemanager.googleapis.com/projects/34968801978
//
// The old projectOf only consulted the asset name, so every bucket normalized
// to a node with an EMPTY Project. Because metrics.EnrichMetrics derives its
// project list from node.Project (distinctProjects), a scan whose graph was
// only buckets found zero projects and skipped metric enrichment entirely —
// which meant `no_lifecycle_policy` could never fire on a live scan (it gates
// on a bucket metric).
//
// This fixture is deliberately built from the REAL CAI shape described above,
// not the hand-written "//storage.googleapis.com/projects/alpha-data" shape
// that earlier audits used — that shape does not exist in CAI output, which is
// exactly why the defect survived two audits. The assertion is the minimal
// one that fails against the old code: Project must be non-empty.
func TestNormalizeBucket_ProjectFromParentFallback(t *testing.T) {
	a := &RawAsset{
		Name:      "//storage.googleapis.com/alpha-data-archive",
		AssetType: TypeBucket,
		Resource: &RawResource{
			Version:  "v1",
			Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
			Location: "US",
			Data: json.RawMessage(`{
				"name":         "alpha-data-archive",
				"storageClass": "STANDARD",
				"location":     "US",
				"locationType": "multi-region"
			}`),
		},
	}

	n, err := Normalize(a, nil)
	if err != nil {
		t.Fatalf("Normalize(bucket): %v", err)
	}
	if n == nil {
		t.Fatalf("Normalize(bucket) returned nil; expected a bucket node")
	}
	if n.Kind != graph.KindBucket {
		t.Fatalf("expected KindBucket, got %s", n.Kind)
	}
	if n.Project == "" {
		t.Fatalf("bucket node has empty Project; projectOf must fall back to the CAI parent " +
			"(this is the bug that disabled metric enrichment for the whole scan)")
	}
}

// TestNormalizeBucket_ProjectFromNumberParent documents that when the ONLY
// project signal is the parent NUMBER (the real CAI shape for a bucket), that
// number IS the Project value. The task forbids resolving numbers to IDs via
// an API call, so the number is used verbatim. This keeps a scan's buckets
// project-parented even when no project ID can be recovered.
func TestNormalizeBucket_ProjectFromNumberParent(t *testing.T) {
	a := &RawAsset{
		Name:      "//storage.googleapis.com/alpha-data-archive",
		AssetType: TypeBucket,
		Resource: &RawResource{
			Version:  "v1",
			Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
			Location: "US",
			Data:     json.RawMessage(`{"name":"alpha-data-archive"}`),
		},
	}

	n, err := Normalize(a, nil)
	if err != nil {
		t.Fatalf("Normalize(bucket): %v", err)
	}
	if n == nil {
		t.Fatalf("Normalize(bucket) returned nil")
	}
	if n.Project != "34968801978" {
		t.Fatalf("Project = %q; want the parent project number 34968801978 as the fallback", n.Project)
	}
}

// TestNormalizeDisk_ProjectIDPreferredOverParentNumber is the project-ID
// precedence guarantee for task 2: an asset whose NAME embeds a project ID
// must report that ID, NEVER the project number in the envelope's parent.
// Compute-style assets carry the ID in the name
// ("//compute.googleapis.com/projects/alpha-data-storage/zones/..."); the
// number is only a fallback for assets that cannot name their project (GCS
// buckets). Without this, a single report would spell the same project two
// ways — an unreadable number for one asset type and the human ID for
// another.
func TestNormalizeDisk_ProjectIDPreferredOverParentNumber(t *testing.T) {
	a := &RawAsset{
		// Name carries the project ID.
		Name:      "//compute.googleapis.com/projects/alpha-data-storage/zones/us-central1-a/disks/named-pd-01",
		AssetType: TypeDisk,
		Resource: &RawResource{
			Version: "v1",
			// Parent holds the NUMBER; it must NOT win over the name's ID.
			Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
			Location: "us-central1-a",
			Data: json.RawMessage(`{
				"name": "named-pd-01",
				"sizeGb": 200,
				"zone": "projects/alpha-data-storage/zones/us-central1-a"
			}`),
		},
	}

	n, err := Normalize(a, nil)
	if err != nil {
		t.Fatalf("Normalize(disk): %v", err)
	}
	if n == nil {
		t.Fatalf("Normalize(disk) returned nil")
	}
	if n.Project != "alpha-data-storage" {
		t.Fatalf("Project = %q; the asset name's project ID must win over the parent number 34968801978", n.Project)
	}
}

// TestNormalizeInstance_ProjectIDFromName is the same ID-precedence guarantee
// for the instance normalizer, which is the most common asset type in a scan.
// The name embeds the ID; the parent (with a number) must not override it.
func TestNormalizeInstance_ProjectIDFromName(t *testing.T) {
	a := &RawAsset{
		Name:      "//compute.googleapis.com/projects/alpha-data-storage/zones/us-central1-a/instances/web-0",
		AssetType: TypeInstance,
		Resource: &RawResource{
			Version:  "v1",
			Parent:   "//cloudresourcemanager.googleapis.com/projects/34968801978",
			Location: "us-central1-a",
			Data: json.RawMessage(`{
				"name": "web-0",
				"machineType": "projects/alpha-data-storage/zones/us-central1-a/machineTypes/n2-standard-2",
				"status": "RUNNING"
			}`),
		},
	}

	n, err := Normalize(a, nil)
	if err != nil {
		// normalizeInstance touches sz.Spec / sz.Family only when machineType
		// is non-empty AND sz != nil; a nil sizer therefore cannot panic — the
		// catalog-derived attrs simply stay absent.
		t.Fatalf("Normalize(instance) with nil sizer: %v", err)
	}
	if n == nil {
		t.Fatalf("Normalize(instance) returned nil")
	}
	if n.Project != "alpha-data-storage" {
		t.Fatalf("Project = %q; want the asset-name project ID, not the parent number", n.Project)
	}
}

// TestNormalizeInstance_CreatedBy_MIGMarker asserts that the instance
// normalizer extracts the `created-by` instance metadata item as AttrCreatedBy
// — the signal Cloud Asset Inventory uses to mark a managed instance group
// member. The value names an instanceGroupManagers resource self-link.
func TestNormalizeInstance_CreatedBy_MIGMarker(t *testing.T) {
	a := &RawAsset{
		Name:      "//compute.googleapis.com/projects/p/zones/us-central1-a/instances/web-0",
		AssetType: TypeInstance,
		Resource: &RawResource{
			Version:  "v1",
			Parent:   "//cloudresourcemanager.googleapis.com/projects/123",
			Location: "us-central1-a",
			Data: json.RawMessage(`{
				"name": "web-0",
				"machineType": "projects/p/zones/us-central1-a/machineTypes/n1-standard-4",
				"status": "RUNNING",
				"metadata": {
					"items": [
						{"key": "created-by", "value": "projects/p/zones/us-central1-a/instanceGroupManagers/web-mig"},
						{"key": "startup-script", "value": "echo hi"}
					]
				}
			}`),
		},
	}

	n, err := Normalize(a, nil)
	if err != nil {
		t.Fatalf("Normalize(instance): %v", err)
	}
	if n == nil {
		t.Fatalf("Normalize(instance) returned nil")
	}
	got, ok := n.Str(AttrCreatedBy)
	if !ok || got == "" {
		t.Fatalf("created_by attribute absent on a MIG member; want %q", got)
	}
	if got != "projects/p/zones/us-central1-a/instanceGroupManagers/web-mig" {
		t.Fatalf("created_by = %q, want the full instanceGroupManagers self-link", got)
	}
}
