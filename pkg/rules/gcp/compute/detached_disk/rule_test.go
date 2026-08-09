// Package detached_disk_test exercises the detached_disk rule against graph
// fixtures shaped like REAL Cloud Asset Inventory output: a disk node with the
// normalized attributes the GCP normalizer (pkg/cloud/gcp/normalize.go) writes
// from a SearchAllResources payload, and — where relevant — an instance node
// wired up with an attached_to edge exactly as the ingestion linker would.
//
// The intent mirrors the discipline of the underutilized_instance / unused
// reserved IP regression tests: every firing case asserts the EXACT monthly
// waste figure, every skip path asserts the SPECIFIC SkipCode (never merely
// "nothing fired"), and each price-driven branch uses a fake Pricer that
// resolves only the SKUs under test so an unrelated lookup cannot mask a
// broken predicate.
package detached_disk

import (
	"context"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
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

// created is the wall-clock creation instant every disk fixture anchors to, so
// detached_days is deterministic across the whole suite.
var created = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// diskNode returns a fully valid detached disk (a pd-ssd created, never
// attached, READY, 100 GiB). Individual tests mutate specific attributes to
// force a desired skip branch; the base-case node clears every predicate up to
// the cost formula.
func diskNode() *graph.Node {
	n := &graph.Node{
		ID:       graph.Ref("//compute.googleapis.com/projects/p/zones/us-central1-a/disks/d1"),
		Kind:     graph.KindDisk,
		Name:     "d1",
		Project:  "p",
		Location: "us-central1-a", // RegionOf -> "us-central1"
	}
	n.SetAttr("size_gb", 100.0)
	n.SetAttr("disk_type", "pd-ssd")
	n.SetAttr("status", "READY")
	n.SetAttr("creation_timestamp", created.Format(time.RFC3339))
	return n
}

// runEval builds a one-node (or one-disk-plus-instance) graph, freezes it, and
// runs the rule, returning findings and the skip tally.
func runEval(t *testing.T, nodes []*graph.Node, edges []graph.Edge, pricer pricing.Pricer, now time.Time) ([]rules.Finding, map[rules.SkipCode]int) {
	t.Helper()
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

// TestEval_DetachedDisk_Fires is the primary firing case: a 100 GiB pd-ssd,
// never attached, READY, detached 19 days (>= MinDetachedDays). The exact
// waste is sizeGB * capPrice = 100 * 0.170 = $17.00 — there is no IOPS or
// throughput charge for pd-ssd, and the fake pricer reports ErrNoPrice for
// those legs (which the rule treats as zero).
func TestEval_DetachedDisk_Fires(t *testing.T) {
	n := diskNode()
	now := created.Add(19 * 24 * time.Hour) // 19 days detached
	findings, skips := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity: {"pd-ssd": 0.170},
		}},
		now,
	)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	if f.MonthlyWasteUSD != 17.00 {
		t.Errorf("MonthlyWasteUSD = %v, want 17.00 (100 GiB * $0.170/GiB)", f.MonthlyWasteUSD)
	}
	if f.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0 (never_attached + READY)", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing disk, got %+v", skips)
	}

	// Evidence must reflect the never_attached age basis that produced the
	// confidence figure — this pins the detachedDays three-branch function.
	foundBasis, foundDays := false, false
	for _, ev := range f.Evidence {
		if ev.Key == "age_basis" && ev.Value == "never_attached" {
			foundBasis = true
		}
		if ev.Key == "detached_days" && ev.Value == "19" {
			foundDays = true
		}
	}
	if !foundBasis {
		t.Errorf("expected evidence age_basis=never_attached, got %+v", f.Evidence)
	}
	if !foundDays {
		t.Errorf("expected evidence detached_days=19, got %+v", f.Evidence)
	}
}

// TestEval_AllCostComponents_Fires exercises the FULL cost formula
// sizeGB*capPrice + iops*iopsPrice + mbps*thrPrice with all three legs priced
// and nonzero. A hyperdisk-balanced disk at 10 GiB, 100 provisioned IOPS and
// 50 MB/s throughput:
//
//	10 * 0.088 + 100 * 0.005 + 50 * 0.040 = 0.88 + 0.50 + 2.00 = $3.38
//
// If any of the three legs were dropped from the sum (or its price ignored),
// this exact figure would no longer match.
func TestEval_AllCostComponents_Fires(t *testing.T) {
	n := diskNode()
	n.SetAttr("size_gb", 10.0) // override the 100 GiB base fixture
	n.SetAttr("disk_type", "hyperdisk-balanced")
	n.SetAttr("provisioned_iops", 100.0)
	n.SetAttr("provisioned_throughput_mbps", 50.0)
	now := created.Add(10 * 24 * time.Hour) // detached 10 days

	findings, _ := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity:   {"hyperdisk-balanced": 0.088},
			pricing.KindDiskIOPS:       {"hyperdisk-balanced": 0.005},
			pricing.KindDiskThroughput: {"hyperdisk-balanced": 0.040},
		}},
		now,
	)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	if f := findings[0]; f.MonthlyWasteUSD != 3.38 {
		t.Errorf("MonthlyWasteUSD = %v, want 3.38 (capacity 0.88 + iops 0.50 + throughput 2.00)", f.MonthlyWasteUSD)
	}
}

// TestEval_RegionalSKU_PricesRegionalSku verifies the diskSKU regional branch:
// replica_zone_count >= 2 appends "-regional" to the SKU token (pd-ssd ->
// pd-ssd-regional) and the cost must use THAT regional unit price. A 100 GiB
// regional pd-ssd at $0.340/GiB = $34.00. If diskSKU ignored the replica count
// the rule would price 100 * 0.170 = $17.00 and this assertion would fail.
func TestEval_RegionalSKU_PricesRegionalSku(t *testing.T) {
	n := diskNode()
	n.SetAttr("replica_zone_count", 2.0)
	now := created.Add(10 * 24 * time.Hour)

	findings, _ := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity: {"pd-ssd-regional": 0.340},
		}},
		now,
	)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	if f := findings[0]; f.MonthlyWasteUSD != 34.00 {
		t.Errorf("MonthlyWasteUSD = %v, want 34.00 (regional pd-ssd at $0.340/GiB)", f.MonthlyWasteUSD)
	}
}

// TestEval_LastDetachAgeBasis_Fires exercises the last_detach_time branch of
// detachedDays: with last_detach_time present it is used (instead of creation
// time) and age_basis = last_detach. The disk here was created 90 days ago but
// detached only 10 days ago; 10 days is what must drive the finding.
func TestEval_LastDetachAgeBasis_Fires(t *testing.T) {
	n := diskNode()
	lastDetach := created.Add(80 * 24 * time.Hour) // created, then detached 10 days ago
	n.SetAttr("last_detach_time", lastDetach.Format(time.RFC3339))
	now := created.Add(90 * 24 * time.Hour)

	findings, _ := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity: {"pd-ssd": 0.170},
		}},
		now,
	)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	if f := findings[0]; f.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0 (last_detach basis)", f.Confidence)
	}
	found := false
	for _, ev := range findings[0].Evidence {
		if ev.Key == "age_basis" && ev.Value == "last_detach" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected evidence age_basis=last_detach, got %+v", findings[0].Evidence)
	}
}

// TestEval_CreationFallbackAgeBasis_Fires exercises the creation_fallback
// branch: last_detach_time absent AND last_attach_time present forces the CAI
// inconsistency fallback to creation time, with confidence dropped to 0.9.
func TestEval_CreationFallbackAgeBasis_Fires(t *testing.T) {
	n := diskNode()
	n.SetAttr("last_attach_time", created.Add(70*24*time.Hour).Format(time.RFC3339))
	now := created.Add(9 * 24 * time.Hour) // 9 days detached

	findings, _ := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity: {"pd-ssd": 0.170},
		}},
		now,
	)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	if f := findings[0]; f.Confidence != 0.9 {
		t.Errorf("Confidence = %v, want 0.9 (creation_fallback basis)", f.Confidence)
	}
	found := false
	for _, ev := range findings[0].Evidence {
		if ev.Key == "age_basis" && ev.Value == "creation_fallback" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected evidence age_basis=creation_fallback, got %+v", findings[0].Evidence)
	}
}

// TestEval_TelluryExemptLabel_Skips is the P0 short-circuit: even a perfectly
// valid firing disk carrying tellury-exempt=true must be skipped for the label
// and never produce a finding nor reach any other predicate.
func TestEval_TelluryExemptLabel_Skips(t *testing.T) {
	n := diskNode()
	n.Labels = map[string]string{"tellury-exempt": "true"}
	now := created.Add(30 * 24 * time.Hour)

	findings, skips := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity: {"pd-ssd": 0.170},
		}},
		now,
	)

	if len(findings) != 0 {
		t.Fatalf("exempt disk must not fire, got %+v", findings)
	}
	if skips[rules.SkipExemptLabel] != 1 {
		t.Errorf("want SkipExemptLabel recorded once, got %+v", skips)
	}
}

// TestEval_MissingAttr_Skips covers P1 shape-validity: a disk whose disk_type
// is absent must skip as missing_attribute rather than fire at some assumed
// price. (Each missing shape field shares the same code; one representative is
// enough to prove the gate is live — deleting the disk_type check fails this.)
func TestEval_MissingAttr_Skips(t *testing.T) {
	n := diskNode()
	n.Attrs["disk_type"] = nil // force the P1 missing-attribute gate
	now := created.Add(10 * 24 * time.Hour)

	findings, skips := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity: {"pd-ssd": 0.170},
		}},
		now,
	)

	if len(findings) != 0 {
		t.Fatalf("missing disk_type must not fire, got %+v", findings)
	}
	if skips[rules.SkipMissingAttr] != 1 {
		t.Errorf("want SkipMissingAttr recorded once, got %+v", skips)
	}
}

// TestEval_AttachedToEdge_Skips covers P2: a disk with an instance
// --attached_to--> disk edge is in use and must skip as attached, with a
// DISTINCT reason from the missing-attribute path.
func TestEval_AttachedToEdge_Skips(t *testing.T) {
	disk := diskNode()
	inst := &graph.Node{
		ID:   graph.Ref("//compute.googleapis.com/projects/p/zones/us-central1-a/instances/i1"),
		Kind: graph.KindInstance,
		Name: "i1",
	}
	now := created.Add(10 * 24 * time.Hour)

	findings, skips := runEval(t, []*graph.Node{disk, inst},
		[]graph.Edge{{From: inst.ID, To: disk.ID, Kind: graph.EdgeAttachedTo}},
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity: {"pd-ssd": 0.170},
		}},
		now,
	)

	if len(findings) != 0 {
		t.Fatalf("attached disk must not fire, got %+v", findings)
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached recorded once for an attached_to edge, got %+v", skips)
	}
	if skips[rules.SkipMissingAttr] != 0 {
		t.Errorf("attached disk must skip as in_use, not missing_attribute, got %+v", skips)
	}
}

// TestEval_CAIUsers_Skips covers P3: even with no graph edge, a nonzero CAI
// user_count means something is attached and the disk must skip as attached —
// the dual-signal cross-check that prevents misreporting a disk CAI still
// reports as being used.
func TestEval_CAIUsers_Skips(t *testing.T) {
	n := diskNode()
	n.SetAttr("user_count", 1.0)
	now := created.Add(10 * 24 * time.Hour)

	findings, skips := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity: {"pd-ssd": 0.170},
		}},
		now,
	)

	if len(findings) != 0 {
		t.Fatalf("disk with users[] must not fire, got %+v", findings)
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached recorded once for nonzero user_count, got %+v", skips)
	}
}

// TestEval_NonBillingStatus_Skips covers P4: a disk mid-creation (CREATING) has
// no stable billable capacity and must skip as non_billing_status, NOT fire.
func TestEval_NonBillingStatus_Skips(t *testing.T) {
	n := diskNode()
	n.SetAttr("status", "CREATING")
	now := created.Add(10 * 24 * time.Hour)

	findings, skips := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity: {"pd-ssd": 0.170},
		}},
		now,
	)

	if len(findings) != 0 {
		t.Fatalf("CREATING disk must not fire, got %+v", findings)
	}
	if skips[rules.SkipNonBillingStatus] != 1 {
		t.Errorf("want SkipNonBillingStatus recorded once, got %+v", skips)
	}
}

// TestEval_NoPrice_Skips covers the capacity price gate: the rule must never
// assume $0 for a disk whose SKU/region cannot be priced (Invariant I4).
func TestEval_NoPrice_Skips(t *testing.T) {
	n := diskNode()
	now := created.Add(10 * 24 * time.Hour)

	findings, skips := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			// nothing priced: capacity lookup fails
		}},
		now,
	)

	if len(findings) != 0 {
		t.Fatalf("disk with no price must not fire at $0, got %+v", findings)
	}
	if skips[rules.SkipNoPrice] != 1 {
		t.Errorf("want SkipNoPrice recorded once, got %+v", skips)
	}
}

// TestEval_BelowMinWaste_Skips covers P7: a small disk whose computed waste
// falls under the $0.10 noise floor must skip as below_min_waste. A 2 GiB
// pd-standard at $0.040/GiB = $0.08 < $0.10.
func TestEval_BelowMinWaste_Skips(t *testing.T) {
	n := diskNode()
	n.SetAttr("size_gb", 2.0)
	n.SetAttr("disk_type", "pd-standard")
	now := created.Add(10 * 24 * time.Hour)

	findings, skips := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity: {"pd-standard": 0.040},
		}},
		now,
	)

	if len(findings) != 0 {
		t.Fatalf("sub-noise-floor disk must not fire, got %+v", findings)
	}
	if skips[rules.SkipBelowMinWaste] != 1 {
		t.Errorf("want SkipBelowMinWaste recorded once, got %+v", skips)
	}
}

// TestEval_DetachedDaysBoundary checks both sides of MinDetachedDays=7:
// exactly 7 days fired (7.0 is NOT < 7.0), 6 days skipped as recently
// detached. Deleting the `detachedDays < MinDetachedDays` gate makes the 6-day
// case fire and this test fails.
func TestEval_DetachedDaysBoundary(t *testing.T) {
	pricer := diskPricer{unit: map[pricing.Kind]map[string]float64{
		pricing.KindDiskCapacity: {"pd-ssd": 0.170},
	}}

	t.Run("exactly 7 days fires", func(t *testing.T) {
		n := diskNode()
		now := created.Add(7 * 24 * time.Hour) // detachedDays == 7.0
		findings, _ := runEval(t, []*graph.Node{n}, nil, pricer, now)
		if len(findings) != 1 {
			t.Fatalf("want 1 finding at exactly 7 days, got %d (%+v)", len(findings), findings)
		}
	})

	t.Run("6 days, just under, skips", func(t *testing.T) {
		n := diskNode()
		now := created.Add(6 * 24 * time.Hour) // detachedDays == 6.0
		findings, skips := runEval(t, []*graph.Node{n}, nil, pricer, now)
		if len(findings) != 0 {
			t.Fatalf("6-day-old disk must not fire, got %+v", findings)
		}
		if skips[rules.SkipRecentlyDetached] != 1 {
			t.Errorf("want SkipRecentlyDetached recorded once at 6 days, got %+v", skips)
		}
	})
}

// TestEval_SkipsAndFindingsDisjoint guards the invariant that a node either
// produces a finding or records a skip, never both (same discipline as
// underutilized_instance's NoDoubleCounting regression).
func TestEval_SkipsAndFindingsDisjoint(t *testing.T) {
	n := diskNode()
	now := created.Add(19 * 24 * time.Hour)

	findings, skips := runEval(t, []*graph.Node{n}, nil,
		diskPricer{unit: map[pricing.Kind]map[string]float64{
			pricing.KindDiskCapacity: {"pd-ssd": 0.170},
		}},
		now,
	)

	if len(findings) != 1 {
		t.Fatalf("expected the firing fixture to produce exactly one finding, got %d", len(findings))
	}
	if len(skips) != 0 {
		t.Errorf("a firing disk must record zero skips (skips and findings are disjoint), got %+v", skips)
	}
}
