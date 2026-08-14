package gcp

import (
	"encoding/json"
	"os"
	"testing"

	billingpb "cloud.google.com/go/billing/apiv1/billingpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// The multi-region image substitution rests on two facts about the live Cloud
// Billing catalogue, and this test pins BOTH against a recorded response rather
// than against anybody's memory of them:
//
//  1. StorageImage publishes no multi-region SKU. Every one of its entries is
//     regional, so a custom image in "eu"/"us"/"asia" — which is where GCP puts
//     one by DEFAULT — resolves nothing and skips as unpriced.
//  2. MachineImage publishes all three multi-regions at the SAME rate. That
//     shared rate is what the substitution borrows.
//
// If Google ever publishes a multi-region StorageImage SKU, fact 1 breaks here
// and the substitution should be deleted rather than left shadowing a real
// price. If the three machine-image rates ever diverge, fact 2 breaks here and
// "the multi-region rate" stops being a single number worth borrowing.
//
// Recorded 2026-08-14 from services/6F81-5844-456A, walking every page (32,242
// SKUs) and keeping all 41 StorageImage SKUs plus the 3 multi-region
// MachineImage SKUs.

const recordedMultiRegionSKUs = "testdata/listskus-multiregion-image-recorded.json"

func loadRecordedSKUs(t *testing.T) []*billingpb.Sku {
	t.Helper()
	raw, err := os.ReadFile(recordedMultiRegionSKUs)
	if err != nil {
		t.Fatalf("read recorded catalogue: %v", err)
	}
	var doc struct {
		SKUs []json.RawMessage `json:"skus"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse recorded catalogue: %v", err)
	}
	if len(doc.SKUs) == 0 {
		t.Fatal("recorded catalogue is empty")
	}
	out := make([]*billingpb.Sku, 0, len(doc.SKUs))
	for _, r := range doc.SKUs {
		var s billingpb.Sku
		if err := protojson.Unmarshal(r, &s); err != nil {
			t.Fatalf("unmarshal SKU: %v", err)
		}
		out = append(out, &s)
	}
	return out
}

// unitPrice returns the SKU's last tier rate in whole currency units.
func unitPrice(s *billingpb.Sku) (float64, bool) {
	pi := s.GetPricingInfo()
	if len(pi) == 0 {
		return 0, false
	}
	tiers := pi[len(pi)-1].GetPricingExpression().GetTieredRates()
	if len(tiers) == 0 {
		return 0, false
	}
	m := tiers[len(tiers)-1].GetUnitPrice()
	return float64(m.GetUnits()) + float64(m.GetNanos())/1e9, true
}

// Fact 1: the gap the substitution exists to fill is real.
func TestRecorded_NoMultiRegionStorageImageSKU(t *testing.T) {
	var storageImage, multi int
	for _, s := range loadRecordedSKUs(t) {
		if s.GetCategory().GetResourceGroup() != "StorageImage" {
			continue
		}
		storageImage++
		for _, r := range s.GetServiceRegions() {
			if isMultiRegion(r) {
				multi++
				t.Errorf("StorageImage SKU %q (%s) is multi-region %q: the "+
					"substitution now shadows a published price and must be removed",
					s.GetSkuId(), s.GetDescription(), r)
			}
		}
	}
	if storageImage == 0 {
		t.Fatal("no StorageImage SKUs in the recording")
	}
	t.Logf("%d StorageImage SKUs recorded, %d multi-region", storageImage, multi)
}

// Fact 2: the borrowed rate is one number, not three.
func TestRecorded_MultiRegionMachineImageRatesAgree(t *testing.T) {
	// The three real SKU ids, so a silent catalogue change is visible as a
	// missing id rather than as a quietly different price.
	want := map[string]string{
		"B6B2-44BB-2476": "us",
		"80E5-E18F-9FA4": "europe",
		"94B7-C7FA-15DA": "asia",
	}

	prices := map[string]float64{}
	for _, s := range loadRecordedSKUs(t) {
		if s.GetCategory().GetResourceGroup() != "MachineImage" {
			continue
		}
		region, ok := want[s.GetSkuId()]
		if !ok {
			continue
		}
		p, ok := unitPrice(s)
		if !ok {
			t.Fatalf("SKU %s (%s) carries no price", s.GetSkuId(), s.GetDescription())
		}
		prices[region] = p
	}

	if len(prices) != len(want) {
		t.Fatalf("found %d of the %d recorded multi-region machine-image SKUs: %v",
			len(prices), len(want), prices)
	}
	first := prices["us"]
	for region, p := range prices {
		if p != first {
			t.Errorf("multi-region rate differs: %s = %v, us = %v — there is no "+
				"single multi-region rate left to borrow", region, p, first)
		}
	}
	t.Logf("multi-region machine-image rate = %v across %v", first, len(prices))
}

// The substitution only works if these SKUs match as machine-image storage in
// the first place. A resource-group rename would otherwise strand it silently,
// the same way the "ImageStorage"/"MachineImageStorage" tokens once did.
func TestRecorded_MultiRegionMachineImageSKUsMatch(t *testing.T) {
	matched := 0
	for _, s := range loadRecordedSKUs(t) {
		if s.GetCategory().GetResourceGroup() != "MachineImage" {
			continue
		}
		kind, token, ok := matchSKU(s)
		if !ok {
			t.Errorf("recorded SKU %s (%s) no longer matches", s.GetSkuId(), s.GetDescription())
			continue
		}
		if kind != pricing.KindMachineImageStorage {
			t.Errorf("SKU %s matched kind %v, want KindMachineImageStorage — the "+
				"substitution reads that kind's index", s.GetSkuId(), kind)
		}
		if token != imgSKU {
			t.Errorf("SKU %s token = %q, want %q — the substitution looks up this token",
				s.GetSkuId(), token, imgSKU)
		}
		matched++
	}
	if matched == 0 {
		t.Fatal("no MachineImage SKUs matched in the recording")
	}
}
