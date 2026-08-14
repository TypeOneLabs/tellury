package gcp

import (
	"encoding/json"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

func TestNormalizeCustomImage_StoredBytesAndLocation(t *testing.T) {
	a := &RawAsset{
		Name:      "//compute.googleapis.com/projects/p/global/images/img-1",
		AssetType: TypeImage,
		Resource: &RawResource{
			Version:  "v1",
			Parent:   "//cloudresourcemanager.googleapis.com/projects/p",
			Location: "global",
			Data: json.RawMessage(`{
				"name": "img-1",
				"id": "1234567890",
				"family": "web",
				"status": "READY",
				"creationTimestamp": "2024-01-01T00:00:00Z",
				"archiveSizeBytes": "5368709120",
				"diskSizeGb": "100",
				"storageLocations": ["us-central1"]
			}`),
		},
	}

	n, err := Normalize(a, nil)
	if err != nil {
		t.Fatalf("Normalize(image): %v", err)
	}
	if n == nil {
		t.Fatalf("Normalize(image) returned nil")
	}
	if n.Kind != graph.KindImage {
		t.Fatalf("Kind = %q, want image", n.Kind)
	}
	if n.AssetType != TypeImage {
		t.Fatalf("AssetType = %q, want %q", n.AssetType, TypeImage)
	}
	if got, _ := n.Str(AttrImageID); got != "1234567890" {
		t.Errorf("image_id = %q, want 1234567890", got)
	}
	if got, _ := n.Str(AttrFamily); got != "web" {
		t.Errorf("family = %q, want web", got)
	}
	if got, _ := n.Str(AttrStatus); got != "READY" {
		t.Errorf("status = %q, want READY", got)
	}
	if got, _ := n.Num(AttrStorageBytes); got != 5368709120 {
		t.Errorf("storage_bytes = %v, want 5368709120", got)
	}
	if got, _ := n.Num(AttrSourceDiskSizeGB); got != 100 {
		t.Errorf("source_disk_size_gb = %v, want 100", got)
	}
	if got, _ := n.Str(AttrStorageLocation); got != "us-central1" {
		t.Errorf("storage_location = %q, want us-central1", got)
	}
	if n.Location != "us-central1" {
		t.Errorf("node Location = %q, want us-central1", n.Location)
	}
	if refsComplete, _ := n.Bool(AttrReferencesComplete); refsComplete {
		t.Errorf("references_complete default = true, want false until the template pass runs")
	}
	if cnt, _ := n.Num(AttrReferenceCount); cnt != 0 {
		t.Errorf("reference_count default = %v, want 0", cnt)
	}
}

func TestNormalizeCustomImage_MultipleStorageLocationsSkipsLocation(t *testing.T) {
	a := &RawAsset{
		Name:      "//compute.googleapis.com/projects/p/global/images/img-2",
		AssetType: TypeImage,
		Resource: &RawResource{
			Version:  "v1",
			Location: "global",
			Data: json.RawMessage(`{
				"name": "img-2",
				"status": "READY",
				"creationTimestamp": "2024-01-01T00:00:00Z",
				"archiveSizeBytes": "1073741824",
				"storageLocations": ["us-central1", "europe-west1"]
			}`),
		},
	}

	n, err := Normalize(a, nil)
	if err != nil {
		t.Fatalf("Normalize(image): %v", err)
	}
	if n == nil {
		t.Fatalf("Normalize(image) returned nil")
	}
	if _, ok := n.Str(AttrStorageLocation); ok {
		t.Errorf("storage_location should be absent with multiple distinct storageLocations, got %q", n.Attrs[AttrStorageLocation])
	}
	if n.Location != "" {
		t.Errorf("node Location = %q, want empty when storage location is ambiguous", n.Location)
	}
}

func TestNormalizeMachineImage_TotalStorageBytes(t *testing.T) {
	a := &RawAsset{
		Name:      "//compute.googleapis.com/projects/p/global/machineImages/mi-1",
		AssetType: TypeMachineImage,
		Resource: &RawResource{
			Version:  "v1",
			Parent:   "//cloudresourcemanager.googleapis.com/projects/p",
			Location: "global",
			Data: json.RawMessage(`{
				"name": "mi-1",
				"id": "9876543210",
				"status": "READY",
				"creationTimestamp": "2024-02-01T00:00:00Z",
				"totalStorageBytes": "2147483648",
				"storageLocations": ["us-east1"]
			}`),
		},
	}

	n, err := Normalize(a, nil)
	if err != nil {
		t.Fatalf("Normalize(machine image): %v", err)
	}
	if n == nil {
		t.Fatalf("Normalize(machine image) returned nil")
	}
	if n.Kind != graph.KindImage {
		t.Fatalf("Kind = %q, want image", n.Kind)
	}
	if n.AssetType != TypeMachineImage {
		t.Fatalf("AssetType = %q, want %q", n.AssetType, TypeMachineImage)
	}
	if got, _ := n.Str(AttrMachineImageID); got != "9876543210" {
		t.Errorf("machine_image_id = %q, want 9876543210", got)
	}
	if got, _ := n.Num(AttrStorageBytes); got != 2147483648 {
		t.Errorf("storage_bytes = %v, want 2147483648 (totalStorageBytes)", got)
	}
	if got, _ := n.Str(AttrStorageLocation); got != "us-east1" {
		t.Errorf("storage_location = %q, want us-east1", got)
	}
	if n.Location != "us-east1" {
		t.Errorf("node Location = %q, want us-east1", n.Location)
	}
}
