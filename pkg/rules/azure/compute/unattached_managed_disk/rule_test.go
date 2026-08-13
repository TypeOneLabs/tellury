// Package unattached_managed_disk_test exercises the unattached_managed_disk
// rule against synthetic graph nodes shaped exactly like the Azure normalizer's
// output (pkg/cloud/azure/normalize.go): a disk node with the attributes the
// Resource Graph path writes — sku_name, disk_size_gb, disk_state, managed_by
// and time_created.
//
// The discipline mirrors the GCP and AWS rule tests: every firing case asserts
// the EXACT monthly waste figure, every skip path asserts the SPECIFIC
// SkipCode (never merely "nothing fired"), and the price-driven branch uses a
// fake Pricer that resolves only the managed-disk SKU under test.
package unattached_managed_disk

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	azurerules "github.com/TypeOneLabs/tellury/pkg/rules/azure"
)

// diskPricer prices exactly the managed-disk (kind, tier-SKU) surface and
// nothing else. Any lookup outside the configured map misses with ErrNoPrice,
// matching pricing.Pricer's contract.
type diskPricer struct {
	unit map[string]float64 // tier SKU -> USD per disk-month
}

func (f diskPricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if kind != pricing.KindManagedDisk {
		return 0, "", pricing.ErrNoPrice
	}
	price, ok := f.unit[sku]
	if !ok {
		return 0, "", pricing.ErrNoPrice
	}
	return price, region, nil
}

func (f diskPricer) MonthlyCost(it pricing.Item) (float64, error) {
	unit, _, err := f.UnitPrice(it.Kind, it.Provider, it.SKU, it.Region)
	if err != nil {
		return 0, err
	}
	return unit * it.Quantity, nil
}

// created is the wall-clock creation instant every disk fixture anchors to, so
// detached_days is deterministic across the whole suite.
var created = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// diskNode returns a fully valid firing fixture: an unattached Premium_LRS
// 128 GiB disk created 30 days before now. Individual tests mutate specific
// attributes to force a desired skip branch; the base-case node clears every
// predicate up to the cost formula.
func diskNode() *graph.Node {
	n := &graph.Node{
		ID:       graph.Ref("/subscriptions/sub-1/resourceGroups/rg-a/providers/Microsoft.Compute/disks/disk-a"),
		Kind:     graph.KindDisk,
		Name:     "disk-a",
		Provider: "azure",
		Project:  "sub-1",
		Location: "westeurope",
	}
	n.SetAttr(azurerules.AttrResourceID, "/subscriptions/sub-1/resourceGroups/rg-a/providers/Microsoft.Compute/disks/disk-a")
	n.SetAttr(azurerules.AttrSKUName, "Premium_LRS")
	n.SetAttr(azurerules.AttrDiskSizeGB, 128.0)
	n.SetAttr(azurerules.AttrDiskState, "Unattached")
	n.SetAttr(azurerules.AttrManagedBy, "")
	n.SetAttr(azurerules.AttrTimeCreated, created.Format(time.RFC3339))
	return n
}

// p10Pricer prices exactly the P10 LRS tier this rule derives from the base
// fixture (Premium_LRS + 128 GiB).
func p10Pricer() diskPricer {
	return diskPricer{unit: map[string]float64{"P10 LRS": 21.68}}
}

func runEval(t *testing.T, n *graph.Node, pricer pricing.Pricer, now time.Time) ([]rules.Finding, map[rules.SkipCode]int) {
	t.Helper()
	if now.IsZero() {
		now = created.Add(30 * 24 * time.Hour)
	}
	g := graph.New()
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: pricer,
		Now:   now,
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

// TestEval_UnattachedManagedDisk_Fires is the firing case: an unattached
// Premium_LRS 128 GiB disk created 30 days ago. The ARM SKU + size maps to
// the billable tier "P10 LRS", whose flat rate is the entire waste:
// $21.68/month.
//
// MUTATION CHECK (mandatory, performed before this PR was opened):
//  1. In rule.go, changed the billing_state guard from
//     `state == "Unattached"` to `state == "Attached"`, leaving this test
//     unchanged.
//  2. `go test ./pkg/rules/azure/compute/unattached_managed_disk/ -run TestEval_UnattachedManagedDisk_Fires -count=1`
//     → FAILED: "want 1 finding, got 0 ([])" — an Unattached disk no longer
//     cleared the (mutated) billing-state gate.
//  3. Restored the guard to `state == "Unattached"`. Test passes again.
func TestEval_UnattachedManagedDisk_Fires(t *testing.T) {
	n := diskNode()
	findings, skips := runEval(t, n, p10Pricer(), time.Time{})

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	if f.MonthlyWasteUSD != 21.68 {
		t.Errorf("MonthlyWasteUSD = %v, want 21.68 (P10 LRS per disk-month)", f.MonthlyWasteUSD)
	}
	if f.Confidence != 0.85 {
		t.Errorf("Confidence = %v, want 0.85 (creation_fallback age basis)", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing disk, got %+v", skips)
	}

	// Evidence: the ARM SKU, derived tier token, state, age basis and price
	// with its provenance.
	byKey := map[string]string{}
	for _, e := range f.Evidence {
		byKey[e.Key] = e.Value
	}
	if byKey["size_gb"] != "128" {
		t.Errorf("evidence size_gb = %q, want 128", byKey["size_gb"])
	}
	if byKey["sku_name"] != "Premium_LRS" {
		t.Errorf("evidence sku_name = %q, want Premium_LRS", byKey["sku_name"])
	}
	if byKey["disk_sku"] != "P10 LRS" {
		t.Errorf("evidence disk_sku = %q, want P10 LRS", byKey["disk_sku"])
	}
	if byKey["disk_state"] != "Unattached" {
		t.Errorf("evidence disk_state = %q, want Unattached", byKey["disk_state"])
	}
	if byKey["age_basis"] != "creation_fallback" {
		t.Errorf("evidence age_basis = %q, want creation_fallback", byKey["age_basis"])
	}
	if byKey["detached_days"] != "30" {
		t.Errorf("evidence detached_days = %q, want 30", byKey["detached_days"])
	}
	if byKey["unit_price_disk_month"] != "$21.6800" {
		t.Errorf("evidence unit_price_disk_month = %q, want $21.6800", byKey["unit_price_disk_month"])
	}
	if !strings.Contains(byKey["price_source"], "sku=P10 LRS") {
		t.Errorf("evidence price_source = %q, want it to name sku=P10 LRS", byKey["price_source"])
	}
}

// TestEval_MissingAttr_Skips covers G1-G4: a disk missing a shape field must
// skip as missing_attribute rather than fire at some assumed price. Each
// missing shape field shares the same code; the table proves every gate is
// live.
func TestEval_MissingAttr_Skips(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*graph.Node)
	}{
		{name: "disk_size_gb", mutate: func(n *graph.Node) { delete(n.Attrs, azurerules.AttrDiskSizeGB) }},
		{name: "sku_name", mutate: func(n *graph.Node) { delete(n.Attrs, azurerules.AttrSKUName) }},
		{name: "disk_state", mutate: func(n *graph.Node) { delete(n.Attrs, azurerules.AttrDiskState) }},
		{name: "time_created", mutate: func(n *graph.Node) { delete(n.Attrs, azurerules.AttrTimeCreated) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := diskNode()
			tc.mutate(n)
			findings, skips := runEval(t, n, p10Pricer(), time.Time{})

			if len(findings) != 0 {
				t.Fatalf("disk missing %s must not fire, got %+v", tc.name, findings)
			}
			if skips[rules.SkipMissingAttr] != 1 {
				t.Errorf("want SkipMissingAttr recorded once for missing %s, got %+v", tc.name, skips)
			}
		})
	}
}

// TestEval_Attached_Skips covers G5's two ARM signals: diskState "Attached"
// and a non-empty managedBy both mean the disk is in use, and must skip as
// in_use — never fire.
func TestEval_Attached_Skips(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*graph.Node)
	}{
		{name: "attached_state", mutate: func(n *graph.Node) {
			n.SetAttr(azurerules.AttrDiskState, "Attached")
		}},
		{name: "managed_by_nonempty", mutate: func(n *graph.Node) {
			n.SetAttr(azurerules.AttrManagedBy, "/subscriptions/sub-1/resourceGroups/rg-a/providers/Microsoft.Compute/virtualMachines/vm-a")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := diskNode()
			tc.mutate(n)
			findings, skips := runEval(t, n, p10Pricer(), time.Time{})

			if len(findings) != 0 {
				t.Fatalf("attached disk must not fire, got %+v", findings)
			}
			if skips[rules.SkipAttached] != 1 {
				t.Errorf("want SkipAttached recorded once for %s, got %+v", tc.name, skips)
			}
			if skips[rules.SkipMissingAttr] != 0 {
				t.Errorf("attached disk must skip as in_use, not missing_attribute, got %+v", skips)
			}
		})
	}
}

// TestEval_NonBillingState_Skips covers G6: a disk whose state is neither
// Attached nor Unattached is not a stable billing unattached charge and must
// skip as non_billing_status.
func TestEval_NonBillingState_Skips(t *testing.T) {
	n := diskNode()
	n.SetAttr(azurerules.AttrDiskState, "Reserved")
	findings, skips := runEval(t, n, p10Pricer(), time.Time{})

	if len(findings) != 0 {
		t.Fatalf("non-billing disk must not fire, got %+v", findings)
	}
	if skips[rules.SkipNonBillingStatus] != 1 {
		t.Errorf("want SkipNonBillingStatus recorded once, got %+v", skips)
	}
}

// TestEval_DetachedDaysBoundary checks both sides of MinDetachedDays=7:
// exactly 7 days fires (7.0 is NOT < 7.0), 6 days skips as recently detached.
func TestEval_DetachedDaysBoundary(t *testing.T) {
	t.Run("exactly 7 days fires", func(t *testing.T) {
		n := diskNode()
		now := created.Add(7 * 24 * time.Hour)
		findings, _ := runEval(t, n, p10Pricer(), now)
		if len(findings) != 1 {
			t.Fatalf("want 1 finding at exactly 7 days, got %d (%+v)", len(findings), findings)
		}
	})
	t.Run("6 days, just under, skips", func(t *testing.T) {
		n := diskNode()
		now := created.Add(6 * 24 * time.Hour)
		findings, skips := runEval(t, n, p10Pricer(), now)
		if len(findings) != 0 {
			t.Fatalf("6-day-old disk must not fire, got %+v", findings)
		}
		if skips[rules.SkipRecentlyDetached] != 1 {
			t.Errorf("want SkipRecentlyDetached recorded once at 6 days, got %+v", skips)
		}
	})
}

// TestEval_NoPrice_Skips covers the price gate: the rule must never assume $0
// for a disk whose tier or SKU/region cannot be priced (Invariant I4). The
// unknown-size case is a tier-mapping miss, and the empty-pricer case is a
// tier-mapping hit with no price row; both are SkipNoPrice.
func TestEval_NoPrice_Skips(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*graph.Node)
		pricer pricing.Pricer
	}{
		{
			name:   "tier_unresolved",
			mutate: func(n *graph.Node) { n.SetAttr(azurerules.AttrDiskSizeGB, 129.0) },
			pricer: p10Pricer(),
		},
		{
			name:   "price_unresolved",
			mutate: func(n *graph.Node) {},
			pricer: diskPricer{unit: map[string]float64{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := diskNode()
			tc.mutate(n)
			findings, skips := runEval(t, n, tc.pricer, time.Time{})

			if len(findings) != 0 {
				t.Fatalf("disk with no price must not fire at $0, got %+v", findings)
			}
			if skips[rules.SkipNoPrice] != 1 {
				t.Errorf("want SkipNoPrice recorded once for %s, got %+v", tc.name, skips)
			}
		})
	}
}

// TestEval_BelowMinWaste_Skips covers the noise floor: a 4 GiB Premium_LRS
// disk maps to P1 LRS; at $0.02/month it is below the $0.10 floor and must
// skip as below_min_waste.
func TestEval_BelowMinWaste_Skips(t *testing.T) {
	n := diskNode()
	n.SetAttr(azurerules.AttrDiskSizeGB, 4.0)
	findings, skips := runEval(t, n, diskPricer{unit: map[string]float64{"P1 LRS": 0.02}}, time.Time{})

	if len(findings) != 0 {
		t.Fatalf("sub-noise-floor disk must not fire, got %+v", findings)
	}
	if skips[rules.SkipBelowMinWaste] != 1 {
		t.Errorf("want SkipBelowMinWaste recorded once, got %+v", skips)
	}
}

// TestEval_TelluryExemptLabel_Skips is the P0 short-circuit: even a perfectly
// valid firing disk carrying tellury-exempt=true must be skipped for the label
// and never produce a finding nor reach any other predicate.
func TestEval_TelluryExemptLabel_Skips(t *testing.T) {
	n := diskNode()
	n.Labels = map[string]string{"tellury-exempt": "true"}
	findings, skips := runEval(t, n, p10Pricer(), time.Time{})

	if len(findings) != 0 {
		t.Fatalf("exempt disk must not fire, got %+v", findings)
	}
	if skips[rules.SkipExemptLabel] != 1 {
		t.Errorf("want SkipExemptLabel recorded once, got %+v", skips)
	}
}

// TestEval_SkipsAndFindingsDisjoint guards the invariant that a node either
// produces a finding or records a skip, never both.
func TestEval_SkipsAndFindingsDisjoint(t *testing.T) {
	findings, skips := runEval(t, diskNode(), p10Pricer(), time.Time{})
	if len(findings) != 1 {
		t.Fatalf("expected the firing fixture to produce exactly one finding, got %d", len(findings))
	}
	if len(skips) != 0 {
		t.Errorf("a firing disk must record zero skips (skips and findings are disjoint), got %+v", skips)
	}
}
