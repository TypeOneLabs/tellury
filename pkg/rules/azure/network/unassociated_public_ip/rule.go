// Package unassociated_public_ip implements the "unassociated_public_ip"
// FinOps rule: a Standard Azure public IP address with no IP configuration
// association still bills the hourly rate while providing nothing. It is the
// Azure mirror of the GCP unused_reserved_ip rule and the AWS unassociated_eip
// rule: the same flat-cost shape (the whole monthly charge is waste), the same
// $0.10/month noise floor, and evidence carrying the SKU, allocation method,
// public IP and unit price with its provenance.
package unassociated_public_ip

import (
	"context"
	"fmt"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	azurerules "github.com/TypeOneLabs/tellury/pkg/rules/azure"
)

// ID is the stable rule identifier, and the RULE column value in CLI output.
const ID = "unassociated_public_ip"

// PublicIPSKU is the pricing catalogue SKU token for a Standard static IPv4
// public IP address. It is exactly the Azure Retail Prices API skuName
// ("Standard") that pkg/pricing/azure/catalog.go's staticIPFilters indexes.
// Basic is deliberately not priced by this rule; it is treated as a
// non-billing status for tellury's v1 Standard-only model.
const PublicIPSKU = "Standard"

// MinMonthlyWasteUSD hides findings below $0.10/month (noise floor), matching
// the convention every other native rule uses. In practice a Standard public
// IP always clears this by a wide margin (~$3.65/mo at $0.005/hr).
const MinMonthlyWasteUSD = 0.10

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "azure",
		Service:  "network",
		Title:    "Public IP address is not associated with any resource",
		Description: "A Standard Azure public IP address with no IP " +
			"configuration association still bills the hourly rate while " +
			"providing nothing. Azure charges for every Standard public IP " +
			"that is not associated with a resource; associating it or " +
			"deleting it removes the charge. There is no partial component to " +
			"compute — the entire hourly charge is waste.",
		Severity:           rules.SeverityMedium,
		RequiredAssetTypes: []string{azurerules.TypePublicIP},
		RequiredMetrics:    nil, // pure attribute check on the ARG row — zero metric cost
		Remediation:        "az network public-ip delete --ids ID",
		Origin:             "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindAddress }

// Guards are evaluated in order; the first to return false decides the
// SkipCode recorded for the node. The engine's built-in exempt-label check
// runs before any of these — a rule must never declare it.
func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		// G1: SKU present — it is the pricing discriminator. A payload without
		// it cannot be priced.
		{Name: "sku_name_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(azurerules.AttrSKUName)
				return ok && v != ""
			}},
		// G2: allocation method present. A missing field is an unparsed
		// payload, never an assumption about how the address bills.
		{Name: "public_ip_allocation_method_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(azurerules.AttrPublicIPAllocationMethod)
				return ok && v != ""
			}},
		// G3: billable SKU. Only "Standard" is priced by this rule; "Basic"
		// is treated as a non-billing status for the v1 Standard-only model.
		{Name: "billable_sku", SkipCode: rules.SkipNonBillingStatus,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				sku, _ := n.Str(azurerules.AttrSKUName)
				return sku == PublicIPSKU
			}},
		// G4: association count present. The normalizer writes
		// ip_configuration_count unconditionally (0 or 1), so a missing key
		// means the payload was not parsed — a missing-attribute diagnosis,
		// NOT a business fact of "unassociated". This guard is what keeps a
		// replayed snapshot with no count from being reported as waste.
		{Name: "ip_configuration_count_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				_, ok := n.Num(azurerules.AttrIPConfigurationCount)
				return ok
			}},
		// G5: not associated. The countable signal is 0 when Resource Graph
		// returned no properties.ipConfiguration, 1 when it returned one. An
		// associated address is in use and not waste.
		{Name: "not_associated", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				count, _ := n.Num(azurerules.AttrIPConfigurationCount)
				return count == 0
			}},
	}
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	// The whole monthly reservation cost is waste — there is no partial
	// component to subtract, unlike an oversized resource. The Azure Retail
	// Prices API prices the Standard public IP dimension per hour ("1 Hour"),
	// so the monthly waste is unit × HoursPerMonth, the same shape
	// unused_reserved_ip and unassociated_eip use.
	region := pricing.RegionOf(n.Location)
	unit, resolvedRegion, err := p.Price.UnitPrice(pricing.KindStaticIP, "azure", PublicIPSKU, region)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never a $0 assumption (Invariant I4)
	}
	monthlyWaste := unit * pricing.HoursPerMonth

	// Stash the values ExtraEvidence needs. ExtraEvidence has no Pass, so the
	// price-source entry is rendered here — the only place the pricer is
	// reachable — and carried through nc.
	nc.Set("currency", rules.CurrencyOf(p))
	nc.Set("unit_price_hourly", unit)
	nc.Set("price_source", rules.PriceEvidence("price_source", p.Price, pricing.KindStaticIP, PublicIPSKU, resolvedRegion))

	return []rules.CostBranch{{
		Waste:      pricing.Round2(monthlyWaste),
		Confidence: 1.0, // deterministic attribute check; no measurement involved
		Label:      "full_hourly_charge",
	}}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

// EvidenceKeys returns nil: ExtraEvidence renders every entry so the
// formatting is explicit and the conditional ip_address entry stays out of the
// auto-collected prefix.
func (rule) EvidenceKeys() []string { return nil }

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	sku, _ := n.Str(azurerules.AttrSKUName)
	allocationMethod, _ := n.Str(azurerules.AttrPublicIPAllocationMethod)
	unit, _ := nc.Get("unit_price_hourly")

	count, _ := n.Num(azurerules.AttrIPConfigurationCount)
	ev := []rules.Evidence{
		{Key: "sku_name", Value: sku},
		{Key: "public_ip_allocation_method", Value: allocationMethod},
		{Key: "ip_configuration_count", Value: fmt.Sprintf("%.0f", count)},
		rules.EvMoneyIn("unit_price_hourly", curStr, unit.(float64), 4),
	}
	if ip, ok := n.Str(azurerules.AttrIPAddress); ok && ip != "" {
		ev = append(ev, rules.Evidence{Key: "ip_address", Value: ip})
	}
	if v, ok := nc.Get("price_source"); ok {
		ev = append(ev, v.(rules.Evidence))
	}
	return ev
}
