// Package unattached_ebs_volume_test exercises the unattached_ebs_volume
// rule against synthetic graph nodes shaped exactly like the AWS normalizer's
// output (pkg/cloud/aws/normalize.go): a volume node with the attributes the
// DescribeVolumes path writes, and — where relevant — an instance node wired
// up with an attached_to edge exactly as the ingestion linker would.
//
// The discipline mirrors the GCP rule tests: every firing case asserts the
// EXACT monthly waste figure, every skip path asserts the SPECIFIC SkipCode
// (never merely "nothing fired"), and each price-driven branch uses a fake
// Pricer that resolves only the SKUs under test so an unrelated lookup cannot
// mask a broken predicate.
package unattached_ebs_volume

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	awsrules "github.com/TypeOneLabs/tellury/pkg/rules/aws"
)

// diskPricer prices an exact (Kind, sku-token) surface and nothing else, so a
// test can never accidentally succeed (or fail) because an unrelated pricing
// dimension answered. Any lookup outside the configured map misses with
// ErrNoPrice, matching pricing.Pricer's contract.
type diskPricer struct {
	unit map[pricing.Kind]map[string]float64 // kind -> sku -> USD unit price
}

func (f diskPricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	skus, ok := f.unit[kind]
	if !ok {
		return 0, "", pricing.ErrNoPrice
	}
	price, ok := skus[sku]
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

// created is the wall-clock creation instant every volume fixture anchors to,
// so detached_days is deterministic across the whole suite.
var created = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// volumeNode returns a fully valid firing fixture: an available gp3 volume,
// 100 GiB, created 30 days before now, no attachments. Individual tests mutate
// specific attributes to force a desired skip branch; the base-case node
// clears every predicate up to the cost formula.
func volumeNode() *graph.Node {
	n := &graph.Node{
		ID:       graph.Ref("accounts/123456789012/regions/us-east-1/volumes/vol-abc123"),
		Kind:     graph.KindDisk,
		Name:     "vol-abc123",
		Provider: "aws",
		Project:  "123456789012",
		Location: "us-east-1a", // RegionOf -> "us-east-1"
	}
	n.SetAttr(awsrules.AttrSizeGB, 100.0)
	n.SetAttr(awsrules.AttrVolumeType, "gp3")
	n.SetAttr(awsrules.AttrState, awsrules.StateAvailable)
	n.SetAttr(awsrules.AttrCreateTime, created.Format(time.RFC3339))
	n.SetAttr(awsrules.AttrAttachmentCount, 0.0)
	return n
}

// gp3Pricer prices exactly the gp3 capacity leg (the base firing case's only
// priced dimension); the IOPS/throughput legs miss and are treated as zero by
// the rule, exactly as a type without a provisioned-IOPS charge is treated.
func gp3Pricer() diskPricer {
	return diskPricer{unit: map[pricing.Kind]map[string]float64{
		pricing.KindDiskCapacity: {"gp3": 0.08},
	}}
}

// runEval builds a graph from the given nodes and edges, freezes it, and runs
// the rule through the real adapter (rules.AdaptNodeRule), returning findings
// and the skip tally. now defaults to created+30d when zero.
func runEval(t *testing.T, nodes []*graph.Node, edges []graph.Edge, pricer pricing.Pricer, now time.Time) ([]rules.Finding, map[rules.SkipCode]int) {
	t.Helper()
	if now.IsZero() {
		now = created.Add(30 * 24 * time.Hour)
	}
	g := graph.New()
	for _, n := range nodes {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for _, e := range edges {
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
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

// TestEval_UnattachedEBSVolume_Fires is the firing case: an available,
// unattached gp3 volume, 100 GiB, created 30 days ago (well past
// MinDetachedDays). The exact waste is sizeGB * capPrice = 100 * 0.08 = $8.00:
// the fake pricer prices only the capacity leg, so the IOPS/throughput legs
// miss and the rule treats them as zero (the same way a non-provisioned type
// is treated).
//
// MUTATION CHECK (mandatory, performed before this PR was opened):
//  1. In rule.go, changed the available_state guard from
//     `s == awsrules.StateAvailable` to `s == awsrules.StateInUse`,
//     leaving this test unchanged.
//  2. `go test ./pkg/rules/aws/ec2/unattached_ebs_volume/ -run TestEval_UnattachedEBSVolume_Fires -count=1`
//     → FAILED: "want 1 finding, got 0 ([])" — an available volume no longer
//     cleared the (mutated) in-use gate, so the test proved it actually
//     detects the available-state condition rather than merely running.
//  3. Restored the guard to StateAvailable. Test passes again.
func TestEval_UnattachedEBSVolume_Fires(t *testing.T) {
	n := volumeNode()
	findings, skips := runEval(t, []*graph.Node{n}, nil, gp3Pricer(), time.Time{})

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	if f.MonthlyWasteUSD != 8.00 {
		t.Errorf("MonthlyWasteUSD = %v, want 8.00 (100 GiB * $0.08/GiB)", f.MonthlyWasteUSD)
	}
	if f.Confidence != 0.85 {
		t.Errorf("Confidence = %v, want 0.85 (creation_fallback age basis)", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing volume, got %+v", skips)
	}

	// Evidence: the size/type/state and the price with its provenance.
	byKey := map[string]string{}
	for _, e := range f.Evidence {
		byKey[e.Key] = e.Value
	}
	if byKey["size_gb"] != "100" {
		t.Errorf("evidence size_gb = %q, want 100", byKey["size_gb"])
	}
	if byKey["volume_type"] != "gp3" {
		t.Errorf("evidence volume_type = %q, want gp3", byKey["volume_type"])
	}
	if byKey["disk_sku"] != "gp3" {
		t.Errorf("evidence disk_sku = %q, want gp3", byKey["disk_sku"])
	}
	if byKey["state"] != "available" {
		t.Errorf("evidence state = %q, want available", byKey["state"])
	}
	if byKey["age_basis"] != "creation_fallback" {
		t.Errorf("evidence age_basis = %q, want creation_fallback", byKey["age_basis"])
	}
	if byKey["detached_days"] != "30" {
		t.Errorf("evidence detached_days = %q, want 30", byKey["detached_days"])
	}
	if byKey["unit_price_gib_month"] != "$0.0800" {
		t.Errorf("evidence unit_price_gib_month = %q, want $0.0800", byKey["unit_price_gib_month"])
	}
	if !strings.Contains(byKey["price_source"], "sku=gp3") {
		t.Errorf("evidence price_source = %q, want it to name sku=gp3", byKey["price_source"])
	}
}

// TestEval_AllCostComponents_Fires exercises the FULL cost formula
// sizeGB*capPrice + iops*iopsPrice + mbps*thrPrice with all three legs priced
// and nonzero: 100 GiB gp3, 3000 provisioned IOPS, 250 MB/s throughput.
//
//	100*0.08 + 3000*0.005 + 250*0.04 = 8.00 + 15.00 + 10.00 = $33.00
//
// If any of the three legs were dropped from the sum (or its price ignored),
// this exact figure would no longer match.
func TestEval_AllCostComponents_Fires(t *testing.T) {
	n := volumeNode()
	n.SetAttr(awsrules.AttrIops, 3000.0)
	n.SetAttr(awsrules.AttrThroughput, 250.0)

	findings, _ := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity:   {"gp3": 0.08},
			pricing.KindDiskIOPS:       {"gp3": 0.005},
			pricing.KindDiskThroughput: {"gp3": 0.04},
		}},
		time.Time{},
	)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	if f := findings[0]; f.MonthlyWasteUSD != 33.00 {
		t.Errorf("MonthlyWasteUSD = %v, want 33.00 (capacity 8.00 + iops 15.00 + throughput 10.00)", f.MonthlyWasteUSD)
	}
}

// TestEval_AttachmentCountAbsent_Fires pins ABSENCE MEANS ZERO for the
// attachment_count attribute: a volume whose payload predates the
// always-written attachment_count must behave EXACTLY like attachment_count ==
// 0 — no attachments, so the rule still fires. A regression to "skip on
// absence" (cnt, ok := ...; return ok && cnt == 0) would silently lose every
// finding for such a payload; this test fails under that mutation.
func TestEval_AttachmentCountAbsent_Fires(t *testing.T) {
	n := volumeNode()
	delete(n.Attrs, awsrules.AttrAttachmentCount)
	findings, skips := runEval(t, []*graph.Node{n}, nil, gp3Pricer(), time.Time{})

	if len(findings) != 1 {
		t.Fatalf("want 1 finding when attachment_count is absent (absence means zero), got %d (%+v)", len(findings), findings)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing volume, got %+v", skips)
	}
}

// TestEval_AttachedToEdge_Skips covers G5's graph half: a volume with an
// instance --attached_to--> volume edge is in use and must skip as attached,
// with a DISTINCT reason from the missing-attribute path.
func TestEval_AttachedToEdge_Skips(t *testing.T) {
	vol := volumeNode()
	inst := &graph.Node{
		ID:   graph.Ref("accounts/123456789012/regions/us-east-1/instances/i-0cafe"),
		Kind: graph.KindInstance,
		Name: "i-0cafe",
	}
	findings, skips := runEval(t, []*graph.Node{vol, inst},
		[]graph.Edge{{From: inst.ID, To: vol.ID, Kind: graph.EdgeAttachedTo}},
		gp3Pricer(), time.Time{})

	if len(findings) != 0 {
		t.Fatalf("attached volume must not fire, got %+v", findings)
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached recorded once for an attached_to edge, got %+v", skips)
	}
	if skips[rules.SkipMissingAttr] != 0 {
		t.Errorf("attached volume must skip as in_use, not missing_attribute, got %+v", skips)
	}
}

// TestEval_AttachmentCountNonzero_Skips covers G5's attribute half: even with
// no graph edge, a nonzero attachment_count means something is attached and
// the volume must skip as attached.
func TestEval_AttachmentCountNonzero_Skips(t *testing.T) {
	n := volumeNode()
	n.SetAttr(awsrules.AttrAttachmentCount, 1.0)
	findings, skips := runEval(t, []*graph.Node{n}, nil, gp3Pricer(), time.Time{})

	if len(findings) != 0 {
		t.Fatalf("volume with attachments must not fire, got %+v", findings)
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached recorded once for nonzero attachment_count, got %+v", skips)
	}
}

// TestEval_NonBillingState_Skips covers G6: a volume in any state other than
// "available" is not a stable, billing, unattached charge — in-use means
// attached, and creating/deleting/deleted/error accrue no stable bill — so it
// must skip as non_billing_status, never fire.
func TestEval_NonBillingState_Skips(t *testing.T) {
	for _, state := range []string{
		awsrules.StateInUse,
		awsrules.StateCreating,
		awsrules.StateDeleting,
		awsrules.StateDeleted,
		awsrules.StateError,
	} {
		t.Run(state, func(t *testing.T) {
			n := volumeNode()
			n.SetAttr(awsrules.AttrState, state)
			findings, skips := runEval(t, []*graph.Node{n}, nil, gp3Pricer(), time.Time{})

			if len(findings) != 0 {
				t.Fatalf("want 0 findings for a volume in state %s, got %+v", state, findings)
			}
			if skips[rules.SkipNonBillingStatus] != 1 {
				t.Errorf("want SkipNonBillingStatus recorded once for %s, got %+v", state, skips)
			}
		})
	}
}

// TestEval_MissingAttr_Skips covers G1-G4: a volume missing a shape field must
// skip as missing_attribute rather than fire at some assumed price. (Each
// missing shape field shares the same code; a couple of representatives prove
// the gates are live — deleting either check fails this test.)
func TestEval_MissingAttr_Skips(t *testing.T) {
	for _, name := range []string{"size_gb", "volume_type", "state", "create_time"} {
		t.Run(name, func(t *testing.T) {
			n := volumeNode()
			delete(n.Attrs, name)
			findings, skips := runEval(t, []*graph.Node{n}, nil, gp3Pricer(), time.Time{})

			if len(findings) != 0 {
				t.Fatalf("volume missing %s must not fire, got %+v", name, findings)
			}
			if skips[rules.SkipMissingAttr] != 1 {
				t.Errorf("want SkipMissingAttr recorded once for missing %s, got %+v", name, skips)
			}
		})
	}
}

// TestEval_DetachedDaysBoundary checks both sides of MinDetachedDays=7:
// exactly 7 days fires (7.0 is NOT < 7.0), 6 days skips as recently detached.
// Deleting the `days < MinDetachedDays` gate makes the 6-day case fire and
// this test fails.
func TestEval_DetachedDaysBoundary(t *testing.T) {
	t.Run("exactly 7 days fires", func(t *testing.T) {
		n := volumeNode()
		now := created.Add(7 * 24 * time.Hour)
		findings, _ := runEval(t, []*graph.Node{n}, nil, gp3Pricer(), now)
		if len(findings) != 1 {
			t.Fatalf("want 1 finding at exactly 7 days, got %d (%+v)", len(findings), findings)
		}
	})
	t.Run("6 days, just under, skips", func(t *testing.T) {
		n := volumeNode()
		now := created.Add(6 * 24 * time.Hour)
		findings, skips := runEval(t, []*graph.Node{n}, nil, gp3Pricer(), now)
		if len(findings) != 0 {
			t.Fatalf("6-day-old volume must not fire, got %+v", findings)
		}
		if skips[rules.SkipRecentlyDetached] != 1 {
			t.Errorf("want SkipRecentlyDetached recorded once at 6 days, got %+v", skips)
		}
	})
}

// TestEval_NoPrice_Skips covers the capacity price gate: the rule must never
// assume $0 for a volume whose SKU/region cannot be priced (Invariant I4).
func TestEval_NoPrice_Skips(t *testing.T) {
	n := volumeNode()
	findings, skips := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{}}, // nothing priced
		time.Time{})

	if len(findings) != 0 {
		t.Fatalf("volume with no price must not fire at $0, got %+v", findings)
	}
	if skips[rules.SkipNoPrice] != 1 {
		t.Errorf("want SkipNoPrice recorded once, got %+v", skips)
	}
}

// TestEval_BelowMinWaste_Skips covers the noise floor: a tiny volume whose
// computed waste falls under $0.10/month must skip as below_min_waste. A
// 1 GiB gp3 at $0.08 = $0.08 < $0.10.
func TestEval_BelowMinWaste_Skips(t *testing.T) {
	n := volumeNode()
	n.SetAttr(awsrules.AttrSizeGB, 1.0)
	findings, skips := runEval(t, []*graph.Node{n}, nil, gp3Pricer(), time.Time{})

	if len(findings) != 0 {
		t.Fatalf("sub-noise-floor volume must not fire, got %+v", findings)
	}
	if skips[rules.SkipBelowMinWaste] != 1 {
		t.Errorf("want SkipBelowMinWaste recorded once, got %+v", skips)
	}
}

// TestEval_TelluryExemptLabel_Skips is the P0 short-circuit: even a perfectly
// valid firing volume carrying tellury-exempt=true must be skipped for the
// label and never produce a finding nor reach any other predicate.
func TestEval_TelluryExemptLabel_Skips(t *testing.T) {
	n := volumeNode()
	n.Labels = map[string]string{"tellury-exempt": "true"}
	findings, skips := runEval(t, []*graph.Node{n}, nil, gp3Pricer(), time.Time{})

	if len(findings) != 0 {
		t.Fatalf("exempt volume must not fire, got %+v", findings)
	}
	if skips[rules.SkipExemptLabel] != 1 {
		t.Errorf("want SkipExemptLabel recorded once, got %+v", skips)
	}
}

// TestEval_SkipsAndFindingsDisjoint guards the invariant that a node either
// produces a finding or records a skip, never both.
func TestEval_SkipsAndFindingsDisjoint(t *testing.T) {
	findings, skips := runEval(t, []*graph.Node{volumeNode()}, nil, gp3Pricer(), time.Time{})
	if len(findings) != 1 {
		t.Fatalf("expected the firing fixture to produce exactly one finding, got %d", len(findings))
	}
	if len(skips) != 0 {
		t.Errorf("a firing volume must record zero skips (skips and findings are disjoint), got %+v", skips)
	}
}
