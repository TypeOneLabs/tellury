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

// TestSnapshotStorage_OnlyTheStandardRateIsIndexed pins the discriminator
// against the RECORDED response.
//
// The Storage Snapshot family returns five GB-Mo products for one region, and
// only one is the ordinary snapshot rate. Indexing on family and unit alone
// wrote all five to the same key, so the last one the API returned decided the
// price — an archive rate is a quarter of the truth, and which one won changed
// between runs.
//
// The previous fixture held a single hand-made product and therefore could not
// express this at all. Its sku was "SNAPSHOT-US-EAST-1" where real SKUs are
// opaque tokens, its usagetype carried a US-West prefix on a us-east-1 record,
// and its storageMedia said "Amazon EBS Snapshots" where the API says
// "Amazon S3". Three invented fields, none of them load-bearing, in a file
// named "recorded".
func TestSnapshotStorage_OnlyTheStandardRateIsIndexed(t *testing.T) {
	raw, err := os.ReadFile("testdata/getproducts-snapshot-recorded.json")
	if err != nil {
		t.Fatalf("read recorded response: %v", err)
	}
	var products []json.RawMessage
	if err := json.Unmarshal(raw, &products); err != nil {
		t.Fatalf("parse recorded response: %v", err)
	}
	if len(products) < 5 {
		t.Fatalf("recorded response has %d products; it must contain the archive and "+
			"outposts siblings or it cannot prove they are excluded", len(products))
	}

	var indexed []catalogueEntry
	for _, p := range products {
		doc, err := parsePriceListDoc(string(p))
		if err != nil {
			t.Fatalf("parse product: %v", err)
		}
		indexed = append(indexed, indexDoc(doc)...)
	}

	if len(indexed) != 1 {
		for _, e := range indexed {
			t.Logf("  indexed: kind=%s sku=%s region=%s price=%v", e.kind, e.sku, e.region, e.price)
		}
		t.Fatalf("indexed %d snapshot entries, want exactly 1: more than one means the "+
			"archive, outposts or underbilling rate can overwrite the real one", len(indexed))
	}
	if got := indexed[0].price; got != 0.05 {
		t.Errorf("indexed price = %v, want 0.05 (the EBS:SnapshotUsage rate for us-east-1); "+
			"0.0125 is the archive tier and 0.027 is Outposts", got)
	}
}
