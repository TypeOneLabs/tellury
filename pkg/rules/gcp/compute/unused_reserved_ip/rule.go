// Package unused_reserved_ip implements the "unused_reserved_ip" FinOps
// rule: a reserved (static) EXTERNAL IP address with nothing attached to it
// still bills the full hourly reservation rate for doing nothing. Unlike an
// oversized instance or an over-provisioned disk, there is no partial waste
// to compute here — an unattached reserved address contributes zero value,
// so its entire monthly cost is the waste figure.
package unused_reserved_ip

import (
	"context"
	"fmt"
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
	// external static IP address. It matches both the embedded price
	// table's "static_ip.unattached" entry and the token the live Cloud
	// Billing Catalog lookup (pkg/pricing/gcp/catalog.go's matchSKU)
	// indexes IP-range/static-IP SKUs under, so a live answer and the
	// embedded fallback resolve the exact same key.
	StaticIPSKU = "unattached"
)

func init() { rules.Register(rule{}) }

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

func (rule) Eval(ctx context.Context, p *rules.Pass) ([]rules.Finding, error) {
	var out []rules.Finding

	p.Graph.ByKind(graph.KindAddress, func(n *graph.Node) bool {
		if ctx.Err() != nil {
			return false
		}

		// P0: exemption label.
		if n.Labels["tellury-exempt"] == "true" {
			p.SkipNode(ID, n.ID, rules.SkipExemptLabel)
			return true
		}

		// P1: shape valid.
		addrType, ok := n.Str("address_type")
		if !ok || addrType == "" {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}
		status, ok := n.Str("status")
		if !ok || status == "" {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}

		// P2: internal addresses are free — not waste. Distinct reason from
		// "in use" so --explain-skips can tell the two apart.
		if addrType != "EXTERNAL" {
			p.SkipNode(ID, n.ID, rules.SkipInternalAddress)
			return true
		}

		// P3: in-use addresses are doing their job — not waste.
		if status == "IN_USE" {
			p.SkipNode(ID, n.ID, rules.SkipAttached)
			return true
		}
		if status != "RESERVED" {
			// e.g. a transient "RESERVING" state: no stable billable waste
			// to report yet.
			p.SkipNode(ID, n.ID, rules.SkipNonBillingStatus)
			return true
		}

		// P4: defensive cross-check against CAI's own users[] — status and
		// the users list must agree that nothing is attached, exactly the
		// dual-signal pattern detached_disk uses for disks.
		userCount, _ := n.Num("address_user_count") // absent => treat as 0
		if userCount > 0 {
			p.SkipNode(ID, n.ID, rules.SkipAttached)
			return true
		}

		// Cost model: the whole monthly reservation cost is waste — there is
		// no partial component to subtract, unlike an oversized resource.
		region := pricing.RegionOf(n.Location)
		unit, resolvedRegion, err := p.Price.UnitPrice(pricing.KindStaticIP, "gcp", StaticIPSKU, region)
		if err != nil {
			p.SkipNode(ID, n.ID, rules.SkipNoPrice)
			return true
		}
		monthlyWaste := unit * pricing.HoursPerMonth

		// P5: material.
		if monthlyWaste < MinMonthlyWasteUSD {
			p.SkipNode(ID, n.ID, rules.SkipBelowMinWaste)
			return true
		}

		ev := []rules.Evidence{
			{Key: "address_type", Value: addrType},
			{Key: "status", Value: status},
			{Key: "unit_price_hourly", Value: fmt.Sprintf("$%.4f", unit)},
		}
		if purpose, ok := n.Str("address_purpose"); ok && purpose != "" {
			ev = append(ev, rules.Evidence{Key: "address_purpose", Value: purpose})
		}
		if ip, ok := n.Str("address_ip"); ok && ip != "" {
			ev = append(ev, rules.Evidence{Key: "address_ip", Value: ip})
		}
		ev = append(ev, rules.PriceEvidence("price_source", p.Price, pricing.KindStaticIP, StaticIPSKU, resolvedRegion))

		out = append(out, rules.Finding{
			RuleID:          ID,
			ResourceID:      n.ID,
			Resource:        n.Display(),
			Kind:            n.Kind,
			Project:         n.Project,
			Location:        n.Location,
			MonthlyWasteUSD: round2(monthlyWaste),
			Confidence:      1.0, // deterministic attribute check; no measurement involved
			Evidence:        ev,
		})
		return true
	})
	return out, nil
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
