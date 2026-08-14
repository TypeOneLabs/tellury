package azure

import (
	"context"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// The fixtures below use "sizeInGB" because that is the field Azure Resource
// Graph actually returns on storageProfile.osDiskImage — verified against a
// live gallery image version, whose only size field is sizeInGB. They
// previously used "sizeInBytes", the same field the normalizer read and
// neither the API nor the docs mention, so the tests confirmed the code agreed
// with itself while every real gallery image version skipped as
// missing_attribute.

func TestNormalizeGalleryImageVersion_MapsFieldsSizeAndReplicas(t *testing.T) {
	const versionID = "/subscriptions/sub-1/resourceGroups/rg-gallery/providers/Microsoft.Compute/galleries/gal-a/images/img-a/versions/1.0.0"
	row := map[string]any{
		"id":             versionID,
		"name":           "1.0.0",
		"type":           "microsoft.compute/galleries/images/versions",
		"location":       "West Europe",
		"resourceGroup":  "rg-gallery",
		"subscriptionId": "sub-1",
		"properties": map[string]any{
			"provisioningState": "Succeeded",
			"publishingProfile": map[string]any{
				"publishedDate": "2024-01-02T03:04:05Z",
				"targetRegions": []any{
					map[string]any{
						"name":                 "West Europe",
						"regionalReplicaCount": 3.0,
						"storageAccountType":   "Standard_LRS",
					},
					map[string]any{
						"name": "East US",
					},
				},
			},
			"storageProfile": map[string]any{
				"osDiskImage": map[string]any{
					"sizeInGB": 100.0,
				},
				"dataDiskImages": []any{
					map[string]any{"sizeInGB": 10.0},
					map[string]any{"sizeInGB": 20.0},
				},
			},
		},
	}

	n := NormalizeGalleryImageVersion(row)
	if n == nil {
		t.Fatal("NormalizeGalleryImageVersion returned nil")
	}
	if n.Kind != graph.KindImage {
		t.Errorf("Kind = %s, want image", n.Kind)
	}
	if n.AssetType != TypeGalleryImageVersion {
		t.Errorf("AssetType = %q, want %q", n.AssetType, TypeGalleryImageVersion)
	}
	if n.Location != "westeurope" {
		t.Errorf("Location = %q, want westeurope", n.Location)
	}
	if got, _ := n.Str(AttrResourceID); got != versionID {
		t.Errorf("resource_id = %q, want %q", got, versionID)
	}
	if got, _ := n.Str(AttrGalleryImageID); got != "/subscriptions/sub-1/resourceGroups/rg-gallery/providers/Microsoft.Compute/galleries/gal-a/images/img-a" {
		t.Errorf("gallery_image_id = %q, want the parent definition ID", got)
	}
	if got, _ := n.Str(AttrCreationTimestamp); got != "2024-01-02T03:04:05Z" {
		t.Errorf("creation_timestamp = %q, want publishedDate", got)
	}
	if got, _ := n.Str(AttrProvisioningState); got != "Succeeded" {
		t.Errorf("provisioning_state = %q, want Succeeded", got)
	}

	sizeBytes, ok := n.Num(AttrGallerySizeBytes)
	if !ok {
		t.Fatal("size_bytes absent")
	}
	wantSize := 130.0 * (1 << 30)
	if sizeBytes != wantSize {
		t.Errorf("size_bytes = %v, want %v (100 + 10 + 20 GiB)", sizeBytes, wantSize)
	}

	replicaCount, ok := n.Num(AttrGalleryReplicaCount)
	if !ok {
		t.Fatal("replica_count absent")
	}
	if replicaCount != 4 {
		t.Errorf("replica_count = %v, want 4 (3 + default 1)", replicaCount)
	}

	regions, ok := n.Attrs[AttrGalleryReplicaRegions].([]map[string]any)
	if !ok {
		t.Fatalf("replica_regions has type %T, want []map[string]any", n.Attrs[AttrGalleryReplicaRegions])
	}
	if len(regions) != 2 {
		t.Fatalf("replica_regions = %d entries, want 2", len(regions))
	}
	if regions[0]["region"] != "westeurope" || regions[0]["replica_count"] != 3.0 || regions[0]["storage_account_type"] != "Standard_LRS" {
		t.Errorf("first replica region = %#v, want westeurope/3/Standard_LRS", regions[0])
	}
	if regions[1]["region"] != "eastus" || regions[1]["replica_count"] != 1.0 || regions[1]["storage_account_type"] != "Standard_LRS" {
		t.Errorf("second replica region = %#v, want eastus/1/Standard_LRS defaults", regions[1])
	}
}

func TestNormalizeGalleryImageVersion_MissingTargetRegionsIsAbsent(t *testing.T) {
	row := map[string]any{
		"id":             "/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/galleries/gal/images/img/versions/1.0.0",
		"name":           "1.0.0",
		"type":           "microsoft.compute/galleries/images/versions",
		"location":       "westeurope",
		"resourceGroup":  "rg",
		"subscriptionId": "sub-1",
		"properties": map[string]any{
			"provisioningState": "Succeeded",
			"publishingProfile": map[string]any{
				"publishedDate": "2024-01-02T03:04:05Z",
			},
			"storageProfile": map[string]any{
				"osDiskImage": map[string]any{"sizeInGB": 100.0},
			},
		},
	}

	n := NormalizeGalleryImageVersion(row)
	if n == nil {
		t.Fatal("NormalizeGalleryImageVersion returned nil")
	}
	if _, ok := n.Attrs[AttrGalleryReplicaRegions]; ok {
		t.Error("replica_regions must be absent when targetRegions is absent; the rule then skips SkipMissingAttr")
	}
	if _, ok := n.Attrs[AttrGalleryReplicaCount]; ok {
		t.Error("replica_count must be absent when targetRegions is absent")
	}
}

func TestCollectImageReferences_ExactVersionAndScaleSetDefinition(t *testing.T) {
	versionID := "/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/galleries/gal/images/img/versions/1.0.0"
	definitionID := "/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/galleries/gal/images/img"

	rows := []map[string]any{
		{
			"type": argTypeVM,
			"properties": map[string]any{
				"storageProfile": map[string]any{
					"imageReference": map[string]any{"id": versionID},
				},
			},
		},
		{
			"type": argTypeVMSS,
			"properties": map[string]any{
				"virtualMachineProfile": map[string]any{
					"storageProfile": map[string]any{
						"imageReference": map[string]any{"id": definitionID},
					},
				},
			},
		},
		{
			"type": argTypeVM,
			"properties": map[string]any{
				"storageProfile": map[string]any{
					"imageReference": map[string]any{"publisher": "Canonical", "offer": "UbuntuServer"},
				},
			},
		},
	}

	refs, complete := collectImageReferences(rows)
	if !complete {
		t.Error("references_complete = false for well-formed VM/VMSS rows")
	}
	if len(refs.refs) != 2 {
		t.Fatalf("collected %d references, want 2", len(refs.refs))
	}

	versionRow := map[string]any{
		"id":             versionID,
		"name":           "1.0.0",
		"type":           argTypeGalleryImageVersion,
		"location":       "westeurope",
		"resourceGroup":  "rg",
		"subscriptionId": "sub-1",
		"properties": map[string]any{
			"provisioningState": "Succeeded",
			"publishingProfile": map[string]any{
				"publishedDate": "2024-01-02T03:04:05Z",
				"targetRegions": []any{
					map[string]any{"name": "westeurope"},
				},
			},
			"storageProfile": map[string]any{
				"osDiskImage": map[string]any{"sizeInGB": 100.0},
			},
		},
	}
	n := NormalizeGalleryImageVersion(versionRow)
	if n == nil {
		t.Fatal("NormalizeGalleryImageVersion returned nil")
	}

	count, sources := refs.CountFor(n)
	if count != 2 {
		t.Errorf("reference_count = %v, want 2 (exact VM + parent-definition VMSS)", count)
	}
	if len(sources) != 2 || sources[0] != "vm" || sources[1] != "vmss" {
		t.Errorf("reference_sources = %v, want [vm vmss]", sources)
	}
}

func TestCollectImageReferences_MalformedVMSSMarksIncomplete(t *testing.T) {
	rows := []map[string]any{
		{
			"type":       argTypeVMSS,
			"properties": "not-an-object",
		},
	}
	refs, complete := collectImageReferences(rows)
	if complete {
		t.Error("references_complete = true for a malformed VMSS row")
	}
	if len(refs.refs) != 0 {
		t.Errorf("collected references = %d, want 0", len(refs.refs))
	}
}

func TestProviderIngest_GalleryImageVersionReferencesScaleSetDefinition(t *testing.T) {
	versionID := "/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/galleries/gal/images/img/versions/1.0.0"
	definitionID := "/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/galleries/gal/images/img"

	rows := []map[string]any{
		{
			"id":             versionID,
			"name":           "1.0.0",
			"type":           argTypeGalleryImageVersion,
			"location":       "westeurope",
			"resourceGroup":  "rg",
			"subscriptionId": "sub-1",
			"properties": map[string]any{
				"provisioningState": "Succeeded",
				"publishingProfile": map[string]any{
					"publishedDate": "2024-01-02T03:04:05Z",
					"targetRegions": []any{
						map[string]any{"name": "westeurope", "regionalReplicaCount": 3.0, "storageAccountType": "Standard_LRS"},
					},
				},
				"storageProfile": map[string]any{
					"osDiskImage": map[string]any{"sizeInGB": 100.0},
				},
			},
		},
		{
			"id":             "/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-a",
			"name":           "vm-a",
			"type":           argTypeVM,
			"location":       "westeurope",
			"resourceGroup":  "rg",
			"subscriptionId": "sub-1",
			"properties": map[string]any{
				"storageProfile": map[string]any{
					"imageReference": map[string]any{"id": versionID},
				},
			},
		},
		{
			"id":             "/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss-a",
			"name":           "vmss-a",
			"type":           argTypeVMSS,
			"location":       "westeurope",
			"resourceGroup":  "rg",
			"subscriptionId": "sub-1",
			"properties": map[string]any{
				"virtualMachineProfile": map[string]any{
					"storageProfile": map[string]any{
						"imageReference": map[string]any{"id": definitionID},
					},
				},
			},
		},
	}

	p, err := New(context.Background(),
		WithOffline(),
		WithLogger(newTestLogger()),
		WithFixture(&Fixture{
			ManagementGroups: map[string]*ManagementGroupFixture{},
			Subscriptions: map[string]*SubscriptionFixture{
				"sub-1": {Resources: rows},
			},
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	sc := cloud.Scope{Provider: "azure", Azure: &cloud.AzureScope{Subscription: "sub-1"}}
	gr, err := p.Ingest(context.Background(), sc, []string{TypeGalleryImageVersion})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	n, ok := gr.Node(graph.Ref(versionID))
	if !ok {
		t.Fatalf("gallery image version node %s missing", versionID)
	}
	if got, ok := n.Bool(AttrReferencesComplete); !ok || !got {
		t.Errorf("references_complete = %v, %v; want true, true", got, ok)
	}
	if got, ok := n.Num(AttrReferenceCount); !ok || got != 2 {
		t.Errorf("reference_count = %v, %v; want 2 (VM exact + VMSS definition)", got, ok)
	}
	sources, _ := n.Attrs[AttrReferenceSources].([]string)
	if len(sources) != 2 || sources[0] != "vm" || sources[1] != "vmss" {
		t.Errorf("reference_sources = %v, want [vm vmss]", sources)
	}
}
