package pricing

// Source identifies which of the three price sources answered a lookup.
// Precedence, highest first: SourceOverride > SourceLiveAPI > SourceEmbedded.
// This order is enforced by whichever Pricer implements ProvenancePricer
// (currently pkg/cloud/gcp.CatalogPricer); it is documented here too because
// every rule's evidence and the --price-file help text must agree on the
// exact same three names.
type Source string

const (
	// SourceOverride: the value came from a --price-file override entry.
	SourceOverride Source = "price_file"
	// SourceLiveAPI: the value came from a live pricing catalog API call
	// (e.g. GCP's Cloud Billing Catalog API), cached for the scan's duration.
	SourceLiveAPI Source = "live_api"
	// SourceEmbedded: the value came from the embedded static price table,
	// either because no live API was available/permitted, or because the
	// live catalogue had no matching SKU.
	SourceEmbedded Source = "embedded_fallback"
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
// a bare StaticPricer used directly, with no override applied) is a plain
// embedded-table pricer and rules should treat the absence of provenance as
// "no claim made", not as an error.
type ProvenancePricer interface {
	LastLookup(kind Kind, sku, region string) (Provenance, bool)
}

// OverlayLoader is optionally implemented by a Pricer that supports
// --price-file overrides. Both StaticPricer and any live-API-backed Pricer
// that wraps one (e.g. gcp.CatalogPricer) implement this with the same
// signature, so callers like the CLI never need to know which concrete type
// they were handed.
type OverlayLoader interface {
	OverlayFile(path string) error
}

// ─────────────────────────────────────────────────────────────────────────────
// Currency plumbing
// ─────────────────────────────────────────────────────────────────────────────
//
// The Cloud Billing Catalog API prices the whole catalogue in one ISO 4217
// currency (ListSkusRequest.CurrencyCode), and a billing account's currency
// is fixed at creation — so the right currency for a scan is knowable, but
// only best-effort (detection needs billing permission that a scan may
// legitimately lack, and the embedded fallback table is USD-only). The three
// optional interfaces below let the CLI thread a currency into a Pricer and
// then disclose, after evaluation, what the figures are ACTUALLY in. The
// default — no interface, no currency — is exactly today's USD behaviour, so
// a default scan's output is unchanged.

// CurrencyInfo describes the currency a Pricer's answers are actually in.
//
// Requested is the ISO 4217 code the live catalogue was asked to price in
// ("" means the API default, USD). Effective is the code the figures are
// actually in — equal to Requested when the live catalogue answered, and
// "USD" when the embedded fallback table (which is USD-only) answered
// instead. Mixed reports that USD fallback prices were used while a non-USD
// currency was requested — the exact trap where an operator asked for EUR
// figures but the tool answered in USD.
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
