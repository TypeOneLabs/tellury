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
