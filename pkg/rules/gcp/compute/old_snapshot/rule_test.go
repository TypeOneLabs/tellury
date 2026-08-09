package old_snapshot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// now is the fixed evaluation instant behind every test so age predicates are
// deterministic (the same --at discipline the CLI tests use).
var now = time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)

// fakePricer prices exactly the snapshot-storage SKU this rule looks up;
// every other lookup misses, matching ErrNoPrice semantics.
type fakePricer struct {
	unit float64
}

func (f fakePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if kind == pricing.KindSnapshotStorage && sku == SnapshotStorageSKU {
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

// snapshotNode builds a snapshot node. gib is the BILLABLE size — the
// incremental bytes Google charges for — not the source disk's size. The
// helper also writes a deliberately much larger source_disk_size_gb, so any
// test that accidentally prices the disk instead of the billable bytes
// produces an obviously wrong number rather than a plausible one.
func snapshotNode(gib float64, createdAt string) *graph.Node {
	n := &graph.Node{
		ID:       graph.Ref("//compute.googleapis.com/projects/p/global/snapshots/s1"),
		Kind:     graph.KindSnapshot,
		Name:     "s1",
		Project:  "p",
		Location: "us-central1",
	}
	n.SetAttr("storage_bytes", gib*(1<<30))
	n.SetAttr("source_disk_size_gb", gib*10)
	n.SetAttr("creation_timestamp", createdAt)
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
		Now: now,
	}
	findings, err := rules.AdaptNodeRule(rule{}).Eval(context.Background(), p)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	return findings, skipCounts
}

// TestEval_OldSnapshot_Fires is the firing case: a snapshot well past the
// 90-day window. The entire monthly storage cost is reported as waste.
//
// MUTATION CHECK (mandatory, performed before this PR was opened):
//  1. Set MaxAgeDays = 200 in rule.go, leaving this test unchanged.
//  2. `go test ./pkg/rules/gcp/compute/old_snapshot/ -run TestEval_OldSnapshot_Fires -count=1`
//     → FAILED: "want 1 finding, got 0" — a 120-day-old snapshot no longer
//     cleared the (mutated) 200-day gate, so the test proved it actually
//     detects the age condition rather than merely running.
//  3. Restored MaxAgeDays = 90. Test passes again.
func TestEval_OldSnapshot_Fires(t *testing.T) {
	createdAt := now.AddDate(0, 0, -120) // 120 days old — past the 90-day window
	n := snapshotNode(250, createdAt.Format(time.RFC3339))
	findings, skips := runEval(t, n, 0.026)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	// 250 GiB x $0.026/GiB-month = $6.50/month, exactly.
	if f.MonthlyWasteUSD != 6.50 {
		t.Errorf("MonthlyWasteUSD = %v, want 6.50 (250 GiB x $0.026/GiB-month)", f.MonthlyWasteUSD)
	}
	if f.Confidence != 0.7 {
		t.Errorf("Confidence = %v, want 0.7 (deterministic age gate, interpretive delete judgment)", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing resource, got %+v", skips)
	}

	// Evidence: auto-collected attrs first, then guard-computed values.
	byKey := map[string]string{}
	for _, e := range f.Evidence {
		byKey[e.Key] = e.Value
	}
	if byKey["source_disk_size_gb"] != "2500" {
		t.Errorf("evidence source_disk_size_gb = %q, want 2500", byKey["source_disk_size_gb"])
	}
	if byKey["creation_timestamp"] != createdAt.Format(time.RFC3339) {
		t.Errorf("evidence creation_timestamp = %q, want %q", byKey["creation_timestamp"], createdAt.Format(time.RFC3339))
	}
	if byKey["age_days"] != "120" {
		t.Errorf("evidence age_days = %q, want 120", byKey["age_days"])
	}
	if byKey["unit_price_gib_month"] != "$0.0260" {
		t.Errorf("evidence unit_price_gib_month = %q, want $0.0260", byKey["unit_price_gib_month"])
	}
	if !strings.Contains(byKey["price_source"], "sku=standard") {
		t.Errorf("evidence price_source = %q, want it to name sku=standard", byKey["price_source"])
	}
}

// TestEval_AgeBoundary pins the exact boundary: 90 days fires, 89 days skips.
// This is the mutation-sensitive test — a threshold typo of one day flips it.
func TestEval_AgeBoundary(t *testing.T) {
	at90 := now.AddDate(0, 0, -MaxAgeDays) // exactly the window: fires
	findings, skips := runEval(t, snapshotNode(100, at90.Format(time.RFC3339)), 0.026)
	if len(findings) != 1 {
		t.Fatalf("at exactly %d days want 1 finding, got %d (%+v)", MaxAgeDays, len(findings), findings)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips at the boundary, got %+v", skips)
	}

	at89 := now.AddDate(0, 0, -(MaxAgeDays - 1)) // one day inside the window: skips
	findings, skips = runEval(t, snapshotNode(100, at89.Format(time.RFC3339)), 0.026)
	if len(findings) != 0 {
		t.Fatalf("at %d days want 0 findings, got %+v", MaxAgeDays-1, findings)
	}
	if skips[rules.SkipTooYoung] != 1 {
		t.Errorf("want SkipTooYoung recorded once, got %+v", skips)
	}
}

// TestEval_YoungSnapshot_Skips: a snapshot inside the retention window is a
// live recovery point, not waste — and must record a DIFFERENT skip reason
// than a malformed payload so --explain-skips can tell them apart.
func TestEval_YoungSnapshot_Skips(t *testing.T) {
	createdAt := now.AddDate(0, 0, -10) // 10 days old
	n := snapshotNode(250, createdAt.Format(time.RFC3339))
	findings, skips := runEval(t, n, 0.026)

	if len(findings) != 0 {
		t.Fatalf("want 0 findings for a young snapshot, got %+v", findings)
	}
	if skips[rules.SkipTooYoung] != 1 {
		t.Errorf("want SkipTooYoung recorded once, got %+v", skips)
	}
	if skips[rules.SkipMissingAttr] != 0 {
		t.Errorf("a young-but-well-formed snapshot must not be reported as missing attribute, got %+v", skips)
	}
}

// TestEval_MissingSize_Skips: a payload that never produced storage_bytes has
// no billable size the rule can price, and the rule refuses to guess. This is
// distinct from a snapshot whose billable size is genuinely zero, below.
func TestEval_MissingSize_Skips(t *testing.T) {
	createdAt := now.AddDate(0, 0, -120)
	n := snapshotNode(0, createdAt.Format(time.RFC3339))
	delete(n.Attrs, "storage_bytes") // payload not parsed, not "free"
	findings, skips := runEval(t, n, 0.026)

	if len(findings) != 0 {
		t.Fatalf("want 0 findings without a billable size, got %+v", findings)
	}
	if skips[rules.SkipMissingAttr] != 1 {
		t.Errorf("want SkipMissingAttr recorded once, got %+v", skips)
	}
}

// TestEval_FullyDeduplicatedSnapshot_CostsNothing pins the case that exposed
// the original defect. A snapshot fully deduplicated against the rest of its
// chain occupies zero billable bytes and costs nothing, even though its source
// disk was large — a real organization had exactly this: a 10 GiB source disk
// with storageBytes 0, which the rule used to report as $0.26/month of waste.
//
// It must skip as immaterial, NOT as missing data: the payload parsed fine and
// the answer is simply "this is free".
func TestEval_FullyDeduplicatedSnapshot_CostsNothing(t *testing.T) {
	createdAt := now.AddDate(0, 0, -120)
	n := snapshotNode(0, createdAt.Format(time.RFC3339)) // storage_bytes 0, source disk 0*10
	n.SetAttr("source_disk_size_gb", 10.0)               // large source, nothing billable
	findings, skips := runEval(t, n, 0.026)

	if len(findings) != 0 {
		t.Fatalf("a fully deduplicated snapshot is free; want 0 findings, got %+v", findings)
	}
	if skips[rules.SkipMissingAttr] != 0 {
		t.Errorf("zero billable bytes is parsed data, not missing data; got %+v", skips)
	}
	if skips[rules.SkipBelowMinWaste] != 1 {
		t.Errorf("want SkipBelowMinWaste recorded once, got %+v", skips)
	}
}

// TestEval_NoPrice_Skips: the rule never assumes $0 when the SKU/region does
// not resolve (Invariant I4).
func TestEval_NoPrice_Skips(t *testing.T) {
	createdAt := now.AddDate(0, 0, -120)
	n := snapshotNode(250, createdAt.Format(time.RFC3339))
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
		Now: now,
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

// TestEval_BelowMinWaste_Skips: a snapshot that clears every guard but whose
// computed waste falls under the noise floor is skipped as SkipBelowMinWaste,
// not reported. 1 GiB x $0.026 = $0.026 < MinMonthlyWasteUSD ($0.10).
func TestEval_BelowMinWaste_Skips(t *testing.T) {
	createdAt := now.AddDate(0, 0, -120)
	n := snapshotNode(1, createdAt.Format(time.RFC3339))
	findings, skips := runEval(t, n, 0.026)

	if len(findings) != 0 {
		t.Fatalf("want 0 findings below the noise floor, got %+v", findings)
	}
	if skips[rules.SkipBelowMinWaste] != 1 {
		t.Errorf("want SkipBelowMinWaste recorded once, got %+v", skips)
	}
}

type noPricePricer struct{}

func (noPricePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	return 0, "", pricing.ErrNoPrice
}

func (noPricePricer) MonthlyCost(it pricing.Item) (float64, error) {
	return 0, pricing.ErrNoPrice
}
