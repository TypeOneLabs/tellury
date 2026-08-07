// Package no_lifecycle_policy_test exercises the no_lifecycle_policy rule
// against graph fixtures shaped like REAL Cloud Asset Inventory output: a
// bucket node carrying the normalized attributes the GCP normalizer writes
// from a SearchAllResources payload, with a bucket_total_bytes_mean metric
// attached the way the enrichment pass would.
//
// The discipline mirrors the other shipped-rule tests: the firing case asserts
// the EXACT monthly waste figure from the cost formula, every skip path
// asserts the SPECIFIC SkipCode (never merely "nothing fired"), and the fake
// Pricer resolves only the STANDARD/NEARLINE storage classes so an unrelated
// lookup cannot mask a broken cost branch.
package no_lifecycle_policy

import (
	"context"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// gcsPricer prices an exact (Kind, sku-token) surface and nothing else. Only
// KindGCSStorage for the two classes under test is configured; any other
// lookup misses with ErrNoPrice, matching pricing.Pricer's contract.
type gcsPricer struct {
	unit map[string]float64 // storage-class token -> USD per GiB/month
}

func (f gcsPricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if kind != pricing.KindGCSStorage {
		return 0, "", pricing.ErrNoPrice
	}
	price, ok := f.unit[sku]
	if !ok {
		return 0, "", pricing.ErrNoPrice
	}
	return price, region, nil
}

func (f gcsPricer) MonthlyCost(it pricing.Item) (float64, error) {
	unit, _, err := f.UnitPrice(it.Kind, it.Provider, it.SKU, it.Region)
	if err != nil {
		return 0, err
	}
	return unit * it.Quantity, nil
}

// bucketNode returns a fully valid, un-lifecycled STANDARD bucket with a solid
// 100 GiB byte metric (100 samples, full coverage, 7-day window) that clears
// every predicate up to the class-transition cost formula. Individual tests
// mutate specific attributes / drop the metric to force a desired branch.
func bucketNode(totalBytes float64) *graph.Node {
	n := &graph.Node{
		ID:       graph.Ref("//storage.googleapis.com/projects/_/buckets/b1"),
		Kind:     graph.KindBucket,
		Name:     "b1",
		Project:  "p",
		Location: "us-central1", // RegionOf -> "us-central1"; both classes pricable
	}
	n.SetAttr("bucket_name", "b1")
	n.SetAttr("lifecycle_rule_count", 0.0)
	n.SetAttr("storage_class", "STANDARD")
	n.Metrics = map[string]graph.MetricValue{
		metrics.KeyBucketTotalBytesMean: {
			Value:      totalBytes,
			Unit:       "bytes",
			Stat:       "mean",
			WindowDays: 7,
			Samples:    100,
			Coverage:   1.0,
			ExpectedSamples: 100,
		},
	}
	return n
}

func runEval(t *testing.T, nodes []*graph.Node, pricer pricing.Pricer) ([]rules.Finding, map[rules.SkipCode]int) {
	t.Helper()
	g := graph.New()
	for _, n := range nodes {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: pricer,
		Skip: func(ruleID string, id graph.Ref, code rules.SkipCode) {
			skipCounts[code]++
		},
	}
	findings, err := rule{}.Eval(context.Background(), p)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	return findings, skipCounts
}

// defaultPricer prices STANDARD at $0.020/GiB and NEARLINE at $0.010/GiB in
// every region, the fixed embedded-table values. delta = $0.010/GiB.
func defaultPricer() gcsPricer {
	return gcsPricer{unit: map[string]float64{
		"STANDARD": 0.020,
		"NEARLINE": 0.010,
	}}
}

// TestEval_BucketNoLifecycle_Fires is the primary firing case: a 100 GiB
// STANDARD bucket with zero lifecycle rules. Exact waste from the cost model:
//
//	monthlyWaste = totalGiB * ColdFraction * (STANDARD - NEARLINE)
//	            = 100 * 0.60 * (0.020 - 0.010)
//	            = 100 * 0.60 * 0.010
//	            = $0.60
//
// If any of ColdFraction, the from/to price, or the GiB conversion were wrong,
// this exact figure would no longer match.
func TestEval_BucketNoLifecycle_Fires(t *testing.T) {
	const totalBytes = 100 << 30 // 100 GiB
	n := bucketNode(float64(totalBytes))
	findings, skips := runEval(t, []*graph.Node{n}, defaultPricer())

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	if f.MonthlyWasteUSD != 0.60 {
		t.Errorf("MonthlyWasteUSD = %v, want 0.60 (100 GiB * 0.60 cold * $0.010 delta)", f.MonthlyWasteUSD)
	}
	if f.Confidence != 0.60 {
		t.Errorf("Confidence = %v, want 0.60 (fixed rule constant)", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing bucket, got %+v", skips)
	}

	// The cost model's two pricers must both be accounted for in the evidence.
	seenFrom, seenTo := false, false
	for _, ev := range f.Evidence {
		if ev.Key == "delta_per_gb_month" && ev.Value == "$0.0100" {
			seenFrom = true
		}
		if ev.Key == "from_price_source" && len(ev.Value) > 0 {
			seenTo = true
		}
	}
	if !seenFrom {
		t.Errorf("expected evidence delta_per_gb_month=$0.0100, got %+v", f.Evidence)
	}
	if !seenTo {
		t.Errorf("expected evidence from_price_source, got %+v", f.Evidence)
	}
}

// TestEval_LegacyStandardClasses_Fire pins normalizeStorageClass: a bucket
// whose storage_class is the legacy REGIONAL spelling (which bills at STANDARD
// rates) must FIRE, not be skipped as not-standard. 200 GiB of REGIONAL-stored
// bytes: 200 * 0.60 * 0.010 = $1.20.
func TestEval_LegacyStandardClasses_Fire(t *testing.T) {
	n := bucketNode(float64(200 << 30))
	n.SetAttr("storage_class", "REGIONAL")
	findings, _ := runEval(t, []*graph.Node{n}, defaultPricer())

	if len(findings) != 1 {
		t.Fatalf("REGIONAL (legacy STANDARD-equivalent) bucket must fire, got %d findings (%+v)", len(findings), findings)
	}
	if f := findings[0]; f.MonthlyWasteUSD != 1.20 {
		t.Errorf("MonthlyWasteUSD = %v, want 1.20 (200 * 0.60 * 0.010)", f.MonthlyWasteUSD)
	}
}

// TestEval_TelluryExemptLabel_Skips is the P0 short-circuit: even a perfectly
// valid firing bucket carrying tellury-exempt=true must skip for the label and
// never reach any other predicate.
func TestEval_TelluryExemptLabel_Skips(t *testing.T) {
	n := bucketNode(float64(100 << 30))
	n.Labels = map[string]string{"tellury-exempt": "true"}
	findings, skips := runEval(t, []*graph.Node{n}, defaultPricer())

	if len(findings) != 0 {
		t.Fatalf("exempt bucket must not fire, got %+v", findings)
	}
	if skips[rules.SkipExemptLabel] != 1 {
		t.Errorf("want SkipExemptLabel recorded once, got %+v", skips)
	}
}

// TestEval_HasLifecycle_Skips covers the detection gate: a bucket with a
// nonzero lifecycle_rule_count must skip as has_lifecycle, not fire, even
// though the class is STANDARD and the bytes are well above the floor.
func TestEval_HasLifecycle_Skips(t *testing.T) {
	n := bucketNode(float64(100 << 30))
	n.SetAttr("lifecycle_rule_count", 2.0)
	findings, skips := runEval(t, []*graph.Node{n}, defaultPricer())

	if len(findings) != 0 {
		t.Fatalf("bucket with lifecycle rules must not fire, got %+v", findings)
	}
	if skips[rules.SkipHasLifecycle] != 1 {
		t.Errorf("want SkipHasLifecycle recorded once, got %+v", skips)
	}
}

// TestEval_NotStandardClass_Skips covers detection: a NEARLINE-class bucket has
// no STANDARD->NEARLINE transition available and must skip as
// not_standard_class, with a DISTINCT reason from has_lifecycle.
func TestEval_NotStandardClass_Skips(t *testing.T) {
	n := bucketNode(float64(100 << 30))
	n.SetAttr("storage_class", "NEARLINE")
	findings, skips := runEval(t, []*graph.Node{n}, defaultPricer())

	if len(findings) != 0 {
		t.Fatalf("NEARLINE-class bucket must not fire, got %+v", findings)
	}
	if skips[rules.SkipNotStandardClass] != 1 {
		t.Errorf("want SkipNotStandardClass recorded once, got %+v", skips)
	}
	if skips[rules.SkipHasLifecycle] != 0 {
		t.Errorf("NEARLINE bucket must not be reported as has_lifecycle, got %+v", skips)
	}
}

// TestEval_Autoclass_Skips covers the Autoclass supersession gate: a STANDARD
// bucket with no manual lifecycle but Autoclass enabled must skip as
// autoclass_enabled.
func TestEval_Autoclass_Skips(t *testing.T) {
	n := bucketNode(float64(100 << 30))
	n.SetAttr("autoclass_enabled", true)
	findings, skips := runEval(t, []*graph.Node{n}, defaultPricer())

	if len(findings) != 0 {
		t.Fatalf("Autoclass bucket must not fire, got %+v", findings)
	}
	if skips[rules.SkipAutoclass] != 1 {
		t.Errorf("want SkipAutoclass recorded once, got %+v", skips)
	}
}

// TestEval_RetentionLocked_Skips covers the retention-lock gate: a bucket whose
// retention policy is locked prevents any class transition and must skip as
// retention_locked.
func TestEval_RetentionLocked_Skips(t *testing.T) {
	n := bucketNode(float64(100 << 30))
	n.SetAttr("retention_locked", true)
	findings, skips := runEval(t, []*graph.Node{n}, defaultPricer())

	if len(findings) != 0 {
		t.Fatalf("retention-locked bucket must not fire, got %+v", findings)
	}
	if skips[rules.SkipRetentionLocked] != 1 {
		t.Errorf("want SkipRetentionLocked recorded once, got %+v", skips)
	}
}

// TestEval_NoMetric_Skips covers the metric gate (Invariant I5): a bucket with
// no byte metric (or too few samples) must skip as no_metric, never fire at an
// assumed $0.
func TestEval_NoMetric_Skips(t *testing.T) {
	n := bucketNode(float64(100 << 30))
	n.Metrics = map[string]graph.MetricValue{} // drop the metric entirely
	findings, skips := runEval(t, []*graph.Node{n}, defaultPricer())

	if len(findings) != 0 {
		t.Fatalf("bucket with no byte metric must not fire, got %+v", findings)
	}
	if skips[rules.SkipNoMetric] != 1 {
		t.Errorf("want SkipNoMetric recorded once, got %+v", skips)
	}
}

// TestEval_InsufficientMetricCoverage_Skips exercises the coverage branch of
// MetricOK's gate (Samples >= 7 AND Coverage >= 0.20): a bucket with plenty of
// samples but sub-20% coverage must skip as no_metric, not fire on a
// low-confidence mean.
func TestEval_InsufficientMetricCoverage_Skips(t *testing.T) {
	n := bucketNode(float64(100 << 30))
	n.Metrics[metrics.KeyBucketTotalBytesMean] = graph.MetricValue{
		Value:          float64(100 << 30),
		Unit:           "bytes",
		Stat:           "mean",
		WindowDays:     7,
		Samples:        100,
		Coverage:       0.10, // below the 0.20 gate
		ExpectedSamples: 100,
	}
	findings, skips := runEval(t, []*graph.Node{n}, defaultPricer())

	if len(findings) != 0 {
		t.Fatalf("low-coverage bucket must not fire, got %+v", findings)
	}
	if skips[rules.SkipNoMetric] != 1 {
		t.Errorf("want SkipNoMetric recorded once for low coverage, got %+v", skips)
	}
}

// TestEval_NoPrice_Skips covers the price gate: the rule must never assume a
// $0 delta when either STANDARD or NEARLINE fails to resolve in a region.
// Here both classes are unknowable, so the from-price lookup fails first.
func TestEval_NoPrice_Skips(t *testing.T) {
	n := bucketNode(float64(100 << 30))
	findings, skips := runEval(t, []*graph.Node{n}, gcsPricer{unit: map[string]float64{}})

	if len(findings) != 0 {
		t.Fatalf("bucket with no resolvable prices must not fire at a $0 delta, got %+v", findings)
	}
	if skips[rules.SkipNoPrice] != 1 {
		t.Errorf("want SkipNoPrice recorded once, got %+v", skips)
	}
}

// TestEval_MinBytesBoundary checks both sides of MinBytes = 10 GiB:
// exactly 10 GiB fires (10 GiB is NOT < MinBytes), and 10 GiB - 1 byte skips
// as below_min_bytes. Deleting the `totalBytes < MinBytes` gate makes the
// sub-floor case fire and this test fails.
//
// Waste at exactly 10 GiB: 10 * 0.60 * 0.010 = $0.06.
func TestEval_MinBytesBoundary(t *testing.T) {
	const minBytes float64 = 10 << 30

	t.Run("exactly MinBytes fires", func(t *testing.T) {
		n := bucketNode(minBytes)
		findings, _ := runEval(t, []*graph.Node{n}, defaultPricer())
		if len(findings) != 1 {
			t.Fatalf("want 1 finding at exactly 10 GiB, got %d (%+v)", len(findings), findings)
		}
		if f := findings[0]; f.MonthlyWasteUSD != 0.06 {
			t.Errorf("MonthlyWasteUSD = %v, want 0.06 (10 * 0.60 * 0.010)", f.MonthlyWasteUSD)
		}
	})

	t.Run("one byte under MinBytes skips", func(t *testing.T) {
		n := bucketNode(minBytes - 1)
		findings, skips := runEval(t, []*graph.Node{n}, defaultPricer())
		if len(findings) != 0 {
			t.Fatalf("sub-MinBytes bucket must not fire, got %+v", findings)
		}
		if skips[rules.SkipBelowMinBytes] != 1 {
			t.Errorf("want SkipBelowMinBytes recorded once, got %+v", skips)
		}
	})
}

// TestEval_SkipsAndFindingsDisjoint guards the invariant that a node either
// produces a finding or records a skip, never both.
func TestEval_SkipsAndFindingsDisjoint(t *testing.T) {
	n := bucketNode(float64(100 << 30))
	findings, skips := runEval(t, []*graph.Node{n}, defaultPricer())

	if len(findings) != 1 {
		t.Fatalf("expected the firing fixture to produce exactly one finding, got %d", len(findings))
	}
	if len(skips) != 0 {
		t.Errorf("a firing bucket must record zero skips (skips and findings are disjoint), got %+v", skips)
	}
}
