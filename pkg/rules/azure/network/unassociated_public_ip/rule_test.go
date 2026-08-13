// Package unassociated_public_ip_test exercises the unassociated_public_ip
// rule against synthetic graph nodes shaped exactly like the Azure normalizer's
// output (pkg/cloud/azure/normalize.go): an address node with the attributes
// the Resource Graph path writes — sku_name, public_ip_allocation_method,
// ip_address and the derived ip_configuration_count.
//
// The discipline mirrors the GCP and AWS rule tests: every firing case asserts
// the EXACT monthly waste figure, every skip path asserts the SPECIFIC
// SkipCode (never merely "nothing fired"), and the price-driven branch uses a
// fake Pricer that resolves only the Standard public IP SKU under test.
package unassociated_public_ip

import (
	"context"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	azurerules "github.com/TypeOneLabs/tellury/pkg/rules/azure"
)

// fakePricer prices exactly the Standard static-IP SKU this rule looks up;
// every other lookup misses, matching ErrNoPrice semantics.
type fakePricer struct {
	unit float64
}

func (f fakePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if kind == pricing.KindStaticIP && sku == PublicIPSKU {
		return f.unit, region, nil
	}
	return 0, "", pricing.ErrNoPrice
}

func (f fakePricer) MonthlyCost(it pricing.Item) (float64, error) {
	unit, _, err := f.UnitPrice(it.Kind, it.Provider, it.SKU, it.Region)
	if err != nil {
		return 0, err
	}
	return unit * it.Quantity, nil
}

// pipNode returns a fully valid firing fixture: a Standard, Static,
// unassociated public IP in westeurope. Individual tests mutate specific
// attributes to force a desired skip branch; the base-case node clears every
// predicate up to the cost formula.
func pipNode() *graph.Node {
	n := &graph.Node{
		ID:       graph.Ref("/subscriptions/sub-1/resourceGroups/rg-b/providers/Microsoft.Network/publicIPAddresses/pip-a"),
		Kind:     graph.KindAddress,
		Name:     "pip-a",
		Provider: "azure",
		Project:  "sub-1",
		Location: "westeurope",
	}
	n.SetAttr(azurerules.AttrResourceID, "/subscriptions/sub-1/resourceGroups/rg-b/providers/Microsoft.Network/publicIPAddresses/pip-a")
	n.SetAttr(azurerules.AttrSKUName, "Standard")
	n.SetAttr(azurerules.AttrPublicIPAllocationMethod, "Static")
	n.SetAttr(azurerules.AttrIPAddress, "203.0.113.10")
	n.SetAttr(azurerules.AttrIPConfigurationCount, 0.0)
	return n
}

func runEval(t *testing.T, n *graph.Node, priceUnit float64) ([]rules.Finding, map[rules.SkipCode]int) {
	t.Helper()
	g := graph.New()
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: fakePricer{unit: priceUnit},
		Skip: func(ruleID string, id graph.Ref, code rules.SkipCode) {
			skipCounts[code]++
		},
	}
	findings, err := rules.AdaptNodeRule(rule{}).Eval(context.Background(), p)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	return findings, skipCounts
}

// TestEval_UnassociatedPublicIP_Fires is the firing case: a Standard, Static,
// unassociated public IP. The entire monthly cost (unit × HoursPerMonth) is
// reported as waste: 0.005 × 730 = $3.65.
//
// MUTATION CHECK (mandatory, performed before this PR was opened):
//  1. In rule.go, changed the billable_sku guard from
//     `sku == PublicIPSKU` to `sku == "Basic"`, leaving this test unchanged.
//  2. `go test ./pkg/rules/azure/network/unassociated_public_ip/ -run TestEval_UnassociatedPublicIP_Fires -count=1`
//     → FAILED: "want 1 finding, got 0 ([])" — a Standard address no longer
//     cleared the (mutated) billable-SKU gate.
//  3. Restored the guard to `sku == PublicIPSKU`. Test passes again.
func TestEval_UnassociatedPublicIP_Fires(t *testing.T) {
	n := pipNode()
	findings, skips := runEval(t, n, 0.005)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	wantWaste := pricing.Round2(0.005 * pricing.HoursPerMonth)
	if f.MonthlyWasteUSD != wantWaste {
		t.Errorf("MonthlyWasteUSD = %v, want %v (0.005/hr x %v h/month)", f.MonthlyWasteUSD, wantWaste, pricing.HoursPerMonth)
	}
	if f.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0 (deterministic attribute check)", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing address, got %+v", skips)
	}

	// Evidence: the SKU, allocation method, derived count, public IP and the
	// hourly price with its provenance.
	byKey := map[string]string{}
	for _, e := range f.Evidence {
		byKey[e.Key] = e.Value
	}
	if byKey["sku_name"] != "Standard" {
		t.Errorf("evidence sku_name = %q, want Standard", byKey["sku_name"])
	}
	if byKey["public_ip_allocation_method"] != "Static" {
		t.Errorf("evidence public_ip_allocation_method = %q, want Static", byKey["public_ip_allocation_method"])
	}
	if byKey["ip_configuration_count"] != "0" {
		t.Errorf("evidence ip_configuration_count = %q, want 0", byKey["ip_configuration_count"])
	}
	if byKey["ip_address"] != "203.0.113.10" {
		t.Errorf("evidence ip_address = %q, want 203.0.113.10", byKey["ip_address"])
	}
	if byKey["unit_price_hourly"] != "$0.0050" {
		t.Errorf("evidence unit_price_hourly = %q, want $0.0050", byKey["unit_price_hourly"])
	}
	if !strings.Contains(byKey["price_source"], "sku=Standard") {
		t.Errorf("evidence price_source = %q, want it to name sku=Standard", byKey["price_source"])
	}
}

// TestEval_SKUTokenPinned pins the rule's query token to the literal
// "Standard" — the Azure Retail Prices API skuName pkg/pricing/azure indexes
// under pricing.KindStaticIP.
func TestEval_SKUTokenPinned(t *testing.T) {
	if PublicIPSKU != "Standard" {
		t.Fatalf("PublicIPSKU = %q, want %q (the Retail Prices API skuName verbatim)", PublicIPSKU, "Standard")
	}
}

// TestEval_MissingAttr_Skips covers G1, G2 and G4: a public IP missing a shape
// field must skip as missing_attribute rather than fire at some assumed price.
// The ip_configuration_count case is the critical one — a missing count means
// the payload was not parsed, and must NOT be reported as unassociated waste.
func TestEval_MissingAttr_Skips(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*graph.Node)
	}{
		{name: "sku_name", mutate: func(n *graph.Node) { delete(n.Attrs, azurerules.AttrSKUName) }},
		{name: "public_ip_allocation_method", mutate: func(n *graph.Node) { delete(n.Attrs, azurerules.AttrPublicIPAllocationMethod) }},
		{name: "ip_configuration_count", mutate: func(n *graph.Node) { delete(n.Attrs, azurerules.AttrIPConfigurationCount) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := pipNode()
			tc.mutate(n)
			findings, skips := runEval(t, n, 0.005)

			if len(findings) != 0 {
				t.Fatalf("public IP missing %s must not fire, got %+v", tc.name, findings)
			}
			if skips[rules.SkipMissingAttr] != 1 {
				t.Errorf("want SkipMissingAttr recorded once for missing %s, got %+v", tc.name, skips)
			}
		})
	}
}

// TestEval_BasicSKU_Skips covers G3: only Standard is billable in this rule.
// A Basic address is treated as a non-billing status and must skip as
// non_billing_status — never fire at a Standard rate.
func TestEval_BasicSKU_Skips(t *testing.T) {
	n := pipNode()
	n.SetAttr(azurerules.AttrSKUName, "Basic")
	findings, skips := runEval(t, n, 0.005)

	if len(findings) != 0 {
		t.Fatalf("Basic public IP must not fire, got %+v", findings)
	}
	if skips[rules.SkipNonBillingStatus] != 1 {
		t.Errorf("want SkipNonBillingStatus recorded once, got %+v", skips)
	}
	if skips[rules.SkipMissingAttr] != 0 {
		t.Errorf("Basic is a non-billing SKU, not a missing attribute, got %+v", skips)
	}
}

// TestEval_AssociatedPublicIP_Skips covers G5: an associated address is in use
// — it is attached to an IP configuration and Azure does not bill the
// unassociated charge — so it must skip as in_use, never fire.
func TestEval_AssociatedPublicIP_Skips(t *testing.T) {
	n := pipNode()
	n.SetAttr(azurerules.AttrIPConfigurationCount, 1.0)
	n.SetAttr(azurerules.AttrIPConfiguration, map[string]any{
		"id": "/subscriptions/sub-1/resourceGroups/rg-b/providers/Microsoft.Network/networkInterfaces/nic-a/ipConfigurations/ipconfig1",
	})
	findings, skips := runEval(t, n, 0.005)

	if len(findings) != 0 {
		t.Fatalf("associated public IP must not fire, got %+v", findings)
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached recorded once for an associated address, got %+v", skips)
	}
	if skips[rules.SkipMissingAttr] != 0 {
		t.Errorf("associated address must skip as in_use, not missing_attribute, got %+v", skips)
	}
}

// TestEval_NoPrice_Skips: the rule never assumes $0 when the SKU/region does
// not resolve (Invariant I4).
func TestEval_NoPrice_Skips(t *testing.T) {
	n := pipNode()
	g := graph.New()
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: noPricePricer{},
		Skip: func(ruleID string, id graph.Ref, code rules.SkipCode) {
			skipCounts[code]++
		},
	}
	findings, err := rules.AdaptNodeRule(rule{}).Eval(context.Background(), p)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("want 0 findings when no price resolves, got %+v", findings)
	}
	if skipCounts[rules.SkipNoPrice] != 1 {
		t.Errorf("want SkipNoPrice recorded once, got %+v", skipCounts)
	}
}

// TestEval_BelowMinWaste_Skips covers the noise floor: an address whose
// computed waste falls under $0.10/month must skip as below_min_waste. A
// $0.0001/hr rate x 730 h = $0.073 < $0.10.
func TestEval_BelowMinWaste_Skips(t *testing.T) {
	n := pipNode()
	findings, skips := runEval(t, n, 0.0001)

	if len(findings) != 0 {
		t.Fatalf("sub-noise-floor address must not fire, got %+v", findings)
	}
	if skips[rules.SkipBelowMinWaste] != 1 {
		t.Errorf("want SkipBelowMinWaste recorded once, got %+v", skips)
	}
}

// TestEval_TelluryExemptLabel_Skips is the P0 short-circuit: even a perfectly
// valid firing address carrying tellury-exempt=true must be skipped for the
// label and never produce a finding nor reach any other predicate.
func TestEval_TelluryExemptLabel_Skips(t *testing.T) {
	n := pipNode()
	n.Labels = map[string]string{"tellury-exempt": "true"}
	findings, skips := runEval(t, n, 0.005)

	if len(findings) != 0 {
		t.Fatalf("exempt address must not fire, got %+v", findings)
	}
	if skips[rules.SkipExemptLabel] != 1 {
		t.Errorf("want SkipExemptLabel recorded once, got %+v", skips)
	}
}

// TestEval_SkipsAndFindingsDisjoint guards the invariant that a node either
// produces a finding or records a skip, never both.
func TestEval_SkipsAndFindingsDisjoint(t *testing.T) {
	findings, skips := runEval(t, pipNode(), 0.005)
	if len(findings) != 1 {
		t.Fatalf("expected the firing fixture to produce exactly one finding, got %d", len(findings))
	}
	if len(skips) != 0 {
		t.Errorf("a firing address must record zero skips (skips and findings are disjoint), got %+v", skips)
	}
}

type noPricePricer struct{}

func (noPricePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	return 0, "", pricing.ErrNoPrice
}

func (noPricePricer) MonthlyCost(it pricing.Item) (float64, error) {
	return 0, pricing.ErrNoPrice
}
