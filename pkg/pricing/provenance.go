package pricing

// Source identifies which price source answered a lookup.
type Source string

const (
	// SourceLiveAPI: the value came from a live pricing catalog API call
	// (e.g. GCP's Cloud Billing Catalog API, AWS's Price List API), cached
	// for the scan's duration.
	SourceLiveAPI Source = "live_api"
	// SourceFixture: the value came from a price fixture file loaded via
	// TELLURY_PRICE_FIXTURE — a test-only hook, never a user-facing flag.
	// This is also the source recorded when a Pricer has no provenance
	// tracking (a bare StaticPricer without a CatalogPricer wrapping it).
	SourceFixture Source = "fixture"
)

// Provenance records which source answered one price lookup, and which SKU
// and region resolved it, so a Finding's evidence can always say where a
// dollar amount came from.
type Provenance struct {
	Source Source
	SKU    string
	Region string
}

// ProvenancePricer is optionally implemented by a Pricer that can report the
// provenance of its most recent UnitPrice/MonthlyCost answer for a given
// (kind, sku, region) key. Rules that want traceable evidence type-assert
// their Pricer to this interface; a Pricer that does not implement it (e.g.
// a bare StaticPricer used directly) is a fixture-backed pricer and rules
// should record SourceFixture as the provenance.
type ProvenancePricer interface {
	LastLookup(kind Kind, sku, region string) (Provenance, bool)
}

// ─────────────────────────────────────────────────────────────────────────────
// Currency plumbing
// ─────────────────────────────────────────────────────────────────────────────
//
// The Cloud Billing Catalog API prices the whole catalogue in one ISO 4217
// currency (ListSkusRequest.CurrencyCode), and a billing account's currency
// is fixed at creation — so the right currency for a scan is knowable, but
// only best-effort (detection needs billing permission that a scan may
// legitimately lack, and offline scans have no API to ask). The three
// optional interfaces below let the CLI thread a currency into a Pricer and
// then disclose, after evaluation, what the figures are ACTUALLY in. The
// default — no interface, no currency — is exactly today's USD behaviour, so
// a default scan's output is unchanged.

// CurrencyInfo describes the currency a Pricer's answers are actually in.
//
// Requested is the ISO 4217 code the live catalogue was asked to price in
// ("" means the API default, USD). Effective is the code the figures are
// actually in — equal to Requested when the live catalogue answered, and
// "USD" when an offline scan had no live API to consult. Mixed reports that
// USD prices were used while a non-USD currency was requested — the exact
// trap where an operator asked for EUR figures but the tool answered in USD.
type CurrencyInfo struct {
	Requested string
	Effective string
	Mixed     bool
}

// CurrencyReporter is optionally implemented by a Pricer that can disclose
// which currency its answers are actually in. The CLI consults it after rule
// evaluation so every output format can name the currency and flag USD
// fallback contamination.
type CurrencyReporter interface {
	CurrencyInfo() CurrencyInfo
}

// CurrencySetter is optionally implemented by a Pricer whose catalogue can be
// fetched in a chosen currency. The CLI applies best-effort detection (which
// may need the ingested graph, so it runs after ingestion) before rule
// evaluation; the pricer must be set before its first UnitPrice call.
type CurrencySetter interface {
	SetCurrency(code string)
}

// CatalogueErrorer is optionally implemented by a Pricer whose live catalogue
// load can fail in a way that must abort the scan rather than silently fall
// back — the canonical case being a well-formed but unsupported currency
// code, which Cloud Billing rejects with InvalidArgument. The CLI checks it
// after rule evaluation and surfaces the error, naming the currency.
type CatalogueErrorer interface {
	CatalogueError() error
}
