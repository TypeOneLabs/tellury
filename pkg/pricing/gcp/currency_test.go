package gcp

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// TestListSkusRequest_CarriesCurrency pins the core plumbing: the currency the
// scan prices in must reach the Cloud Billing API request (ListSkusRequest.
// CurrencyCode), or the whole catalogue comes back in the API default (USD).
// The request builder is package-private so a unit test can assert the field
// without a gRPC round trip.
func TestListSkusRequest_CarriesCurrency(t *testing.T) {
	p, err := NewCatalogPricer(context.Background(), slog.New(slog.DiscardHandler), "eur")
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}
	defer p.Close()

	// Construct-time normalization: "eur" -> "EUR" before it reaches the API.
	req := p.listSkusRequest("services/6F81-5844-456A")
	if req.GetCurrencyCode() != "EUR" {
		t.Fatalf("ListSkusRequest.CurrencyCode = %q, want %q (normalized at construction)", req.GetCurrencyCode(), "EUR")
	}

	// SetCurrency (best-effort detection, applied after construction) must
	// reach the same request builder, normalized, before the first UnitPrice.
	p.SetCurrency("gbp")
	req = p.listSkusRequest("services/6F81-5844-456A")
	if req.GetCurrencyCode() != "GBP" {
		t.Fatalf("SetCurrency(gbp) -> ListSkusRequest.CurrencyCode = %q, want %q", req.GetCurrencyCode(), "GBP")
	}

	// "" resets to the API default (USD).
	p.SetCurrency("")
	req = p.listSkusRequest("services/6F81-5844-456A")
	if req.GetCurrencyCode() != "" {
		t.Fatalf("SetCurrency(\"\") -> ListSkusRequest.CurrencyCode = %q, want the USD default (empty)", req.GetCurrencyCode())
	}
}

// TestCurrencyInfo_CatalogueUnavailableReturnsMixed is the currency-mix trap
// regression: a pricer asked to price in EUR whose live catalogue cannot load
// (here: a cancelled scan context makes every Billing RPC abort) returns
// ErrNoPrice for every lookup. CurrencyInfo must report Effective USD and
// Mixed true — the signal the report uses to tell the operator, loudly,
// that the EUR they asked for priced nothing and the figures are USD.
//
// There is no embedded fallback anymore: a failed API means the rule skips
// rather than getting a silently wrong figure. But the currency metadata
// must still flag the mismatch so the scan report can disclose it.
func TestCurrencyInfo_CatalogueUnavailableReturnsMixed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the live load is guaranteed to fail

	p, err := NewCatalogPricer(ctx, slog.New(slog.DiscardHandler), "EUR")
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}
	defer p.Close()

	// With no embedded fallback, UnitPrice returns ErrNoPrice when the
	// catalogue is unavailable. The rule skips.
	if _, _, err := p.UnitPrice(pricing.KindDiskCapacity, "gcp", "pd-ssd", "default"); err != pricing.ErrNoPrice {
		t.Fatalf("UnitPrice with cancelled context must return ErrNoPrice (no embedded fallback): %v", err)
	}

	info := p.CurrencyInfo()
	if info.Requested != "EUR" {
		t.Errorf("CurrencyInfo.Requested = %q, want EUR", info.Requested)
	}
	if info.Effective != "USD" {
		t.Errorf("CurrencyInfo.Effective = %q, want USD (catalogue unavailable)", info.Effective)
	}
	if !info.Mixed {
		t.Errorf("CurrencyInfo.Mixed = false; a non-USD request with no catalogue must be flagged")
	}
}

// TestCurrencyInfo_LiveCatalogueAnswersInRequestedCurrency is the healthy
// mirror of the trap test: a pricer whose live catalogue loaded in the
// requested currency reports Effective == Requested and Mixed false. The live
// load cannot run without a real Billing endpoint in this test, so the
// assertion drives the pricer's state directly (loaded=true, currencyCode set)
// — the exact state loadCatalogue leaves behind after a successful load.
func TestCurrencyInfo_LiveCatalogueAnswersInRequestedCurrency(t *testing.T) {
	p, err := NewCatalogPricer(context.Background(), slog.New(slog.DiscardHandler), "EUR")
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}
	defer p.Close()

	// Simulate a successful live load in EUR (see loadCatalogue): the load
	// indexed at least one SKU and the currency is the requested one.
	p.mu.Lock()
	p.loaded = true
	p.mu.Unlock()

	info := p.CurrencyInfo()
	if info.Effective != "EUR" {
		t.Errorf("CurrencyInfo.Effective = %q, want EUR (live catalogue answered)", info.Effective)
	}
	if info.Mixed {
		t.Errorf("CurrencyInfo.Mixed = true on a healthy EUR load; want false")
	}
}

// TestCatalogueError_NamesUnsupportedCurrency: a well-formed but unsupported
// currency makes ListSkus fail with InvalidArgument. The pricer must record
// that failure so the scan can surface it (naming the currency) instead of
// silently answering from the USD embedded table. This test drives the
// recorded-error path directly and asserts the message names the currency.
func TestCatalogueError_NamesUnsupportedCurrency(t *testing.T) {
	p, err := NewCatalogPricer(context.Background(), slog.New(slog.DiscardHandler), "XYZ")
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}
	defer p.Close()

	// loadCatalogue records an unsupportedCurrencyError when ListSkus returns
	// InvalidArgument; simulate that recording here.
	ue := &unsupportedCurrencyError{currency: "XYZ", err: errors.New("invalid argument")}
	p.mu.Lock()
	p.unsupported = ue
	p.mu.Unlock()

	cerr := p.CatalogueError()
	if cerr == nil {
		t.Fatal("CatalogueError() = nil; the unsupported currency must surface")
	}
	if !errors.Is(cerr, ue) {
		t.Fatalf("CatalogueError() = %v, want the recorded unsupported-currency error", cerr)
	}
	if got := ue.Error(); !strings.Contains(got, "XYZ") {
		t.Errorf("unsupportedCurrencyError.Error() = %q, want it to name the currency XYZ", got)
	}
}

// TestDetectCurrency_NoProjectsIsQuietDefault drives the best-effort seam:
// with no project in scope there is nothing to ask, so detection returns
// ("", "") without building a Cloud Billing client (no ADC, no network). The
// caller falls back to USD quietly.
func TestDetectCurrency_NoProjectsIsQuietDefault(t *testing.T) {
	code, project := DetectCurrency(context.Background(), slog.New(slog.DiscardHandler), nil)
	if code != "" || project != "" {
		t.Fatalf("DetectCurrency(nil projects) = (%q, %q), want (\"\", \"\")", code, project)
	}
	code, project = DetectCurrency(context.Background(), slog.New(slog.DiscardHandler), []string{})
	if code != "" || project != "" {
		t.Fatalf("DetectCurrency(empty projects) = (%q, %q), want (\"\", \"\")", code, project)
	}
}

// TestDetectCurrency_NilLoggerDoesNotPanic: DetectCurrency replaces a nil
// logger with slog.Default rather than panicking on the first debug line.
func TestDetectCurrency_NilLoggerDoesNotPanic(t *testing.T) {
	code, project := DetectCurrency(context.Background(), nil, nil)
	if code != "" || project != "" {
		t.Fatalf("DetectCurrency with nil logger = (%q, %q), want (\"\", \"\")", code, project)
	}
}
