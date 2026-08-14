package azure

import (
	"context"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

func galleryFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "retail-prices-gallery-recorded.json")
}

func galleryFixtureCatalog(t *testing.T) *CatalogPricer {
	t.Helper()
	t.Setenv("TELLURY_PRICE_FIXTURE", galleryFixturePath(t))
	p, err := NewCatalogPricer(context.Background(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}
	return p
}

func TestCatalogPricer_GalleryImageStorageResolvesRecordedRates(t *testing.T) {
	p := galleryFixtureCatalog(t)

	cases := []struct {
		sku  string
		want float64
	}{
		{"Standard_LRS", 0.05},
		{"StandardSSD_LRS", 0.08},
		{"Premium_LRS", 0.17},
	}
	for _, tc := range cases {
		unit, region, err := p.UnitPrice(pricing.KindGalleryImageStorage, "azure", tc.sku, "westeurope")
		if err != nil {
			t.Fatalf("UnitPrice(gallery_image_storage, %s, westeurope): %v", tc.sku, err)
		}
		if unit != tc.want {
			t.Errorf("UnitPrice(%s) = %v, want %v", tc.sku, unit, tc.want)
		}
		if region != "westeurope" {
			t.Errorf("UnitPrice(%s) region = %q, want westeurope", tc.sku, region)
		}
	}
}

func TestStaticPricer_GalleryImageStorageResolvesRecordedRates(t *testing.T) {
	p, err := NewStaticPricerFromFile(galleryFixturePath(t))
	if err != nil {
		t.Fatalf("NewStaticPricerFromFile: %v", err)
	}

	cases := []struct {
		sku  string
		want float64
	}{
		{"Standard_LRS", 0.05},
		{"StandardSSD_LRS", 0.08},
		{"Premium_LRS", 0.17},
	}
	for _, tc := range cases {
		unit, region, err := p.UnitPrice(pricing.KindGalleryImageStorage, "azure", tc.sku, "westeurope")
		if err != nil {
			t.Fatalf("StaticPricer(%s): %v", tc.sku, err)
		}
		if unit != tc.want || region != "westeurope" {
			t.Errorf("StaticPricer(%s) = %v @ %q, want %v @ westeurope", tc.sku, unit, region, tc.want)
		}
	}
}

func TestGalleryImageStorageFilters_Pinned(t *testing.T) {
	for _, tc := range []struct {
		sku         string
		productName string
		retailSKU   string
		meterName   string
	}{
		{"Standard_LRS", "Standard HDD Managed Disks", "Standard LRS", "Standard LRS Disk"},
		{"StandardSSD_LRS", "Standard SSD Managed Disks", "Standard SSD LRS", "Standard SSD LRS Disk"},
		{"Premium_LRS", "Premium SSD Managed Disks", "Premium LRS", "Premium LRS Disk"},
	} {
		filters, err := galleryImageStorageFilters(tc.sku, "westeurope")
		if err != nil {
			t.Fatalf("galleryImageStorageFilters(%s): %v", tc.sku, err)
		}
		wantEq := map[string]string{
			"serviceName":          "Storage",
			"armRegionName":        "westeurope",
			"skuName":              tc.retailSKU,
			"type":                 "Consumption",
			"productName":          tc.productName,
			"meterName":            tc.meterName,
			"isPrimaryMeterRegion": "true",
			"tierMinimumUnits":     "0",
		}
		for _, f := range filters {
			if f.Op != filterEq {
				t.Errorf("gallery storage filter has non-equality predicate %s %s; storage rows need no spot/windows exclusions", f.Field, f.Value)
				continue
			}
			want, ok := wantEq[f.Field]
			if !ok {
				t.Errorf("unexpected equality filter %q", f.Field)
				continue
			}
			if f.Value != want {
				t.Errorf("filter %s = %q, want %q", f.Field, f.Value, want)
			}
			delete(wantEq, f.Field)
		}
		for field := range wantEq {
			t.Errorf("filter %q missing", field)
		}
	}
}

func TestRecordedGalleryRequestMatchesServerFilter(t *testing.T) {
	entries, err := loadFixtureEntries(galleryFixturePath(t))
	if err != nil {
		t.Fatalf("loadFixtureEntries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("gallery fixture has %d entries, want 3", len(entries))
	}

	for _, tc := range []struct {
		entryName string
		sku       string
	}{
		{"gallery_image_storage_standard_lrs_westeurope", "Standard_LRS"},
		{"gallery_image_storage_standardssd_lrs_westeurope", "StandardSSD_LRS"},
		{"gallery_image_storage_premium_lrs_westeurope", "Premium_LRS"},
	} {
		t.Run(tc.entryName, func(t *testing.T) {
			var entry *retailFixtureEntry
			for i := range entries {
				if entries[i].Name == tc.entryName {
					entry = &entries[i]
					break
				}
			}
			if entry == nil {
				t.Fatalf("fixture entry %q not found", tc.entryName)
			}
			filters, err := galleryImageStorageFilters(tc.sku, "westeurope")
			if err != nil {
				t.Fatalf("galleryImageStorageFilters: %v", err)
			}
			u, err := url.Parse(entry.URL)
			if err != nil {
				t.Fatalf("parse recorded URL: %v", err)
			}
			if got, want := serverFilter(filters), u.Query().Get("$filter"); got != want {
				t.Errorf("serverFilter mismatch\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestGalleryImageStorageFilterRejectsUnknownSKU(t *testing.T) {
	if _, err := galleryImageStorageFilters("Standard_LRS_v2", "westeurope"); err != pricing.ErrNoPrice {
		t.Fatalf("unknown storageAccountType error = %v, want ErrNoPrice", err)
	}
}

func TestNormalizeGalleryImageUnitPrice(t *testing.T) {
	cases := []struct {
		unit  string
		price float64
		want  float64
		ok    bool
	}{
		{"1 GiB/Month", 0.05, 0.05, true},
		{"1 GiB /Month", 0.05, 0.05, true},
		{"1 GB/Month", 0.05, 0.05 * decimalGBPerGiB, true},
		{"1 GB /Month", 0.05, 0.05 * decimalGBPerGiB, true},
		{"1/Month", 0.05, 0, false},
		{"1 Hour", 0.05, 0, false},
	}
	for _, tc := range cases {
		got, ok := normalizeGalleryImageUnitPrice(tc.unit, tc.price)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("normalizeGalleryImageUnitPrice(%q, %v) = (%v, %v), want (%v, %v)",
				tc.unit, tc.price, got, ok, tc.want, tc.ok)
		}
	}
}

func TestGalleryImageStorageServerFilterHasNoSpotExclusions(t *testing.T) {
	filters, err := galleryImageStorageFilters("Standard_LRS", "westeurope")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range filters {
		if f.Op == filterNotContains {
			t.Fatalf("gallery storage filter contains a spot/windows exclusion %s %s; storage rows cannot collide with those VM rows", f.Field, f.Value)
		}
	}
	if got := serverFilter(filters); strings.Contains(got, "Spot") || strings.Contains(got, "Windows") {
		t.Errorf("gallery storage server filter contains a VM-row exclusion: %s", got)
	}
}
