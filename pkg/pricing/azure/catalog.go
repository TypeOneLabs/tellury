// Package azure is the Azure pricing model for tellury. It has one live
// implementation of pricing.Pricer:
//
//   - CatalogPricer (this file): the public, unauthenticated Azure Retail
//     Prices API, resolved lazily per (kind, sku, region) and cached for the
//     scan's lifetime. When TELLURY_PRICE_FIXTURE is set, the recorded Retail
//     Prices responses are loaded instead of calling the API — a test-only
//     hook, never a user-facing flag.
//
//   - StaticPricer (static.go): reads recorded Retail Prices API responses
//     from a JSON file on disk. It is reached only through
//     TELLURY_PRICE_FIXTURE. There is no embedded table and no automatic
//     fallback.
//
// The API is public and free; pricing needs no credentials. That is why the
// filter set is the whole job: one SKU in one region returns Consumption,
// Reservation and DevTestConsumption rows, plus spot and low-priority
// variants, plus Windows and Linux. Taking the first row returns a real price
// for the wrong thing. Every lookup therefore builds the full predicate set
// below and then re-asserts every predicate against the returned rows, so the
// live request and the fixture matcher share one filter table.
package azure

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

const (
	retailPricesBaseURL = "https://prices.azure.com/api/retail/prices"
	// The Retail Prices API rejects an OData filter with more than 15 total
	// predicates. The full matcher has more than 15 (all equality predicates
	// plus all nine contains exclusions); serverFilter therefore sends as many
	// of the exclusion contains terms as fit and the client-side matcher still
	// enforces every one. This limit was discovered against the live API and
	// is pinned by TestServerFilterUnderPredicateLimit.
	maxRetailServerPredicates = 15
)

type filterOp int

const (
	filterEq filterOp = iota
	filterNotContains
	filterStartsWith
	filterContains
)

// priceFilter is one predicate of the Azure price lookup. The same slice is
// used to build the live OData $filter and to match every row returned by the
// API or by a recorded fixture. The constants are exactly the filters the
// design calls non-optional; field/value spelling is pinned by the recorded
// retail-prices-recorded.json responses, not guessed.
type priceFilter struct {
	Field string
	Value string
	Op    filterOp
}

type skuKey struct {
	kind   pricing.Kind
	sku    string
	region string
}

type resolvedSKU struct {
	unitPrice     float64
	region        string
	unitOfMeasure string
	sku           string
}

// CatalogPricer implements pricing.Pricer over the Azure Retail Prices API.
// The live API is the ONLY source. When TELLURY_PRICE_FIXTURE is set, the
// recorded API responses are loaded instead of calling the API — a test-only
// hook, never a user-facing flag.
//
// There is no embedded fallback table. An unresolvable price returns
// pricing.ErrNoPrice and the rule skips with SkipNoPrice; tellury never
// guesses a dollar figure.
type CatalogPricer struct {
	log    *slog.Logger
	ctx    context.Context
	client *http.Client

	mu    sync.Mutex
	cache map[skuKey]resolvedSKU

	// VM pricing has a second cache axis: size + region + OS. A single SKU in
	// a single region has a distinct Linux and Windows row, so it cannot share
	// the (kind, sku, region) cache key.
	vmCache       map[vmPriceKey]resolvedSKU
	vmUnpriceable map[vmPriceKey]bool

	// unpriceable records keys the API has already refused, so a scan asks
	// once rather than once per node. Without it, a subscription full of disks
	// sharing one unresolvable SKU issues one HTTP request per disk — and an
	// unresolvable SKU is not hypothetical: a wrong product name made every
	// Standard HDD disk exactly that case, so a lookup bug became a request
	// storm. An API outage behaves the same way.
	unpriceable map[skuKey]bool
	last        map[string]pricing.Provenance

	fixtureOnce    sync.Once
	fixtureEntries []retailFixtureEntry
	fixtureErr     error
}

var (
	_ pricing.Pricer           = (*CatalogPricer)(nil)
	_ pricing.ProvenancePricer = (*CatalogPricer)(nil)
	_ pricing.CurrencyReporter = (*CatalogPricer)(nil)
	_ VMPricer                 = (*CatalogPricer)(nil)
)

// NewCatalogPricer builds a pricer over the live Azure Retail Prices API. It
// performs no RPCs itself: each (kind, sku, region) is fetched lazily on
// first use and cached for the scan's lifetime.
//
// When TELLURY_PRICE_FIXTURE is set, the pricer loads recorded Retail Prices
// API responses from that file instead of calling the live API. This is a
// test-only hook, not a user-facing flag.
//
// ctx is retained so the live HTTP request honours the scan's --timeout
// deadline. The Azure Retail Prices API is unauthenticated, so there is no
// credential resolution and no client construction that can fail.
func NewCatalogPricer(ctx context.Context, log *slog.Logger) (*CatalogPricer, error) {
	if log == nil {
		log = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &CatalogPricer{
		log:           log,
		ctx:           ctx,
		client:        &http.Client{Timeout: 30 * time.Second},
		cache:         map[skuKey]resolvedSKU{},
		vmCache:       map[vmPriceKey]resolvedSKU{},
		vmUnpriceable: map[vmPriceKey]bool{},
		unpriceable:   map[skuKey]bool{},
		last:          map[string]pricing.Provenance{},
	}, nil
}

// CurrencyInfo implements pricing.CurrencyReporter. The Azure Retail Prices
// API prices every dimension in USD and offers no currency choice in this
// batch, so figures are always USD.
func (c *CatalogPricer) CurrencyInfo() pricing.CurrencyInfo {
	return pricing.CurrencyInfo{Requested: "", Effective: "USD"}
}

// UnitPrice implements pricing.Pricer with ONE source: the live Retail Prices
// API (or, when TELLURY_PRICE_FIXTURE is set, its recorded replacement). Any
// failure to resolve returns pricing.ErrNoPrice, never a guessed number.
func (c *CatalogPricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if v, res, err := c.liveUnitPrice(kind, sku, region); err == nil {
		c.record(kind, sku, region, pricing.Provenance{Source: pricing.SourceLiveAPI, SKU: res.sku, Region: res.region})
		return v, res.region, nil
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
// recent UnitPrice/MonthlyCost answer for (kind, sku, region).
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

func priceFixturePath() string {
	return os.Getenv("TELLURY_PRICE_FIXTURE")
}

// liveUnitPrice resolves (kind, sku, region) against the live API or the
// recorded fixture. The result is cached for the pricer's lifetime, so each
// distinct key incurs at most one HTTP request (or one fixture scan).
func (c *CatalogPricer) liveUnitPrice(kind pricing.Kind, sku, region string) (float64, resolvedSKU, error) {
	region = LocationRegion(region)
	key := skuKey{kind: kind, sku: sku, region: region}

	c.mu.Lock()
	if res, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return res.unitPrice, res, nil
	}
	if c.unpriceable[key] {
		c.mu.Unlock()
		return 0, resolvedSKU{}, pricing.ErrNoPrice
	}
	c.mu.Unlock()

	filters, err := lookupFilters(kind, sku, region)
	if err != nil {
		return 0, resolvedSKU{}, err
	}

	var rows []retailItem
	if path := priceFixturePath(); path != "" {
		entries, err := c.loadFixture(path)
		if err != nil {
			return 0, resolvedSKU{}, err
		}
		for _, entry := range entries {
			page, err := parseRetailPage(entry.Body)
			if err != nil {
				c.log.Debug("azure: could not parse recorded price response; skipping", "name", entry.Name, "err", err)
				continue
			}
			rows = append(rows, page.Items...)
		}
	} else {
		rows, err = c.fetchLiveContext(c.ctx, filters)
		if err != nil {
			c.log.Warn("azure: Retail Prices API unavailable; resources requiring prices will skip", "err", err)
			return 0, resolvedSKU{}, err
		}
	}

	res, ok := selectPrice(rows, filters, kind)
	if !ok {
		// Remember the refusal. The answer will not change within a scan, and
		// re-asking once per node turns one unresolvable SKU into one HTTP
		// request per resource that carries it.
		c.mu.Lock()
		c.unpriceable[key] = true
		c.mu.Unlock()
		return 0, resolvedSKU{}, pricing.ErrNoPrice
	}

	c.mu.Lock()
	c.cache[key] = res
	c.mu.Unlock()
	return res.unitPrice, res, nil
}

func (c *CatalogPricer) loadFixture(path string) ([]retailFixtureEntry, error) {
	c.fixtureOnce.Do(func() {
		c.fixtureEntries, c.fixtureErr = loadFixtureEntries(path)
	})
	if c.fixtureErr != nil {
		return nil, c.fixtureErr
	}
	return c.fixtureEntries, nil
}

func (c *CatalogPricer) fetchLive(filters []priceFilter) ([]retailItem, error) {
	return c.fetchLiveContext(c.ctx, filters)
}

func (c *CatalogPricer) fetchLiveContext(ctx context.Context, filters []priceFilter) ([]retailItem, error) {
	if ctx == nil {
		ctx = c.ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	u := retailPricesBaseURL + "?$filter=" + url.QueryEscape(serverFilter(filters))
	var rows []retailItem
	for u != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &retailPricesError{statusCode: resp.StatusCode, body: body}
		}
		page, err := parseRetailPage(body)
		if err != nil {
			return nil, err
		}
		rows = append(rows, page.Items...)
		if page.NextPageLink == nil || *page.NextPageLink == "" {
			break
		}
		u = *page.NextPageLink
	}
	return rows, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Filter table and OData construction
// ─────────────────────────────────────────────────────────────────────────────

// lookupFilters returns the complete filter set for one (kind, sku, region)
// lookup. The live request is built from it and every returned row is matched
// against it. The concrete serviceName/productName/meterName spellings are
// pinned by the recorded API responses; they are NOT the same as the ARM
// provider SKU names (for example, the ARM disk SKU is Premium_LRS, while the
// Retail Prices row's skuName is "P10 LRS" and its armSkuName is
// "Premium_SSD_Managed_Disk_P10").
func lookupFilters(kind pricing.Kind, sku, region string) ([]priceFilter, error) {
	region = LocationRegion(region)
	if region == "" {
		return nil, pricing.ErrNoPrice
	}

	switch kind {
	case pricing.KindManagedDisk:
		productName, meterName, ok := managedDiskProduct(sku)
		if !ok {
			return nil, pricing.ErrNoPrice
		}
		return managedDiskFilters(sku, region, productName, meterName), nil

	case pricing.KindStaticIP:
		// The only billable public-IP SKU the Azure rule queries is the
		// Standard static IPv4 address. The Retail Prices API row is
		// distinguished from "Public IP Prefix" by productName and meterName,
		// because both share skuName "Standard".
		if sku != "Standard" {
			return nil, pricing.ErrNoPrice
		}
		return staticIPFilters(region), nil

	default:
		return nil, pricing.ErrNoPrice
	}
}

func managedDiskFilters(sku, region, productName, meterName string) []priceFilter {
	return append([]priceFilter{
		{Field: "serviceName", Value: "Storage", Op: filterEq},
		{Field: "armRegionName", Value: region, Op: filterEq},
		{Field: "skuName", Value: sku, Op: filterEq},
		{Field: "type", Value: "Consumption", Op: filterEq},
		{Field: "productName", Value: productName, Op: filterEq},
		{Field: "meterName", Value: meterName, Op: filterEq},
		{Field: "isPrimaryMeterRegion", Value: "true", Op: filterEq},
		{Field: "tierMinimumUnits", Value: "0", Op: filterEq},
	}, spotAndWindowsExclusions()...)
}

// staticIPFilters deliberately OMITS isPrimaryMeterRegion, which the managed-disk
// filter enforces. That asymmetry is measured, not accidental: querying the live
// Retail Prices API for the Standard static IPv4 meter returns
// isPrimaryMeterRegion=false in every region checked (northeurope, westeurope,
// swedencentral, eastus, uksouth), while the S4 LRS disk meter returns true in
// all five. Adding the predicate here would match zero rows and make every
// public IP unpriceable. Pinned by TestPrimaryMeterRegionAsymmetry.
func staticIPFilters(region string) []priceFilter {
	return append([]priceFilter{
		{Field: "serviceName", Value: "Virtual Network", Op: filterEq},
		{Field: "armRegionName", Value: region, Op: filterEq},
		{Field: "skuName", Value: "Standard", Op: filterEq},
		{Field: "type", Value: "Consumption", Op: filterEq},
		{Field: "productName", Value: "IP Addresses", Op: filterEq},
		{Field: "meterName", Value: "Standard IPv4 Static Public IP", Op: filterEq},
		{Field: "tierMinimumUnits", Value: "0", Op: filterEq},
	}, spotAndWindowsExclusions()...)
}

// spotExclusions returns the six not-contains terms shared by every Linux
// consumption lookup. They are enforced by the fixture/live-row matcher; the
// live OData request sends as many as the Retail Prices API accepts.
func spotExclusions() []priceFilter {
	return []priceFilter{
		{Field: "skuName", Value: "Spot", Op: filterNotContains},
		{Field: "skuName", Value: "Low Priority", Op: filterNotContains},
		{Field: "meterName", Value: "Spot", Op: filterNotContains},
		{Field: "meterName", Value: "Low Priority", Op: filterNotContains},
		{Field: "productName", Value: "Spot", Op: filterNotContains},
		{Field: "productName", Value: "Low Priority", Op: filterNotContains},
	}
}

// windowsExclusions returns the three not-contains terms that keep a Linux row
// from being selected from a SKU that also has a Windows meter.
func windowsExclusions() []priceFilter {
	return []priceFilter{
		{Field: "productName", Value: "Windows", Op: filterNotContains},
		{Field: "skuName", Value: "Windows", Op: filterNotContains},
		{Field: "meterName", Value: "Windows", Op: filterNotContains},
	}
}

// spotAndWindowsExclusions returns the constant exclusion predicates for a
// Linux row. They are all enforced by the fixture/live-row matcher; the live
// OData request sends as many as the Retail Prices API accepts (see
// serverFilter and its predicate limit).
func spotAndWindowsExclusions() []priceFilter {
	return append(spotExclusions(), windowsExclusions()...)
}

// serverFilter renders the OData $filter sent to the live API. It sends every
// equality, starts-with and contains predicate, then as many not-contains
// exclusions as fit within the API's observed total-predicate limit; the
// remaining not-contains terms are enforced only by the row matcher, never
// silently dropped from the lookup.
//
// The equality values are rendered according to their OData type: string
// fields are single-quoted, while the API's boolean and numeric fields must
// be unquoted (isPrimaryMeterRegion eq true, tierMinimumUnits eq 0). This
// matches the recorded live request exactly.
func serverFilter(filters []priceFilter) string {
	parts := make([]string, 0, len(filters))
	for _, f := range filters {
		if f.Op != filterEq {
			continue
		}
		parts = append(parts, eqPredicate(f.Field, f.Value))
	}
	for _, f := range filters {
		if f.Op != filterStartsWith && f.Op != filterContains && f.Op != filterNotContains {
			continue
		}
		if len(parts)+1 > maxRetailServerPredicates {
			continue
		}
		switch f.Op {
		case filterStartsWith:
			parts = append(parts, startsWithPredicate(f.Field, f.Value))
		case filterContains:
			parts = append(parts, containsPredicate(f.Field, f.Value))
		case filterNotContains:
			parts = append(parts, containsFalsePredicate(f.Field, f.Value))
		}
	}
	return strings.Join(parts, " and ")
}

func eqPredicate(field, value string) string {
	switch field {
	case "isPrimaryMeterRegion":
		// OData booleans are unquoted true/false.
		if value == "true" || value == "false" {
			return field + " eq " + value
		}
	case "tierMinimumUnits":
		// OData numeric literals are unquoted. The filter table keeps this
		// value as a string solely so the row matcher and serverFilter share
		// the same priceFilter shape; serverFilter emits the numeric form.
		if _, err := strconv.ParseFloat(value, 64); err == nil {
			return field + " eq " + value
		}
	}
	return field + " eq '" + escapeOData(value) + "'"
}

func startsWithPredicate(field, value string) string {
	return "startswith(" + field + ",'" + escapeOData(value) + "')"
}

func containsPredicate(field, value string) string {
	return "contains(" + field + ",'" + escapeOData(value) + "')"
}

func containsFalsePredicate(field, value string) string {
	return "contains(" + field + ",'" + escapeOData(value) + "') eq false"
}

// escapeOData escapes a single-quoted OData string literal.
func escapeOData(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// ─────────────────────────────────────────────────────────────────────────────
// Row matching and price normalization
// ─────────────────────────────────────────────────────────────────────────────

func matchFilters(row retailItem, filters []priceFilter) bool {
	for _, f := range filters {
		got := rowField(row, f.Field)
		switch f.Op {
		case filterEq:
			if got != f.Value {
				return false
			}
		case filterNotContains:
			if strings.Contains(got, f.Value) {
				return false
			}
		case filterStartsWith:
			if !strings.HasPrefix(got, f.Value) {
				return false
			}
		case filterContains:
			if !strings.Contains(got, f.Value) {
				return false
			}
		}
	}
	return true
}

func rowField(row retailItem, field string) string {
	switch field {
	case "serviceName":
		return row.ServiceName
	case "armRegionName":
		return row.ArmRegionName
	case "skuName":
		return row.SKUName
	case "type":
		return row.Type
	case "productName":
		return row.ProductName
	case "meterName":
		return row.MeterName
	case "isPrimaryMeterRegion":
		return strconv.FormatBool(row.IsPrimaryMeterRegion)
	case "tierMinimumUnits":
		return strconv.FormatFloat(row.TierMinimumUnits, 'f', -1, 64)
	case "armSkuName":
		return row.ArmSKUName
	default:
		return ""
	}
}

// selectPrice applies the full matcher to every row and normalizes the first
// (and only) matching row's unitOfMeasure to the canonical unit implied by
// kind. If zero or more than one distinct billable row can be priced, it
// returns false — the caller skips rather than guessing which of several
// plausible rows is the right one.
func selectPrice(rows []retailItem, filters []priceFilter, kind pricing.Kind) (resolvedSKU, bool) {
	var found *resolvedSKU
	for i := range rows {
		row := &rows[i]
		if !matchFilters(*row, filters) {
			continue
		}
		price, ok := normalizeUnitPrice(kind, row.UnitOfMeasure, row.UnitPrice)
		if !ok {
			continue
		}
		res := resolvedSKU{
			unitPrice:     price,
			region:        row.ArmRegionName,
			unitOfMeasure: row.UnitOfMeasure,
			sku:           row.SKUName,
		}
		if found != nil && (found.unitPrice != res.unitPrice || found.region != res.region || found.sku != res.sku) {
			// More than one distinct billable row survived the filter set.
			// Do not take the first one.
			return resolvedSKU{}, false
		}
		if found == nil {
			found = &res
		}
	}
	if found == nil {
		return resolvedSKU{}, false
	}
	return *found, true
}

// normalizeUnitPrice converts a Retail Prices row's unitOfMeasure into the
// canonical unit of the requested pricing.Kind:
//
//	KindStaticIP    -> per hour
//	KindManagedDisk -> per disk-month
//	KindVMInstance  -> per hour (the Azure VM rule multiplies by HoursPerMonth)
//
// Unknown or unhandled units return false; tellury never guesses a conversion
// factor.
func normalizeUnitPrice(kind pricing.Kind, unitOfMeasure string, unitPrice float64) (float64, bool) {
	unit := strings.TrimSpace(strings.ToLower(unitOfMeasure))
	switch kind {
	case pricing.KindStaticIP:
		switch unit {
		case "1 hour":
			return unitPrice, true
		case "1/month", "1 /month":
			return unitPrice / pricing.HoursPerMonth, true
		default:
			return 0, false
		}
	case pricing.KindManagedDisk:
		switch unit {
		case "1/month", "1 /month":
			return unitPrice, true
		case "1 hour":
			return unitPrice * pricing.HoursPerMonth, true
		default:
			return 0, false
		}
	case pricing.KindVMInstance:
		switch unit {
		case "1 hour":
			return unitPrice, true
		case "1/month", "1 /month":
			return unitPrice / pricing.HoursPerMonth, true
		default:
			return 0, false
		}
	default:
		return 0, false
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Retail Prices API response shape
// ─────────────────────────────────────────────────────────────────────────────

type retailPage struct {
	BillingCurrency string       `json:"BillingCurrency"`
	Items           []retailItem `json:"Items"`
	NextPageLink    *string      `json:"NextPageLink"`
	Count           int          `json:"Count"`
}

type retailItem struct {
	CurrencyCode         string  `json:"currencyCode"`
	TierMinimumUnits     float64 `json:"tierMinimumUnits"`
	RetailPrice          float64 `json:"retailPrice"`
	UnitPrice            float64 `json:"unitPrice"`
	ArmRegionName        string  `json:"armRegionName"`
	Location             string  `json:"location"`
	EffectiveStartDate   string  `json:"effectiveStartDate"`
	MeterID              string  `json:"meterId"`
	MeterName            string  `json:"meterName"`
	ProductID            string  `json:"productId"`
	SKUID                string  `json:"skuId"`
	ProductName          string  `json:"productName"`
	SKUName              string  `json:"skuName"`
	ServiceName          string  `json:"serviceName"`
	ServiceID            string  `json:"serviceId"`
	ServiceFamily        string  `json:"serviceFamily"`
	UnitOfMeasure        string  `json:"unitOfMeasure"`
	Type                 string  `json:"type"`
	IsPrimaryMeterRegion bool    `json:"isPrimaryMeterRegion"`
	ArmSKUName           string  `json:"armSkuName"`
}

type retailPricesError struct {
	statusCode int
	body       []byte
}

func (e *retailPricesError) Error() string {
	msg := strings.TrimSpace(string(e.body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return "azure: Retail Prices API returned HTTP " + strconv.Itoa(e.statusCode) + ": " + msg
}

func parseRetailPage(body []byte) (retailPage, error) {
	var page retailPage
	if err := json.Unmarshal(body, &page); err != nil {
		return retailPage{}, err
	}
	return page, nil
}
