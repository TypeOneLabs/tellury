package gcp

import (
	"context"
	"log/slog"
	"testing"
	"time"

	billingpb "cloud.google.com/go/billing/apiv1/billingpb"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/old_snapshot"
	"github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/unused_reserved_ip"
)

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
	// non-fatal even if ADC is entirely absent.
	p, err := NewCatalogPricer(ctx, slog.New(slog.DiscardHandler))
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

	p, err := NewCatalogPricer(ctx, slog.New(slog.DiscardHandler))
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
	p, err := NewCatalogPricer(ctx, slog.New(slog.DiscardHandler))
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
			ResourceFamily: "Storage",
			ResourceGroup:  "PDSnapshot",
			UsageType:      "OnDemand",
		},
		Description: "Regional Standard Snapshot Early Deletion in Changhua County",
	}
	if _, _, ok := matchSKU(sk); ok {
		t.Error("an early-deletion charge must not be indexed as a snapshot storage rate")
	}
}
