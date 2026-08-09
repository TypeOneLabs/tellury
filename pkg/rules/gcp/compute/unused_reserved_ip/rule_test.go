package unused_reserved_ip

import (
	"context"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// fakePricer prices exactly the static-IP SKU this rule looks up; every
// other lookup misses, matching ErrNoPrice semantics.
type fakePricer struct {
	unit float64
}

func (f fakePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if kind == pricing.KindStaticIP && sku == StaticIPSKU {
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

func addrNode(addrType, status string, userCount float64) *graph.Node {
	n := &graph.Node{
		ID:       graph.Ref("//compute.googleapis.com/projects/p/regions/us-central1/addresses/a1"),
		Kind:     graph.KindAddress,
		Name:     "a1",
		Project:  "p",
		Location: "us-central1",
	}
	n.SetAttr("address_type", addrType)
	n.SetAttr("status", status)
	n.SetAttr("address_user_count", userCount)
	return n
}

func runEval(t *testing.T, n *graph.Node) ([]rules.Finding, map[rules.SkipCode]int) {
	g := graph.New()
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: fakePricer{unit: 0.01}, // -> $7.30/mo, comfortably above the noise floor
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

// TestEval_ExternalReservedUnattached_Fires is the firing case: EXTERNAL +
// RESERVED + no users. The entire monthly cost is reported as waste.
func TestEval_ExternalReservedUnattached_Fires(t *testing.T) {
	n := addrNode("EXTERNAL", "RESERVED", 0)
	findings, skips := runEval(t, n)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	wantWaste := 0.01 * pricing.HoursPerMonth
	if f.MonthlyWasteUSD != round2(wantWaste) {
		t.Errorf("MonthlyWasteUSD = %v, want %v (the whole reservation cost)", f.MonthlyWasteUSD, round2(wantWaste))
	}
	if f.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0 (deterministic attribute check)", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing resource, got %+v", skips)
	}
}

// TestEval_AddressUserCountAbsent_Fires pins ABSENCE MEANS ZERO for the
// address_user_count attribute: a CAI payload whose users[] was never parsed
// (or a replayed snapshot missing the key entirely) must behave EXACTLY like
// user_count == 0 — the address has no users, so the rule still fires. A
// regression to "skip on absence" (uc, ok := ...; return ok && uc == 0)
// would silently lose this finding; this test fails under that mutation.
func TestEval_AddressUserCountAbsent_Fires(t *testing.T) {
	n := addrNode("EXTERNAL", "RESERVED", 0)
	delete(n.Attrs, "address_user_count") // users[] was never parsed -> key absent
	findings, skips := runEval(t, n)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding when address_user_count is absent (absence means zero), got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	wantWaste := 0.01 * pricing.HoursPerMonth
	if f.MonthlyWasteUSD != round2(wantWaste) {
		t.Errorf("MonthlyWasteUSD = %v, want %v (the whole reservation cost)", f.MonthlyWasteUSD, round2(wantWaste))
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing resource, got %+v", skips)
	}
}

// TestEval_TransientStatus_Skips pins the reserved_status guard against the
// exact regression that real GCP states expose: an address sitting in a
// transient status such as RESERVING or CREATING is NOT a billable reserved
// address (no stable charge while the reservation is being set up), so it
// must be skipped as non_billing_status, never reported as waste. This is
// the test that fails if reserved_status is loosened to
// `status != "IN_USE"`, which would sweep every transient-status address
// into the firing set.
func TestEval_TransientStatus_Skips(t *testing.T) {
	for _, status := range []string{"RESERVING", "CREATING"} {
		t.Run(status, func(t *testing.T) {
			n := addrNode("EXTERNAL", status, 0)
			findings, skips := runEval(t, n)

			if len(findings) != 0 {
				t.Fatalf("want 0 findings for an address in status %s, got %+v", status, findings)
			}
			if skips[rules.SkipNonBillingStatus] != 1 {
				t.Errorf("want SkipNonBillingStatus recorded once for %s, got %+v", status, skips)
			}
		})
	}
}

// TestEval_InternalAddress_Skips: internal addresses are free, never waste.
func TestEval_InternalAddress_Skips(t *testing.T) {
	n := addrNode("INTERNAL", "RESERVED", 0)
	findings, skips := runEval(t, n)

	if len(findings) != 0 {
		t.Fatalf("want 0 findings for an internal address, got %+v", findings)
	}
	if skips[rules.SkipInternalAddress] != 1 {
		t.Errorf("want SkipInternalAddress recorded once, got %+v", skips)
	}
}

// TestEval_InUseExternalAddress_Skips: an in-use address is not waste, and
// must record a DIFFERENT skip reason than an internal address so
// --explain-skips can tell the two apart.
func TestEval_InUseExternalAddress_Skips(t *testing.T) {
	n := addrNode("EXTERNAL", "IN_USE", 1)
	findings, skips := runEval(t, n)

	if len(findings) != 0 {
		t.Fatalf("want 0 findings for an in-use address, got %+v", findings)
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached recorded once, got %+v", skips)
	}
	if skips[rules.SkipInternalAddress] != 0 {
		t.Errorf("in-use external address must not be reported as SkipInternalAddress, got %+v", skips)
	}
}

// TestEval_ReservedButHasUsers_Skips: a defensive cross-check — even if
// status somehow said RESERVED, a nonzero users[] count means something is
// attached, and the rule must still skip as "in use", not fire.
func TestEval_ReservedButHasUsers_Skips(t *testing.T) {
	n := addrNode("EXTERNAL", "RESERVED", 1)
	findings, skips := runEval(t, n)

	if len(findings) != 0 {
		t.Fatalf("want 0 findings when users[] is nonzero, got %+v", findings)
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached recorded once, got %+v", skips)
	}
}

// TestEval_NoPrice_Skips: the rule never assumes $0 when the SKU/region does
// not resolve.
func TestEval_NoPrice_Skips(t *testing.T) {
	n := addrNode("EXTERNAL", "RESERVED", 0)
	g := graph.New()
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: fakePricer{unit: 0}, // returns ErrNoPrice for everything except a match we never provide
		Skip: func(ruleID string, id graph.Ref, code rules.SkipCode) {
			skipCounts[code]++
		},
	}
	// Force a pricer that never resolves the SKU at all.
	p.Price = noPricePricer{}

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

type noPricePricer struct{}

func (noPricePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	return 0, "", pricing.ErrNoPrice
}

func (noPricePricer) MonthlyCost(it pricing.Item) (float64, error) {
	return 0, pricing.ErrNoPrice
}
