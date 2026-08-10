package gcp

import (
	"context"
	"log/slog"
	"math"
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

// TestCatalogueProgress_InvokesRegisteredCallback pins the progress seam the
// CLI uses to report the pricing catalogue load as its own phase:
// SetCatalogueProgress stores a callback and reportCatalogueProgress invokes
// it with the exact (done, total, final) arguments — including final=true on
// the completion call, so the CLI's phase always ends. It also pins that
// setting nil disables further invocations (the offline static pricer never
// registers one).
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
// turning the load error into the embedded fallback and never blocking on the
// passed deadline.
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

	// UnitPrice falls back to the embedded table, never hanging on the Billing
	// API. Exactly the "deadline-bounded, then embedded" behaviour the scan
	// needs.
	unit, region, err := p.UnitPrice(pricing.KindDiskCapacity, "gcp", "pd-ssd", "default")
	if err != nil {
		t.Fatalf("UnitPrice with a cancelled scan context must fall back to embedded, not fail: %v", err)
	}
	if unit != 0.170 || region != "default" {
		t.Fatalf("expected embedded 0.170/default, got %v/%q", unit, region)
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
// lifetime (sync.Once caches loadErr), and every subsequent UnitPrice resolves
// from the embedded table without re-entering the API. This is the contract
// that stops a hanging Billing API from stalling the scan: the FIRST resource
// trip to the API hits the deadline, the error is cached, and all later
// resources price instantly from the embedded table.
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
	unit, region, err := p.UnitPrice(pricing.KindDiskCapacity, "gcp", "pd-ssd", "default")
	if err != nil {
		t.Fatalf("failed on first UnitPrice: %v", err)
	}
	if unit != 0.170 {
		t.Fatalf("embedded fallback should answer 0.170, got %v (%q)", unit, region)
	}

	// Second call must not re-enter the API: same embedded answer, proving
	// loadErr is cached via sync.Once.
	unit2, _, err := p.UnitPrice(pricing.KindDiskCapacity, "gcp", "pd-ssd", "default")
	if err != nil || unit2 != unit {
		t.Fatalf("second UnitPrice diverged (loadErr must be cached via sync.Once): unit=%v err=%v", unit2, err)
	}
}

// TestMatchSKU_StaticIPTokenPinned is the regression test for the static-IP
// pricing token mismatch: matchSKU indexed live Cloud Billing static-IP SKUs
// under "external-static" while the unused_reserved_ip rule queried
// StaticIPSKU = "unattached" (the same token the embedded table's
// static_ip.unattached entry is keyed under). The live lookup therefore NEVER
// matched: every static-IP price silently resolved from the embedded
// fallback, provenance always read "embedded_fallback", and a region whose
// live rate differs from the table was mispriced with no error.
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
//
// The Category values below are copied from a live catalogue response, not
// invented, which is the whole point: an invented group name is what broke it.
func TestMatchSKU_SnapshotTokenPinned(t *testing.T) {
	sk := &billingpb.Sku{
		Category: &billingpb.Category{
			ServiceDisplayName: "Compute Engine",
			// Copied verbatim from a live catalogue response. Cloud Billing
			// files snapshot SKUs under family "Storage", NOT "Compute" — an
			// earlier version of this test invented "Compute", so the test
			// passed while no live SKU matched and every snapshot silently
			// used the embedded rate.
			ResourceFamily: "Storage",
			ResourceGroup:  "PDSnapshot",
			UsageType:      "OnDemand",
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
// per-GiB-month rate. Indexing one as the storage rate would overwrite the real
// rate for that region with an unrelated number.
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
// the exact constants the no_lifecycle_policy rule queries: FromClass =
// "STANDARD" and ToClass = "NEARLINE" are the two classes whose price delta the
// rule prices (the STANDARD→NEARLINE class-transition waste). If matchSKU's
// storage-class spelling ever drifts from those constants, the live catalogue
// would never match and every GCS class-transition price would silently fall
// back to the embedded table with no error — the same silent-fallback bug
// class that broke static IPs and snapshots.
//
// matchSKU switches on ResourceFamily "Storage" (the family a live catalogue
// response confirmed for snapshot SKUs; Cloud Storage SKUs sit in the same
// family) and on the storage-class keyword in the description. The resource
// group is not consulted for GCS, so it is left off the fixture.
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
// the whole pricing path shares. The underutilized_instance rule prices a
// custom shape by passing the node's machine_family token to
// KindVMCustomCPU/KindVMCustomRAM; the normalizer derives that token from the
// machine-type name via ParseCustomMachineType ("n2-custom-8-32768-ext" ->
// family "n2-custom"), and matchSKU derives it from a live custom-instance SKU
// description by taking the leading family word ("N2 Custom Instance Core
// running in Americas" -> "n2-custom"). All three must spell the token the
// same way, or the live lookup resolves a key the rule never queries and every
// custom-shape price silently falls back to the embedded table.
//
// The family prefix in the description is what customFamilyFromDescription
// reads (fields[0] + "-custom"): without it the parser would emit
// "custom-custom", so a description that starts with the family name is the
// only spelling that can produce the machine-catalog token.
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
// against the detached_disk rule's lookup: the rule prices a disk under
// pricing.DiskSKU(disk_type, replica_zone_count) — e.g. "pd-ssd" for a zonal
// pd-ssd and "pd-ssd-regional" for one replicated across >= 2 zones — and
// matchSKU must index the live catalogue under exactly those tokens. Each
// token must also exist as a key in the embedded table, so the live answer and
// the fallback resolve the same key (the static-IP/snapshot failure mode).
//
// This pins the TOKEN agreement only. The resource groups below are matchSKU's
// own switch literals; whether the live catalogue really returns them for
// these SKUs cannot be confirmed from this repo and is flagged as unverified
// in the catalog audit — this test guards against token drift between matchSKU
// and the rule/embedded table, not against a wrong group name. It also does
// not cover pd-balanced or hyperdisk-* capacity SKUs, which matchSKU does not
// currently index at all (also flagged in the audit).
func TestMatchSKU_DiskCapacityTokensPinned(t *testing.T) {
	static, err := NewStaticPricer()
	if err != nil {
		t.Fatalf("NewStaticPricer: %v", err)
	}
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
				t.Errorf("embedded table has no disk_capacity entry for matchSKU token %q: "+
					"live and fallback vocabularies have drifted", token)
			}
		})
	}
}

// TestUnitPriceOf_SubCentPrecision pins full-precision parsing of Cloud
// Billing's units+nanos money. This function used to truncate to whole cents,
// which was invisible for the round-cent USD SKUs anyone happened to check and
// catastrophic everywhere else: coldline storage ($0.004/GiB-month) and custom
// RAM ($0.004446/GiB-hour) both truncated to ZERO, pricing them free, and
// every non-USD scan lost precision because a converted rate almost never
// lands on a round cent.
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
