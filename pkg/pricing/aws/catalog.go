// AWS live pricing catalogue over the AWS Price List API (pricing:GetProducts).
//
// GetProducts is the single AWS pricing source tellury uses, mirroring the
// Cloud Billing Catalog API on the GCP side: the response's PriceList elements
// are aws_v1 product documents, each carrying the product metadata (product
// family and attributes) and the OnDemand price dimensions. The catalogue is
// fetched lazily, once per pricer (i.e. once per scan), and indexed by
// (kind, sku, region) — never per resource.
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
//   - The Elastic IP SKU token is the product's operation attribute VERBATIM
//     ("AdditionalAddress" for the hourly charge an unassociated address
//     accrues). The rule queries that exact constant and the pinning test
//     asserts catalogue-token == rule-token.
//
// Both tokens are pinned by pkg/pricing/aws/catalog_test.go against a
// recorded GetProducts response, so a rename by AWS fails the test before it
// can silently degrade every lookup to the embedded table.
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

// CatalogPricer implements pricing.Pricer over the AWS Price List API, with
// the embedded StaticPricer as its fallback. Precedence (highest first):
//
//  1. --price-file override (pricing.SourceOverride)
//  2. live GetProducts catalogue (pricing.SourceLiveAPI), cached for the
//     lifetime of this CatalogPricer, i.e. for the duration of one scan
//  3. embedded static price table (pricing.SourceEmbedded)
//
// Every price is traceable: LastLookup exposes the pricing.Provenance (source
// + SKU + region) of the most recent answer for a key, which is exactly what
// a rule's PriceEvidence helper turns into a Finding's evidence entry.
type CatalogPricer struct {
	log    *slog.Logger
	client *awspricing.Client
	static *StaticPricer

	// staticBaseline is the pristine, never-overlaid embedded StaticPricer
	// built once at construction. overrideValue resolves a key against it to
	// decide whether the (overlaid) `static` table genuinely differs from
	// what the embedded table would have answered — i.e. whether --price-file
	// actually set it.
	staticBaseline *StaticPricer

	// ctx is the scan's context, captured at construction. The catalogue
	// load — the only RPC path in this pricer — runs against it, so a hanging
	// Price List API honours the CLI's --timeout deadline instead of stalling
	// the whole scan past it uncancellably.
	ctx context.Context

	once      sync.Once
	loadErr   error
	skusByKey map[skuKey]resolvedSKU // (kind, sku, region) -> price, catalogue cache

	mu     sync.Mutex
	last   map[string]pricing.Provenance // provKey(kind,sku,region) -> last answer
	loaded bool

	catalogueProgress func(done, total int, final bool)
}

var (
	_ pricing.Pricer           = (*CatalogPricer)(nil)
	_ pricing.ProvenancePricer = (*CatalogPricer)(nil)
	_ pricing.OverlayLoader    = (*CatalogPricer)(nil)
	_ pricing.CurrencyReporter = (*CatalogPricer)(nil)
)

// NewCatalogPricer builds a pricer that prefers the live AWS Price List API
// and falls back to the embedded static table. It performs no RPCs itself:
// the catalogue is fetched lazily, once, on first UnitPrice call (see
// loadCatalogue), so an offline or credential-less scan never pays for the
// call it cannot make.
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
	static, err := NewStaticPricer()
	if err != nil {
		return nil, err
	}
	// Build the pristine baseline once at construction and reuse it for the
	// lifetime of the pricer, exactly like the GCP catalogue pricer.
	staticBaseline, err := NewStaticPricer()
	if err != nil {
		return nil, err
	}
	client := awspricing.NewFromConfig(cfg, func(o *awspricing.Options) { o.Region = "us-east-1" })
	return &CatalogPricer{
		log:            log,
		ctx:            ctx,
		client:         client,
		static:         static,
		staticBaseline: staticBaseline,
		skusByKey:      map[skuKey]resolvedSKU{},
		last:           map[string]pricing.Provenance{},
	}, nil
}

// OverlayFile implements pricing.OverlayLoader: --price-file always applies
// on top of the embedded fallback table. It does not touch the live
// catalogue cache — the override is still consulted first, on every lookup —
// so this is sufficient to give the override the highest precedence.
func (c *CatalogPricer) OverlayFile(path string) error {
	return c.static.OverlayFile(path)
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

// UnitPrice implements pricing.Pricer with the documented precedence:
// --price-file override > live Price List API > embedded table.
func (c *CatalogPricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	// 1. --price-file override always wins, if this exact key was changed by
	// an overlay (see overrideValue for how "was changed" is decided).
	if v, resolvedRegion, ok := c.overrideValue(kind, sku, region); ok {
		c.record(kind, sku, region, pricing.Provenance{Source: pricing.SourceOverride, SKU: sku, Region: resolvedRegion})
		return v, resolvedRegion, nil
	}

	// 2. Live API, cached for the scan's lifetime. The catalogue load runs
	// against c.ctx — the scan's deadline-bounded context.
	if c.client != nil {
		if v, res, err := c.liveUnitPrice(kind, sku, region); err == nil {
			c.record(kind, sku, region, pricing.Provenance{Source: pricing.SourceLiveAPI, SKU: sku, Region: res.region})
			return v, res.region, nil
		}
	}

	// 3. Embedded fallback (USD only).
	v, resolvedRegion, err := c.static.UnitPrice(kind, provider, sku, region)
	if err != nil {
		return 0, "", err
	}
	c.record(kind, sku, region, pricing.Provenance{Source: pricing.SourceEmbedded, SKU: sku, Region: resolvedRegion})
	return v, resolvedRegion, nil
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
// never-overlaid embedded table — i.e. it was genuinely set by --price-file.
func (c *CatalogPricer) overrideValue(kind pricing.Kind, sku, region string) (float64, string, bool) {
	overlaid, resolvedRegion, err := c.static.UnitPrice(kind, "aws", sku, region)
	if err != nil {
		return 0, "", false
	}
	baseline := c.staticBaseline
	if baseline == nil {
		return 0, "", false
	}
	pristineVal, _, pristineErr := baseline.UnitPrice(kind, "aws", sku, region)
	if pristineErr != nil || pristineVal != overlaid {
		return overlaid, resolvedRegion, true
	}
	return 0, "", false
}

// liveUnitPrice resolves (kind, sku, region) against the cached catalogue,
// loading it on first use. The load runs against c.ctx (the scan's
// deadline-bounded context), so a hanging Price List API fails cleanly at
// --timeout and UnitPrice falls back to the embedded table rather than
// stalling the rule.
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

// loadCatalogue fetches the EC2 price list exactly once and indexes every
// product this pricer models into skusByKey. This is the only place the
// Price List API is called — everything else reads the cache built here.
//
// FILTERED BY PRODUCT FAMILY, and verified. Fetching the whole AmazonEC2 list
// is correct but unusable: it is hundreds of thousands of products — every
// instance type, every region, every data-transfer rate — paged 100 at a time
// to find a handful of EBS and address rates. Measured against a live account
// it spent 1m39s and had still not finished, making the scan's pricing step
// longer than everything else combined.
//
// The reason it was unfiltered is real and worth preserving: a filter on the
// wrong attribute name returns an empty result with NO error, and a silently
// empty catalogue is the failure class this file exists to prevent. So the
// filter is verified rather than trusted — a family that returns nothing is
// treated as a broken filter, not as an empty catalogue, and the load falls
// back to the unfiltered fetch. Correctness is preserved; the cost is paid
// only when a family name has actually drifted.
func (c *CatalogPricer) loadCatalogue(ctx context.Context) error {
	c.reportCatalogueProgress(0, 1, false)

	n, err := c.loadFamilies(ctx, priceProductFamilies)
	if err != nil {
		c.reportCatalogueProgress(0, 1, true)
		c.log.Warn("aws: GetProducts unavailable; pricing will use the embedded fallback table", "err", err)
		return err
	}
	if n == 0 {
		// Every family came back empty. Either the product-family names have
		// drifted or the filter is being rejected silently; either way the
		// unfiltered fetch is the honest answer rather than an empty table.
		c.log.Warn("aws: filtered price fetch returned nothing; retrying unfiltered",
			"families", priceProductFamilies)
		n, err = c.loadFamilies(ctx, nil)
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

// priceProductFamilies are the GetProducts productFamily values holding every
// rate tellury models: EBS capacity, IOPS and throughput live under "Storage",
// and the hourly charge for an address under "Elastic IP". Both were read from
// a real GetProducts response, not guessed — the recorded fixture in testdata
// contains exactly these two.
var priceProductFamilies = []string{"Storage", "Elastic IP"}

// loadFamilies fetches and indexes the catalogue, one filtered request per
// family. A nil families slice fetches everything, which is the fallback path.
func (c *CatalogPricer) loadFamilies(ctx context.Context, families []string) (int, error) {
	if len(families) == 0 {
		return c.loadOne(ctx, "")
	}
	total := 0
	for _, f := range families {
		n, err := c.loadOne(ctx, f)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// loadOne runs a single GetProducts pagination, optionally filtered to one
// product family, and indexes what it returns.
func (c *CatalogPricer) loadOne(ctx context.Context, family string) (int, error) {
	in := &awspricing.GetProductsInput{
		ServiceCode:   aws.String("AmazonEC2"),
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
// aws_v1 product documents and the pure indexer
// ─────────────────────────────────────────────────────────────────────────────

// priceListDoc is the aws_v1 product document GetProducts returns in each
// PriceList element. The shape is fixed by the Price List API's aws_v1
// format: a "product" block (productFamily, attributes, sku) plus a "terms"
// block whose OnDemand offer terms carry priceDimensions with a "unit" and a
// "pricePerUnit" map keyed by currency code.
type priceListDoc struct {
	Product struct {
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
// EBS volumes: a product with productFamily "Storage" whose volumeApiName
// attribute is a real DescribeVolumes.VolumeType value. The SKU token is the
// volumeApiName attribute VERBATIM — the same string the
// unattached_ebs_volume rule queries (its volume_type attr comes straight
// from DescribeVolumes.VolumeType). The price list ALSO carries a
// human-friendly "volumeType" attribute ("General Purpose" for gp3), which is
// NOT the token: indexing that would make every lookup miss and silently fall
// back to the embedded table — the exact defect class this catalogue was
// written to prevent. Each price dimension is indexed under the pricing.Kind
// its unit declares: "GB-Mo" -> KindDiskCapacity, "IOPS-Mo" ->
// KindDiskIOPS, "MBps-Mo" -> KindDiskThroughput. A unit this catalogue does
// not model is skipped (no price -> the rule skips rather than guesses),
// never guessed at.
//
// Elastic IPs: a product with productFamily "Elastic IP". The SKU token is
// the operation attribute verbatim ("AdditionalAddress" for the hourly charge
// an unassociated address accrues), indexed under KindStaticIP from the
// "Hrs" dimension.
func indexDoc(doc *priceListDoc) []catalogueEntry {
	region, ok := regionOfProduct(doc.Product.Attributes)
	if !ok {
		// An unmapped location is skipped, never guessed at.
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
	case "Elastic IP":
		op := doc.Product.Attributes["operation"]
		if op == "" {
			return nil
		}
		var out []catalogueEntry
		for _, dim := range priceDimensions(doc) {
			if dim.unit != "Hrs" {
				continue
			}
			out = append(out, catalogueEntry{kind: pricing.KindStaticIP, sku: op, region: region, price: dim.price})
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
func kindForUnit(unit string) pricing.Kind {
	switch unit {
	case "GB-Mo":
		return pricing.KindDiskCapacity
	case "IOPS-Mo":
		return pricing.KindDiskIOPS
	case "MBps-Mo":
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
}
