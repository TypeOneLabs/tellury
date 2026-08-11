package rules

import (
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// mixedProvenancePricer answers every lookup from a fixture-backed path (so
// PriceEvidence's provenance type-assert always succeeds) while recording a
// different per-kind provenance: exactly what a live CatalogPricer does when
// a compound price's legs were answered by different sources — e.g. capacity
// from the live pricing API and IOPS/throughput from a fixture.
type mixedProvenancePricer struct {
	prov map[pricing.Kind]pricing.Provenance
}

func (m mixedProvenancePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	return 1, region, nil
}

func (m mixedProvenancePricer) MonthlyCost(it pricing.Item) (float64, error) {
	return 0, nil
}

func (m mixedProvenancePricer) LastLookup(kind pricing.Kind, sku, region string) (pricing.Provenance, bool) {
	p, ok := m.prov[kind]
	return p, ok
}

// TestPriceEvidenceFor_MixedSourceCompoundPriceReportsBothSources is the
// regression test for audit finding #1 (the provenance half): when a compound
// price (capacity + IOPS + throughput, or CPU + RAM) is summed from several
// priced components and different components are answered by different
// sources — the live API answering one leg and a fixture another — the
// Finding must report a price-source evidence entry for EVERY contributing
// component, never just the dominant one. Otherwise the evidence mispresents
// where the summed number came from.
//
// This test drives PriceEvidenceFor directly with a pricer whose provenance
// says capacity came from the live API and IOPS came from a fixture, then
// asserts both sources appear on the rendered evidence — each leg keyed
// distinctly (price_source_capacity / price_source_iops) so a reader can
// tell which component each source belongs to.
func TestPriceEvidenceFor_MixedSourceCompoundPriceReportsBothSources(t *testing.T) {
	p := mixedProvenancePricer{
		prov: map[pricing.Kind]pricing.Provenance{
			pricing.KindDiskCapacity: {Source: pricing.SourceLiveAPI, SKU: "pd-ssd", Region: "us-central1"},
			pricing.KindDiskIOPS:     {Source: pricing.SourceFixture, SKU: "pd-ssd", Region: "default"},
		},
	}

	ev := PriceEvidenceFor("price_source", p,
		PricedComponent{Kind: pricing.KindDiskCapacity, SKU: "pd-ssd", Region: "us-central1", Key: "capacity"},
		PricedComponent{Kind: pricing.KindDiskIOPS, SKU: "pd-ssd", Region: "default", Key: "iops"},
	)

	if len(ev) != 2 {
		t.Fatalf("want 2 price-source entries (one per contributing component), got %d: %+v", len(ev), ev)
	}

	got := map[string]string{}
	for _, e := range ev {
		got[e.Key] = e.Value
	}

	capacityVal, ok := got["price_source_capacity"]
	if !ok {
		t.Fatalf("missing price_source_capacity entry; got %+v", got)
	}
	if capacityVal != "live_api sku=pd-ssd region=us-central1" {
		t.Errorf("capacity leg rendered %q; want live_api provenance", capacityVal)
	}

	iopsVal, ok := got["price_source_iops"]
	if !ok {
		t.Fatalf("missing price_source_iops entry; got %+v", got)
	}
	if iopsVal != "fixture sku=pd-ssd region=default" {
		t.Errorf("iops leg rendered %q; want fixture provenance", iopsVal)
	}

	// The distinct keys prove the two sources are NOT collapsed onto the
	// dominant capacity source — the exact defect the finding describes.
	foundLive, foundFixture := false, false
	for _, v := range got {
		if v == "live_api sku=pd-ssd region=us-central1" {
			foundLive = true
		}
		if v == "fixture sku=pd-ssd region=default" {
			foundFixture = true
		}
	}
	if !foundLive || !foundFixture {
		t.Fatalf("expected both live_api and fixture sources reported; got %+v", got)
	}
}

// TestPriceEvidenceFor_SingleComponentPreservesSingleKey guards the other
// half of the contract: a single-component price keeps the original
// single-evidence key (no "_<leg>" suffix), so the change is purely additive
// for the already-single-dimension case — existing evidence consumers and the
// dominant-source legacy behaviour are untouched.
func TestPriceEvidenceFor_SingleComponentPreservesSingleKey(t *testing.T) {
	p := mixedProvenancePricer{
		prov: map[pricing.Kind]pricing.Provenance{
			pricing.KindVMInstance: {Source: pricing.SourceLiveAPI, SKU: "n1-standard-4", Region: "default"},
		},
	}

	ev := PriceEvidenceFor("current_price_source", p,
		PricedComponent{Kind: pricing.KindVMInstance, SKU: "n1-standard-4", Region: "default", Key: "instance"},
	)

	if len(ev) != 1 {
		t.Fatalf("single component must render exactly one entry, got %d: %+v", len(ev), ev)
	}
	if ev[0].Key != "current_price_source" {
		t.Errorf("single component must keep the undecorated key, got %q", ev[0].Key)
	}
	if ev[0].Value != "live_api sku=n1-standard-4 region=default" {
		t.Errorf("unexpected single-component value %q", ev[0].Value)
	}
}
