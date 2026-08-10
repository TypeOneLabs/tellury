package gcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// CatalogPricer implements pricing.Pricer (plus pricing.ProvenancePricer and
// pricing.OverlayLoader) over the Cloud Billing Catalog API
// (cloud.google.com/go/billing/apiv1, CloudCatalogClient), with the embedded
// StaticPricer as its fallback. Precedence (highest first), enforced right
// here in UnitPrice, and stated in `tellury scan --help` (see the --price-file
// flag text in internal/cli/scan.go):
//
//  1. --price-file override (pricing.SourceOverride)
//  2. live Cloud Billing Catalog API (pricing.SourceLiveAPI), cached for the
//     lifetime of this CatalogPricer, i.e. for the duration of one scan
//  3. embedded static price table (pricing.SourceEmbedded)
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
	static *StaticPricer

	// staticBaseline is the pristine, never-overlaid embedded StaticPricer
	// built once at construction. overrideValue resolves a key against it
	// to decide whether the (overlaid) `static` table genuinely differs
	// from what the embedded price file would have answered — i.e. whether
	// --price-file actually set it. Built once and reused across every
	// UnitPrice call rather than rebuilt-and-decoded per lookup.
	staticBaseline *StaticPricer

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
	// "the embedded USD table priced them" in CurrencyInfo.
	loaded bool
	// mixed records that a USD embedded-fallback price was used while a
	// non-USD currency was requested — the currency-mix trap. Read by
	// CurrencyInfo for the report's currency_mixed disclosure.
	mixed bool
	// unsupported is non-nil when the live catalogue rejected the requested
	// currency (InvalidArgument from ListSkus). Such a failure must surface
	// to the scan, not silently fall back to the USD embedded table.
	unsupported error

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
}

var (
	_ pricing.Pricer           = (*CatalogPricer)(nil)
	_ pricing.ProvenancePricer = (*CatalogPricer)(nil)
	_ pricing.OverlayLoader    = (*CatalogPricer)(nil)
	_ pricing.CurrencySetter   = (*CatalogPricer)(nil)
	_ pricing.CurrencyReporter = (*CatalogPricer)(nil)
	_ pricing.CatalogueErrorer = (*CatalogPricer)(nil)
)

// NewCatalogPricer builds a pricer that prefers the live Cloud Billing
// Catalog API and falls back to the embedded static table. It performs no
// RPCs itself: the catalogue is fetched lazily, once, on first UnitPrice
// call (see loadCatalogue), so a scan with no billing permission never even
// attempts the call - it just quietly resolves everything through the
// embedded fallback instead. Failure to build the API client at all
// (e.g. ADC entirely absent) is likewise non-fatal here: NewCatalogPricer
// still returns a usable pricer, just one whose live path is permanently
// unavailable.
//
// currencyCode is the ISO 4217 code the catalogue is fetched in ("" = USD).
// The live Cloud Billing API prices the whole catalogue in this currency
// (ListSkusRequest.CurrencyCode), so the snapshot SKU prices come back
// converted. A well-formed but unsupported code makes ListSkus fail with
// InvalidArgument; the pricer records that failure (CatalogueError) and
// refuses to silently fall back to the USD embedded table, and UnitPrice
// surfaces it so the scan cannot complete with wrong-currency figures.
//
// The pristine embedded baseline used to detect --price-file overrides is
// built exactly once here and stored (see staticBaseline), so
// overrideValue never pays a per-lookup rebuild/JSON-decode cost.
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
	static, err := NewStaticPricer()
	if err != nil {
		return nil, err
	}
	// Build the pristine baseline once at construction and reuse it for the
	// lifetime of the pricer. This is the exact JSON-decoded table the
	// embedded fallback would answer with before any --price-file overlay:
	// comparing the live `static` table against it every UnitPrice call
	// tells us conclusively whether an overlay changed that entry.
	staticBaseline, err := NewStaticPricer()
	if err != nil {
		return nil, err
	}
	client, err := billing.NewCloudCatalogClient(ctx)
	if err != nil {
		log.Warn("gcp: cloud billing catalog client unavailable; pricing will use the embedded fallback table", "err", err)
		client = nil
	}
	return &CatalogPricer{
		log:            log,
		ctx:            ctx,
		client:         client,
		static:         static,
		staticBaseline: staticBaseline,
		currencyCode:   currencyCode,
		skusByKey:      map[skuKey]resolvedSKU{},
		last:           map[string]pricing.Provenance{},
	}, nil
}

// Close releases the underlying gRPC connection, if any.
func (c *CatalogPricer) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// OverlayFile implements pricing.OverlayLoader: --price-file always applies
// on top of the embedded fallback table. It does not touch the live
// catalogue cache - the override is still consulted first, on every lookup,
// regardless of whether the live API answered (see UnitPrice), so this is
// sufficient to give the override the highest precedence.
func (c *CatalogPricer) OverlayFile(path string) error {
	return c.static.OverlayFile(path)
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
// prices in the requested currency), otherwise "USD" — the embedded table's
// currency. Mixed is true when a non-USD currency was requested and any
// embedded (USD) price was used, or when the requested currency could not
// price the catalogue at all (so every answer came from the USD table).
func (c *CatalogPricer) CurrencyInfo() pricing.CurrencyInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	info := pricing.CurrencyInfo{Requested: c.currencyCode}
	if c.loaded && c.currencyCode != "" {
		info.Effective = c.currencyCode
	} else {
		info.Effective = "USD"
	}
	info.Mixed = c.mixed
	if c.currencyCode != "" && c.currencyCode != "USD" && info.Effective == "USD" {
		// The requested currency priced nothing: every figure the scan
		// reports came from the USD embedded table. That is a full currency
		// mismatch, not the quiet normal degradation of a USD-requesting scan
		// losing live access.
		info.Mixed = true
	}
	return info
}

// CatalogueError implements pricing.CatalogueErrorer. Non-nil only when the
// live catalogue rejected the requested currency (an InvalidArgument from
// ListSkus), which a scan must surface rather than silently fall back to the
// USD embedded table.
func (c *CatalogPricer) CatalogueError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unsupported
}

// UnitPrice implements pricing.Pricer with the documented precedence:
// --price-file override > live Cloud Billing Catalog API > embedded table.
func (c *CatalogPricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	// 1. --price-file override always wins, if this exact key was changed
	// by an overlay (see overrideValue for how "was changed" is decided).
	if v, resolvedRegion, ok := c.overrideValue(kind, sku, region); ok {
		c.record(kind, sku, region, pricing.Provenance{Source: pricing.SourceOverride, SKU: sku, Region: resolvedRegion})
		return v, resolvedRegion, nil
	}

	// 2. Live API, cached for the scan's lifetime. The catalogue load runs
	// against c.ctx — the scan's deadline-bounded context — so a Billing API
	// hang cannot outlive --timeout.
	if c.client != nil {
		if v, res, err := c.liveUnitPrice(kind, sku, region); err == nil {
			c.record(kind, sku, region, pricing.Provenance{Source: pricing.SourceLiveAPI, SKU: res.skuID, Region: res.region})
			return v, res.region, nil
		} else {
			// A well-formed-but-unsupported currency is an operator error to
			// surface, not a degradation to absorb: falling back here would
			// silently answer the EUR-asking scan in USD.
			var ue *unsupportedCurrencyError
			if errors.As(err, &ue) {
				return 0, "", ue
			}
		}
	}

	// 3. Embedded fallback. This table is USD only, so an answer here is
	// always in USD — record that so CurrencyInfo can flag the mix when a
	// non-USD currency was requested.
	v, resolvedRegion, err := c.static.UnitPrice(kind, provider, sku, region)
	if err != nil {
		return 0, "", err
	}
	c.noteEmbeddedUSD()
	c.record(kind, sku, region, pricing.Provenance{Source: pricing.SourceEmbedded, SKU: sku, Region: resolvedRegion})
	return v, resolvedRegion, nil
}

// noteEmbeddedUSD records that the embedded USD table answered a lookup. When
// a non-USD currency was requested this is the currency-mix trap and must be
// disclosed on the report (via CurrencyInfo).
func (c *CatalogPricer) noteEmbeddedUSD() {
	c.mu.Lock()
	if c.currencyCode != "" && c.currencyCode != "USD" {
		c.mixed = true
	}
	c.mu.Unlock()
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

// overrideValue reports whether (kind, sku, region) resolves differently in
// the (possibly overlaid) static table than it would in a pristine,
// never-overlaid embedded table - i.e. it was genuinely set by
// --price-file. The pristine baseline is built once at construction
// (staticBaseline) and reused on every lookup, so this comparison never
// pays a rebuild-and-decode cost per UnitPrice call.
func (c *CatalogPricer) overrideValue(kind pricing.Kind, sku, region string) (float64, string, bool) {
	overlaid, resolvedRegion, err := c.static.UnitPrice(kind, "gcp", sku, region)
	if err != nil {
		return 0, "", false
	}
	baseline := c.staticBaseline
	if baseline == nil {
		// Cannot tell the two apart; be conservative rather than claim an
		// override that might not exist. Falls through to live/embedded.
		return 0, "", false
	}
	pristineVal, _, pristineErr := baseline.UnitPrice(kind, "gcp", sku, region)
	if pristineErr != nil || pristineVal != overlaid {
		// Either the embedded table never had this entry at all (the
		// overlay introduced it), or the value changed: both are
		// conclusive evidence of a genuine override.
		return overlaid, resolvedRegion, true
	}
	return 0, "", false
}

// ─────────────────────────────────────────────────────────────────────────────
// Live Cloud Billing Catalog lookups
// ─────────────────────────────────────────────────────────────────────────────

// billingServiceForKind maps a pricing.Kind to the Cloud Billing public
// service display name ListServices returns, so ListSkus can be scoped to
// the right service instead of walking the entire (very large) catalogue.
var billingServiceForKind = map[pricing.Kind]string{
	pricing.KindDiskCapacity:    "Compute Engine",
	pricing.KindDiskIOPS:        "Compute Engine",
	pricing.KindDiskThroughput:  "Compute Engine",
	pricing.KindVMInstance:      "Compute Engine",
	pricing.KindVMCustomCPU:     "Compute Engine",
	pricing.KindVMCustomRAM:     "Compute Engine",
	pricing.KindStaticIP:        "Compute Engine",
	pricing.KindSnapshotStorage: "Compute Engine",
	pricing.KindGCSStorage:      "Cloud Storage",
	pricing.KindGCSRetrieval:    "Cloud Storage",
	pricing.KindGCSOpsClassA:    "Cloud Storage",
}

// liveUnitPrice resolves (kind, sku, region) against the cached catalogue,
// loading it on first use. The load runs against c.ctx (the scan's
// deadline-bounded context passed to NewCatalogPricer), so a hanging Billing
// API fails cleanly at --timeout and UnitPrice falls back to the embedded
// table rather than stalling the rule. sku is tellury's internal SKU token
// (e.g. "pd-ssd", "n1-standard", "STANDARD") - matchSKU below maps that token
// onto real Cloud Billing SKU descriptions.
func (c *CatalogPricer) liveUnitPrice(kind pricing.Kind, sku, region string) (float64, resolvedSKU, error) {
	c.once.Do(func() { c.loadErr = c.loadCatalogue(c.ctx) })
	if c.loadErr != nil {
		return 0, resolvedSKU{}, c.loadErr
	}

	// Exact region, then region prefix, then "default"/"global" (SKUs with
	// no region restriction), same fallback order as the embedded table.
	for _, candidate := range regionCandidates(region) {
		if res, ok := c.skusByKey[skuKey{kind: kind, sku: sku, region: candidate}]; ok {
			return res.unitPrice, res, nil
		}
	}
	return 0, resolvedSKU{}, pricing.ErrNoPrice
}

// regionCandidates returns the ordered lookup fallback chain for region.
func regionCandidates(region string) []string {
	out := []string{region}
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
// Cloud Billing API is called - everything else in this file reads the
// cache built here, satisfying the "cache the catalogue for the duration of
// a scan, do not call the API per resource" requirement. A permission
// failure here (the expected shape of "caller lacks billing access") is
// logged once as a warning and returned so liveUnitPrice's caller (UnitPrice)
// falls back to the embedded table for every subsequent lookup too, since
// c.loadErr is cached in c.once.Do.
//
// A well-formed but unsupported currency (ListSkus InvalidArgument) is NOT
// absorbed: it is recorded on the pricer (CatalogueError) and returned, so
// UnitPrice surfaces it instead of silently answering the non-USD scan in
// USD.
//
// ctx is the scan's context (see CatalogPricer.ctx): every ListServices /
// ListSkus RPC inherits its deadline, so the CLI --timeout bounds this load.
func (c *CatalogPricer) loadCatalogue(ctx context.Context) error {
	wantServices := map[string]bool{}
	for _, name := range billingServiceForKind {
		wantServices[name] = true
	}

	serviceIDs, err := c.resolveServiceIDs(ctx, wantServices)
	if err != nil {
		c.log.Warn("gcp: could not list Cloud Billing services; pricing will use the embedded fallback table", "err", err)
		return err
	}

	total := 0
	for displayName, serviceID := range serviceIDs {
		n, err := c.indexServiceSKUs(ctx, displayName, serviceID)
		if err != nil {
			var ue *unsupportedCurrencyError
			if errors.As(err, &ue) {
				c.mu.Lock()
				c.unsupported = ue
				c.mu.Unlock()
				return ue
			}
			c.log.Warn("gcp: could not list SKUs for billing service; pricing for it will use the embedded fallback table",
				"service", displayName, "err", err)
			continue
		}
		total += n
	}
	c.mu.Lock()
	c.loaded = total > 0
	c.mu.Unlock()
	c.log.Debug("cloud billing catalogue loaded", "skus_indexed", total, "services", len(serviceIDs), "currency", c.currencyCode)
	return nil
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
// surface it (naming the currency) rather than silently fall back to the USD
// embedded table: an operator who asked for EUR figures must not get USD
// numbers without being told the request was impossible.
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
// vocabulary the embedded table and every rule already use (e.g. "pd-ssd",
// "n1-standard", "unattached", "STANDARD"/"NEARLINE" storage classes).
// Returns ok=false for every SKU tellury does not model - the vast majority
// of the catalogue.
//
// The static-IP token is pinned to the exact constant the unused_reserved_ip
// rule queries: matchSKU returns "unattached" for IP-range/static-IP SKUs,
// and TestMatchSKU_StaticIPTokenPinned asserts that token equals
// unused_reserved_ip.StaticIPSKU so a live answer and the embedded fallback
// can never resolve different keys (and every static-IP price silently fall
// back to the embedded table) again.
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
			// Indexed under the same "unattached" token the embedded table's
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
// other error here) as non-fatal and falls back to the embedded table.
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
