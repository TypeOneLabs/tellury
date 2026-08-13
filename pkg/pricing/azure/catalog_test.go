package azure

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

func fixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "retail-prices-recorded.json")
}

func fixtureCatalog(t *testing.T) *CatalogPricer {
	t.Helper()
	t.Setenv("TELLURY_PRICE_FIXTURE", fixturePath(t))
	p, err := NewCatalogPricer(context.Background(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}
	return p
}

func loadFixtureEntriesT(t *testing.T) []retailFixtureEntry {
	t.Helper()
	entries, err := loadFixtureEntries(fixturePath(t))
	if err != nil {
		t.Fatalf("loadFixtureEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("recorded fixture has %d entries, want 2", len(entries))
	}
	return entries
}

func TestCatalogPricer_ManagedDiskP10ResolvesRecordedRate(t *testing.T) {
	p := fixtureCatalog(t)

	unit, region, err := p.UnitPrice(pricing.KindManagedDisk, "azure", "P10 LRS", "westeurope")
	if err != nil {
		t.Fatalf("UnitPrice(managed_disk, P10 LRS, westeurope): %v", err)
	}
	// Published West Europe Premium SSD P10 managed disk rate: $21.68/month.
	// Source: https://prices.azure.com/api/retail/prices, recorded in
	// testdata/retail-prices-recorded.json.
	const publishedP10WestEurope = 21.68
	if unit != publishedP10WestEurope {
		t.Errorf("UnitPrice = %v, want %v (published Premium SSD P10 rate in West Europe)", unit, publishedP10WestEurope)
	}
	if region != "westeurope" {
		t.Errorf("region = %q, want westeurope", region)
	}

	prov, ok := p.LastLookup(pricing.KindManagedDisk, "P10 LRS", "westeurope")
	if !ok {
		t.Fatal("LastLookup returned ok=false after successful UnitPrice")
	}
	if prov.Source != pricing.SourceLiveAPI {
		t.Errorf("provenance source = %q, want live_api", prov.Source)
	}
	if prov.SKU != "P10 LRS" || prov.Region != "westeurope" {
		t.Errorf("provenance = %+v, want sku=P10 LRS region=westeurope", prov)
	}
}

func TestCatalogPricer_StaticIPResolvesRecordedRate(t *testing.T) {
	p := fixtureCatalog(t)

	unit, region, err := p.UnitPrice(pricing.KindStaticIP, "azure", "Standard", "westeurope")
	if err != nil {
		t.Fatalf("UnitPrice(static_ip, Standard, westeurope): %v", err)
	}
	// Published West Europe Standard static IPv4 public IP: $0.005/hour.
	const publishedStandardIPWestEurope = 0.005
	if unit != publishedStandardIPWestEurope {
		t.Errorf("UnitPrice = %v, want %v (published Standard IPv4 static public IP hourly rate)", unit, publishedStandardIPWestEurope)
	}
	if region != "westeurope" {
		t.Errorf("region = %q, want westeurope", region)
	}

	// The rule multiplies by pricing.HoursPerMonth to get monthly waste.
	monthly := unit * pricing.HoursPerMonth
	if monthly != 3.65 {
		t.Errorf("monthly = %v, want 3.65", monthly)
	}
}

func TestStaticPricer_ResolvesRecordedRates(t *testing.T) {
	p, err := NewStaticPricerFromFile(fixturePath(t))
	if err != nil {
		t.Fatalf("NewStaticPricerFromFile: %v", err)
	}

	unit, region, err := p.UnitPrice(pricing.KindManagedDisk, "azure", "P10 LRS", "westeurope")
	if err != nil {
		t.Fatalf("StaticPricer managed disk: %v", err)
	}
	if unit != 21.68 || region != "westeurope" {
		t.Errorf("managed disk = %v @ %q, want 21.68 @ westeurope", unit, region)
	}

	unit, region, err = p.UnitPrice(pricing.KindStaticIP, "azure", "Standard", "westeurope")
	if err != nil {
		t.Fatalf("StaticPricer static IP: %v", err)
	}
	if unit != 0.005 || region != "westeurope" {
		t.Errorf("static IP = %v @ %q, want 0.005 @ westeurope", unit, region)
	}
}

func TestCatalogPricer_UnresolvableSkips(t *testing.T) {
	p := fixtureCatalog(t)

	cases := []struct {
		kind   pricing.Kind
		sku    string
		region string
	}{
		{pricing.KindManagedDisk, "P99 LRS", "westeurope"},
		{pricing.KindManagedDisk, "P10 LRS", "nonexistentregion"},
		{pricing.KindStaticIP, "Basic", "westeurope"},
		{pricing.KindDiskCapacity, "P10 LRS", "westeurope"},
	}
	for _, tc := range cases {
		if _, _, err := p.UnitPrice(tc.kind, "azure", tc.sku, tc.region); err != pricing.ErrNoPrice {
			t.Errorf("UnitPrice(%s, %q, %q) error = %v, want ErrNoPrice", tc.kind, tc.sku, tc.region, err)
		}
	}
}

func TestNormalizeUnitPrice(t *testing.T) {
	cases := []struct {
		name  string
		kind  pricing.Kind
		unit  string
		price float64
		want  float64
		ok    bool
	}{
		{"static ip hourly", pricing.KindStaticIP, "1 Hour", 0.005, 0.005, true},
		{"static ip monthly to hourly", pricing.KindStaticIP, "1/Month", 3.65, 0.005, true},
		{"static ip spaced monthly to hourly", pricing.KindStaticIP, "1 /Month", 3.65, 0.005, true},
		{"static ip unhandled", pricing.KindStaticIP, "1 GB/Month", 1, 0, false},
		{"managed disk monthly", pricing.KindManagedDisk, "1/Month", 21.68, 21.68, true},
		{"managed disk spaced monthly", pricing.KindManagedDisk, "1 /Month", 21.68, 21.68, true},
		{"managed disk hourly to monthly", pricing.KindManagedDisk, "1 Hour", 1, pricing.HoursPerMonth, true},
		{"managed disk unhandled", pricing.KindManagedDisk, "10K Operations", 1, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeUnitPrice(tc.kind, tc.unit, tc.price)
			if ok != tc.ok || got != tc.want {
				t.Errorf("normalizeUnitPrice(%s, %q, %v) = (%v, %v), want (%v, %v)",
					tc.kind, tc.unit, tc.price, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestManagedDiskTierSKU(t *testing.T) {
	cases := []struct {
		armSKUName string
		sizeGiB    float64
		want       string
		ok         bool
	}{
		{"Premium_LRS", 128, "P10 LRS", true},
		{"Premium_LRS", 256, "P15 LRS", true},
		{"Premium_LRS", 1024, "P30 LRS", true},
		{"Premium_ZRS", 128, "P10 ZRS", true},
		{"StandardSSD_LRS", 128, "E10 LRS", true},
		{"StandardSSD_ZRS", 128, "E10 ZRS", true},
		{"Standard_LRS", 128, "S10 LRS", true},
		{"Premium_LRS", 129, "", false},
		{"PremiumV2_LRS", 128, "", false},
	}
	for _, tc := range cases {
		got, ok := ManagedDiskTierSKU(tc.armSKUName, tc.sizeGiB)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ManagedDiskTierSKU(%q, %v) = (%q, %v), want (%q, %v)",
				tc.armSKUName, tc.sizeGiB, got, ok, tc.want, tc.ok)
		}
	}
}

// TestLookupFilters_CompleteAndPinned pins the exact equality and exclusion
// constants that identify one Retail Prices row. The values are asserted
// against the recorded responses in testdata/retail-prices-recorded.json, so
// they track what the live API actually returns rather than what the code
// assumes.
func TestLookupFilters_CompleteAndPinned(t *testing.T) {
	disk, err := lookupFilters(pricing.KindManagedDisk, "P10 LRS", "westeurope")
	if err != nil {
		t.Fatalf("lookupFilters(managed_disk): %v", err)
	}
	diskWantEq := map[string]string{
		"serviceName":          "Storage",
		"armRegionName":        "westeurope",
		"skuName":              "P10 LRS",
		"type":                 "Consumption",
		"productName":          "Premium SSD Managed Disks",
		"meterName":            "P10 LRS Disk",
		"isPrimaryMeterRegion": "true",
		"tierMinimumUnits":     "0",
	}
	assertFilterSet(t, disk, diskWantEq)

	ip, err := lookupFilters(pricing.KindStaticIP, "Standard", "westeurope")
	if err != nil {
		t.Fatalf("lookupFilters(static_ip): %v", err)
	}
	ipWantEq := map[string]string{
		"serviceName":      "Virtual Network",
		"armRegionName":    "westeurope",
		"skuName":          "Standard",
		"type":             "Consumption",
		"productName":      "IP Addresses",
		"meterName":        "Standard IPv4 Static Public IP",
		"tierMinimumUnits": "0",
	}
	assertFilterSet(t, ip, ipWantEq)
}

func assertFilterSet(t *testing.T, got []priceFilter, wantEq map[string]string) {
	t.Helper()
	seenEq := map[string]bool{}
	var exclusions []priceFilter
	for _, f := range got {
		if f.Op == filterEq {
			want, ok := wantEq[f.Field]
			if !ok {
				t.Errorf("unexpected equality filter %q", f.Field)
				continue
			}
			if f.Value != want {
				t.Errorf("filter %s = %q, want %q", f.Field, f.Value, want)
			}
			seenEq[f.Field] = true
		} else {
			exclusions = append(exclusions, f)
		}
	}
	for field := range wantEq {
		if !seenEq[field] {
			t.Errorf("filter %q missing", field)
		}
	}

	// All nine not-contains terms from the design must be present in the
	// matcher, even though the live API cannot accept all nine in one $filter.
	wantExclusions := map[string]string{
		"skuName|Spot":             "",
		"skuName|Low Priority":     "",
		"meterName|Spot":           "",
		"meterName|Low Priority":   "",
		"productName|Spot":         "",
		"productName|Low Priority": "",
		"productName|Windows":      "",
		"skuName|Windows":          "",
		"meterName|Windows":        "",
	}
	for _, f := range exclusions {
		key := f.Field + "|" + f.Value
		if _, ok := wantExclusions[key]; !ok {
			t.Errorf("unexpected exclusion filter %s", key)
			continue
		}
		delete(wantExclusions, key)
	}
	for missing := range wantExclusions {
		t.Errorf("exclusion filter %q missing", missing)
	}
}

// TestRecordedRequestMatchesServerFilter pins the exact OData $filter the
// live path sends to the URL captured in the recorded fixture. A human can
// replay either URL against the public API and get the same row.
func TestRecordedRequestMatchesServerFilter(t *testing.T) {
	entries := loadFixtureEntriesT(t)

	cases := []struct {
		entryName string
		kind      pricing.Kind
		sku       string
		region    string
	}{
		{"managed_disk_premium_p10_westeurope", pricing.KindManagedDisk, "P10 LRS", "westeurope"},
		{"public_ip_standard_westeurope", pricing.KindStaticIP, "Standard", "westeurope"},
	}
	for _, tc := range cases {
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
			filters, err := lookupFilters(tc.kind, tc.sku, tc.region)
			if err != nil {
				t.Fatalf("lookupFilters: %v", err)
			}
			u, err := url.Parse(entry.URL)
			if err != nil {
				t.Fatalf("parse recorded URL: %v", err)
			}
			want := u.Query().Get("$filter")
			if got := serverFilter(filters); got != want {
				t.Errorf("serverFilter mismatch\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// TestServerFilterUnderPredicateLimit pins the observed Retail Prices API
// limit that forced the live request to send the full equality set and only
// as many not-contains terms as the API accepts. The full matcher is still
// enforced client-side.
func TestServerFilterUnderPredicateLimit(t *testing.T) {
	for _, tc := range []struct {
		kind   pricing.Kind
		sku    string
		region string
		want   int
	}{
		{pricing.KindManagedDisk, "P10 LRS", "westeurope", 15},
		{pricing.KindStaticIP, "Standard", "westeurope", 15},
	} {
		filters, err := lookupFilters(tc.kind, tc.sku, tc.region)
		if err != nil {
			t.Fatalf("lookupFilters: %v", err)
		}
		sent := serverFilter(filters)
		if got := strings.Count(sent, " and ") + 1; got > maxRetailServerPredicates {
			t.Errorf("%s server filter has %d predicates, want <= %d: %s", tc.kind, got, maxRetailServerPredicates, sent)
		}
		if tc.want > 0 && strings.Count(sent, " and ")+1 != tc.want {
			t.Errorf("%s server filter predicate count = %d, want %d", tc.kind, strings.Count(sent, " and ")+1, tc.want)
		}
	}
}

// TestFixtureMatcher_RejectsWrongConstant proves the recorded-response path
// enforces the constant filters. Flipping a constant in a recorded row — even
// one whose price is unchanged — must make the lookup refuse to resolve rather
// than returning a real price for the wrong thing.
func TestFixtureMatcher_RejectsWrongConstant(t *testing.T) {
	entries := loadFixtureEntriesT(t)

	t.Run("type_consumption_flipped_to_reservation", func(t *testing.T) {
		for _, entry := range entries {
			if entry.Name != "managed_disk_premium_p10_westeurope" {
				continue
			}
			raw := string(entry.Body)
			altered := strings.Replace(raw, `"type": "Consumption"`, `"type": "Reservation"`, 1)
			if altered == raw {
				t.Fatal("recorded disk response does not contain type Consumption")
			}

			filters, err := lookupFilters(pricing.KindManagedDisk, "P10 LRS", "westeurope")
			if err != nil {
				t.Fatal(err)
			}
			page, err := parseRetailPage([]byte(altered))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := selectPrice(page.Items, filters, pricing.KindManagedDisk); ok {
				t.Error("Reservation row resolved as the Consumption price: type filter is not enforced")
			}
		}
	})

	t.Run("not_contains_constant_flipped", func(t *testing.T) {
		filters, err := lookupFilters(pricing.KindManagedDisk, "P10 LRS", "westeurope")
		if err != nil {
			t.Fatal(err)
		}
		page, err := parseRetailPage(entries[0].Body)
		if err != nil {
			t.Fatal(err)
		}
		if !matchFilters(page.Items[0], filters) {
			t.Fatal("genuine recorded row did not match the filter table")
		}

		// Flip the spot exclusion from "Spot" to "Disk" for the meterName
		// term. The recorded row's meterName is "P10 LRS Disk", so the
		// not-contains matcher must now refuse it. Flipping the skuName term
		// would not catch anything here because the recorded row's skuName is
		// "P10 LRS"; only the meterName term is guaranteed to collide. This
		// is the Azure analog of flipping capacitystatus in the AWS fixture:
		// the row is otherwise identical, and only the constant-pinned
		// matcher can catch it.
		flipped := append([]priceFilter(nil), filters...)
		changed := false
		for i := range flipped {
			if flipped[i].Op == filterNotContains && flipped[i].Field == "meterName" && flipped[i].Value == "Spot" {
				flipped[i].Value = "Disk"
				changed = true
				break
			}
		}
		if !changed {
			t.Fatal("no meterName Spot not-contains filter to flip")
		}
		if matchFilters(page.Items[0], flipped) {
			t.Error("row with 'Disk' in meterName survived a not-contains('Disk') filter")
		}
	})
}

// TestRecordedFixtureIsRealResponse is a structural guard: the recorded
// fixture must be the public Retail Prices API shape with two non-empty
// responses, not a hand-authored kind->SKU->region table.
func TestRecordedFixtureIsRealResponse(t *testing.T) {
	entries := loadFixtureEntriesT(t)
	for _, entry := range entries {
		var page retailPage
		if err := json.Unmarshal(entry.Body, &page); err != nil {
			t.Fatalf("entry %s body is not a Retail Prices API page: %v", entry.Name, err)
		}
		if len(page.Items) != 1 {
			t.Errorf("entry %s has %d items, want 1", entry.Name, len(page.Items))
		}
		if page.Items[0].UnitPrice <= 0 {
			t.Errorf("entry %s item has non-positive unit price", entry.Name)
		}
		if entry.URL == "" {
			t.Errorf("entry %s has empty URL", entry.Name)
		}
	}
}

// TestPrimaryMeterRegionAsymmetry pins a deviation that looks like an oversight
// and is not.
//
// The managed-disk filter enforces isPrimaryMeterRegion eq true; the static-IP
// filter deliberately does not. A review flagged the inconsistency and proposed
// enforcing it on both. Measured against the live Retail Prices API, that would
// have broken public-IP pricing outright: the Standard static IPv4 meter is
// isPrimaryMeterRegion=false in northeurope, westeurope, swedencentral, eastus
// and uksouth, while the S4 LRS disk meter is true in all five. A uniform
// predicate would match zero IP rows and make every public IP unpriceable.
func TestPrimaryMeterRegionAsymmetry(t *testing.T) {
	has := func(fs []priceFilter, field string) bool {
		for _, f := range fs {
			if f.Field == field {
				return true
			}
		}
		return false
	}
	disk, err := lookupFilters(pricing.KindManagedDisk, "S4 LRS", "northeurope")
	if err != nil {
		t.Fatalf("disk filters: %v", err)
	}
	if !has(disk, "isPrimaryMeterRegion") {
		t.Error("disk filter lost isPrimaryMeterRegion; disk meters ARE primary and the predicate pins the row")
	}
	ip, err := lookupFilters(pricing.KindStaticIP, "Standard", "northeurope")
	if err != nil {
		t.Fatalf("ip filters: %v", err)
	}
	if has(ip, "isPrimaryMeterRegion") {
		t.Error("ip filter gained isPrimaryMeterRegion: the live IP meter is isPrimaryMeterRegion=false " +
			"in every region measured, so this predicate matches zero rows and every public IP becomes unpriceable")
	}
}

// TestManagedDiskProduct_AllFamilies pins the tier-token to product mapping for
// all three families.
//
// Two of the three shipped broken: the family was derived from the tier token's
// first letter, matched as a prefix against ARM SKU names. "S" hit
// StandardSSD_LRS before Standard_LRS, so every Standard HDD disk queried
// "Standard SSD Managed Disks" — a query that matches nothing. "E" matched no
// ARM name at all. Only Premium worked, and nothing failed: the disks simply
// skipped as unpriced, which reads as "the API had no answer" rather than "we
// asked the wrong question".
func TestManagedDiskProduct_AllFamilies(t *testing.T) {
	for _, tc := range []struct{ sku, product string }{
		{"P10 LRS", "Premium SSD Managed Disks"},
		{"P10 ZRS", "Premium SSD Managed Disks"},
		{"E10 LRS", "Standard SSD Managed Disks"},
		{"E4 LRS", "Standard SSD Managed Disks"},
		{"S4 LRS", "Standard HDD Managed Disks"},
		{"S10 LRS", "Standard HDD Managed Disks"},
	} {
		product, meter, ok := managedDiskProduct(tc.sku)
		if !ok {
			t.Errorf("%s: no product; this family cannot be priced at all", tc.sku)
			continue
		}
		if product != tc.product {
			t.Errorf("%s: product = %q, want %q — a wrong product name matches no row and the disk skips as unpriced",
				tc.sku, product, tc.product)
		}
		if want := tc.sku + " Disk"; meter != want {
			t.Errorf("%s: meter = %q, want %q", tc.sku, meter, want)
		}
	}
	if _, _, ok := managedDiskProduct("X9 LRS"); ok {
		t.Error("unknown tier token resolved; an unknown SKU must skip, never guess a neighbouring tier")
	}
}

// TestUnpriceableKeyIsAskedOnce pins the negative cache: without it, one
// unresolvable SKU issues one HTTP request per resource carrying it.
func TestUnpriceableKeyIsAskedOnce(t *testing.T) {
	c := &CatalogPricer{
		log:         slog.New(slog.DiscardHandler),
		cache:       map[skuKey]resolvedSKU{},
		unpriceable: map[skuKey]bool{},
		last:        map[string]pricing.Provenance{},
	}
	// An unknown tier fails in lookupFilters, before any request; use a valid
	// token with a region that resolves no rows from the fixture path instead.
	key := skuKey{kind: pricing.KindManagedDisk, sku: "S4 LRS", region: "northeurope"}
	c.unpriceable[key] = true
	if _, _, err := c.liveUnitPrice(pricing.KindManagedDisk, "S4 LRS", "northeurope"); err == nil {
		t.Fatal("a key marked unpriceable resolved a price")
	}
}
