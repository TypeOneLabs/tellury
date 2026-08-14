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

	foundGBMo := false
	for _, rawDoc := range stored {
		doc, err := parsePriceListDoc(string(rawDoc))
		if err != nil {
			t.Fatalf("parsePriceListDoc: %v", err)
		}
		if doc.Product.ProductFamily != "Storage Snapshot" {
			t.Fatalf("fixture product family = %q, want %q", doc.Product.ProductFamily, "Storage Snapshot")
		}
		// The recorded family contains the archive tiers, the Outposts rate and
		// the underbilling meter alongside the standard one. Only the standard
		// product indexes, and only its dimensions are pinned — the retrieval
		// product is priced per GB, not per GB-month, and must not be indexed
		// at all. Both assumptions below were previously false-by-construction:
		// the fixture held one hand-made product, so "every product indexes"
		// and "every unit is GB-Mo" were true of the file and of nothing else.
		usage := doc.Product.Attributes["usagetype"]
		entries := indexDoc(doc)
		if !isStandardSnapshotUsage(usage) {
			if len(entries) != 0 {
				t.Errorf("non-standard usagetype %q produced %d entries; the archive, "+
					"Outposts and underbilling rates must not be indexed", usage, len(entries))
			}
			continue
		}
		for _, dim := range priceDimensions(doc) {
			if dim.unit != "GB-Mo" && dim.unit != "GB-month" {
				t.Fatalf("unexpected unit %q on the standard snapshot product", dim.unit)
			}
		}
		if len(entries) == 0 {
			t.Fatalf("indexDoc produced no entries for the standard product in %s",
				doc.Product.Attributes["regionCode"])
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
				if e.price != 0.05 {
					t.Errorf("eu-west-1 snapshot price = %v, want 0.05", e.price)
				}
				foundGBMo = true
			default:
				t.Fatalf("unexpected region %q in snapshot fixture", e.region)
			}
		}
	}
	if !foundGBMo {
		t.Fatal("snapshot fixture has no GB-Mo product; the unit pin cannot be asserted")
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

	// Exactly ONE entry per region. More than one means a sibling rate reached
	// the same key and, being a map write, would decide the price by response
	// order.
	byRegion := map[string][]catalogueEntry{}
	for _, e := range indexed {
		byRegion[e.region] = append(byRegion[e.region], e)
	}
	if len(byRegion) < 2 {
		t.Fatalf("recording covers %d region(s); it must cover more than one so the "+
			"region-prefixed usagetype form is exercised too", len(byRegion))
	}
	for region, entries := range byRegion {
		if len(entries) != 1 {
			for _, e := range entries {
				t.Logf("  %s: sku=%s price=%v", e.region, e.sku, e.price)
			}
			t.Errorf("%s indexed %d entries, want 1: the archive, Outposts and underbilling "+
				"rates share this family and unit, and the last one written would win",
				region, len(entries))
			continue
		}
		if got := entries[0].price; got != 0.05 {
			t.Errorf("%s price = %v, want 0.05 (EBS:SnapshotUsage); 0.0125 is the archive "+
				"tier and 0.027 is Outposts", region, got)
		}
	}
}
