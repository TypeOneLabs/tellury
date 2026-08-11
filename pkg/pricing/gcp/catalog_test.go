package gcp

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	billingpb "cloud.google.com/go/billing/apiv1/billingpb"
	"google.golang.org/genproto/googleapis/type/money"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/old_snapshot"
	"github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/unused_reserved_ip"
	"github.com/TypeOneLabs/tellury/pkg/rules/gcp/gcs/no_lifecycle_policy"
)

// gcpPriceFixture loads the GCP price fixture for tests that need a
// populated StaticPricer. It prefers TELLURY_PRICE_FIXTURE; if unset it
// falls back to the testdata file.
func gcpPriceFixture(t *testing.T) *StaticPricer {
	t.Helper()
	path := os.Getenv("TELLURY_PRICE_FIXTURE")
	if path == "" {
		path = filepath.Join("testdata", "price-fixture.json")
	}
	p, err := NewStaticPricerFromFile(path)
	if err != nil {
		t.Fatalf("NewStaticPricerFromFile: %v", err)
	}
	return p
}

// TestCatalogueProgress_InvokesRegisteredCallback pins the progress seam the
// CLI uses to report the pricing catalogue load as its own phase:
// SetCatalogueProgress stores a callback and reportCatalogueProgress invokes
// it with the exact (done, total, final) arguments — including final=true on
// the completion call, so the CLI's phase always ends.
func TestCatalogueProgress_InvokesRegisteredCallback(t *testing.T) {
	p, err := NewCatalogPricer(context.Background(), slog.New(slog.DiscardHandler), "")
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}
	defer p.Close()

	type report struct {
		done, total int
		final       bool
	}
	var (
		mu  sync.Mutex
		got []report
	)
	p.SetCatalogueProgress(func(done, total int, final bool) {
		mu.Lock()
		got = append(got, report{done, total, final})
		mu.Unlock()
	})

	p.reportCatalogueProgress(0, 2, false) // load start
	p.reportCatalogueProgress(1, 2, false) // one service indexed
	p.reportCatalogueProgress(2, 2, true)  // completion

	mu.Lock()
	if len(got) != 3 {
		mu.Unlock()
		t.Fatalf("callback invoked %d times, want 3", len(got))
	}
	want := []report{{0, 2, false}, {1, 2, false}, {2, 2, true}}
	for i, w := range want {
		if got[i] != w {
			mu.Unlock()
			t.Fatalf("callback call %d = %+v, want %+v", i, got[i], w)
		}
	}
	mu.Unlock()

	// Clearing the callback must stop invocations.
	p.SetCatalogueProgress(nil)
	mu.Lock()
	before := len(got)
	mu.Unlock()
	p.reportCatalogueProgress(0, 0, true)
	mu.Lock()
	if len(got) != before {
		mu.Unlock()
		t.Fatalf("callback must not fire after SetCatalogueProgress(nil)")
	}
	mu.Unlock()
}

// TestLiveUnitPrice_ThreadsScanContext is the regression test for finding #2:
// loadCatalogue hardcoded context.Background(), so --timeout could not bound
// the Cloud Billing Catalog load — and since UnitPrice is called
// synchronously during rule evaluation, a hanging Billing API stalled the scan
// past its deadline uncancellably.
//
// The fix threads the scan's context through liveUnitPrice/loadCatalogue, so
// a cancelled/deadline-exceeded context makes every ListServices/ListSkus RPC
// abort promptly. This test proves the thread is intact: it creates a pricer
// with an already-cancelled context and asserts the live path honours it,
// returning ErrNoPrice — the scan then skips rather than falling back to an
// embedded table (there is no embedded table anymore).
func TestLiveUnitPrice_ThreadsScanContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the load must not proceed

	// Build a pricer with a cancelled scan context. NewCatalogPricer must
	// still succeed — the client is dialed lazily, and construction is
	// non-fatal even if ADC is entirely absent. The third argument is the
	// catalogue currency ("" = the API default, USD).
	p, err := NewCatalogPricer(ctx, slog.New(slog.DiscardHandler), "")
	if err != nil {
		t.Fatalf("NewCatalogPricer with cancelled ctx must still construct (lazy dial): %v", err)
	}
	defer p.Close()

	// UnitPrice returns ErrNoPrice, never hanging on the Billing API.
	// There is no embedded fallback: the rule skips rather than guessing.
	_, _, err = p.UnitPrice(pricing.KindDiskCapacity, "gcp", "pd-ssd", "default")
	if err != pricing.ErrNoPrice {
		t.Fatalf("UnitPrice with a cancelled scan context must return ErrNoPrice (no embedded fallback): %v", err)
	}
}

// TestLoadCatalogue_UsesCallerContext verifies the seam directly: loadCatalogue
// runs against the context stored on the pricer (NewCatalogPricer's ctx).
// A pricer built with a live, eventually-expiring context must see that
// deadline reflected in every RPC it issues. Because we cannot inject a fake
// gRPC listener into the SDK iterator here, we assert the proven property:
// the stored ctx is the SAME context the pricer was constructed with. If a
// future edit replaces c.ctx with context.Background() the pointer check
// fails, catching the regression that finding #2 described.
func TestLoadCatalogue_UsesCallerContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	p, err := NewCatalogPricer(ctx, slog.New(slog.DiscardHandler), "")
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}
	defer p.Close()

	if p.ctx != ctx {
		t.Fatalf("CatalogPricer.ctx does not retain the caller's context; loadCatalogue would not be deadline-bounded")
	}
}

// TestLiveUnitPrice_DeadlineExceededOnce asserts that once the context source
// has signalled deadline-exceeded, the live path stays dead for the pricer's
// lifetime (sync.Once caches loadErr), and every subsequent UnitPrice returns
// ErrNoPrice — no embedded fallback exists.
func TestLiveUnitPrice_DeadlineExceededOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := NewCatalogPricer(ctx, slog.New(slog.DiscardHandler), "")
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}
	defer p.Close()

	// Cancel immediately after construction, before the first UnitPrice: the
	// first lookup trips the lazy load under a dead context.
	cancel()
	_, _, err = p.UnitPrice(pricing.KindDiskCapacity, "gcp", "pd-ssd", "default")
	if err != pricing.ErrNoPrice {
		t.Fatalf("UnitPrice with cancelled context must return ErrNoPrice: %v", err)
	}

	// Second call must not re-enter the API: same ErrNoPrice, proving
	// loadErr is cached via sync.Once.
	_, _, err = p.UnitPrice(pricing.KindDiskCapacity, "gcp", "pd-ssd", "default")
	if err != pricing.ErrNoPrice {
		t.Fatalf("second UnitPrice diverged (loadErr must be cached via sync.Once): err=%v", err)
	}
}

// TestMatchSKU_StaticIPTokenPinned is the regression test for the static-IP
// pricing token mismatch: matchSKU indexed live Cloud Billing static-IP SKUs
// under "external-static" while the unused_reserved_ip rule queried
// StaticIPSKU = "unattached" (the same token the fixture's
// static_ip.unattached entry is keyed under). The live lookup therefore NEVER
// matched.
//
// The assertion imports the constant the rule ACTUALLY queries, so the two
// cannot drift apart again: if matchSKU's token or the rule's StaticIPSKU
// ever changes without the other, this test fails.
func TestMatchSKU_StaticIPTokenPinned(t *testing.T) {
	sk := &billingpb.Sku{
		Category: &billingpb.Category{
			ServiceDisplayName: "Compute Engine",
			ResourceFamily:     "Compute",
			ResourceGroup:      "StaticIpAddress",
			UsageType:          "OnDemand",
		},
		Description: "External IPv4 IP address on a standard VM",
	}

	kind, token, ok := matchSKU(sk)
	if !ok {
		t.Fatalf("matchSKU(%q) must match a live static-IP SKU", sk.GetDescription())
	}
	if kind != pricing.KindStaticIP {
		t.Fatalf("matchSKU kind = %v, want %v", kind, pricing.KindStaticIP)
	}
	if token != unused_reserved_ip.StaticIPSKU {
		t.Fatalf("matchSKU token = %q, but the unused_reserved_ip rule queries %q: "+
			"the live catalogue would never match and every static-IP price "+
			"would silently fall back to the embedded table",
			token, unused_reserved_ip.StaticIPSKU)
	}
}

// TestMatchSKU_SnapshotTokenPinned is the snapshot equivalent of the static-IP
// test above, and exists for the same reason: the resource group was wrong
// ("storagesnapshot"), so no live SKU ever matched and every snapshot silently
// resolved from the embedded table — which itself carried a rate roughly half
// the real one. The combined error understated a real snapshot's cost by ~2x.
func TestMatchSKU_SnapshotTokenPinned(t *testing.T) {
	sk := &billingpb.Sku{
		Category: &billingpb.Category{
			ServiceDisplayName: "Compute Engine",
			ResourceFamily:     "Storage",
			ResourceGroup:      "PDSnapshot",
			UsageType:          "OnDemand",
		},
		Description: "Storage PD Snapshot",
	}

	kind, token, ok := matchSKU(sk)
	if !ok {
		t.Fatalf("matchSKU(%q) must match a live snapshot SKU", sk.GetDescription())
	}
	if kind != pricing.KindSnapshotStorage {
		t.Fatalf("matchSKU kind = %v, want %v", kind, pricing.KindSnapshotStorage)
	}
	if token != old_snapshot.SnapshotStorageSKU {
		t.Fatalf("matchSKU token = %q, but the old_snapshot rule queries %q: "+
			"the live catalogue would never match and every snapshot price "+
			"would silently fall back to the embedded table",
			token, old_snapshot.SnapshotStorageSKU)
	}
}

// TestMatchSKU_SnapshotEarlyDeletionIgnored: the PDSnapshot group also carries
// early-deletion charges, which are one-off penalties rather than a standing
// per-GiB-month rate.
func TestMatchSKU_SnapshotEarlyDeletionIgnored(t *testing.T) {
	sk := &billingpb.Sku{
		Category: &billingpb.Category{
			ServiceDisplayName: "Compute Engine",
			ResourceFamily:     "Storage",
			ResourceGroup:      "PDSnapshot",
			UsageType:          "OnDemand",
		},
		Description: "Regional Standard Snapshot Early Deletion in Changhua County",
	}
	if _, _, ok := matchSKU(sk); ok {
		t.Error("an early-deletion charge must not be indexed as a snapshot storage rate")
	}
}

// TestMatchSKU_GCSSkuTokensPinned pins the GCS storage-class tokens against
// the exact constants the no_lifecycle_policy rule queries.
func TestMatchSKU_GCSSkuTokensPinned(t *testing.T) {
	cases := []struct {
		name string
		desc string
		want string
	}{
		{"standard", "Storage - Standard Storage", no_lifecycle_policy.FromClass},
		{"nearline", "Storage Nearline Storage", no_lifecycle_policy.ToClass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sk := &billingpb.Sku{
				Category: &billingpb.Category{
					ServiceDisplayName: "Cloud Storage",
					ResourceFamily:     "Storage",
					UsageType:          "OnDemand",
				},
				Description: tc.desc,
			}
			kind, token, ok := matchSKU(sk)
			if !ok {
				t.Fatalf("matchSKU(%q) must match a live GCS storage-class SKU", tc.desc)
			}
			if kind != pricing.KindGCSStorage {
				t.Fatalf("matchSKU(%q) kind = %v, want %v", tc.desc, kind, pricing.KindGCSStorage)
			}
			if token != tc.want {
				t.Fatalf("matchSKU(%q) token = %q, but the no_lifecycle_policy rule queries %q: "+
					"the live catalogue would never match and every GCS class-transition "+
					"price would silently fall back to the embedded table",
					tc.desc, token, tc.want)
			}
		})
	}
}

// TestMatchSKU_CustomFamilyTokensPinned pins the custom-machine-family token
// the whole pricing path shares.
func TestMatchSKU_CustomFamilyTokensPinned(t *testing.T) {
	cpuSKU := &billingpb.Sku{
		Category: &billingpb.Category{
			ServiceDisplayName: "Compute Engine",
			ResourceFamily:     "Compute",
			ResourceGroup:      "CPU",
			UsageType:          "OnDemand",
		},
		Description: "N2 Custom Instance Core running in Americas",
	}
	kind, cpuToken, ok := matchSKU(cpuSKU)
	if !ok {
		t.Fatalf("matchSKU(%q) must match a live custom-CPU SKU", cpuSKU.GetDescription())
	}
	if kind != pricing.KindVMCustomCPU {
		t.Fatalf("matchSKU custom-CPU kind = %v, want %v", kind, pricing.KindVMCustomCPU)
	}

	ramSKU := &billingpb.Sku{
		Category: &billingpb.Category{
			ServiceDisplayName: "Compute Engine",
			ResourceFamily:     "Compute",
			ResourceGroup:      "RAM",
			UsageType:          "OnDemand",
		},
		Description: "N2 Custom Instance Ram running in Americas",
	}
	ramKind, ramToken, ok := matchSKU(ramSKU)
	if !ok {
		t.Fatalf("matchSKU(%q) must match a live custom-RAM SKU", ramSKU.GetDescription())
	}
	if ramKind != pricing.KindVMCustomRAM {
		t.Fatalf("matchSKU custom-RAM kind = %v, want %v", ramKind, pricing.KindVMCustomRAM)
	}
	if ramToken != cpuToken {
		t.Fatalf("matchSKU custom-CPU token = %q but custom-RAM token = %q; both legs of a custom "+
			"shape must resolve under the same family token", cpuToken, ramToken)
	}

	spec, ok := ParseCustomMachineType("n2-custom-8-32768-ext")
	if !ok {
		t.Fatalf("ParseCustomMachineType(n2-custom-8-32768-ext) must parse")
	}
	if spec.Family != cpuToken {
		t.Fatalf("matchSKU token = %q, machine-catalog family = %q: the underutilized_instance "+
			"rule prices the machine_family token (machine-catalog spelling) but the live "+
			"catalogue is indexed under the matchSKU spelling, so they must agree",
			cpuToken, spec.Family)
	}
}

// TestMatchSKU_DiskCapacityTokensPinned pins the disk-capacity vocabulary
// against the detached_disk rule's lookup.
func TestMatchSKU_DiskCapacityTokensPinned(t *testing.T) {
	static := gcpPriceFixture(t)
	cases := []struct {
		desc  string
		group string
		base  string  // disk_type the detached_disk rule sees
		zones float64 // replica_zone_count
		want  string  // the token both matchSKU and the rule must use
	}{
		{"Storage PD SSD Capacity", "pdssd", "pd-ssd", 0, "pd-ssd"},
		{"Storage PD SSD Capacity Regional", "pdssd", "pd-ssd", 2, "pd-ssd-regional"},
		{"Storage PD Capacity", "storagepdcapacity", "pd-standard", 0, "pd-standard"},
		{"Storage PD Capacity Regional", "storagepdcapacity", "pd-standard", 2, "pd-standard-regional"},
		{"Storage PD Extreme Capacity", "extremepd", "pd-extreme", 0, "pd-extreme"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			sk := &billingpb.Sku{
				Category: &billingpb.Category{
					ServiceDisplayName: "Compute Engine",
					ResourceFamily:     "Compute",
					ResourceGroup:      tc.group,
					UsageType:          "OnDemand",
				},
				Description: tc.desc,
			}
			kind, token, ok := matchSKU(sk)
			if !ok {
				t.Fatalf("matchSKU(%q) must match a live disk-capacity SKU", tc.desc)
			}
			if kind != pricing.KindDiskCapacity {
				t.Fatalf("matchSKU(%q) kind = %v, want %v", tc.desc, kind, pricing.KindDiskCapacity)
			}
			if token != tc.want {
				t.Fatalf("matchSKU(%q) token = %q, want %q", tc.desc, token, tc.want)
			}
			if got := pricing.DiskSKU(tc.base, tc.zones); got != token {
				t.Fatalf("pricing.DiskSKU(%q, %v) = %q != matchSKU token %q: the detached_disk "+
					"rule would query a different key than the live catalogue indexes",
					tc.base, tc.zones, got, token)
			}
			if _, _, err := static.UnitPrice(pricing.KindDiskCapacity, "gcp", token, "default"); err != nil {
				t.Errorf("fixture has no disk_capacity entry for matchSKU token %q: "+
					"live and fixture vocabularies have drifted", token)
			}
		})
	}
}

// TestUnitPriceOf_SubCentPrecision pins full-precision parsing of Cloud
// Billing's units+nanos money.
func TestUnitPriceOf_SubCentPrecision(t *testing.T) {
	cases := []struct {
		name  string
		units int64
		nanos int32
		want  float64
	}{
		{"round cent (the case that hid the bug)", 0, 50_000_000, 0.05},
		{"EUR converted snapshot rate", 0, 43_890_000, 0.04389},
		{"coldline storage, truncated to zero before", 0, 4_000_000, 0.004},
		{"custom RAM per GiB-hour, truncated to zero before", 0, 4_446_000, 0.004446},
		{"whole units plus fraction", 2, 500_000_000, 2.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sk := &billingpb.Sku{PricingInfo: []*billingpb.PricingInfo{{
				PricingExpression: &billingpb.PricingExpression{
					UsageUnit: "GiBy.mo",
					TieredRates: []*billingpb.PricingExpression_TierRate{{
						UnitPrice: &money.Money{
							CurrencyCode: "USD", Units: tc.units, Nanos: tc.nanos,
						},
					}},
				},
			}}}
			got, ok := unitPriceOf(sk)
			if !ok {
				t.Fatal("unitPriceOf returned !ok for a well-formed SKU")
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("unitPriceOf = %v, want %v (a price truncated to cents is "+
					"wrong by up to 100%% for sub-cent rates)", got, tc.want)
			}
		})
	}
}
