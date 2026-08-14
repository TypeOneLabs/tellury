package gcp

import (
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// The multi-region image substitution, pinned.
//
// Measured against the live Cloud Billing catalogue on 2026-08-14 by walking
// every page (32,242 SKUs): StorageImage publishes 45 SKUs and every one is
// regional — there is no "us", "eu", "europe" or "asia" entry. MachineImage
// publishes all three multi-regions at exactly $0.05/GiB-month.
//
// Because GCP defaults a new custom image to a multi-region location, requiring
// an exact StorageImage SKU meant unused_custom_image skipped as unpriced on
// the images most projects actually have. The substitution closes that, and is
// recorded as SourceEquivalentSKU so it is never mistaken for a direct hit.

// pricerWith builds a CatalogPricer over a hand-built index. c.client stays nil
// and the fixture path is set by the caller's environment, so these tests
// exercise the lookup and substitution logic only — matchSKU and the live
// catalogue are covered by image_sku_recorded_test.go against a real response.
func pricerWith(entries map[skuKey]resolvedSKU) *CatalogPricer {
	c := &CatalogPricer{
		skusByKey: entries,
		last:      map[string]pricing.Provenance{},
	}
	c.once.Do(func() {}) // catalogue is pre-loaded; never call the API
	return c
}

const (
	imgSKU     = "standard"
	multiPrice = 0.05
	regnPrice  = 0.06
)

func TestLiveUnitPrice_MultiRegionImageFallsBackToMachineImageSKU(t *testing.T) {
	c := pricerWith(map[skuKey]resolvedSKU{
		// Only the machine-image multi-region SKU exists, exactly as the live
		// catalogue has it.
		{kind: pricing.KindMachineImageStorage, sku: imgSKU, region: "europe"}: {
			skuID: "MI-EUROPE", unitPrice: multiPrice, region: "europe",
		},
	})

	// "eu" is the RESOURCE-side spelling; regionCandidates aliases it to
	// "europe", which is how the catalogue spells the multi-region.
	price, res, err := c.liveUnitPrice(pricing.KindImageStorage, imgSKU, "eu")
	if err != nil {
		t.Fatalf("liveUnitPrice: %v — a multi-region custom image must not be unpriced", err)
	}
	if price != multiPrice {
		t.Errorf("unit price = %v, want %v", price, multiPrice)
	}
	if !res.substitute {
		t.Error("substitute = false: a price from another SKU must be marked, " +
			"or the evidence reads like a direct catalogue hit")
	}
	if res.region != "europe" {
		t.Errorf("resolved region = %q, want %q", res.region, "europe")
	}
}

// A real StorageImage SKU must always win. If the substitution ever shadowed a
// published price, every regional image would silently be priced at the
// multi-region rate.
func TestLiveUnitPrice_RealImageSKUWinsOverSubstitute(t *testing.T) {
	c := pricerWith(map[skuKey]resolvedSKU{
		{kind: pricing.KindImageStorage, sku: imgSKU, region: "europe-west3"}: {
			skuID: "SI-FRANKFURT", unitPrice: regnPrice, region: "europe-west3",
		},
		{kind: pricing.KindMachineImageStorage, sku: imgSKU, region: "europe"}: {
			skuID: "MI-EUROPE", unitPrice: multiPrice, region: "europe",
		},
	})

	price, res, err := c.liveUnitPrice(pricing.KindImageStorage, imgSKU, "europe-west3")
	if err != nil {
		t.Fatalf("liveUnitPrice: %v", err)
	}
	if price != regnPrice {
		t.Errorf("unit price = %v, want the published regional %v", price, regnPrice)
	}
	if res.substitute {
		t.Error("substitute = true for a region with its own published SKU")
	}
}

// The substitution is image-storage only. Nothing else may borrow a price from
// a different product because its own region is missing.
func TestLiveUnitPrice_SubstituteIsImageStorageOnly(t *testing.T) {
	c := pricerWith(map[skuKey]resolvedSKU{
		{kind: pricing.KindMachineImageStorage, sku: imgSKU, region: "europe"}: {
			skuID: "MI-EUROPE", unitPrice: multiPrice, region: "europe",
		},
	})

	if _, _, err := c.liveUnitPrice(pricing.KindSnapshotStorage, imgSKU, "eu"); err == nil {
		t.Error("snapshot storage borrowed the machine-image price; the " +
			"substitution must be narrow to image storage")
	}
}

// A single-region location must never reach the substitution: only the three
// multi-regions have no published StorageImage SKU.
func TestLiveUnitPrice_SubstituteIsMultiRegionOnly(t *testing.T) {
	c := pricerWith(map[skuKey]resolvedSKU{
		{kind: pricing.KindMachineImageStorage, sku: imgSKU, region: "europe-west3"}: {
			skuID: "MI-FRANKFURT", unitPrice: multiPrice, region: "europe-west3",
		},
	})

	// regionCandidates expands "europe-west3" to the prefix "europe", which is
	// a multi-region token — but there is no machine-image SKU there, so this
	// must still miss rather than borrowing the Frankfurt machine-image price.
	if _, _, err := c.liveUnitPrice(pricing.KindImageStorage, imgSKU, "europe-west3"); err == nil {
		t.Error("a regional image borrowed a regional machine-image price; " +
			"the substitution must apply only to multi-region locations")
	}
}

func TestIsMultiRegion(t *testing.T) {
	for _, r := range []string{"us", "eu", "europe", "asia", "EU", "US"} {
		if !isMultiRegion(r) {
			t.Errorf("isMultiRegion(%q) = false, want true", r)
		}
	}
	for _, r := range []string{"europe-west1", "us-central1", "asia-east1", "", "global", "default"} {
		if isMultiRegion(r) {
			t.Errorf("isMultiRegion(%q) = true, want false", r)
		}
	}
}

// The substituted price must surface as its own source. "live_api" would tell a
// reader the catalogue answered for image storage in that region, which it did
// not.
func TestUnitPrice_RecordsEquivalentSKUProvenance(t *testing.T) {
	// UnitPrice consults the live path only when a client or a fixture path is
	// configured. The catalogue is already loaded (once was consumed in
	// pricerWith), so this only opens the gate — no file is ever read.
	t.Setenv("TELLURY_PRICE_FIXTURE", "preloaded")

	c := pricerWith(map[skuKey]resolvedSKU{
		{kind: pricing.KindMachineImageStorage, sku: imgSKU, region: "europe"}: {
			skuID: "MI-EUROPE", unitPrice: multiPrice, region: "europe",
		},
	})

	if _, _, err := c.UnitPrice(pricing.KindImageStorage, "gcp", imgSKU, "eu"); err != nil {
		t.Fatalf("UnitPrice: %v", err)
	}
	prov, ok := c.LastLookup(pricing.KindImageStorage, imgSKU, "eu")
	if !ok {
		t.Fatal("no provenance recorded")
	}
	if prov.Source != pricing.SourceEquivalentSKU {
		t.Errorf("source = %q, want %q", prov.Source, pricing.SourceEquivalentSKU)
	}
	if prov.SKU != "MI-EUROPE" {
		t.Errorf("SKU = %q, want the substituted SKU id so it is traceable", prov.SKU)
	}
}

// TestUnitPrice_ProvenanceFoundUnderTheResolvedRegion pins a defect that made a
// LIVE price describe itself as a fixture.
//
// A rule asks for the resource's location ("eu") and renders its evidence with
// the region UnitPrice RETURNED ("europe", the catalogue's spelling). The
// provenance was keyed only by the region asked for, so that lookup missed and
// PriceEvidence fell back to SourceFixture. Observed in a fully live scan on
// 2026-08-14: old_machine_image reported "fixture sku=standard region=europe".
//
// Both spellings must resolve, and neither may report SourceFixture.
func TestUnitPrice_ProvenanceFoundUnderTheResolvedRegion(t *testing.T) {
	t.Setenv("TELLURY_PRICE_FIXTURE", "preloaded")

	c := pricerWith(map[skuKey]resolvedSKU{
		{kind: pricing.KindMachineImageStorage, sku: imgSKU, region: "europe"}: {
			skuID: "MI-EUROPE", unitPrice: multiPrice, region: "europe",
		},
	})

	if _, resolved, err := c.UnitPrice(pricing.KindMachineImageStorage, "gcp", imgSKU, "eu"); err != nil {
		t.Fatalf("UnitPrice: %v", err)
	} else if resolved != "europe" {
		t.Fatalf("resolved region = %q, want %q", resolved, "europe")
	}

	for _, region := range []string{"eu", "europe"} {
		prov, ok := c.LastLookup(pricing.KindMachineImageStorage, imgSKU, region)
		if !ok {
			t.Errorf("no provenance under region %q: evidence rendered with this "+
				"spelling would claim a fixture answered a live scan", region)
			continue
		}
		if prov.Source == pricing.SourceFixture {
			t.Errorf("region %q reports SourceFixture for a catalogue answer", region)
		}
	}
}
