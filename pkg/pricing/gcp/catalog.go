package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"

	billing "cloud.google.com/go/billing/apiv1"
	billingpb "cloud.google.com/go/billing/apiv1/billingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Cloud Billing Catalog pricer
// ─────────────────────────────────────────────────────────────────────────────
//
// CatalogPricer implements pricing.Pricer (plus pricing.ProvenancePricer,
// pricing.CurrencySetter, pricing.CurrencyReporter and
// pricing.CatalogueErrorer) over the Cloud Billing Catalog API
// (cloud.google.com/go/billing/apiv1, CloudCatalogClient).
//
// The live API is the ONLY source. There is no embedded fallback table. A
// price that cannot be resolved from the live catalogue returns ErrNoPrice and
// the rule skips rather than guessing at a dollar figure.
//
// When the TELLURY_PRICE_FIXTURE environment variable is set, the catalogue
// loads from that file instead of calling the API. This is a test-only hook,
// never a user-facing flag. The file format is the generic
// kind -> SKU -> region -> price table.
//
// The catalogue is fetched at most once per process (see loadOnce): it is
// large (thousands of SKUs across the handful of services tellury prices) and
// almost entirely static within a scan's lifetime, so per-resource calls
// would be both slow and a needless drain on quota.
//
// Every price is traceable: LastLookup exposes the pricing.Provenance
// (source + SKU + region) of the most recent UnitPrice/MonthlyCost answer
// for a given key, which is exactly what pkg/rules/gcp/attrs.go's
// PriceEvidence helper turns into a Finding's evidence entry.
type CatalogPricer struct {
	log    *slog.Logger
	client *billing.CloudCatalogClient

	// ctx is the scan's context, captured at construction (NewCatalogPricer
	// is handed the scan context by pkg/cloud/gcp.New). The catalogue load —
	// the only RPC path in this pricer — runs against it, so a hanging
	// Billing API honours the CLI's --timeout deadline instead of stalling
	// the whole scan past it incancellably. UnitPrice itself has no context
	// parameter (it is a pricing.Pricer method, called synchronously during
	// rule evaluation), so this is the seam through which the deadline
	// travels.
	ctx context.Context

	once      sync.Once
	loadErr   error
	skusByKey map[skuKey]resolvedSKU // (kind, sku, region) -> resolvedSKU, catalogue cache

	// currencyCode is the ISO 4217 code the catalogue is fetched in
	// (ListSkusRequest.CurrencyCode). "" means the API default, USD. It is
	// set at construction from the explicit --currency/TELLURY_CURRENCY flag
	// and may be overwritten by SetCurrency after best-effort billing-account
	// detection (which needs the ingested graph and therefore runs after the
	// provider is built). It MUST be final before the first UnitPrice call:
	// the catalogue load is cached for the pricer's lifetime, so a late
	// change would silently price the whole scan in the wrong currency.
	currencyCode string

	// loaded reports whether the live catalogue indexed at least one SKU. It
	// is what distinguishes "the requested currency priced the answers" from
	// "no prices resolved" in CurrencyInfo.
	loaded bool

	// unsupported is non-nil when the live catalogue rejected the requested
	// currency (InvalidArgument from ListSkus). Such a failure must surface
	// to the scan, not silently fall back.
	unsupported error

	// catalogueProgress, when non-nil, is invoked as the live catalogue
	// loads: (done, total) counts billing services indexed, with final=true
	// on the last call whether the load succeeded or not. Set via
	// SetCatalogueProgress; see there for the exact call pattern.
	catalogueProgress func(done, total int, final bool)

	mu   sync.Mutex
	last map[string]pricing.Provenance // provKey(kind,sku,region) -> last answer
}

type skuKey struct {
	kind   pricing.Kind
	sku    string
	region string
}

type resolvedSKU struct {
	skuID     string // Cloud Billing SKU id, e.g. "6F81-5844-456A"
	unitPrice float64
	region    string
	// substitute marks a price answered by an equivalent SKU rather than the
	// one asked for, so UnitPrice can record SourceEquivalentSKU and the
	// substitution reaches the finding's evidence.
	substitute bool
}

var (
	_ pricing.Pricer           = (*CatalogPricer)(nil)
	_ pricing.ProvenancePricer = (*CatalogPricer)(nil)
	_ pricing.CurrencySetter   = (*CatalogPricer)(nil)
	_ pricing.CurrencyReporter = (*CatalogPricer)(nil)
	_ pricing.CatalogueErrorer = (*CatalogPricer)(nil)
)

// NewCatalogPricer builds a pricer over the live Cloud Billing Catalog API.
// It performs no RPCs itself: the catalogue is fetched lazily, once, on first
// UnitPrice call (see loadCatalogue).
//
// When TELLURY_PRICE_FIXTURE is set, the catalogue loads from that file
// (a generic kind->SKU->region->price table) instead of calling the API.
// This is a test-only hook, not a user-facing flag.
//
// Failure to build the API client at all (e.g. ADC entirely absent) is
// likewise non-fatal here: NewCatalogPricer still returns a usable pricer,
// just one whose live path is permanently unavailable. Every price lookup
// will then return ErrNoPrice and rules will skip.
//
// currencyCode is the ISO 4217 code the catalogue is fetched in ("" = USD).
// The live Cloud Billing API prices the whole catalogue in this currency
// (ListSkusRequest.CurrencyCode), so the snapshot SKU prices come back
// converted. A well-formed but unsupported code makes ListSkus fail with
// InvalidArgument; the pricer records that failure (CatalogueError) and
// surfaces it so the scan cannot complete with wrong-currency figures.
//
// ctx is retained on the pricer so the eventual lazy catalogue load runs
// under the scan's context and deadline (it is the `--timeout` derived from
// internal/cli.runScan). Passing context.Background() is fine but loses that
// guarantee; the CLI never does.
func NewCatalogPricer(ctx context.Context, log *slog.Logger, currencyCode string) (*CatalogPricer, error) {
	if log == nil {
		log = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	currencyCode = strings.ToUpper(strings.TrimSpace(currencyCode))
	client, err := billing.NewCloudCatalogClient(ctx)
	if err != nil {
		log.Warn("gcp: cloud billing catalog client unavailable; resources requiring prices will skip", "err", err)
		client = nil
	}
	return &CatalogPricer{
		log:          log,
		ctx:          ctx,
		client:       client,
		currencyCode: currencyCode,
		skusByKey:    map[skuKey]resolvedSKU{},
		last:         map[string]pricing.Provenance{},
	}, nil
}

// Close releases the underlying gRPC connection, if any.
func (c *CatalogPricer) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// SetCurrency implements pricing.CurrencySetter. It changes the catalogue
// currency after construction (best-effort detection runs after graph
// ingestion, which is after the provider is built). It MUST be called before
// the first UnitPrice call: the catalogue load is cached for the pricer's
// lifetime, so a late change would silently price the whole scan in the wrong
// currency. Codes are normalized to uppercase; "" resets to the USD default.
func (c *CatalogPricer) SetCurrency(code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.currencyCode = strings.ToUpper(strings.TrimSpace(code))
}

// CurrencyInfo implements pricing.CurrencyReporter. Effective is the
// requested currency when the live catalogue loaded (the only source that
// prices in the requested currency), otherwise "USD".
func (c *CatalogPricer) CurrencyInfo() pricing.CurrencyInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	info := pricing.CurrencyInfo{Requested: c.currencyCode}
	if c.loaded && c.currencyCode != "" {
		info.Effective = c.currencyCode
	} else {
		info.Effective = "USD"
	}
	if c.currencyCode != "" && c.currencyCode != "USD" && info.Effective == "USD" {
		// The requested currency priced nothing.
		info.Mixed = true
	}
	return info
}

// CatalogueError implements pricing.CatalogueErrorer. Non-nil only when the
// live catalogue rejected the requested currency (an InvalidArgument from
// ListSkus), which a scan must surface rather than silently fall back.
func (c *CatalogPricer) CatalogueError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unsupported
}

// SetCatalogueProgress registers a callback the pricer invokes as its live
// catalogue loads: once at the start with (0, services, false), once after
// each service is indexed, and once more with final=true on completion —
// whether the load succeeded or not (total is 0 when no service could be
// resolved). The callback is invoked from whichever goroutine triggers the
// lazy load (a rule worker) and must not block for long; it must be nil or
// already-settled when UnitPrice first runs, because the catalogue load is
// cached for the pricer's lifetime. Optional.
func (c *CatalogPricer) SetCatalogueProgress(f func(done, total int, final bool)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.catalogueProgress = f
}

// reportCatalogueProgress invokes the registered catalogue-load callback, if
// any. It snapshots the callback under the mutex and invokes it outside so
// the (single) loading goroutine never holds the lock across a caller's
// function.
func (c *CatalogPricer) reportCatalogueProgress(done, total int, final bool) {
	c.mu.Lock()
	f := c.catalogueProgress
	c.mu.Unlock()
	if f != nil {
		f(done, total, final)
	}
}

// UnitPrice implements pricing.Pricer with ONE source: the live Cloud Billing
// Catalog API (or, when TELLURY_PRICE_FIXTURE is set, its recorded
// replacement). There is no embedded fallback. A price that cannot be
// resolved returns ErrNoPrice, and the rule skips rather than guessing.
func (c *CatalogPricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	// Live API (or price fixture), cached for the scan's lifetime.
	if c.client != nil || priceFixturePath() != "" {
		if v, res, err := c.liveUnitPrice(kind, sku, region); err == nil {
			source := pricing.SourceLiveAPI
			if res.substitute {
				source = pricing.SourceEquivalentSKU
			}
			prov := pricing.Provenance{Source: source, SKU: res.skuID, Region: res.region}
			// Record under BOTH the requested and the resolved region.
			//
			// A rule asks for "eu" (the resource's location) and the catalogue
			// answers under "europe"; the rule then renders its evidence with
			// the region UnitPrice returned — the resolved one. Keying the
			// provenance only by the requested region made that lookup miss,
			// and PriceEvidence falls back to reporting SourceFixture. A live
			// scan therefore claimed its price came from a test fixture: seen
			// in old_machine_image evidence as "fixture sku=standard
			// region=europe" on 2026-08-14.
			c.record(kind, sku, region, prov)
			if res.region != region {
				c.record(kind, sku, res.region, prov)
			}
			return v, res.region, nil
		} else {
			// A well-formed-but-unsupported currency is an operator error to
			// surface, not a degradation to absorb.
			var ue *unsupportedCurrencyError
			if errors.As(err, &ue) {
				return 0, "", ue
			}
		}
	}

	return 0, "", pricing.ErrNoPrice
}

// MonthlyCost implements pricing.Pricer: price * quantity, using UnitPrice so
// provenance recording is identical to a direct UnitPrice call.
func (c *CatalogPricer) MonthlyCost(it pricing.Item) (float64, error) {
	unit, _, err := c.UnitPrice(it.Kind, it.Provider, it.SKU, it.Region)
	if err != nil {
		return 0, err
	}
	return unit * it.Quantity, nil
}

// LastLookup implements pricing.ProvenancePricer: the provenance of the most
// recent UnitPrice/MonthlyCost call for (kind, sku, region). A rule calls
// this immediately after pricing a resource so the Finding it builds can
// carry the exact source and SKU that produced the number. Returns
// ok=false if that exact key was never successfully resolved.
func (c *CatalogPricer) LastLookup(kind pricing.Kind, sku, region string) (pricing.Provenance, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.last[provKey(kind, sku, region)]
	return p, ok
}

func (c *CatalogPricer) record(kind pricing.Kind, sku, region string, p pricing.Provenance) {
	c.mu.Lock()
	c.last[provKey(kind, sku, region)] = p
	c.mu.Unlock()
}

func provKey(kind pricing.Kind, sku, region string) string {
	return string(kind) + "|" + sku + "|" + region
}

// priceFixturePath returns the value of TELLURY_PRICE_FIXTURE, or "" when
// unset. This is a test-only hook: it lets golden fixtures price without
// network access. It is deliberately not a user-facing flag.
func priceFixturePath() string {
	return os.Getenv("TELLURY_PRICE_FIXTURE")
}

// ─────────────────────────────────────────────────────────────────────────────
// Live Cloud Billing Catalog lookups
// ─────────────────────────────────────────────────────────────────────────────

// billingServiceForKind maps a pricing.Kind to the Cloud Billing public
// service display name ListServices returns, so ListSkus can be scoped to
// the right service instead of walking the entire (very large) catalogue.
var billingServiceForKind = map[pricing.Kind]string{
	pricing.KindDiskCapacity:        "Compute Engine",
	pricing.KindDiskIOPS:            "Compute Engine",
	pricing.KindDiskThroughput:      "Compute Engine",
	pricing.KindVMInstance:          "Compute Engine",
	pricing.KindVMCustomCPU:         "Compute Engine",
	pricing.KindVMCustomRAM:         "Compute Engine",
	pricing.KindStaticIP:            "Compute Engine",
	pricing.KindSnapshotStorage:     "Compute Engine",
	pricing.KindImageStorage:        "Compute Engine",
	pricing.KindMachineImageStorage: "Compute Engine",
	pricing.KindGCSStorage:          "Cloud Storage",
	pricing.KindGCSRetrieval:        "Cloud Storage",
	pricing.KindGCSOpsClassA:        "Cloud Storage",
}

// liveUnitPrice resolves (kind, sku, region) against the cached catalogue,
// loading it on first use. The load runs against c.ctx (the scan's
// deadline-bounded context passed to NewCatalogPricer), so a hanging Billing
// API fails cleanly at --timeout.
//
// When TELLURY_PRICE_FIXTURE is set, the catalogue loads from that file
// instead of calling the API. sku is tellury's internal SKU token
// (e.g. "pd-ssd", "n1-standard", "STANDARD") - matchSKU below maps that token
// onto real Cloud Billing SKU descriptions — but when loading from a fixture
// file, the token is looked up directly (the fixture already uses tellury's
// internal tokens).
func (c *CatalogPricer) liveUnitPrice(kind pricing.Kind, sku, region string) (float64, resolvedSKU, error) {
	c.once.Do(func() { c.loadErr = c.loadCatalogue(c.ctx) })
	if c.loadErr != nil {
		return 0, resolvedSKU{}, c.loadErr
	}

	// Exact region, then region prefix, then "default"/"global" (SKUs with
	// no region restriction), same fallback order the old embedded table used.
	for _, candidate := range regionCandidates(region) {
		if res, ok := c.skusByKey[skuKey{kind: kind, sku: sku, region: candidate}]; ok {
			return res.unitPrice, res, nil
		}
	}
	if res, ok := c.multiRegionImageSubstitute(kind, sku, region); ok {
		return res.unitPrice, res, nil
	}
	return 0, resolvedSKU{}, pricing.ErrNoPrice
}

// multiRegionImageSubstitute prices a custom image stored in a MULTI-REGION
// location, which the catalogue does not publish a SKU for.
//
// Measured against the live catalogue on 2026-08-14 (32,242 SKUs, every page):
// StorageImage has 45 SKUs and every one is regional — there is no "us", "eu",
// "europe" or "asia" entry. MachineImage, the same storage product for machine
// images, publishes all three multi-regions, each at exactly $0.05/GiB-month.
// The generic un-suffixed "Storage Image" SKU is also $0.05. Two independent
// entries in the catalogue agree on the multi-region rate, and the regional
// prices of the two products track each other within ~1%.
//
// This matters because GCP DEFAULTS a new custom image to a multi-region
// location. Requiring an exact StorageImage SKU meant the rule skipped as
// unpriced on the images most projects actually have.
//
// The substitution is deliberately narrow: image storage only, multi-region
// locations only, and only after every normal candidate has missed. The answer
// is marked so it is recorded as SourceEquivalentSKU and a reader can see the
// figure came from the machine-image SKU.
func (c *CatalogPricer) multiRegionImageSubstitute(kind pricing.Kind, sku, region string) (resolvedSKU, bool) {
	if kind != pricing.KindImageStorage {
		return resolvedSKU{}, false
	}
	for _, candidate := range regionCandidates(region) {
		if !isMultiRegion(candidate) {
			continue
		}
		if res, ok := c.skusByKey[skuKey{kind: pricing.KindMachineImageStorage, sku: sku, region: candidate}]; ok {
			res.substitute = true
			return res, true
		}
	}
	return resolvedSKU{}, false
}

// isMultiRegion reports whether a CATALOGUE region token names one of GCP's
// three multi-regions. Note "eu" is the resource-side spelling that
// regionCandidates aliases to "europe"; both are accepted so this holds
// wherever it is called from.
func isMultiRegion(region string) bool {
	switch strings.ToLower(region) {
	case "us", "eu", "europe", "asia":
		return true
	}
	return false
}

// regionCandidates returns the ordered lookup fallback chain for region.
func regionCandidates(region string) []string {
	out := []string{region}

	// GCP names the EU multi-region "eu" on a RESOURCE and "europe" in the
	// CATALOGUE. Verified live: the multi-region serviceRegions on both
	// PDSnapshot and MachineImage SKUs are "us", "asia" and "europe", while a
	// snapshot or image in that location reports "eu". The two never met, so
	// anything stored in the EU multi-region resolved no SKU and skipped as
	// unpriced — silently, because a missing price is a skip and not an error.
	//
	// The alias belongs here rather than in the graph's location: the resource
	// really is in "eu", the region node should say so, and existing tests
	// pin that. It is the catalogue that spells it differently.
	if strings.EqualFold(region, "eu") {
		out = append(out, "europe")
	}

	if idx := strings.IndexByte(region, '-'); idx > 0 {
		out = append(out, region[:idx])
	}
	out = append(out, "default", "global")
	return out
}

// listSkusRequest builds the ListSkus request for one service, carrying the
// pricer's currency. Kept as its own method so a unit test can assert the
// currency reaches the request without a gRPC round trip.
func (c *CatalogPricer) listSkusRequest(serviceID string) *billingpb.ListSkusRequest {
	return &billingpb.ListSkusRequest{Parent: serviceID, CurrencyCode: c.currencyCode}
}

// loadCatalogue fetches every SKU of every service this pricer cares about,
// exactly once, and indexes it into skusByKey. This is the only place the
// Cloud Billing API is called — everything else in this file reads the
// cache built here.
//
// When TELLURY_PRICE_FIXTURE is set, the catalogue loads from that file
// (a generic kind->SKU->region->price table) instead of calling the API.
// This is a test-only hook, not a user-facing flag.
//
// A permission failure here (the expected shape of "caller lacks billing
// access") is logged once as a warning and returned so liveUnitPrice's caller
// (UnitPrice) returns ErrNoPrice for every subsequent lookup too, since
// c.loadErr is cached in c.once.Do.
//
// A well-formed but unsupported currency (ListSkus InvalidArgument) is NOT
// absorbed: it is recorded on the pricer (CatalogueError) and returned, so
// UnitPrice surfaces it instead of silently answering the non-USD scan in
// USD.
//
// ctx is the scan's context (see CatalogPricer.ctx): every ListServices /
// ListSkus RPC inherits its deadline, so the CLI --timeout bounds this load.
//
// The registered catalogue progress callback (SetCatalogueProgress) is
// invoked as the load proceeds.
func (c *CatalogPricer) loadCatalogue(ctx context.Context) error {
	// TELLURY_PRICE_FIXTURE: load from file, no API call.
	if path := priceFixturePath(); path != "" {
		n, err := loadPriceFixture(path, c.skusByKey)
		if err != nil {
			c.reportCatalogueProgress(0, 0, true)
			c.log.Warn("gcp: price fixture load failed; no prices will resolve", "path", path, "err", err)
			return err
		}
		c.mu.Lock()
		c.loaded = n > 0
		c.mu.Unlock()
		c.reportCatalogueProgress(1, 1, true)
		c.log.Debug("gcp price catalogue loaded from fixture", "entries_indexed", n, "path", path)
		return nil
	}

	wantServices := map[string]bool{}
	for _, name := range billingServiceForKind {
		wantServices[name] = true
	}

	serviceIDs, err := c.resolveServiceIDs(ctx, wantServices)
	if err != nil {
		c.reportCatalogueProgress(0, 0, true)
		c.log.Warn("gcp: could not list Cloud Billing services; resources requiring prices will skip", "err", err)
		return err
	}

	total := 0
	done := 0
	nServices := len(serviceIDs)
	c.reportCatalogueProgress(0, nServices, false)
	for displayName, serviceID := range serviceIDs {
		n, err := c.indexServiceSKUs(ctx, displayName, serviceID)
		done++
		if err != nil {
			var ue *unsupportedCurrencyError
			if errors.As(err, &ue) {
				c.mu.Lock()
				c.unsupported = ue
				c.mu.Unlock()
				c.reportCatalogueProgress(done, nServices, true)
				return ue
			}
			c.log.Warn("gcp: could not list SKUs for billing service; resources requiring prices for it will skip",
				"service", displayName, "err", err)
		} else {
			total += n
		}
		c.reportCatalogueProgress(done, nServices, false)
	}
	c.reportCatalogueProgress(done, nServices, true)
	c.mu.Lock()
	c.loaded = total > 0
	c.mu.Unlock()
	c.log.Debug("cloud billing catalogue loaded", "skus_indexed", total, "services", len(serviceIDs), "currency", c.currencyCode)
	return nil
}

// loadPriceFixture loads a generic kind->SKU->region->price table from a
// JSON file and indexes it into skusByKey. This is the TELLURY_PRICE_FIXTURE
// path — a test-only hook, never a user-facing flag.
func loadPriceFixture(path string, skusByKey map[skuKey]resolvedSKU) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var t table
	if err := json.Unmarshal(data, &t); err != nil {
		return 0, err
	}
	n := 0
	for kind, skus := range t {
		for sku, regions := range skus {
			for region, price := range regions {
				skusByKey[skuKey{kind: kind, sku: sku, region: region}] = resolvedSKU{
					skuID:     sku,
					unitPrice: price,
					region:    region,
				}
				n++
			}
		}
	}
	return n, nil
}

// resolveServiceIDs pages ListServices once and returns the "services/XXXX"
// resource name for each display name tellury needs.
func (c *CatalogPricer) resolveServiceIDs(ctx context.Context, want map[string]bool) (map[string]string, error) {
	out := make(map[string]string, len(want))
	it := c.client.ListServices(ctx, &billingpb.ListServicesRequest{})
	for {
		svc, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, mapBillingError("ListServices", err)
		}
		if want[svc.GetDisplayName()] {
			out[svc.GetDisplayName()] = svc.GetName()
		}
		if len(out) == len(want) {
			break
		}
	}
	return out, nil
}

// indexServiceSKUs pages ListSkus for one service and indexes every SKU this
// pricer can match onto a pricing.Kind (via matchSKU) into skusByKey. The
// request carries the pricer's currency, so the prices indexed here are
// already in that currency (the API converts the whole catalogue on the
// server side).
func (c *CatalogPricer) indexServiceSKUs(ctx context.Context, displayName, serviceID string) (int, error) {
	n := 0
	it := c.client.ListSkus(ctx, c.listSkusRequest(serviceID))
	for {
		sk, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			// A well-formed but unsupported currency code is rejected by
			// ListSkus with InvalidArgument. This is an operator error to
			// surface (naming the currency), not a permission-style
			// degradation to absorb.
			if st, ok := status.FromError(err); ok && st.Code() == codes.InvalidArgument {
				return n, &unsupportedCurrencyError{currency: c.currencyCode, err: err}
			}
			return n, mapBillingError("ListSkus", err)
		}
		kind, skuToken, ok := matchSKU(sk)
		if !ok {
			continue
		}
		unit, ok := unitPriceOf(sk)
		if !ok {
			continue
		}
		for _, region := range regionsOf(sk) {
			c.skusByKey[skuKey{kind: kind, sku: skuToken, region: region}] = resolvedSKU{
				skuID:     sk.GetSkuId(),
				unitPrice: unit,
				region:    region,
			}
			n++
		}
	}
	return n, nil
}

// unsupportedCurrencyError wraps the InvalidArgument a ListSkus call returns
// for a well-formed but unsupported ISO 4217 currency code. A scan must
// surface it (naming the currency) rather than silently fall back.
type unsupportedCurrencyError struct {
	currency string
	err      error
}

func (e *unsupportedCurrencyError) Error() string {
	if e.currency == "" {
		return fmt.Sprintf("gcp: cloud billing catalogue rejected the currency request: %v", e.err)
	}
	return fmt.Sprintf("gcp: cloud billing catalogue does not support currency %q: %v", e.currency, e.err)
}

func (e *unsupportedCurrencyError) Unwrap() error { return e.err }

// matchSKU derives tellury's (Kind, sku-token) pair from a Cloud Billing SKU's
// category/description, so the live catalogue lines up with the same SKU
// vocabulary every rule already use (e.g. "pd-ssd",
// "n1-standard", "unattached", "STANDARD"/"NEARLINE" storage classes).
// Returns ok=false for every SKU tellury does not model - the vast majority
// of the catalogue.
//
// The static-IP token is pinned to the exact constant the unused_reserved_ip
// rule queries: matchSKU returns "unattached" for IP-range/static-IP SKUs,
// and TestMatchSKU_StaticIPTokenPinned asserts that token equals
// unused_reserved_ip.StaticIPSKU so a live answer and the fixture file can
// never resolve different keys.
func matchSKU(sk *billingpb.Sku) (pricing.Kind, string, bool) {
	cat := sk.GetCategory()
	desc := strings.ToLower(sk.GetDescription())
	resourceGroup := strings.ToLower(cat.GetResourceGroup())
	usageType := cat.GetUsageType()

	// Persistent disk snapshots are matched on resource group ALONE, before the
	// family switch, because Cloud Billing files them under ResourceFamily
	// "Storage" — not "Compute", where a reader would look for a Compute Engine
	// SKU, and not where an earlier version of this function put them. Their
	// group name "PDSnapshot" is unique in the catalogue, so it identifies them
	// without help from the family.
	//
	// Billed per GiB-month (usage unit GiBy.mo), which is why the rule converts
	// bytes with 1<<30 rather than 1e9. The group holds three kinds of SKU and
	// only the first is a standing storage rate:
	//   "Storage PD Snapshot"                the standard rate
	//   "... Archive Snapshot Data Storage"  the archive tier
	//   "... Snapshot Early Deletion"        a one-off charge, not a rate
	if resourceGroup == "pdsnapshot" && usageType == "OnDemand" {
		switch {
		case strings.Contains(desc, "early deletion"):
			return "", "", false
		case strings.Contains(desc, "archive"):
			return pricing.KindSnapshotStorage, "archive", true
		default:
			return pricing.KindSnapshotStorage, "standard", true
		}
	}

	// Custom images and machine images are also Compute Engine storage, billed
	// per GiB-month. The Cloud Billing resource groups are distinct from
	// PDSnapshot and are matched before the family switch for the same reason:
	// a reader would otherwise look under "Compute" and miss them.
	//
	// THE TOKENS ARE "storageimage" AND "machineimage", verified against the live
	// Cloud Billing catalogue. They were originally written as "imagestorage"
	// and "machineimagestorage" — plausible, symmetric with the Kind names, and
	// matching nothing. Every live image lookup would have missed and every
	// image skipped as unpriced, which is the same defect that hid in the GCP
	// snapshot token and the Azure managed-disk families: a wrong SKU token does
	// not fail, it silently prices nothing.
	if usageType == "OnDemand" {
		switch resourceGroup {
		case "storageimage":
			if strings.Contains(desc, "early deletion") {
				return "", "", false
			}
			return pricing.KindImageStorage, "standard", true
		case "machineimage":
			if strings.Contains(desc, "early deletion") {
				return "", "", false
			}
			return pricing.KindMachineImageStorage, "standard", true
		}
	}

	switch cat.GetResourceFamily() {
	case "Compute":
		if usageType != "OnDemand" {
			return "", "", false // exclude Spot/Preemptible/Commitment SKUs from v1 matching
		}
		switch resourceGroup {
		case "n1standard", "n2standard", "n2dstandard", "e2standard", "n4standard",
			"c2standard", "c2dstandard", "c3standard", "c3dstandard", "t2dstandard", "t2astandard":
			if family, ok := machineFamilyFromDescription(desc); ok {
				return pricing.KindVMInstance, family, true
			}
		case "cpu":
			if family, ok := customFamilyFromDescription(desc); ok {
				return pricing.KindVMCustomCPU, family, true
			}
		case "ram":
			if family, ok := customFamilyFromDescription(desc); ok {
				return pricing.KindVMCustomRAM, family, true
			}
		case "ssd", "pdssd":
			if strings.Contains(desc, "regional") {
				return pricing.KindDiskCapacity, "pd-ssd-regional", true
			}
			return pricing.KindDiskCapacity, "pd-ssd", true
		case "storagepdcapacity", "pdstandard":
			if strings.Contains(desc, "regional") {
				return pricing.KindDiskCapacity, "pd-standard-regional", true
			}
			return pricing.KindDiskCapacity, "pd-standard", true
		case "extremepd":
			return pricing.KindDiskCapacity, "pd-extreme", true
		case "iprange", "staticipaddress":
			// A reserved external (static) IP: a flat per-address-hour rate.
			// Indexed under the same "unattached" token the fixture's
			// static_ip.unattached entry and the unused_reserved_ip rule use,
			// so the live catalogue resolves the exact key the rule queries.
			return pricing.KindStaticIP, "unattached", true
		}
	case "Storage":
		switch {
		case strings.Contains(desc, "standard storage"):
			return pricing.KindGCSStorage, "STANDARD", true
		case strings.Contains(desc, "nearline storage"):
			return pricing.KindGCSStorage, "NEARLINE", true
		case strings.Contains(desc, "coldline storage"):
			return pricing.KindGCSStorage, "COLDLINE", true
		case strings.Contains(desc, "archive storage"):
			return pricing.KindGCSStorage, "ARCHIVE", true
		case strings.Contains(desc, "class a"):
			return pricing.KindGCSOpsClassA, "STANDARD", true
		case strings.Contains(desc, "retrieval"):
			return pricing.KindGCSRetrieval, "STANDARD", true
		}
	}
	return "", "", false
}

// machineFamilyFromDescription recovers a machine-family token from a
// Compute Engine predefined-instance SKU description, e.g. "N1 Instance
// Core running in Americas" -> "n1-standard". Cloud Billing prices
// predefined shapes per-core/per-GB, the same as custom shapes, so the live
// catalogue carries no single per-instance rate for a machine type.
//
// KNOWN GAP (flagged in the catalog audit as unverified): no rule consumes
// these family-granular tokens. Rules query KindVMInstance with the full
// machine type ("n1-standard-4"), which the per-core live SKUs cannot
// produce, so predefined-instance lookups always fall back to the embedded
// per-instance table. Closing the gap is a rule-side cost-model change
// (vCPU × family-core-rate + RAM × family-ram-rate — the formula
// instanceMonthlyCost already applies to custom shapes) and must be verified
// against the live catalogue; it is not a token alignment and is out of
// scope for the fixes here.
func machineFamilyFromDescription(desc string) (string, bool) {
	fields := strings.Fields(desc)
	if len(fields) == 0 {
		return "", false
	}
	family := strings.ToLower(fields[0])
	if family == "" {
		return "", false
	}
	return family + "-standard", true
}

// customFamilyFromDescription recovers the custom-machine-type family token
// (e.g. "n2-custom") a "Custom Instance Core running in ..." /
// "Custom Instance Ram running in ..." SKU description belongs to.
func customFamilyFromDescription(desc string) (string, bool) {
	if !strings.Contains(desc, "custom") {
		return "", false
	}
	fields := strings.Fields(desc)
	if len(fields) == 0 {
		return "", false
	}
	family := strings.ToLower(fields[0])
	if family == "" {
		return "", false
	}
	return family + "-custom", true
}

// unitPriceOf extracts the current tier's unit price, in the catalogue's
// currency (the currency requested via ListSkusRequest.CurrencyCode, or USD
// by default), from a SKU's PricingInfo. Cloud Billing expresses money as
// (units, nanos) pairs; tellury keeps money in integer cents on the cost
// path, so the conversion goes through whole cents (units*100 + nanos/1e7)
// rather than through a float dollar intermediate, matching the convention
// pricing.Round2 and the embedded/overlay JSON tables already use. The
// result is still returned as float64 because that is pricing.Pricer's
// existing return type (shared with StaticPricer) - only the conversion
// arithmetic itself avoids float money math.
func unitPriceOf(sk *billingpb.Sku) (float64, bool) {
	infos := sk.GetPricingInfo()
	if len(infos) == 0 {
		return 0, false
	}
	// Cloud Billing returns PricingInfo in chronological order; the last
	// entry is the currently effective price.
	expr := infos[len(infos)-1].GetPricingExpression()
	if expr == nil {
		return 0, false
	}
	rates := expr.GetTieredRates()
	if len(rates) == 0 {
		return 0, false
	}
	// tellury's cost model is linear, not tiered (matching every embedded
	// table entry): use the base (first) tier's rate.
	money := rates[0].GetUnitPrice()
	if money == nil {
		return 0, false
	}
	// Full precision, NOT cents. Cloud Billing expresses a price as whole units
	// plus nanos, and cloud unit prices are routinely far finer than a cent:
	// coldline storage is $0.004/GiB-month and custom RAM $0.004446/GiB-hour.
	// Truncating to cents rounded both to ZERO, silently pricing them free, and
	// cost a vCPU-hour about 10% of its value. It also broke every non-USD
	// scan wholesale, since a converted price almost never lands on a round
	// cent — EUR 0.043890 became 0.04, understating a real bill by 9%.
	return float64(money.GetUnits()) + float64(money.GetNanos())/1e9, true
}

// regionsOf renders the lowercase region tokens a SKU is offered in, or
// "default" for SKUs with no region restriction (Cloud Billing's convention
// for globally-priced SKUs, e.g. static IPs and some storage classes).
func regionsOf(sk *billingpb.Sku) []string {
	regions := sk.GetServiceRegions()
	if len(regions) == 0 {
		return []string{"default"}
	}
	out := make([]string, 0, len(regions))
	for _, r := range regions {
		out = append(out, strings.ToLower(r))
	}
	sort.Strings(out)
	return out
}

// mapBillingError turns a raw gRPC status from the Catalog API into a
// message an operator can act on, mirroring mapListAssetsError /
// mapMonitoringError. A missing billing.viewer role is the expected failure
// mode for an otherwise-healthy scan; loadCatalogue treats it (and every
// other error here) as non-fatal: resources requiring prices will skip.
func mapBillingError(rpc string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("gcp: cloud billing %s: %w", rpc, err)
	}
	switch st.Code() {
	case codes.PermissionDenied:
		return fmt.Errorf(
			"gcp: permission denied calling Cloud Billing %s: grant roles/billing.viewer "+
				"(or cloudbilling.services.list / cloudbilling.skus.list) to the identity behind "+
				"your Application Default Credentials: %s", rpc, st.Message())
	case codes.Unauthenticated:
		return fmt.Errorf(
			"gcp: unauthenticated calling Cloud Billing %s: no valid Application Default Credentials "+
				"found (run `gcloud auth application-default login` or set GOOGLE_APPLICATION_CREDENTIALS): %s",
			rpc, st.Message())
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return fmt.Errorf("gcp: cloud billing %s unreachable: %w", rpc, context.DeadlineExceeded)
	case codes.ResourceExhausted:
		return fmt.Errorf("gcp: Cloud Billing Catalog quota exceeded calling %s; retry later: %s", rpc, st.Message())
	default:
		return fmt.Errorf("gcp: cloud billing %s: %s: %s", rpc, st.Code(), st.Message())
	}
}
