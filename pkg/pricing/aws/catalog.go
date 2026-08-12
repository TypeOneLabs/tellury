// AWS live pricing catalogue over the AWS Price List API (pricing:GetProducts).
//
// GetProducts is the single AWS pricing source tellury uses, mirroring the
// Cloud Billing Catalog API on the GCP side: the response's PriceList elements
// are aws_v1 product documents, each carrying the product metadata (product
// family and attributes) and the OnDemand price dimensions. The catalogue is
// fetched lazily, once per pricer (i.e. once per scan), and indexed by
// (kind, sku, region) — never per resource.
//
// There is no embedded fallback table. A price that cannot be resolved from
// the live API makes the rule SKIP with SkipNoPrice rather than guessing at
// a dollar figure. When the TELLURY_PRICE_FIXTURE environment variable is set,
// the catalogue loads from that file instead of calling the API — a test-only
// hook, never a user-facing flag. The file format is the generic
// kind -> SKU -> region -> price table.
//
// SKU TOKEN DISCIPLINE. This file exists because two SKU-token defects
// shipped on the GCP side: a static IP and a snapshot each queried tokens the
// live catalogue did not use, so every lookup missed and fell back silently
// to an embedded table that was itself wrong, and both were caught only by
// comparing a figure against a real invoice. The AWS side is written to fail
// that way in a test instead:
//
//   - The EBS SKU token is the product's volumeApiName attribute VERBATIM —
//     the same string DescribeVolumes.VolumeType returns ("gp3", "io2",
//     "st1", ...). The price list ALSO carries a human-friendly "volumeType"
//     attribute ("General Purpose" for gp3); that is NOT the token. Indexing
//     "volumeType" would make every lookup miss exactly like the GCP defect.
//     The token is additionally whitelisted against the EC2 SDK's own
//     VolumeType enum, so a product that is not an EBS volume tellury models
//     is never indexed at all.
//   - The static IP SKU token is "AdditionalAddress" — the historical
//     operation attribute of the old AmazonEC2 "Elastic IP" family product.
//     The live API now surfaces this charge under serviceCode AmazonVPC with
//     NO productFamily, matched by usagetype suffix. The indexer maps it to
//     the canonical "AdditionalAddress" token the rule queries.
//
// Both tokens are pinned by pkg/pricing/aws/catalog_test.go against a
// recorded GetProducts response, so a rename by AWS fails the test before it
// can silently degrade every lookup to the embedded table.
//
//
// INSTANCE PRICING IS A SEPARATE, LAZY LOOKUP — NOT A PRELOAD. "Compute
// Instance" is the largest product family in the AWS price list: every
// instance type times every OS, tenancy, preinstalled software, license model
// and capacity status, in every region, paginated at MaxResults=100. Adding
// it to priceServiceFamilies would make every scan's catalogue load unusably
// slow (the unfiltered AmazonEC2 fetch was measured at 1m39s and still not
// finished). Instead, the InstancePrice method builds a targeted GetProducts
// call with full TERM_MATCH filters, runs it once per unique (region,
// instanceType, operatingSystem) tuple encountered during a scan, and caches
// the result in a map[instancePriceKey]float64 on the pricer. There is no
// productFamily filter — the eight attribute filters alone narrow the result
// set to exactly one product.
//
// THE FILTER SET IS THE WHOLE JOB. A GetProducts for one instance type in one
// region returns many products that differ only in attributes. Apply all of:
// termType=OnDemand, capacitystatus=Used, tenancy=Shared, preInstalledSw=NA,
// licenseModel="No License required", and operatingSystem matched to the
// instance's platform. Omitting capacitystatus alone can index a
// capacity-reservation rate as the On-Demand rate — a wrong number that looks
// entirely plausible.
//
// The Price List API is reachable only from us-east-1 and ap-south-1, so the
// client is pinned to us-east-1 regardless of where the scanned resources
// live; prices are looked up by the product's own region attribute, not by
// the client's region.
package aws

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	awspricing "github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// skuKey is the catalogue index key: one pricing dimension of one SKU in one
// region.
type skuKey struct {
	kind   pricing.Kind
	sku    string
	region string
}

type resolvedSKU struct {
	unitPrice float64
	region    string
}

// catalogueEntry is one price the indexDoc matcher derives from one aws_v1
// product document: a (kind, sku, region) -> price mapping.
type catalogueEntry struct {
	kind   pricing.Kind
	sku    string
	region string
	price  float64
}

// instancePriceKey is the lazy-instance-price cache key: one On-Demand
// instance rate for one (region, instanceType, operatingSystem) tuple.
type instancePriceKey struct {
	region          string
	instanceType    string
	operatingSystem string
}

// CatalogPricer implements pricing.Pricer over the AWS Price List API.
// The live API is the ONLY source. When the API is unreachable or has no
// matching product, the pricer returns ErrNoPrice and the rule skips the
// resource — it never guesses a dollar figure.
//
// When TELLURY_PRICE_FIXTURE is set, the catalogue loads from that file
// instead of calling the API. This is a test-only hook, not a user-facing
// flag; it exists so golden fixtures can price without network access.
//
// Every price is traceable: LastLookup exposes the pricing.Provenance (source
// + SKU + region) of the most recent answer for a key, which is exactly what
// a rule's PriceEvidence helper turns into a Finding's evidence entry.
type CatalogPricer struct {
	log    *slog.Logger
	client *awspricing.Client

	// ctx is the scan's context, captured at construction. The catalogue
	// load — the only RPC path in this pricer — runs against it, so a hanging
	// Price List API honours the CLI's --timeout deadline instead of stalling
	// the whole scan past it uncancellably.
	ctx context.Context

	once      sync.Once
	loadErr   error
	skusByKey map[skuKey]resolvedSKU // (kind, sku, region) -> price, catalogue cache

	// instancePrices is the lazy instance-price cache, keyed by (region,
	// instanceType, operatingSystem). It is populated on first access per key,
	// never during construction, because "Compute Instance" is the largest
	// product family in the AWS price list and must not be preloaded.
	instancePrices map[instancePriceKey]float64

	// instancePricesLoaded is true when instancePrices has been populated from
	// a fixture (either via TELLURY_PRICE_FIXTURE's vm_instance entries, or via
	// direct cache population in tests). When true, InstancePrice returns
	// ErrNoPrice on cache miss instead of calling the live API.
	instancePricesLoaded bool

	mu     sync.Mutex
	last   map[string]pricing.Provenance // provKey(kind,sku,region) -> last answer
	loaded bool

	catalogueProgress func(done, total int, final bool)
}

var (
	_ pricing.Pricer           = (*CatalogPricer)(nil)
	_ pricing.ProvenancePricer = (*CatalogPricer)(nil)
	_ pricing.CurrencyReporter = (*CatalogPricer)(nil)
)

// NewCatalogPricer builds a pricer over the live AWS Price List API. It
// performs no RPCs itself: the catalogue is fetched lazily, once, on first
// UnitPrice call (see loadCatalogue).
//
// When the environment variable TELLURY_PRICE_FIXTURE is set, it points at a
// JSON price file in the generic kind->SKU->region->price table format, and
// the catalogue loads from that file instead of calling the API. This is a
// test-only hook, not a user-facing flag: it exists so golden fixtures can
// price without network access or credentials.
//
// cfg is the scan's AWS config (the default credential chain); the pricing
// client is pinned to us-east-1, the only region (with ap-south-1) that
// serves GetProducts.
//
// ctx is retained on the pricer so the eventual lazy catalogue load runs
// under the scan's context and deadline.
func NewCatalogPricer(ctx context.Context, log *slog.Logger, cfg aws.Config) (*CatalogPricer, error) {
	if log == nil {
		log = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client := awspricing.NewFromConfig(cfg, func(o *awspricing.Options) { o.Region = "us-east-1" })
	return &CatalogPricer{
		log:            log,
		ctx:            ctx,
		client:         client,
		skusByKey:      map[skuKey]resolvedSKU{},
		instancePrices: map[instancePriceKey]float64{},
		last:           map[string]pricing.Provenance{},
	}, nil
}

// CurrencyInfo implements pricing.CurrencyReporter. The AWS Price List API
// prices every dimension in USD and offers no currency choice, so a scan's
// figures are always in USD: Effective is "USD" and a non-USD request is
// never silently answered in a different currency because no other currency
// was ever possible.
func (c *CatalogPricer) CurrencyInfo() pricing.CurrencyInfo {
	return pricing.CurrencyInfo{Requested: "", Effective: "USD"}
}

// SetCatalogueProgress registers a callback the pricer invokes as its live
// catalogue loads: once at the start with (0, 1, false), once with (1, 1,
// true) on completion — whether the load succeeded or not. The callback is
// invoked from whichever goroutine triggers the lazy load (a rule worker) and
// must not block for long; it must be nil or already-settled when UnitPrice
// first runs, because the catalogue load is cached for the pricer's lifetime.
// Optional.
func (c *CatalogPricer) SetCatalogueProgress(f func(done, total int, final bool)) {
	c.mu.Lock()
	c.catalogueProgress = f
	c.mu.Unlock()
}

func (c *CatalogPricer) reportCatalogueProgress(done, total int, final bool) {
	c.mu.Lock()
	f := c.catalogueProgress
	c.mu.Unlock()
	if f != nil {
		f(done, total, final)
	}
}

// UnitPrice implements pricing.Pricer with ONE source: the live Price List
// API (or, when TELLURY_PRICE_FIXTURE is set, its recorded replacement).
// There is no fallback. A price that cannot be resolved returns ErrNoPrice,
// and the rule skips rather than guessing.
func (c *CatalogPricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	// Live API (or price fixture), cached for the scan's lifetime.
	if v, res, err := c.liveUnitPrice(kind, sku, region); err == nil {
		c.record(kind, sku, region, pricing.Provenance{Source: pricing.SourceLiveAPI, SKU: sku, Region: res.region})
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

// instancePriceFixturePath returns the value of TELLURY_INSTANCE_PRICE_FIXTURE,
// or "" when unset. This is a separate test-only hook for instance product
// GetProducts responses. When set, InstancePrice loads from this file instead
// of calling the live API.
func instancePriceFixturePath() string {
	return os.Getenv("TELLURY_INSTANCE_PRICE_FIXTURE")
}

// liveUnitPrice resolves (kind, sku, region) against the cached catalogue,
// loading it on first use. The load runs against c.ctx (the scan's
// deadline-bounded context), so a hanging Price List API fails cleanly at
// --timeout. When TELLURY_PRICE_FIXTURE is set, the catalogue loads from that
// file instead of calling the API.
func (c *CatalogPricer) liveUnitPrice(kind pricing.Kind, sku, region string) (float64, resolvedSKU, error) {
	c.once.Do(func() { c.loadErr = c.loadCatalogue(c.ctx) })
	if c.loadErr != nil {
		return 0, resolvedSKU{}, c.loadErr
	}

	// Exact region, then "default". AWS has no multi-region aggregate tier,
	// so there is deliberately no region-prefix fallback here (the GCP
	// catalogue's "us" -> "us-east-1" style prefix is a GCP concept).
	for _, candidate := range []string{region, "default"} {
		if res, ok := c.skusByKey[skuKey{kind: kind, sku: sku, region: candidate}]; ok {
			return res.unitPrice, res, nil
		}
	}
	return 0, resolvedSKU{}, pricing.ErrNoPrice
}

// loadCatalogue fetches prices exactly once and indexes every product this
// pricer models into skusByKey. This is the only place the Price List API is
// called for preloaded families — everything else reads the cache built here.
//
// When TELLURY_PRICE_FIXTURE is set, the catalogue loads from that file
// (a generic kind->SKU->region->price table) instead of calling the API.
//
// It fetches across two service codes:
//   - AmazonEC2: EBS Storage (capacity), System Operation (IOPS), and
//     Provisioned Throughput, each filtered by productFamily.
//   - AmazonVPC: unfiltered (the address product has NO productFamily), with
//     usagetype-based matching inside indexDoc.
//
// FILTERED BY PRODUCT FAMILY, and verified. Fetching the whole AmazonEC2 list
// is correct but unusable: it is hundreds of thousands of products — every
// instance type, every region, every data-transfer rate — paged 100 at a time
// to find a handful of EBS and address rates. Measured against a live account
// it spent 1m39s and had still not finished, making the scan's pricing step
// longer than everything else combined.
func (c *CatalogPricer) loadCatalogue(ctx context.Context) error {
	c.reportCatalogueProgress(0, 1, false)

	// TELLURY_PRICE_FIXTURE: load from file, no API call.
	if path := priceFixturePath(); path != "" {
		n, err := loadPriceFixture(path, c.skusByKey, c.instancePrices)
		if err != nil {
			c.reportCatalogueProgress(0, 1, true)
			c.log.Warn("aws: price fixture load failed; no prices will resolve", "path", path, "err", err)
			return err
		}
		c.mu.Lock()
		c.loaded = n > 0
		c.instancePricesLoaded = true
		c.mu.Unlock()
		c.reportCatalogueProgress(1, 1, true)
		c.log.Debug("aws price catalogue loaded from fixture", "entries_indexed", n, "path", path)
		return nil
	}

	n, err := c.loadServices(ctx, priceServiceFamilies)
	if err != nil {
		c.reportCatalogueProgress(0, 1, true)
		c.log.Warn("aws: GetProducts unavailable; resources requiring prices will skip", "err", err)
		return err
	}
	if n == 0 {
		// Every family came back empty. Either the product-family names have
		// drifted or the filter is being rejected silently; either way the
		// unfiltered fetch is the honest answer rather than an empty table.
		c.log.Warn("aws: filtered price fetch returned nothing; retrying unfiltered")
		n, err = c.loadOne(ctx, "AmazonEC2", "")
		if err != nil {
			c.reportCatalogueProgress(0, 1, true)
			return err
		}
	}

	c.mu.Lock()
	c.loaded = n > 0
	c.mu.Unlock()
	c.reportCatalogueProgress(1, 1, true)
	c.log.Debug("aws price catalogue loaded", "entries_indexed", n)
	return nil
}

// loadPriceFixture loads a generic kind->SKU->region->price table from a
// JSON file and indexes it into skusByKey. If the table contains vm_instance
// entries, those are also loaded into instancePrices for lazy instance-price
// lookups.
func loadPriceFixture(path string, skusByKey map[skuKey]resolvedSKU, instancePrices map[instancePriceKey]float64) (int, error) {
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
					unitPrice: price,
					region:    region,
				}
				n++
			}
		}
	}

	// Also load instance prices from the vm_instance kind in the fixture.
	// SKU convention: "instanceType/operatingSystem" (e.g. "t3.medium/Linux").
	if vmSkus, ok := t[pricing.KindVMInstance]; ok {
		for sku, regions := range vmSkus {
			// Parse instanceType and operatingSystem from the compound SKU.
			instanceType, operatingSystem := parseInstancePriceSKU(sku)
			for region, price := range regions {
				instancePrices[instancePriceKey{
					region:          region,
					instanceType:    instanceType,
					operatingSystem: operatingSystem,
				}] = price
				n++
			}
		}
	}

	return n, nil
}

// parseInstancePriceSKU splits a compound SKU of the form
// "instanceType/operatingSystem" into its parts. When there is no "/",
// operatingSystem defaults to "Linux".
func parseInstancePriceSKU(sku string) (instanceType, operatingSystem string) {
	if idx := strings.LastIndex(sku, "/"); idx >= 0 {
		return sku[:idx], sku[idx+1:]
	}
	return sku, "Linux"
}

// priceServiceFamilies maps each AWS service code to the GetProducts
// productFamily values holding every rate tellury models:
//
//   - AmazonEC2 "Storage"        → disk capacity (GB-Mo / GB-month)
//   - AmazonEC2 "System Operation" → disk IOPS (IOPS-Mo)
//   - AmazonEC2 "Provisioned Throughput" → disk throughput (GiBps-mo)
//   - AmazonVPC nil              → static IP (no productFamily; matched by
//     usagetype suffix inside indexDoc)
//
// All were read from a real GetProducts response, not guessed — the recorded
// fixture in testdata/getproducts-recorded.json contains all four.
//
// "Compute Instance" is deliberately absent from this map. It is the largest
// product family in the AWS price list and adding it here would make every
// scan's catalogue load unusably slow. Instance pricing is handled by the
// separate, lazy InstancePrice method instead.
var priceServiceFamilies = map[string][]string{
	"AmazonEC2": {"Storage", "System Operation", "Provisioned Throughput"},
	"AmazonVPC": nil,
}

// loadServices fetches and indexes the catalogue across multiple service codes.
func (c *CatalogPricer) loadServices(ctx context.Context, services map[string][]string) (int, error) {
	total := 0
	for svc, families := range services {
		if len(families) == 0 {
			n, err := c.loadOne(ctx, svc, "")
			if err != nil {
				return total, err
			}
			total += n
			continue
		}
		for _, f := range families {
			n, err := c.loadOne(ctx, svc, f)
			if err != nil {
				return total, err
			}
			total += n
		}
	}
	return total, nil
}

// loadOne runs a single GetProducts pagination for one service code,
// optionally filtered to one product family, and indexes what it returns.
func (c *CatalogPricer) loadOne(ctx context.Context, serviceCode, family string) (int, error) {
	in := &awspricing.GetProductsInput{
		ServiceCode:   aws.String(serviceCode),
		FormatVersion: aws.String("aws_v1"),
		MaxResults:    aws.Int32(100),
	}
	if family != "" {
		in.Filters = []pricingtypes.Filter{{
			Type:  pricingtypes.FilterTypeTermMatch,
			Field: aws.String("productFamily"),
			Value: aws.String(family),
		}}
	}
	paginator := awspricing.NewGetProductsPaginator(c.client, in)
	n := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return n, err
		}
		for _, raw := range page.PriceList {
			doc, err := parsePriceListDoc(raw)
			if err != nil {
				// One malformed document must not fail the load; it is
				// simply not indexed.
				continue
			}
			for _, e := range indexDoc(doc) {
				c.skusByKey[skuKey{kind: e.kind, sku: e.sku, region: e.region}] = resolvedSKU{
					unitPrice: e.price,
					region:    e.region,
				}
				n++
			}
		}
	}
	return n, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Instance pricing — lazy, targeted GetProducts, NOT a family preload
// ─────────────────────────────────────────────────────────────────────────────

// OSForPlatform maps an EC2 instance's Platform attribute (from
// DescribeInstances) to the operatingSystem value the AWS Price List API's
// GetProducts filter expects.
//
//	"" or "linux/unix"  → "Linux"
//	"windows"           → "Windows"
//	"rhel"              → "RHEL"
//	"suse"              → "SUSE"
//
// An unrecognised platform is returned as-is (the API filter will match
// exactly that string, or miss if it does not exist in the price list).
func OSForPlatform(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "", "linux/unix":
		return "Linux"
	case "windows":
		return "Windows"
	case "rhel":
		return "RHEL"
	case "suse":
		return "SUSE"
	default:
		return platform
	}
}

// InstancePrice returns the On-Demand hourly rate for an EC2 instance type in
// a region with the given operating system. Calling it with operatingSystem
// "Linux" for a t3.medium in us-east-1 returns $0.0416 (the published
// On-Demand Linux price).
//
// It makes a targeted GetProducts call with the full TERM_MATCH filter set:
//
//	ServiceCode:   "AmazonEC2"
//	FormatVersion: "aws_v1"
//	Filters:
//	  1. Type: TERM_MATCH, Field: "instanceType",     Value: <instanceType>
//	  2. Type: TERM_MATCH, Field: "regionCode",       Value: <region>
//	  3. Type: TERM_MATCH, Field: "operatingSystem",  Value: <operatingSystem>
//	  4. Type: TERM_MATCH, Field: "tenancy",          Value: "Shared"
//	  5. Type: TERM_MATCH, Field: "capacitystatus",   Value: "Used"
//	  6. Type: TERM_MATCH, Field: "preInstalledSw",   Value: "NA"
//	  7. Type: TERM_MATCH, Field: "licenseModel",     Value: "No License required"
//	  8. Type: TERM_MATCH, Field: "termType",         Value: "OnDemand"
//
// The price is extracted from the OnDemand price dimension whose unit is
// "Hrs". The result is cached in the pricer's instancePrices map for the
// scan's duration; subsequent calls for the same key return the cached value
// without an API call.
//
// When the TELLURY_INSTANCE_PRICE_FIXTURE environment variable is set, the
// method loads raw GetProducts responses from that file instead of calling
// the live API — a test-only hook, never a user-facing flag.
//
// When the pricer's instance-price cache was pre-populated from a fixture
// (via TELLURY_PRICE_FIXTURE's vm_instance entries, or direct population in
// tests), cache misses return ErrNoPrice rather than calling the live API.
//
// An unresolvable price returns pricing.ErrNoPrice. The rule skips the
// instance with SkipNoPrice — there is no fallback table and none is added.
func (c *CatalogPricer) InstancePrice(ctx context.Context, region, instanceType, operatingSystem string) (float64, error) {
	key := instancePriceKey{
		region:          region,
		instanceType:    instanceType,
		operatingSystem: operatingSystem,
	}

	c.mu.Lock()
	if price, ok := c.instancePrices[key]; ok {
		c.mu.Unlock()
		return price, nil
	}
	fixturePopulated := c.instancePricesLoaded
	c.mu.Unlock()

	// When the instance price cache was loaded from a fixture, a cache miss
	// is definitive: the fixture does not contain this key, and we must not
	// fall through to the live API (there may be no credentials).
	if fixturePopulated {
		return 0, pricing.ErrNoPrice
	}

	price, err := c.fetchInstancePrice(ctx, region, instanceType, operatingSystem)
	if err != nil {
		return 0, err
	}

	c.mu.Lock()
	c.instancePrices[key] = price
	c.mu.Unlock()

	return price, nil
}

// fetchInstancePrice runs the targeted GetProducts call for one (region,
// instanceType, operatingSystem) tuple. It is the live-API path of
// InstancePrice; the caller handles caching.
func (c *CatalogPricer) fetchInstancePrice(ctx context.Context, region, instanceType, operatingSystem string) (float64, error) {
	// TELLURY_INSTANCE_PRICE_FIXTURE: load from recorded fixture file.
	if path := instancePriceFixturePath(); path != "" {
		return c.fetchInstancePriceFromFixture(path, region, instanceType, operatingSystem)
	}

	// ── Live API path ──
	// Build the full filter set. Every filter is TERM_MATCH — the only filter
	// type GetProducts accepts. Omitting any one of the constant filters
	// (tenancy, capacitystatus, preInstalledSw, licenseModel, termType) can
	// silently return a capacity-reservation, dedicated-host, or
	// preinstalled-software variant whose price looks plausible but is wrong.
	in := &awspricing.GetProductsInput{
		ServiceCode:   aws.String("AmazonEC2"),
		FormatVersion: aws.String("aws_v1"),
		MaxResults:    aws.Int32(100),
		Filters: []pricingtypes.Filter{
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("instanceType"), Value: aws.String(instanceType)},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("regionCode"), Value: aws.String(region)},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("operatingSystem"), Value: aws.String(operatingSystem)},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("tenancy"), Value: aws.String("Shared")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("capacitystatus"), Value: aws.String("Used")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("preInstalledSw"), Value: aws.String("NA")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("licenseModel"), Value: aws.String("No License required")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("termType"), Value: aws.String("OnDemand")},
		},
	}

	paginator := awspricing.NewGetProductsPaginator(c.client, in)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return 0, err
		}
		for _, raw := range page.PriceList {
			doc, err := parsePriceListDoc(raw)
			if err != nil {
				continue
			}
			// Extract the OnDemand hourly rate. A Compute Instance product
			// has exactly one OnDemand price dimension with unit "Hrs".
			for _, dim := range priceDimensions(doc) {
				if dim.unit == "Hrs" {
					return dim.price, nil
				}
			}
		}
	}

	return 0, pricing.ErrNoPrice
}

// fetchInstancePriceFromFixture loads raw GetProducts responses from a JSON
// file and resolves the hourly rate for one (region, instanceType,
// operatingSystem) tuple. The file format is the same as
// getproducts-recorded.json: a JSON array of raw aws_v1 product documents.
func (c *CatalogPricer) fetchInstancePriceFromFixture(path, region, instanceType, operatingSystem string) (float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var stored []json.RawMessage
	if err := json.Unmarshal(data, &stored); err != nil {
		return 0, err
	}
	for _, raw := range stored {
		doc, err := parsePriceListDoc(string(raw))
		if err != nil {
			continue
		}
		// Match by instance type, region, and operating system attributes.
		attrs := doc.Product.Attributes
		if attrs["instanceType"] != instanceType {
			continue
		}
		if attrs["regionCode"] != region {
			continue
		}
		if attrs["operatingSystem"] != operatingSystem {
			continue
		}
		// Extract the OnDemand hourly rate.
		for _, dim := range priceDimensions(doc) {
			if dim.unit == "Hrs" {
				return dim.price, nil
			}
		}
	}
	return 0, pricing.ErrNoPrice
}

// ─────────────────────────────────────────────────────────────────────────────
// aws_v1 product documents and the pure indexer
// ─────────────────────────────────────────────────────────────────────────────

// priceListDoc is the aws_v1 product document GetProducts returns in each
// PriceList element. The shape is fixed by the Price List API's aws_v1
// format: a "product" block (productFamily, attributes, sku) plus a "terms"
// block whose OnDemand offer terms carry priceDimensions with a "unit" and a
// "pricePerUnit" map keyed by currency code. The top-level serviceCode field
// carries "AmazonEC2" or "AmazonVPC".
type priceListDoc struct {
	ServiceCode string `json:"serviceCode"`
	Product     struct {
		ProductFamily string            `json:"productFamily"`
		Attributes    map[string]string `json:"attributes"`
		SKU           string            `json:"sku"`
	} `json:"product"`
	Terms struct {
		OnDemand map[string]struct {
			PriceDimensions map[string]struct {
				Unit         string            `json:"unit"`
				PricePerUnit map[string]string `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

// awsVolumeTypes is the set of volume-type tokens DescribeVolumes.VolumeType
// can return, derived at init from the EC2 SDK's OWN enum
// (ec2types.VolumeType.Values()) — never a hand-copied list. It is the
// whitelist that decides whether a price-list product is an EBS volume tellury
// models: a product whose volumeApiName attribute is not one of these strings
// is not an EBS volume this catalogue prices.
var awsVolumeTypes = func() map[string]bool {
	m := make(map[string]bool, 8)
	for _, vt := range ec2types.VolumeType("").Values() {
		m[string(vt)] = true
	}
	return m
}()

// dimension is one OnDemand price dimension that carried a parseable USD
// price.
type dimension struct {
	unit  string
	price float64
}

// priceDimensions collects every OnDemand price dimension of a product that
// carries a USD price. A dimension without a USD price is skipped, never
// assumed free.
func priceDimensions(doc *priceListDoc) []dimension {
	var out []dimension
	for _, offer := range doc.Terms.OnDemand {
		for _, dim := range offer.PriceDimensions {
			price, ok := usdPrice(dim.PricePerUnit)
			if !ok {
				continue
			}
			out = append(out, dimension{unit: dim.Unit, price: price})
		}
	}
	return out
}

// usdPrice extracts the USD unit price from a pricePerUnit map. The Price
// List API prices every dimension in USD, so "USD" is the only currency read.
func usdPrice(perUnit map[string]string) (float64, bool) {
	s, ok := perUnit["USD"]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parsePriceListDoc decodes one raw aws_v1 product document (a single
// PriceList element) into its typed form. It is pure, so the SKU-pinning
// tests drive the exact path the live load uses.
func parsePriceListDoc(raw string) (*priceListDoc, error) {
	var doc priceListDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// indexDoc derives the (kind, sku, region, price) entries one aws_v1 product
// document contributes to the catalogue. It is PURE — no I/O, no state — so
// the SKU-pinning tests can drive the exact path the live load uses.
//
// EBS capacity: a product with productFamily "Storage" whose volumeApiName
// attribute is a real DescribeVolumes.VolumeType value. The SKU token is the
// volumeApiName attribute VERBATIM. Each price dimension is indexed under the
// pricing.Kind its unit declares.
//
// EBS IOPS: a product with productFamily "System Operation" whose
// volumeApiName is a real VolumeType and whose group is "EBS IOPS" (the base
// tier — tiered rates are skipped). Indexed under KindDiskIOPS.
//
// EBS throughput: a product with productFamily "Provisioned Throughput" whose
// volumeApiName is a real VolumeType. The live API prices throughput in
// GiBps-month ($40.96/GiBps-mo); tellury works in MiB/s, so the price is
// divided by 1024 (40.96/1024 = 0.04). Indexed under KindDiskThroughput.
//
// Static IP: a product with serviceCode "AmazonVPC", NO productFamily, and a
// usagetype ending in "PublicIPv4:InUseAddress". The SKU token is the
// canonical "AdditionalAddress" string the unassociated_eip rule queries.
func indexDoc(doc *priceListDoc) []catalogueEntry {
	region, ok := regionOfProduct(doc.Product.Attributes)
	if !ok {
		return nil
	}

	// Static IP: AmazonVPC product with no productFamily, matched by
	// usagetype suffix. This product has no operation attribute and cannot
	// be found by filtering on productFamily, which is why it was never
	// fetched before. The SKU is hard-coded to "AdditionalAddress" to match
	// the rule's EIPSKU constant.
	if doc.ServiceCode == "AmazonVPC" && doc.Product.ProductFamily == "" {
		usagetype := doc.Product.Attributes["usagetype"]
		if strings.HasSuffix(usagetype, "PublicIPv4:InUseAddress") {
			var out []catalogueEntry
			for _, dim := range priceDimensions(doc) {
				if dim.unit != "Hrs" {
					continue
				}
				out = append(out, catalogueEntry{
					kind:   pricing.KindStaticIP,
					sku:    "AdditionalAddress",
					region: region,
					price:  dim.price,
				})
			}
			return out
		}
		return nil
	}

	switch doc.Product.ProductFamily {
	case "Storage":
		apiName := doc.Product.Attributes["volumeApiName"]
		if !awsVolumeTypes[apiName] {
			return nil
		}
		var out []catalogueEntry
		for _, dim := range priceDimensions(doc) {
			kind := kindForUnit(dim.unit)
			if kind == "" {
				continue
			}
			out = append(out, catalogueEntry{kind: kind, sku: apiName, region: region, price: dim.price})
		}
		return out
	case "System Operation":
		apiName := doc.Product.Attributes["volumeApiName"]
		if !awsVolumeTypes[apiName] {
			return nil
		}
		// Only the base IOPS tier (group "EBS IOPS"), not tiered rates.
		if doc.Product.Attributes["group"] != "EBS IOPS" {
			return nil
		}
		var out []catalogueEntry
		for _, dim := range priceDimensions(doc) {
			if dim.unit != "IOPS-Mo" {
				continue
			}
			out = append(out, catalogueEntry{
				kind:   pricing.KindDiskIOPS,
				sku:    apiName,
				region: region,
				price:  dim.price,
			})
		}
		return out
	case "Provisioned Throughput":
		apiName := doc.Product.Attributes["volumeApiName"]
		if !awsVolumeTypes[apiName] {
			return nil
		}
		var out []catalogueEntry
		for _, dim := range priceDimensions(doc) {
			if dim.unit != "GiBps-mo" {
				continue
			}
			// The live API prices throughput per GiBps-month; tellury works
			// in MiB/s. 1 GiBps = 1024 MiBps, so divide by 1024. Not doing
			// this makes every throughput charge 1024× too large.
			// $40.96 / 1024 = $0.04, which is what the embedded table holds.
			miBpsPrice := dim.price / 1024.0
			// Round to 6 decimal places to avoid floating-point drift:
			// 45.056 / 1024 = 0.044 but floating-point can give 0.044000000000000004.
			miBpsPrice = math.Round(miBpsPrice*1e6) / 1e6
			out = append(out, catalogueEntry{
				kind:   pricing.KindDiskThroughput,
				sku:    apiName,
				region: region,
				price:  miBpsPrice,
			})
		}
		return out
	default:
		return nil
	}
}

// kindForUnit maps a price dimension's unit string to the pricing.Kind tellury
// prices with it. The units are the documented aws_v1 unit strings for EBS
// dimensions. An unknown unit yields "" (not indexed): the rule then skips
// for want of a price rather than pricing the dimension under a guessed kind.
//
// The Price List API is not consistent in its unit spelling: most products use
// "GB-Mo" but some (io2) use "GB-month". Both are capacity.
func kindForUnit(unit string) pricing.Kind {
	switch unit {
	case "GB-Mo", "GB-month":
		return pricing.KindDiskCapacity
	case "IOPS-Mo":
		return pricing.KindDiskIOPS
	case "MBps-Mo":
		return pricing.KindDiskThroughput
	case "GiBps-mo":
		return pricing.KindDiskThroughput
	default:
		return ""
	}
}

// regionOfProduct resolves a product's region, preferring the regionCode
// attribute the API already supplies over the display-name table.
//
// The display-name map alone silently dropped most of the catalogue. Live
// products for Ireland carry location "EU (Ireland)" while the table held
// "Europe (Ireland)", so every Irish rate was skipped and every lookup fell
// through to the embedded fallback — a scan reported prices that looked
// entirely reasonable and were not the live ones. A hand-maintained mapping
// that discards what it does not recognise is the wrong shape when the API
// hands you the canonical value in the next field along.
//
// The table stays as a fallback for older payloads that predate regionCode.
func regionOfProduct(attrs map[string]string) (string, bool) {
	if code := strings.TrimSpace(attrs["regionCode"]); code != "" {
		return code, true
	}
	return regionCodeOf(attrs["location"])
}

// regionCodeOf maps the Price List API's human-readable Location attribute
// (e.g. "US East (N. Virginia)") to the region code pricing lookups use
// ("us-east-1"). The display names are fixed by AWS in the price list and
// change only with a region rename, which the live pinned test is meant to
// catch. An unknown display name is skipped, never guessed at.
func regionCodeOf(location string) (string, bool) {
	code, ok := regionCodeByLocation[location]
	return code, ok
}

// regionCodeByLocation is the Location display name -> region code table for
// the AWS commercial and GovCloud regions tellury indexes.
var regionCodeByLocation = map[string]string{
	"US East (N. Virginia)":     "us-east-1",
	"US East (Ohio)":            "us-east-2",
	"US West (N. California)":   "us-west-1",
	"US West (Oregon)":          "us-west-2",
	"Africa (Cape Town)":        "af-south-1",
	"Asia Pacific (Hong Kong)":  "ap-east-1",
	"Asia Pacific (Hyderabad)":  "ap-south-2",
	"Asia Pacific (Jakarta)":    "ap-southeast-3",
	"Asia Pacific (Melbourne)":  "ap-southeast-4",
	"Asia Pacific (Mumbai)":     "ap-south-1",
	"Asia Pacific (Osaka)":      "ap-northeast-3",
	"Asia Pacific (Seoul)":      "ap-northeast-2",
	"Asia Pacific (Singapore)":  "ap-southeast-1",
	"Asia Pacific (Sydney)":     "ap-southeast-2",
	"Asia Pacific (Tokyo)":      "ap-northeast-1",
	"Canada (Central)":          "ca-central-1",
	"Canada West (Calgary)":     "ca-west-1",
	"Europe (Frankfurt)":        "eu-central-1",
	"Europe (Ireland)":          "eu-west-1",
	"Europe (London)":           "eu-west-2",
	"Europe (Milan)":            "eu-south-1",
	"Europe (Paris)":            "eu-west-3",
	"Europe (Spain)":            "eu-south-2",
	"Europe (Stockholm)":        "eu-north-1",
	"Europe (Zurich)":           "eu-central-2",
	"Israel (Tel Aviv)":         "il-central-1",
	"Middle East (Bahrain)":     "me-south-1",
	"Middle East (UAE)":         "me-central-1",
	"South America (São Paulo)": "sa-east-1",
	"AWS GovCloud (US-East)":    "us-gov-east-1",
	"AWS GovCloud (US-West)":    "us-gov-west-1",
	// The recorded fixture uses "EU (Ireland)" (not "Europe (Ireland)").
	"EU (Ireland)": "eu-west-1",
}
