// Package unassociated_eip_test exercises the unassociated_eip rule against
// synthetic graph nodes shaped exactly like the AWS normalizer's output
// (pkg/cloud/aws/normalize.go): an address node with the attributes the
// DescribeAddresses path writes — domain, public_ip, allocation_id and the
// derived association_state.
//
// The discipline mirrors the GCP rule tests: every firing case asserts the
// EXACT monthly waste figure, every skip path asserts the SPECIFIC SkipCode
// (never merely "nothing fired"), and the price-driven branch uses a fake
// Pricer that resolves only the SKU under test so an unrelated lookup cannot
// mask a broken predicate.
package unassociated_eip

import (
	"context"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	awsrules "github.com/TypeOneLabs/tellury/pkg/rules/aws"
)

// fakePricer prices exactly the static-IP SKU this rule looks up; every
// other lookup misses, matching ErrNoPrice semantics.
type fakePricer struct {
	unit float64
}

func (f fakePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if kind == pricing.KindStaticIP && sku == EIPSKU {
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

// eipNode returns a fully valid firing fixture: a VPC-domain Elastic IP with
// no association, in us-east-1. Individual tests mutate specific attributes
// to force a desired skip branch; the base-case node clears every predicate.
func eipNode() *graph.Node {
	n := &graph.Node{
		ID:       graph.Ref("accounts/123456789012/regions/us-east-1/addresses/eipalloc-0d1"),
		Kind:     graph.KindAddress,
		Name:     "203.0.113.10",
		Provider: "aws",
		Project:  "123456789012",
		Location: "us-east-1",
	}
	n.SetAttr(awsrules.AttrDomain, awsrules.DomainVpc)
	n.SetAttr(awsrules.AttrPublicIP, "203.0.113.10")
	n.SetAttr(awsrules.AttrAllocationID, "eipalloc-0d1")
	n.SetAttr(awsrules.AttrAssociationState, "unassociated")
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

// TestEval_UnassociatedEIP_Fires is the firing case: a VPC-domain Elastic IP
// with no association. The entire monthly cost (unit × HoursPerMonth) is
// reported as waste: 0.005 × 730 = $3.65.
//
// MUTATION CHECK (mandatory, performed before this PR was opened):
//  1. In rule.go, changed the not_associated guard from
//     `return state == "unassociated"` to `return state == "associated"`,
//     leaving this test unchanged.
//  2. `go test ./pkg/rules/aws/ec2/unassociated_eip/ -run TestEval_UnassociatedEIP_Fires -count=1`
//     → FAILED: "want 1 finding, got 0 ([])" — an unassociated address no
//     longer cleared the (mutated) association gate, so the test proved it
//     actually detects the unassociated condition rather than merely running.
//  3. Restored the guard to `state == "unassociated"`. Test passes again.
func TestEval_UnassociatedEIP_Fires(t *testing.T) {
	n := eipNode()
	findings, skips := runEval(t, n, 0.005)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	wantWaste := round2(0.005 * pricing.HoursPerMonth)
	if f.MonthlyWasteUSD != wantWaste {
		t.Errorf("MonthlyWasteUSD = %v, want %v (0.005/hr x %v h/month)", f.MonthlyWasteUSD, wantWaste, pricing.HoursPerMonth)
	}
	if f.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0 (deterministic attribute check)", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing address, got %+v", skips)
	}

	// Evidence: auto-collected attrs first (domain, public_ip), then the
	// computed/conditional entries.
	byKey := map[string]string{}
	for _, e := range f.Evidence {
		byKey[e.Key] = e.Value
	}
	if byKey["domain"] != "vpc" {
		t.Errorf("evidence domain = %q, want vpc", byKey["domain"])
	}
	if byKey["public_ip"] != "203.0.113.10" {
		t.Errorf("evidence public_ip = %q, want 203.0.113.10", byKey["public_ip"])
	}
	if byKey["unit_price_hourly"] != "$0.0050" {
		t.Errorf("evidence unit_price_hourly = %q, want $0.0050", byKey["unit_price_hourly"])
	}
	if byKey["allocation_id"] != "eipalloc-0d1" {
		t.Errorf("evidence allocation_id = %q, want eipalloc-0d1", byKey["allocation_id"])
	}
	if !strings.Contains(byKey["price_source"], "sku=AdditionalAddress") {
		t.Errorf("evidence price_source = %q, want it to name sku=AdditionalAddress", byKey["price_source"])
	}
}

// TestEval_SKUTokenPinned pins the rule's query token to the literal
// "AdditionalAddress" — the operation attribute of the productFamily
// "Elastic IP" product in a recorded GetProducts response (pkg/pricing/aws/
// testdata/ec2-price-list.json). The reverse direction — that the live
// catalogue derives the SAME token — is pinned by pkg/pricing/aws/
// catalog_test.go's TestCatalogPricer_EIPSKUPinned (catalogue-token == this
// constant), and the embedded table by static_test.go's
// TestStaticPricer_SKUTokensAgreeWithRules. Together the three tests close
// the loop that let the GCP static-IP and snapshot tokens ship wrong: the
// rule cannot silently query a token no price source produces.
func TestEval_SKUTokenPinned(t *testing.T) {
	if EIPSKU != "AdditionalAddress" {
		t.Fatalf("EIPSKU = %q, want %q (the GetProducts operation attribute verbatim); "+
			"every price source would miss and fall back silently", EIPSKU, "AdditionalAddress")
	}
}

// TestEval_AssociationStateAbsent_Fires pins ABSENCE MEANS ZERO for the
// association_state attribute: a replayed snapshot missing the key (whose
// normalizer always writes it) must behave EXACTLY like "unassociated" — the
// address bills the hourly rate, so the rule still fires. A regression to
// "skip on absence" would silently lose every finding for such a payload;
// this test fails under that mutation.
func TestEval_AssociationStateAbsent_Fires(t *testing.T) {
	n := eipNode()
	delete(n.Attrs, awsrules.AttrAssociationState) // replayed snapshot missing the derived key
	findings, skips := runEval(t, n, 0.005)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding when association_state is absent (absence means unassociated), got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing address, got %+v", skips)
	}
}

// TestEval_AssociatedEIP_Skips covers G3: an associated address is in use —
// it is attached to a running instance and AWS does not bill it — so it must
// skip as in_use (SkipAttached), never fire.
func TestEval_AssociatedEIP_Skips(t *testing.T) {
	n := eipNode()
	n.SetAttr(awsrules.AttrAssociationState, "associated")
	n.SetAttr(awsrules.AttrAssociationID, "eipassoc-0e1")
	findings, skips := runEval(t, n, 0.005)

	if len(findings) != 0 {
		t.Fatalf("associated address must not fire, got %+v", findings)
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached recorded once for an associated address, got %+v", skips)
	}
	if skips[rules.SkipInternalAddress] != 0 {
		t.Errorf("associated address must skip as in_use, not internal_address, got %+v", skips)
	}
}

// TestEval_UnknownDomain_Skips covers G2: a domain outside the two EIP
// domains ("vpc" | "standard") is not an Elastic IP this rule prices. This is
// the mirror of unused_reserved_ip's internal_address skip — the "not the
// kind of address this rule prices" case.
func TestEval_UnknownDomain_Skips(t *testing.T) {
	n := eipNode()
	n.SetAttr(awsrules.AttrDomain, "edge")
	findings, skips := runEval(t, n, 0.005)

	if len(findings) != 0 {
		t.Fatalf("non-EIP domain must not fire, got %+v", findings)
	}
	if skips[rules.SkipInternalAddress] != 1 {
		t.Errorf("want SkipInternalAddress recorded once for an unknown domain, got %+v", skips)
	}
}

// TestEval_MissingDomain_Skips covers G1: an address payload with no domain
// has no discriminator the rule can price against — it must skip as
// missing_attribute, never fire at some assumed rate.
func TestEval_MissingDomain_Skips(t *testing.T) {
	n := eipNode()
	delete(n.Attrs, awsrules.AttrDomain)
	findings, skips := runEval(t, n, 0.005)

	if len(findings) != 0 {
		t.Fatalf("address without a domain must not fire, got %+v", findings)
	}
	if skips[rules.SkipMissingAttr] != 1 {
		t.Errorf("want SkipMissingAttr recorded once for a missing domain, got %+v", skips)
	}
}

// TestEval_NoPrice_Skips: the rule never assumes $0 when the SKU/region does
// not resolve (Invariant I4).
func TestEval_NoPrice_Skips(t *testing.T) {
	n := eipNode()
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
	n := eipNode()
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
	n := eipNode()
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
	findings, skips := runEval(t, eipNode(), 0.005)
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
