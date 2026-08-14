package gcp

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	billingpb "cloud.google.com/go/billing/apiv1/billingpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// TestMatchSKU_ImageTokensAgainstRecordedCatalogue pins the image resource-group
// tokens against a RECORDED Cloud Billing response rather than a hand-built one.
//
// They shipped as "ImageStorage" and "MachineImageStorage" — plausible,
// symmetric with the pricing.Kind names, and matching nothing the catalogue
// returns. The live groups are "StorageImage" and "MachineImage". A wrong SKU
// token does not fail: it silently prices nothing, every image skips as
// unpriced, and the scan looks clean. That is how the GCP snapshot token and
// the Azure managed-disk families both hid.
//
// Hand-built SKU objects cannot catch this, because whoever writes the test
// writes the same wrong token the code uses. Only a recording can.
func TestMatchSKU_ImageTokensAgainstRecordedCatalogue(t *testing.T) {
	raw, err := os.ReadFile("testdata/listskus-image-recorded.json")
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

	seen := map[pricing.Kind]bool{}
	for _, r := range doc.SKUs {
		var sku billingpb.Sku
		if err := protojson.Unmarshal(r, &sku); err != nil {
			t.Fatalf("unmarshal recorded sku: %v", err)
		}
		kind, token, ok := matchSKU(&sku)
		if !ok {
			t.Errorf("recorded SKU %s (resourceGroup %q) did not match: the token the code "+
				"queries is not the one the catalogue returns, so every image prices as unknown",
				sku.GetSkuId(), sku.GetCategory().GetResourceGroup())
			continue
		}
		if token != "standard" {
			t.Errorf("SKU %s matched token %q, want %q", sku.GetSkuId(), token, "standard")
		}
		seen[kind] = true

		// The rate is per GiB-month; pin it so a unit change is caught too.
		unit := sku.GetPricingInfo()[0].GetPricingExpression().GetUsageUnitDescription()
		if !strings.Contains(strings.ToLower(unit), "gibibyte month") {
			t.Errorf("SKU %s unit = %q, want gibibyte month", sku.GetSkuId(), unit)
		}
	}
	for _, want := range []pricing.Kind{pricing.KindImageStorage, pricing.KindMachineImageStorage} {
		if !seen[want] {
			t.Errorf("no recorded SKU matched %s: that kind cannot be priced from the live catalogue", want)
		}
	}
}
