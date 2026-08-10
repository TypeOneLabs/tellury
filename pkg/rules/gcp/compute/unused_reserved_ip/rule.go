// Package unused_reserved_ip implements the "unused_reserved_ip" FinOps
// rule: a reserved (static) EXTERNAL IP address with nothing attached to it
// still bills the full hourly reservation rate for doing nothing. Unlike an
// oversized instance or an over-provisioned disk, there is no partial waste
// to compute here — an unattached reserved address contributes zero value,
// so its entire monthly cost is the waste figure.
package unused_reserved_ip

import (
	"context"
	"math"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// ID is the stable rule identifier.
const ID = "unused_reserved_ip"

// Constants for this rule.
const (
	// MinMonthlyWasteUSD hides findings below $0.10/month (noise floor),
	// matching the convention every other native rule uses. In practice a
	// reserved external IP always clears this by a wide margin (~$7+/mo).
	MinMonthlyWasteUSD = 0.10

	// StaticIPSKU is the pricing catalogue SKU token for an unattached
	// external static IP address. All three price sources share it verbatim:
	// the embedded table's "static_ip.unattached" entry, the live Cloud
	// Billing Catalog lookup (pkg/pricing/gcp/catalog.go's matchSKU returns
	// this exact token for IP-range/static-IP SKUs), and the key this rule
	// queries — so a live answer and the embedded fallback resolve the same
	// key. pkg/pricing/gcp/catalog_test.go's TestMatchSKU_StaticIPTokenPinned
	// pins matchSKU's token to this constant, so the two can never drift
	// apart again (a drift silently sends every static-IP price to the
	// embedded fallback with no error).
	StaticIPSKU = "unattached"
)

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "gcp",
		Service:  "compute",
		Title:    "Reserved external IP address is not attached to anything",
		Description: "A reserved (static) EXTERNAL IP address with no attached " +
			"instance or forwarding rule bills the full hourly reservation rate " +
			"while providing nothing. An internal address is free and an " +
			"in-use address is not waste; only a reserved, unattached, " +
			"EXTERNAL address is reclaimable, and its entire monthly cost is " +
			"the waste — there is no partial component to compute.",
		Severity:           rules.SeverityMedium,
		RequiredAssetTypes: []string{"compute.googleapis.com/Address"},
		RequiredMetrics:    nil, // pure attribute check on the CAI payload — zero metric cost
		Remediation:        "gcloud compute addresses delete NAME --region REGION",
		Origin:             "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindAddress }

func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		{Name: "address_type_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str("address_type")
				return ok && v != ""
			}},
		{Name: "status_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str("status")
				return ok && v != ""
			}},
		{Name: "external_type", SkipCode: rules.SkipInternalAddress,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				addrType, _ := n.Str("address_type")
				return addrType == "EXTERNAL"
			}},
		{Name: "not_in_use", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				status, _ := n.Str("status")
				return status != "IN_USE"
			}},
		{Name: "reserved_status", SkipCode: rules.SkipNonBillingStatus,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				status, _ := n.Str("status")
				return status == "RESERVED"
			}},
		{Name: "no_cai_users", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				// ABSENCE MEANS ZERO: the normalizer writes
				// address_user_count unconditionally (users[] is always
				// counted, even when empty), so a missing key means "no
				// users", never an unknown to skip on. Skipping on absence
				// would silently lose every finding for an address whose
				// users[] was not parsed.
				uc, _ := n.Num("address_user_count")
				return uc == 0
			}},
	}
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	// The whole monthly reservation cost is waste — there is no partial
	// component to subtract, unlike an oversized resource.
	region := pricing.RegionOf(n.Location)
	unit, resolvedRegion, err := p.Price.UnitPrice(pricing.KindStaticIP, "gcp", StaticIPSKU, region)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never a $0 assumption (Invariant I4)
	}
	monthlyWaste := unit * pricing.HoursPerMonth

	// Stash the values ExtraEvidence needs. ExtraEvidence has no Pass, so
	// the price-source entry is rendered here — the only place the pricer
	// is reachable — and carried through nc.
	// Stashed here because ExtraEvidence has no Pass to ask the pricer.
	nc.Set("currency", rules.CurrencyOf(p))
	nc.Set("unit_price_hourly", unit)
	nc.Set("price_source", rules.PriceEvidence("price_source", p.Price, pricing.KindStaticIP, StaticIPSKU, resolvedRegion))

	return []rules.CostBranch{{
		Waste:      round2(monthlyWaste),
		Confidence: 1.0, // deterministic attribute check; no measurement involved
		Label:      "full_reservation",
	}}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

// EvidenceKeys auto-collects the two leading evidence entries exactly as the
// pre-refactor rule rendered them: address_type and status are node attrs
// whose %v rendering equals the original string values, and they are the
// first two keys of the original evidence list, so auto-collection preserves
// the original order. Everything from unit_price_hourly onward is computed or
// conditionally present, so ExtraEvidence renders it.
func (rule) EvidenceKeys() []string {
	return []string{"address_type", "status"}
}

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	unit, _ := nc.Get("unit_price_hourly")
	ev := []rules.Evidence{
		rules.EvMoneyIn("unit_price_hourly", curStr, unit.(float64), 4),
	}
	if purpose, ok := n.Str("address_purpose"); ok && purpose != "" {
		ev = append(ev, rules.Evidence{Key: "address_purpose", Value: purpose})
	}
	if ip, ok := n.Str("address_ip"); ok && ip != "" {
		ev = append(ev, rules.Evidence{Key: "address_ip", Value: ip})
	}
	if v, ok := nc.Get("price_source"); ok {
		ev = append(ev, v.(rules.Evidence))
	}
	return ev
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
