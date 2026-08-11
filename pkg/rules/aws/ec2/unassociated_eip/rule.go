// Package unassociated_eip implements the "unassociated_eip" FinOps rule: an
// Elastic IP address with no association still bills the hourly rate for
// doing nothing. It is the AWS mirror of the GCP unused_reserved_ip rule: the
// same flat-cost shape (whole monthly cost is waste — there is no partial
// component to compute), the same $0.10/month noise floor, and evidence
// carrying the domain, the public IP and the unit price with its provenance.
//
// AWS exposes no IN_USE/RESERVED status ladder for Elastic IPs: an EIP is
// either associated (has an AssociationId) or it is not. The guard set is
// therefore simpler than the GCP rule's — there is no status ladder and no
// "no users" cross-check beyond the AssociationId, because for AWS the
// association IS the single in-use signal. The normalizer writes
// association_state unconditionally ("associated" | "unassociated"), so the
// rule reads one derived attribute and never needs the graph's topology.
package unassociated_eip

import (
	"context"
	"math"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	awsrules "github.com/TypeOneLabs/tellury/pkg/rules/aws"
)

// ID is the stable rule identifier, and the RULE column value in CLI output.
const ID = "unassociated_eip"

// EIPSKU is the pricing catalogue SKU token for an unassociated Elastic IP
// address. It is NOT an invented spelling: it is the productFamily "Elastic
// IP" product's operation attribute VERBATIM in the AWS Price List API's
// aws_v1 documents (a recorded GetProducts response — pkg/pricing/aws/
// testdata/ec2-price-list.json — carries "operation": "AdditionalAddress"
// for the hourly charge an unassociated address accrues). The rule, the
// embedded table and the live catalogue all index this exact string:
//
//   - the embedded table's "static_ip.AdditionalAddress" entry
//     (pkg/pricing/aws/data/aws_prices.json);
//   - the live catalogue's indexDoc, which derives the token from the same
//     operation attribute (pkg/pricing/aws/catalog.go);
//   - the key this rule queries.
//
// pkg/pricing/aws/catalog_test.go's TestCatalogPricer_EIPSKUPinned pins the
// catalogue token to this constant and pkg/pricing/aws/static_test.go's
// TestStaticPricer_SKUTokensAgreeWithRules pins the embedded table to it, so
// the three can never drift apart again (a drift silently sends every EIP
// price to the embedded fallback with no error — the exact defect class the
// GCP static-IP and snapshot tokens shipped). The rule's own test pins the
// literal value below.
const EIPSKU = "AdditionalAddress"

// MinMonthlyWasteUSD hides findings below $0.10/month (noise floor), matching
// the convention every other native rule uses. In practice an unassociated
// EIP always clears this by a wide margin (~$3.65/mo at $0.005/hr).
const MinMonthlyWasteUSD = 0.10

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "aws",
		Service:  "ec2",
		Title:    "Elastic IP address is not associated with any instance",
		Description: "An Elastic IP address with no association still bills " +
			"the hourly rate while providing nothing. AWS charges for every " +
			"Elastic IP that is not associated with a running instance; " +
			"associating it or releasing it removes the charge. There is no " +
			"partial component to compute — the entire hourly charge is waste.",
		Severity:           rules.SeverityMedium,
		RequiredAssetTypes: []string{awsrules.TypeAddress},
		RequiredMetrics:    nil, // pure attribute check on the DescribeAddresses payload — zero metric cost
		Remediation:        "aws ec2 release-address --allocation-id EIPALLOC",
		Origin:             "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindAddress }

// Guards are evaluated in order; the first to return false decides the
// SkipCode recorded for the node. The engine's built-in exempt-label check
// runs before any of these — a rule must never declare it.
//
// The ladder mirrors unused_reserved_ip's, collapsed onto AWS's binary
// association model: G2 is the "external_type" guard (an address that is not
// an EIP this rule prices) and G3 is the "not_in_use" + "no_cai_users"
// guards (for AWS the AssociationId is the single in-use signal — there is no
// separate users[] list and no RESERVED/IN_USE status to cross-check).
func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		// G1: domain present — "vpc" or "standard". It is the discriminator
		// that separates an Elastic IP from anything else DescribeAddresses
		// could ever return, so a payload without it cannot be priced.
		{Name: "domain_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(awsrules.AttrDomain)
				return ok && v != ""
			}},
		// G2: is an Elastic IP this rule prices. DescribeAddresses returns
		// exactly the two EIP domains ("vpc" or "standard"); a domain outside
		// those two is not an address this rule prices, and an unparsed
		// domain is not one either. This is the mirror of unused_reserved_ip's
		// external_type guard, which skips the "not the kind of address this
		// rule prices" case with SkipInternalAddress.
		{Name: "is_eip", SkipCode: rules.SkipInternalAddress,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				domain, _ := n.Str(awsrules.AttrDomain)
				return domain == awsrules.DomainVpc || domain == awsrules.DomainStandard
			}},
		// G3: not associated. AWS exposes no IN_USE/RESERVED ladder: an EIP
		// is either associated (AssociationId present) or it is not. A
		// missing association_id is exactly "unassociated".
		//
		// ABSENCE MEANS ZERO: the normalizer writes association_state
		// unconditionally ("associated" | "unassociated"), so a replayed
		// snapshot missing the key must behave EXACTLY like "unassociated" —
		// the address bills the hourly rate — never be skipped as an unknown
		// payload. Skipping on absence would silently lose every finding for
		// a payload whose AssociationId was never parsed.
		{Name: "not_associated", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				state, ok := n.Str(awsrules.AttrAssociationState)
				if !ok {
					return true
				}
				return state == "unassociated"
			}},
	}
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	// The whole monthly reservation cost is waste — there is no partial
	// component to subtract, unlike an oversized resource. The price list
	// prices the unassociated-EIP dimension per hour ("Hrs"), so the monthly
	// waste is unit × HoursPerMonth, the same shape unused_reserved_ip uses.
	region := pricing.RegionOf(n.Location)
	unit, resolvedRegion, err := p.Price.UnitPrice(pricing.KindStaticIP, "aws", EIPSKU, region)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never a $0 assumption (Invariant I4)
	}
	monthlyWaste := unit * pricing.HoursPerMonth

	// Stash the values ExtraEvidence needs. ExtraEvidence has no Pass, so
	// the price-source entry is rendered here — the only place the pricer is
	// reachable — and carried through nc.
	nc.Set("currency", rules.CurrencyOf(p))
	nc.Set("unit_price_hourly", unit)
	nc.Set("price_source", rules.PriceEvidence("price_source", p.Price, pricing.KindStaticIP, EIPSKU, resolvedRegion))

	return []rules.CostBranch{{
		Waste:      round2(monthlyWaste),
		Confidence: 1.0, // deterministic attribute check; no measurement involved
		Label:      "full_hourly_charge",
	}}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

// EvidenceKeys auto-collects the two leading evidence entries: domain and
// public_ip are node attrs whose %v rendering equals the original string
// values, and they are the first two keys of the evidence list. Everything
// from unit_price_hourly onward is computed or conditional, so ExtraEvidence
// renders it. This mirrors unused_reserved_ip's evidence assembly.
func (rule) EvidenceKeys() []string {
	return []string{awsrules.AttrDomain, awsrules.AttrPublicIP}
}

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	unit, _ := nc.Get("unit_price_hourly")
	ev := []rules.Evidence{
		rules.EvMoneyIn("unit_price_hourly", curStr, unit.(float64), 4),
	}
	if id, ok := n.Str(awsrules.AttrAllocationID); ok && id != "" {
		ev = append(ev, rules.Evidence{Key: "allocation_id", Value: id})
	}
	if v, ok := nc.Get("price_source"); ok {
		ev = append(ev, v.(rules.Evidence))
	}
	return ev
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
