package rules

import (
	"fmt"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// PriceEvidence renders the provenance of a price lookup as one Evidence
// entry under the given key, e.g. key="price_source" ->
// {"price_source", "live_api sku=n1-standard-4 region=us-central1"}.
//
// Every price a Finding reports MUST be traceable to the SKU that produced
// it and to which of the three sources answered it (--price-file override,
// the live pricing API, or the embedded fallback table) - this is the one
// function every rule uses to keep that promise, so the rendering can never
// drift between rules.
//
// If p implements pricing.ProvenancePricer (currently true for GCP's
// CatalogPricer) and has a recorded answer for (kind, sku, region), that
// answer's real source is used. Otherwise p is a plain pricing.Pricer with
// no provenance tracking (e.g. a bare pricing.StaticPricer, or a test
// fake) - in which case the source is reported as the embedded fallback,
// which is the only source such a Pricer can be.
func PriceEvidence(key string, p pricing.Pricer, kind pricing.Kind, sku, region string) Evidence {
	if pp, ok := p.(pricing.ProvenancePricer); ok {
		if prov, ok := pp.LastLookup(kind, sku, region); ok {
			return Evidence{Key: key, Value: fmt.Sprintf("%s sku=%s region=%s", prov.Source, prov.SKU, prov.Region)}
		}
	}
	return Evidence{Key: key, Value: fmt.Sprintf("%s sku=%s region=%s", pricing.SourceEmbedded, sku, region)}
}

// PricedComponent identifies one pricing dimension that contributed a nonzero
// amount to a compound price (e.g. the CPU and RAM legs of a custom instance
// shape, or the capacity / IOPS / throughput legs of a persistent disk).
// Rules that sum several priced components collect one of these per leg so a
// Finding can attach a price-evidence entry for every contributor — never
// just the dominant one, which would mispresent where the summed number came
// from when different components were answered by different sources (e.g. a
// live-API answer for one leg and an embedded-fallback answer for another).
// Key is a human-legible dimension token (e.g. "cpu", "iops", "capacity")
// used to disambiguate the rendered evidence keys when several components
// contribute.
type PricedComponent struct {
	Kind   pricing.Kind
	SKU    string
	Region string
	Key    string
}

// PriceEvidenceFor renders one price-source evidence entry per component that
// contributed a nonzero amount to a compound price, keeping every leg's real
// provenance visible on the Finding instead of collapsing onto the dominant
// one. With exactly one component it renders a single entry under `key`,
// preserving the original single-dimension behaviour and key; with several,
// each leg gets its own entry keyed `key_<Key>` (e.g. "price_source_cpu",
// "price_source_ram") so a later reader can tell which source answered which
// part of the summed number.
func PriceEvidenceFor(key string, p pricing.Pricer, comps ...PricedComponent) []Evidence {
	if len(comps) == 0 {
		return nil
	}
	if len(comps) == 1 {
		c := comps[0]
		return []Evidence{PriceEvidence(key, p, c.Kind, c.SKU, c.Region)}
	}
	out := make([]Evidence, 0, len(comps))
	for _, c := range comps {
		k := key
		if c.Key != "" {
			k = key + "_" + c.Key
		}
		out = append(out, PriceEvidence(k, p, c.Kind, c.SKU, c.Region))
	}
	return out
}
