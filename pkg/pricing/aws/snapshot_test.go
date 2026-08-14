package aws

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// TestCatalogPricer_EBSSnapshotStoragePinned pins the AWS EBS snapshot-storage
// pricing tokens against a recorded GetProducts response:
//
//   - the product family must be "Storage Snapshot";
//   - the only indexed units are "GB-Mo" and "GB-month";
//   - the SKU token is "standard", matching unused_ami.SnapshotStorageSKU;
//   - the price is indexed under pricing.KindSnapshotStorage.
func TestCatalogPricer_EBSSnapshotStoragePinned(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "getproducts-snapshot-recorded.json"))
	if err != nil {
		t.Fatalf("read snapshot fixture: %v", err)
	}
	var stored []json.RawMessage
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode snapshot fixture: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("snapshot fixture is empty")
	}

	foundGBMo, foundGBMonth := false, false
	for _, rawDoc := range stored {
		doc, err := parsePriceListDoc(string(rawDoc))
		if err != nil {
			t.Fatalf("parsePriceListDoc: %v", err)
		}
		if doc.Product.ProductFamily != "Storage Snapshot" {
			t.Fatalf("fixture product family = %q, want %q", doc.Product.ProductFamily, "Storage Snapshot")
		}
		for _, dim := range priceDimensions(doc) {
			if dim.unit != "GB-Mo" && dim.unit != "GB-month" {
				t.Fatalf("unexpected snapshot unit %q; only GB-Mo/GB-month are pinned", dim.unit)
			}
		}
		entries := indexDoc(doc)
		if len(entries) == 0 {
			t.Fatalf("indexDoc produced no entries for %s", doc.Product.Attributes["regionCode"])
		}
		for _, e := range entries {
			if e.kind != pricing.KindSnapshotStorage {
				t.Fatalf("snapshot product indexed under kind %q, want %q", e.kind, pricing.KindSnapshotStorage)
			}
			if e.sku != "standard" {
				t.Fatalf("snapshot product indexed under sku %q, want %q", e.sku, "standard")
			}
			switch e.region {
			case "us-east-1":
				if e.price != 0.05 {
					t.Errorf("us-east-1 snapshot price = %v, want 0.05", e.price)
				}
				foundGBMo = true
			case "eu-west-1":
				if e.price != 0.055 {
					t.Errorf("eu-west-1 snapshot price = %v, want 0.055", e.price)
				}
				foundGBMonth = true
			default:
				t.Fatalf("unexpected region %q in snapshot fixture", e.region)
			}
		}
	}
	if !foundGBMo {
		t.Fatal("snapshot fixture has no GB-Mo product; the unit pin cannot be asserted")
	}
	if !foundGBMonth {
		t.Fatal("snapshot fixture has no GB-month product; the unit pin cannot be asserted")
	}
}

// TestCatalogPricer_SnapshotStorageResolvesEndToEnd proves the live-catalogue
// path resolves the same SKU the unused_ami rule queries.
func TestCatalogPricer_SnapshotStorageResolvesEndToEnd(t *testing.T) {
	p := fixtureCatalog(t)

	// Load the snapshot fixture into the same cache the normal fixture uses.
	raw, err := os.ReadFile(filepath.Join("testdata", "getproducts-snapshot-recorded.json"))
	if err != nil {
		t.Fatalf("read snapshot fixture: %v", err)
	}
	var stored []json.RawMessage
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode snapshot fixture: %v", err)
	}
	for _, rawDoc := range stored {
		doc, err := parsePriceListDoc(string(rawDoc))
		if err != nil {
			t.Fatalf("parsePriceListDoc: %v", err)
		}
		for _, e := range indexDoc(doc) {
			p.skusByKey[skuKey{kind: e.kind, sku: e.sku, region: e.region}] = resolvedSKU{
				unitPrice: e.price,
				region:    e.region,
			}
		}
	}

	unit, region, err := p.UnitPrice(pricing.KindSnapshotStorage, "aws", "standard", "us-east-1")
	if err != nil {
		t.Fatalf("UnitPrice(snapshot_storage, standard, us-east-1): %v", err)
	}
	if unit != 0.05 {
		t.Errorf("UnitPrice = %v, want 0.05", unit)
	}
	if region != "us-east-1" {
		t.Errorf("resolved region = %q, want us-east-1", region)
	}
}
