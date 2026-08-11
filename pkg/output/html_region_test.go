package output

import (
	"math"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// ---------------------------------------------------------------------------
// Waste-by-region: the third grouping of the same findings
// ---------------------------------------------------------------------------

// TestWasteByRegion_AggregatesCorrectly pins the aggregation arithmetic: two
// findings in us-central1 sum to one bar, one finding in eu to another, and
// the rows' total equals the findings total — the same findings sliced a third
// way, so the rollup invariant (report totals == findings totals) holds by
// construction.
func TestWasteByRegion_AggregatesCorrectly(t *testing.T) {
	findings := []rules.Finding{
		{RuleID: "a", Resource: "disk/1", Project: "p1", Location: "us-central1", MonthlyWasteUSD: 8.00},
		{RuleID: "b", Resource: "disk/2", Project: "p1", Location: "us-central1", MonthlyWasteUSD: 2.50},
		{RuleID: "c", Resource: "disk/3", Project: "p1", Location: "eu", MonthlyWasteUSD: 7.30},
	}
	rows := wasteByRegion(findings)
	if len(rows) != 2 {
		t.Fatalf("wasteByRegion rows = %d, want 2 (us-central1 + eu)", len(rows))
	}
	byLabel := map[string]float64{}
	for _, row := range rows {
		byLabel[row.Label] = row.Total
	}
	if math.Abs(byLabel["us-central1"]-10.50) > 1e-9 {
		t.Errorf("us-central1 total = %.2f, want 10.50 (8.00+2.50)", byLabel["us-central1"])
	}
	if math.Abs(byLabel["eu"]-7.30) > 1e-9 {
		t.Errorf("eu total = %.2f, want 7.30", byLabel["eu"])
	}
	sum := 0.0
	for _, row := range rows {
		sum += row.Total
	}
	if pricing.Round2(sum) != pricing.Round2(8.00+2.50+7.30) {
		t.Errorf("region rows sum to %.2f, want the findings total %.2f", sum, 8.00+2.50+7.30)
	}
}

// TestWasteByRegion_SingleRegionSkipped: a single bar at 100% is decoration,
// not information — the same rule that gates the waste-by-project chart. A
// scan whose findings all live in one region renders no waste-by-region
// section at all.
func TestWasteByRegion_SingleRegionSkipped(t *testing.T) {
	findings := []rules.Finding{
		{RuleID: "a", Resource: "disk/1", Project: "p1", Location: "us-central1", MonthlyWasteUSD: 8.00},
		{RuleID: "b", Resource: "disk/2", Project: "p1", Location: "us-central1", MonthlyWasteUSD: 2.50},
	}
	r := baseReport(findings)
	got := renderHTML(t, r)

	if strings.Contains(got, "Waste by region") {
		t.Errorf("a single-region scan must not render the waste-by-region section:\n%s", got)
	}
	if strings.Contains(got, `aria-label="Monthly waste by region`) {
		t.Errorf("a single-region scan must not render the region bar chart")
	}
}

// TestWasteByRegion_EmptyLocation: a finding with no location groups under an
// explicit "(unknown region)" row, never an empty label — a reader must be
// able to see that some waste could not be attributed geographically.
func TestWasteByRegion_EmptyLocation(t *testing.T) {
	findings := []rules.Finding{
		{RuleID: "a", Resource: "disk/1", Project: "p1", Location: "", MonthlyWasteUSD: 4.00},
		{RuleID: "b", Resource: "disk/2", Project: "p1", Location: "eu", MonthlyWasteUSD: 6.00},
	}
	rows := wasteByRegion(findings)
	byLabel := map[string]float64{}
	for _, row := range rows {
		byLabel[row.Label] = row.Total
	}
	if _, ok := byLabel["(unknown region)"]; !ok {
		t.Errorf("a locationless finding must group under \"(unknown region)\"; got %v", byLabel)
	}
	if math.Abs(byLabel["(unknown region)"]-4.00) > 1e-9 {
		t.Errorf("(unknown region) total = %.2f, want 4.00", byLabel["(unknown region)"])
	}
}

// TestWasteByRegion_DoesNotAffectTotal is the rollup invariant extended to
// the region tier: the hero number, the waste-by-project bars, the
// waste-by-region bars and the waste-by-rule table all sum the SAME findings,
// so they must all equal TotalMonthlyWasteUSD. The fixture models the two
// shapes the task calls out — a project (p1) whose resources span several
// regions, and a project (p2) holding BOTH regional and global resources.
//
//	p1: us-central1 $8.00 + eu $2.50 + asia $5.20 = $15.70
//	p2: us-central1 $7.30 + global $3.10 = $10.40
//	regions: us-central1 $15.30 + eu $2.50 + asia $5.20 + global $3.10
//	rules: detached_disk $17.80 + old_snapshot $5.20 + unused_reserved_ip $3.10
//	hero: $26.10
//
// Region nodes are containers and never produce findings, so this grouping
// cannot add or drop money relative to the other two.
func TestWasteByRegion_DoesNotAffectTotal(t *testing.T) {
	findings := []rules.Finding{
		{RuleID: "detached_disk", Resource: "disk/a1", Project: "p1", Location: "us-central1", MonthlyWasteUSD: 8.00},
		{RuleID: "detached_disk", Resource: "disk/a2", Project: "p1", Location: "eu", MonthlyWasteUSD: 2.50},
		{RuleID: "old_snapshot", Resource: "snapshot/a3", Project: "p1", Location: "asia", MonthlyWasteUSD: 5.20},
		{RuleID: "detached_disk", Resource: "disk/b1", Project: "p2", Location: "us-central1", MonthlyWasteUSD: 7.30},
		{RuleID: "unused_reserved_ip", Resource: "address/b2", Project: "p2", Location: "global", MonthlyWasteUSD: 3.10},
	}
	r := baseReport(findings)
	r.MultiProject = true
	got := renderHTML(t, r)

	total := pricing.Round2(r.TotalMonthlyWasteUSD)
	if total != 26.10 {
		t.Fatalf("fixture total = %.2f, want 26.10", total)
	}

	// Each grouping — project, region, rule — sums to the same total.
	for name, rows := range map[string][]wasteAgg{
		"by project": wasteByProject(findings),
		"by region":  wasteByRegion(findings),
		"by rule":    wasteByRule(findings),
	} {
		sum := 0.0
		for _, row := range rows {
			sum += row.Total
		}
		if pricing.Round2(sum) != total {
			t.Errorf("waste %s rows sum to %.2f, want the findings total %.2f (rollup invariant)", name, sum, total)
		}
	}

	// The hero and the region-specific figures are in the rendered document,
	// and the region chart sits between the project chart and the rule table.
	if !strings.Contains(got, `<div class="hero-number">$26.10</div>`) {
		t.Errorf("hero must be $26.10:\n%s", got)
	}
	for _, want := range []string{"$15.30", "$2.50", "$5.20", "$3.10"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered report missing the region figure %s:\n%s", want, got)
		}
	}
	iProject := strings.Index(got, "<h2>Where the waste is</h2>")
	iRegion := strings.Index(got, "<h2>Waste by region</h2>")
	iRule := strings.Index(got, "<h2>Waste by rule</h2>")
	if iProject < 0 || iRegion < 0 || iRule < 0 {
		t.Fatalf("summary section must contain project, region and rule headings (indexes %d %d %d)", iProject, iRegion, iRule)
	}
	if !(iProject < iRegion && iRegion < iRule) {
		t.Errorf("chart order must be project -> region -> rule (indexes %d %d %d)", iProject, iRegion, iRule)
	}
}
