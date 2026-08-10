package output

import (
	"fmt"
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

// testFinding builds one finding with all fields set, so a test can focus on
// the one field it varies.
func testFinding(i int) rules.Finding {
	return rules.Finding{
		RuleID:          "detached_disk",
		ResourceID:      "",
		Resource:        "disk/pd-standard-01",
		Kind:            "",
		Project:         "my-project",
		Location:        "us-central1-a",
		Severity:        rules.SeverityHigh,
		Confidence:      0.95,
		MonthlyWasteUSD: float64(i) + 0.50,
		Evidence: []rules.Evidence{
			{Key: "detached_days", Value: "19"},
			{Key: "price_source", Value: "embedded_fallback sku=pd-standard"},
		},
		Remediation: "Attach the disk to a running instance or delete it.",
	}
}

// renderHTML renders a Report to a string for assertions.
func renderHTML(t *testing.T, r Report) string {
	t.Helper()
	var buf bytes.Buffer
	if err := RenderHTML(&buf, r); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	return buf.String()
}

// baseReport builds a Report whose totals are derived from its findings the
// same way NewReport derives them (single rounding of the raw sum), so the
// totals-invariant tests below are testing the renderer, not the fixture.
func baseReport(findings []rules.Finding) Report {
	total := 0.0
	for _, f := range findings {
		total += f.MonthlyWasteUSD
	}
	return Report{
		Scope:                "projects/my-project",
		Provider:             "gcp",
		GeneratedAt:          time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC),
		WindowDays:           14,
		Findings:             findings,
		TotalMonthlyWasteUSD: pricing.Round2(total),
		FindingCount:         len(findings),
		ResourcesScanned:     1,
		RulesEvaluated:       3,
		ProjectsAnalyzed:     1,
	}
}

// ---------------------------------------------------------------------------
// Self-contained guarantee
// ---------------------------------------------------------------------------

// TestRenderHTML_NoExternalReferences pins the self-contained guarantee: the
// report must reference nothing external — no remote script, stylesheet,
// image, font, or fetch — because it is emailed and opened offline.
func TestRenderHTML_NoExternalReferences(t *testing.T) {
	r := baseReport([]rules.Finding{testFinding(1), testFinding(2)})
	r.MultiProject = true
	got := renderHTML(t, r)

	for _, needle := range []string{
		"http://", "https://", // no URL of any kind
		"<script src", "<link", "<img", // no external element reference
		"@import", "@font-face", "url(", // no CSS external reference
		"cdn", "fonts.", // no CDN hints
	} {
		if strings.Contains(got, needle) {
			t.Errorf("report must be fully self-contained; found %q", needle)
		}
	}
	if !strings.Contains(got, "<style>") {
		t.Errorf("report must carry its stylesheet inline")
	}
}

// ---------------------------------------------------------------------------
// Zero-findings / degenerate states
// ---------------------------------------------------------------------------

// TestRenderHTML_ZeroFindings_ScanRan: no findings but the scan analyzed
// projects — an honest $0.00 hero and a green check that names the ground the
// scan covered. The scan-details section still renders.
func TestRenderHTML_ZeroFindings_ScanRan(t *testing.T) {
	r := baseReport(nil)
	r.ResourcesScanned = 400
	r.ProjectsAnalyzed = 3
	got := renderHTML(t, r)

	for _, want := range []string{
		`<div class="hero-number">$0.00</div>`,
		"total monthly waste",
		"✅ No waste found among the 3 projects and 400 resources checked.",
		`id="scan-details"`,
		"Scan details",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scan-ran empty report missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "No data") {
		t.Errorf("a scan that ran must not render the \"No data\" state:\n%s", got)
	}
	if strings.Contains(got, `class="findings-table"`) {
		t.Errorf("a scan with no findings must not render a findings table")
	}
}

// TestRenderHTML_ZeroFindings_NothingScanned: no findings AND no projects
// analyzed — the hero is replaced by "No data" and the warning says the scope
// resolved nothing, with the scope and the zero denominator visible.
func TestRenderHTML_ZeroFindings_NothingScanned(t *testing.T) {
	r := baseReport(nil)
	r.ProjectsAnalyzed = 0
	r.ResourcesScanned = 0
	r.Scope = "projects/does-not-exist"
	got := renderHTML(t, r)

	for _, want := range []string{
		`<div class="hero-no-data">No data</div>`,
		"⚠️ The scan resolved zero resources. Check that the scope is correct and the project or folder exists.",
		"projects/does-not-exist",
		"Resources scanned: 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("nothing-scanned empty report missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("the nothing-scanned state must not present a confident $0.00 hero:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// Single finding / single project degradation
// ---------------------------------------------------------------------------

// TestRenderHTML_SingleFinding_NoRedundantControls: one finding means no
// filter controls (they serve no purpose), no show-all button, no Project
// column (single project), and no waste-by-rule summary (a single-row table
// is decoration). The hero must equal the one finding's waste.
func TestRenderHTML_SingleFinding_NoRedundantControls(t *testing.T) {
	r := baseReport([]rules.Finding{testFinding(1)})
	got := renderHTML(t, r)

	for _, absent := range []string{
		`id="filter-search"`, // the input element, not the JS getElementById
		`id="show-all-btn"`,
		"<th>Project</th>",
		"Waste by rule",
		"Where the waste is",
	} {
		if strings.Contains(got, absent) {
			t.Errorf("single finding must not render %q:\n%s", absent, got)
		}
	}
	for _, want := range []string{
		`<div class="hero-number">$1.50</div>`,
		"Showing <span id=\"showing-count\">1</span> of 1 finding",
		`<span class="sev-pill sev-pill-high">HIGH</span>`,
		`style="width:95%"`,
		"95%",
		"detached_days",
		"Pricing",
		"price_source",
		"Fix:",
		"Attach the disk to a running instance or delete it.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("single-finding report missing %q:\n%s", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Multi-project summary: bars + rule table + project column
// ---------------------------------------------------------------------------

// TestRenderHTML_MultiProject_SummaryRenders: a multi-project, multi-rule scan
// renders the waste-by-project SVG, the waste-by-rule table, and the Project
// column. The chart and table are derived from the findings, and their totals
// match the hero (asserted separately below).
func TestRenderHTML_MultiProject_SummaryRenders(t *testing.T) {
	findings := []rules.Finding{
		{RuleID: "detached_disk", Resource: "disk/a", Project: "alpha-proj", Severity: rules.SeverityHigh, Confidence: 0.95, MonthlyWasteUSD: 8.00},
		{RuleID: "detached_disk", Resource: "disk/b", Project: "beta-proj", Severity: rules.SeverityMedium, Confidence: 0.6, MonthlyWasteUSD: 2.50},
		{RuleID: "unused_reserved_ip", Resource: "address/c", Project: "beta-proj", Severity: rules.SeverityLow, Confidence: 0.3, MonthlyWasteUSD: 7.30},
	}
	r := baseReport(findings)
	r.MultiProject = true
	got := renderHTML(t, r)

	for _, want := range []string{
		`<section id="summary">`,
		"<h2>Where the waste is</h2>",
		"<svg",
		"summary-bar-rect",
		"<h2>Waste by rule</h2>",
		`<table class="summary-rules">`,
		"<th>Project</th>",
		"alpha-proj",
		"beta-proj",
		"unused_reserved_ip",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("multi-project report missing %q:\n%s", want, got)
		}
	}
	// The Project column exists and the summary section precedes findings.
	if strings.Index(got, `id="summary"`) > strings.Index(got, `id="findings"`) {
		t.Errorf("summary section must precede the findings section")
	}
}

// ---------------------------------------------------------------------------
// Pagination at 500 findings
// ---------------------------------------------------------------------------

// TestRenderHTML_ManyFindings_Pagination: the table always contains EVERY
// finding; the first maxTableRows are visible, the rest carry beyond-limit and
// are hidden by CSS, and a "Show all N findings" button lifts the limit.
func TestRenderHTML_ManyFindings_Pagination(t *testing.T) {
	const n = 500
	findings := make([]rules.Finding, 0, n)
	for i := 0; i < n; i++ {
		f := testFinding(i + 1)
		f.Resource = "disk/pd-" + strings.Repeat("x", 8) + "-" + string(rune('a'+i%26)) + string(rune('0'+i/26%10))
		findings = append(findings, f)
	}
	r := baseReport(findings)
	got := renderHTML(t, r)

	// All 500 rows in the DOM, never duplicated.
	if c := strings.Count(got, "<tr class=\"finding-row"); c != n {
		t.Errorf("document must carry all %d finding rows (each written once), got %d", n, c)
	}
	if c := strings.Count(got, "class=\"finding-row beyond-limit\""); c != n-maxTableRows {
		t.Errorf("rows past the limit = %d, want %d hidden by the beyond-limit class", c, n-maxTableRows)
	}
	for _, want := range []string{
		"Showing <span id=\"showing-count\">50</span> of 500 findings",
		`id="show-all-btn"`,
		"Show all 500 findings",
		`id="filter-search"`,
		"sev-toggle",
		`id="sort-select"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("500-finding report missing %q:\n%s", want, got)
		}
	}
	// The findings table must appear exactly once — the show-all button reveals
	// rows, it does not re-render the table.
	if c := strings.Count(got, `class="findings-table"`); c != 1 {
		t.Errorf("findings table must be written exactly once, got %d", c)
	}
}

// ---------------------------------------------------------------------------
// Totals invariant — whatever the report shows must equal the findings total
// ---------------------------------------------------------------------------

// TestWasteAggregations_TotalEqualsFindingsTotal verifies the aggregation
// helpers: the sum of every waste-by-rule row and every waste-by-project row
// equals the findings total exactly, including when capping folds a remainder
// into an "other" row. This is the guard against the shipped "totals did not
// match the findings" defects.
func TestWasteAggregations_TotalEqualsFindingsTotal(t *testing.T) {
	var findings []rules.Finding
	for i := 0; i < 12; i++ {
		findings = append(findings, rules.Finding{
			RuleID:          "rule_" + string(rune('a'+i%3)),
			Project:         "project_" + string(rune('a'+i%10)), // 10 distinct projects → capping
			MonthlyWasteUSD: float64(i) + 0.25,
		})
	}
	var raw float64
	for _, f := range findings {
		raw += f.MonthlyWasteUSD
	}
	want := pricing.Round2(raw)

	for name, rows := range map[string][]wasteAgg{
		"by rule":    wasteByRule(findings),
		"by project": wasteByProject(findings),
	} {
		sum := 0.0
		for _, row := range rows {
			sum += row.Total
		}
		if pricing.Round2(sum) != want {
			t.Errorf("waste %s rows sum to %.2f, want the findings total %.2f", name, sum, want)
		}
	}

	// Capping: 10 projects → the top maxSummaryRows plus one "Other (N
	// projects)" row that still carries the remainder's total.
	proj := wasteByProject(findings)
	if len(proj) != maxSummaryRows+1 {
		t.Errorf("capped project rows = %d, want %d + 1 other", len(proj), maxSummaryRows)
	}
	foundOther := false
	for _, row := range proj {
		if row.Key == "other" {
			foundOther = true
			if !strings.HasPrefix(row.Label, "Other (") {
				t.Errorf("other row label = %q, want \"Other (…)\"", row.Label)
			}
		}
	}
	if !foundOther {
		t.Errorf("capped aggregation must fold the remainder into an other row")
	}
}

// TestRenderHTML_HeroTotalMatchesFindingsTotal pins the headline invariant in
// the rendered document: the hero number is the single-rounded sum of the
// findings, and the displayed per-rule and per-project aggregates derive from
// the same numbers.
func TestRenderHTML_HeroTotalMatchesFindingsTotal(t *testing.T) {
	findings := []rules.Finding{
		{RuleID: "a", Resource: "disk/1", Project: "p1", MonthlyWasteUSD: 8.00},
		{RuleID: "a", Resource: "disk/2", Project: "p1", MonthlyWasteUSD: 2.50},
		{RuleID: "b", Resource: "disk/3", Project: "p2", MonthlyWasteUSD: 7.30},
	}
	r := baseReport(findings)
	r.MultiProject = true
	got := renderHTML(t, r)

	want := moneyHTML(r.TotalMonthlyWasteUSD, r.Currency) // $17.80
	if !strings.Contains(got, `<div class="hero-number">`+want+`</div>`) {
		t.Errorf("hero number must equal the findings total %s:\n%s", want, got)
	}
	// Every per-rule row in the summary table plus the hero must be consistent:
	// 8.00+2.50 = 10.50 for rule a, 7.30 for rule b.
	for _, want := range []string{"10.50", "7.30", "17.80"} {
		if !strings.Contains(got, "$"+want) {
			t.Errorf("rendered report missing the $%s figure (rule aggregate or hero total)", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Determinism
// ---------------------------------------------------------------------------

// TestRenderHTML_IsDeterministic: the same Report must render a byte-identical
// document every time — aggregations are sorted, map-backed diagnostics are
// emitted in sorted key order, and the single timestamp lives in one place.
func TestRenderHTML_IsDeterministic(t *testing.T) {
	var findings []rules.Finding
	for i := 0; i < 60; i++ {
		f := testFinding(i + 1)
		f.Project = "proj-" + string(rune('a'+i%3))
		findings = append(findings, f)
	}
	r := baseReport(findings)
	r.MultiProject = true
	r.RuleErrors = map[string]string{"rule_z": "boom", "rule_a": "bang"}
	r.MetricsBlocked = []string{"underutilized_instance", "old_snapshot"}
	r.Skipped = []rules.SkipTally{
		{RuleID: "detached_disk", Code: "in_use", Count: 3},
		{RuleID: "detached_disk", Code: "no_price", Count: 1},
	}

	first := renderHTML(t, r)
	for i := 0; i < 5; i++ {
		if got := renderHTML(t, r); got != first {
			t.Fatalf("render #%d differs from the first; reports must be byte-identical", i+1)
		}
	}
}

// ---------------------------------------------------------------------------
// Warnings: metrics blocked, rule errors, skips
// ---------------------------------------------------------------------------

// TestRenderHTML_MetricsBlockedBanner: every blocked rule ID is named in a
// warning banner between the header and the summary.
func TestRenderHTML_MetricsBlockedBanner(t *testing.T) {
	r := baseReport([]rules.Finding{testFinding(1)})
	r.MetricsBlocked = []string{"underutilized_instance", "old_snapshot"}
	got := renderHTML(t, r)

	for _, want := range []string{
		"2 rules could not be evaluated because the scan data carried no metric series",
		"<code>underutilized_instance</code>",
		"<code>old_snapshot</code>",
		"re-run with a cached snapshot or live API access",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("metrics-blocked banner missing %q:\n%s", want, got)
		}
	}
	// The banner sits between the header and the findings section.
	if strings.Index(got, "warning-blocked") > strings.Index(got, `id="findings"`) {
		t.Errorf("metrics-blocked banner must appear before the findings section")
	}
}

// TestRenderHTML_RuleErrorsBanner: every failed rule and its message is named
// in an always-visible banner, and the scan-details section carries the count.
func TestRenderHTML_RuleErrorsBanner(t *testing.T) {
	r := baseReport([]rules.Finding{testFinding(1)})
	r.RuleErrors = map[string]string{"rule_z": "boom: <unsafe>", "rule_a": "bang"}
	got := renderHTML(t, r)

	for _, want := range []string{
		"2 rules failed during evaluation:",
		"<code>rule_a</code>: bang",
		"<code>rule_z</code>: boom: &lt;unsafe&gt;",
		"<dt>Rule errors</dt><dd>2</dd>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rule-errors surface missing %q:\n%s", want, got)
		}
	}
}

// TestRenderHTML_SkipsInScanDetails: when skips exist, the collapsed scan
// details carries the always-visible summary line and the per (rule, code)
// breakdown table.
func TestRenderHTML_SkipsInScanDetails(t *testing.T) {
	r := baseReport([]rules.Finding{testFinding(1)})
	r.ResourcesSkipped = 4
	r.Skipped = []rules.SkipTally{
		{RuleID: "detached_disk", Code: "in_use", Count: 3},
		{RuleID: "old_snapshot", Code: "no_metric", Count: 1},
	}
	got := renderHTML(t, r)

	for _, want := range []string{
		"4 resources skipped across 2 rules",
		`<table class="skip-table">`,
		"<th>Rule</th><th>Code</th><th>Count</th>",
		"<code>in_use</code>",
		"3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skip surfacing missing %q:\n%s", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Long resource names
// ---------------------------------------------------------------------------

// TestRenderHTML_LongResourceTruncatedWithTitle: a resource name past
// maxResourceRunes is truncated with an ellipsis in the cell and the full name
// travels in the title attribute.
func TestRenderHTML_LongResourceTruncatedWithTitle(t *testing.T) {
	long := "//compute.googleapis.com/projects/my-project/zones/us-central1-a/disks/" +
		"a-very-long-disk-name-that-goes-past-sixty-characters-for-sure-12345"
	f := testFinding(1)
	f.Resource = long
	got := renderHTML(t, baseReport([]rules.Finding{f}))

	if !strings.Contains(got, "…") {
		t.Errorf("long resource must be truncated with an ellipsis")
	}
	if !strings.Contains(got, `title="`+long+`"`) {
		t.Errorf("the full resource name must travel in the title attribute:\n%s", got)
	}
	if strings.Contains(got, long+"</span>") {
		t.Errorf("the full name must not be rendered unwrapped in the cell (it must be truncated)")
	}
}

// ---------------------------------------------------------------------------
// Missing evidence / no remediation
// ---------------------------------------------------------------------------

// TestRenderHTML_EmptyEvidenceAndNoRemediation: no evidence renders an
// explicit "No evidence recorded." line, and an empty Remediation renders
// nothing — some rules genuinely have no one-line remediation.
func TestRenderHTML_EmptyEvidenceAndNoRemediation(t *testing.T) {
	f := testFinding(1)
	f.Evidence = nil
	f.Remediation = ""
	got := renderHTML(t, baseReport([]rules.Finding{f}))

	if !strings.Contains(got, "No evidence recorded.") {
		t.Errorf("empty evidence must render an explicit placeholder:\n%s", got)
	}
	if strings.Contains(got, "Fix:") {
		t.Errorf("empty remediation must render no Fix block:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// Aggregation totals (numeric sanity)
// ---------------------------------------------------------------------------

// TestWasteAggregation_SumsMatchPerFinding confirms each aggregate row equals
// the sum of its own findings — the raw arithmetic the renderer's figures are
// built from, so a displayed "$10.50" can always be traced to findings.
func TestWasteAggregation_SumsMatchPerFinding(t *testing.T) {
	findings := []rules.Finding{
		{RuleID: "a", Project: "p1", MonthlyWasteUSD: 8.00},
		{RuleID: "a", Project: "p1", MonthlyWasteUSD: 2.50},
		{RuleID: "b", Project: "p2", MonthlyWasteUSD: 7.30},
	}
	byRule := wasteByRule(findings)
	if len(byRule) != 2 {
		t.Fatalf("wasteByRule rows = %d, want 2", len(byRule))
	}
	if byRule[0].Label != "a" || math.Abs(byRule[0].Total-10.50) > 1e-9 {
		t.Errorf("rule a total = %s %.2f, want 10.50", byRule[0].Label, byRule[0].Total)
	}
	if byRule[1].Label != "b" || math.Abs(byRule[1].Total-7.30) > 1e-9 {
		t.Errorf("rule b total = %s %.2f, want 7.30", byRule[1].Label, byRule[1].Total)
	}
}

// TestRenderHTML_NoWasteIsQualifiedWhenRulesBlocked: a green tick is a claim
// of a clean bill of health, and it is only honest when every rule actually
// ran. With rules blocked for want of metrics the truthful statement is
// "nothing found among what could be checked" — the distinction this tool
// exists to make, and the one an operator carries away from a checkmark.
func TestRenderHTML_NoWasteIsQualifiedWhenRulesBlocked(t *testing.T) {
	base := Report{
		Scope: "projects/p", Provider: "gcp", WindowDays: 14,
		ProjectsAnalyzed: 1, ResourcesScanned: 3,
	}

	var clean strings.Builder
	if err := RenderHTML(&clean, base); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if !strings.Contains(clean.String(), "✅") {
		t.Error("a scan where every rule ran and found nothing should carry the green tick")
	}

	blocked := base
	blocked.MetricsBlocked = []string{"underutilized_instance"}
	var partial strings.Builder
	if err := RenderHTML(&partial, blocked); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := partial.String()
	if strings.Contains(got, "✅") {
		t.Error("an unqualified green tick over a scan with blocked rules claims a clean " +
			"bill of health the scan cannot support")
	}
	if !strings.Contains(got, "not a clean bill of health") {
		t.Errorf("the empty-findings line must say the scan was partial; got:\n%s", got)
	}
}

// TestRenderHTML_HiddenRowsReachableWithoutScripting: the 50-row limit is a
// screen affordance. Rows past it are in the DOM but hidden by CSS, so without
// the <noscript> override and the print override they cannot be read at all
// with scripting off or on paper — a report that silently omits findings when
// printed is worse than a long one, because nothing on the page says anything
// is missing.
func TestRenderHTML_HiddenRowsReachableWithoutScripting(t *testing.T) {
	r := Report{Scope: "projects/p", Provider: "gcp", WindowDays: 14, ProjectsAnalyzed: 1}
	for i := 0; i < maxTableRows+5; i++ {
		r.Findings = append(r.Findings, rules.Finding{
			RuleID: "detached_disk", Resource: fmt.Sprintf("disk/d%03d", i),
			Project: "p", MonthlyWasteUSD: float64(i + 1), Confidence: 1,
		})
		r.TotalMonthlyWasteUSD += float64(i + 1)
	}
	r.FindingCount = len(r.Findings)

	var sb strings.Builder
	if err := RenderHTML(&sb, r); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := sb.String()

	if !strings.Contains(got, "<noscript>") {
		t.Error("no <noscript> block: with scripting off, findings past the row limit are " +
			"hidden by CSS and nothing can reveal them")
	}
	noscript := got[strings.Index(got, "<noscript>"):]
	noscript = noscript[:strings.Index(noscript, "</noscript>")]
	if !strings.Contains(noscript, "tr.beyond-limit") || !strings.Contains(noscript, "table-row") {
		t.Errorf("the <noscript> block must un-hide the limited rows; got:\n%s", noscript)
	}

	// Take the whole @media print block: its first "}" closes a nested rule,
	// not the block, so slicing at that would cut the window short.
	printBlock := got[strings.Index(got, "@media print"):]
	if end := strings.Index(printBlock, "\n}"); end > 0 {
		printBlock = printBlock[:end]
	}
	if !strings.Contains(printBlock, "tr.beyond-limit") {
		t.Errorf("@media print must un-hide the limited rows, or printing drops findings "+
			"past the limit with no indication; got:\n%s", printBlock)
	}
}
