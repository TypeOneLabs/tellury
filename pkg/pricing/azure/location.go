// Package azure owns Azure's SKU/region spelling and the Retail Prices API
// catalogue. The parent package owns the pricing interfaces and money
// conventions; this package owns the Azure-specific conversions.
package azure

import (
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// LocationRegion is the Azure-specific location canonicaliser. It is the ONE
// place an Azure display name ("West Europe") is folded to ARM form
// ("westeurope"); both graph ingestion (pkg/cloud/azure) and this pricing
// catalogue pass every Azure location through it, so a region node and a
// price lookup can never disagree about place.
//
// It is deliberately a thin wrapper, not a second implementation:
//
//  1. lower-case and trim the input
//  2. remove all whitespace, so "West Europe" becomes "westeurope"
//  3. pass that ARM-form token through pricing.CanonicalRegion, the single
//     provider-agnostic canonicaliser
func LocationRegion(location string) string {
	loc := strings.ToLower(strings.TrimSpace(location))
	loc = strings.Join(strings.Fields(loc), "")
	return pricing.CanonicalRegion(loc)
}
