package gcp

import (
	"testing"

	billingpb "cloud.google.com/go/billing/apiv1/billingpb"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/old_machine_image"
	"github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/unused_custom_image"
)

// TestMatchSKU_ImageStorageTokenPinned pins the live-catalogue image-storage
// resource group to the token unused_custom_image actually queries. If either
// side changes without the other, live lookups silently miss.
// The resource groups below are "StorageImage" and "MachineImage" because that
// is what the live Cloud Billing catalogue returns. These tests originally used
// "ImageStorage" and "MachineImageStorage" — the same invented tokens the code
// queried — so they passed while no real SKU could ever match. A hand-built SKU
// test can only ever confirm that the code agrees with itself; see
// image_sku_recorded_test.go, which compares against a recorded response.

func TestMatchSKU_ImageStorageTokenPinned(t *testing.T) {
	sk := &billingpb.Sku{
		Category: &billingpb.Category{
			ServiceDisplayName: "Compute Engine",
			ResourceFamily:     "Storage",
			ResourceGroup:      "StorageImage",
			UsageType:          "OnDemand",
		},
		Description: "Storage Image",
	}

	kind, token, ok := matchSKU(sk)
	if !ok {
		t.Fatalf("matchSKU(%q) must match a live image-storage SKU", sk.GetDescription())
	}
	if kind != pricing.KindImageStorage {
		t.Fatalf("matchSKU kind = %v, want %v", kind, pricing.KindImageStorage)
	}
	if token != unused_custom_image.ImageStorageSKU {
		t.Fatalf("matchSKU token = %q, but unused_custom_image queries %q: live lookups would never match",
			token, unused_custom_image.ImageStorageSKU)
	}
}

// TestMatchSKU_MachineImageStorageTokenPinned pins the live-catalogue
// machine-image-storage resource group to the token old_machine_image queries.
func TestMatchSKU_MachineImageStorageTokenPinned(t *testing.T) {
	sk := &billingpb.Sku{
		Category: &billingpb.Category{
			ServiceDisplayName: "Compute Engine",
			ResourceFamily:     "Storage",
			ResourceGroup:      "MachineImage",
			UsageType:          "OnDemand",
		},
		Description: "Storage Machine Image",
	}

	kind, token, ok := matchSKU(sk)
	if !ok {
		t.Fatalf("matchSKU(%q) must match a live machine-image-storage SKU", sk.GetDescription())
	}
	if kind != pricing.KindMachineImageStorage {
		t.Fatalf("matchSKU kind = %v, want %v", kind, pricing.KindMachineImageStorage)
	}
	if token != old_machine_image.MachineImageStorageSKU {
		t.Fatalf("matchSKU token = %q, but old_machine_image queries %q: live lookups would never match",
			token, old_machine_image.MachineImageStorageSKU)
	}
}

// TestMatchSKU_ImageEarlyDeletionIgnored guards against indexing a one-off
// early-deletion charge as a standing per-GiB-month storage rate.
func TestMatchSKU_ImageEarlyDeletionIgnored(t *testing.T) {
	sk := &billingpb.Sku{
		Category: &billingpb.Category{
			ServiceDisplayName: "Compute Engine",
			ResourceFamily:     "Storage",
			ResourceGroup:      "StorageImage",
			UsageType:          "OnDemand",
		},
		Description: "Image Early Deletion",
	}
	if _, _, ok := matchSKU(sk); ok {
		t.Error("an image early-deletion charge must not be indexed as image storage")
	}
}

// TestFixtureSupportsImageAndMachineImageStorage ensures the shipped GCP price
// fixture has entries for both new dimensions, so offline scans and tests can
// resolve them through the same StaticPricer path.
func TestFixtureSupportsImageAndMachineImageStorage(t *testing.T) {
	static := gcpPriceFixture(t)
	if _, _, err := static.UnitPrice(pricing.KindImageStorage, "gcp", "standard", "default"); err != nil {
		t.Fatalf("fixture has no image_storage entry: %v", err)
	}
	if _, _, err := static.UnitPrice(pricing.KindMachineImageStorage, "gcp", "standard", "default"); err != nil {
		t.Fatalf("fixture has no machine_image_storage entry: %v", err)
	}
}
