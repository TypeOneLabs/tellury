package gcp

import (
	"context"
	"os"
	"testing"
)

// TestLoadBareSearchResultArray is the round-trip guarantee behind the
// documented fixture recipe: `gcloud asset search-all-resources --format=json`
// prints a BARE top-level JSON array of ResourceSearchResult objects (no
// {"assets": [...]} wrapper), and that exact shape must feed `--fixture`
// without hand-editing. This test encodes the gcloud output shape verbatim and
// asserts LoadFakeLister parses it into RawAssets whose payloads unfold from
// versionedResources the same way the live path does.
func TestLoadBareSearchResultArray(t *testing.T) {
	// This is the EXACT top-level shape `gcloud asset search-all-resources
	// --format=json` emits: a bare JSON array. Each element mirrors the
	// ResourceSearchResult proto: name / assetType / project / folders /
	// organization / location / parentFullResourceName plus versionedResources[].
	blob := `[
  {
    "name": "//compute.googleapis.com/projects/my-project/zones/us-central1-a/instances/web-0",
    "assetType": "compute.googleapis.com/Instance",
    "project": "projects/my-project",
    "folders": [],
    "organization": "",
    "location": "us-central1-a",
    "parentFullResourceName": "//cloudresourcemanager.googleapis.com/projects/34968801978",
    "versionedResources": [
      {
        "version": "v1",
        "resource": {
          "name": "web-0",
          "machineType": "projects/my-project/zones/us-central1-a/machineTypes/n2-standard-2",
          "status": "RUNNING",
          "creationTimestamp": "2024-01-01T00:00:00Z",
          "scheduling": {"preemptible": false, "provisioningModel": "STANDARD"}
        }
      }
    ]
  },
  {
    "name": "//storage.googleapis.com/data-archive",
    "assetType": "storage.googleapis.com/Bucket",
    "project": "projects/my-project",
    "folders": [],
    "organization": "",
    "location": "US",
    "parentFullResourceName": "//cloudresourcemanager.googleapis.com/projects/34968801978",
    "versionedResources": [
      {
        "version": "v1",
        "resource": {
          "name": "data-archive",
          "storageClass": "STANDARD",
          "location": "US",
          "locationType": "multi-region"
        }
      }
    ]
  }
]`

	dir := t.TempDir()
	path := dir + "/capture.json"
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	f, err := LoadFakeLister(path)
	if err != nil {
		t.Fatalf("LoadFakeLister(gcloud array): %v", err)
	}
	if len(f.Assets) != 2 {
		t.Fatalf("expected 2 assets from the captured array, got %d", len(f.Assets))
	}

	// The instance must have its payload unfolded from versionedResources,
	// with its envelope parent and location captured from
	// parentFullResourceName/location, exactly as the live SearchAllResources
	// path produces them.
	var inst *RawAsset
	for _, a := range f.Assets {
		if a.AssetType == TypeInstance {
			inst = a
			break
		}
	}
	if inst == nil {
		t.Fatalf("no instance asset decoded")
	}
	if inst.Project != "projects/my-project" {
		t.Fatalf("instance Project = %q; want projects/my-project", inst.Project)
	}
	if inst.Parent() != "//cloudresourcemanager.googleapis.com/projects/34968801978" {
		t.Fatalf("instance parent = %q; want the search result's parentFullResourceName", inst.Parent())
	}
	if inst.Location() != "us-central1-a" {
		t.Fatalf("instance location = %q; want us-central1-a", inst.Location())
	}
	data := decodeData(inst.Data())
	if m, ok := strOf(data["machineType"]); !ok || m == "" {
		t.Fatalf("instance machineType not unfolded from versionedResources")
	}
	if status, ok := strOf(data["status"]); !ok || status != "RUNNING" {
		t.Fatalf("instance status not unfolded; got %v", data["status"])
	}

	// The bucket must likewise carry its full payload and parent.
	var bkt *RawAsset
	for _, a := range f.Assets {
		if a.AssetType == TypeBucket {
			bkt = a
			break
		}
	}
	if bkt == nil {
		t.Fatalf("no bucket asset decoded")
	}
	data = decodeData(bkt.Data())
	if sc, ok := strOf(data["storageClass"]); !ok || sc != "STANDARD" {
		t.Fatalf("bucket storageClass not unfolded")
	}

	// It must also stream through ListAssets honouring the asset-type filter,
	// the same code path prod uses.
	var visited []string
	err = f.ListAssets(context.Background(), ListRequest{
		Parent:     "projects/my-project",
		AssetTypes: []string{TypeInstance},
	}, func(a *RawAsset) error {
		visited = append(visited, a.AssetType)
		return nil
	})
	if err != nil {
		t.Fatalf("ListAssets over captured array: %v", err)
	}
	if len(visited) != 1 || visited[0] != TypeInstance {
		t.Fatalf("ListAssets filter wrong: visited=%v; want exactly [compute.googleapis.com/Instance]", visited)
	}
}

// TestLoadSearchResultEnvelope guards the second accepted shape: the canonical
// {"assets": [...]} wrapper the shipped fixtures use. It must keep parsing, so
// the bare-array acceptance is purely additive and never breaks existing
// fixtures.
func TestLoadSearchResultEnvelope(t *testing.T) {
	blob := `{"assets":[
	  {
	    "name": "//compute.googleapis.com/projects/p/zones/us-central1-a/disks/d1",
	    "assetType": "compute.googleapis.com/Disk",
	    "project": "projects/p",
	    "resource": {
	      "version": "v1",
	      "parent": "//cloudresourcemanager.googleapis.com/projects/34968801978",
	      "location": "us-central1-a",
	      "data": {"name": "d1", "sizeGb": 200}
	    }
	  }
	]}`

	dir := t.TempDir()
	path := dir + "/wrap.json"
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f, err := LoadFakeLister(path)
	if err != nil {
		t.Fatalf("LoadFakeLister(wrapper): %v", err)
	}
	if len(f.Assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(f.Assets))
	}
	if f.Assets[0].AssetType != TypeDisk {
		t.Fatalf("asset type = %q; want compute.googleapis.com/Disk", f.Assets[0].AssetType)
	}
}
