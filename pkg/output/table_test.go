package output

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// ---------------------------------------------------------------------------
// Defect 1 — project and resource names must never be truncated
// ---------------------------------------------------------------------------

// TestTableProjectAndResourceNamesNeverTruncated pins the defect-1 fix. The
// real output used to read "alpha-da…" and "ib-testi…" because the PROJECT
// column was a fixed 9-rune width, and "snapshot/alpha-des…" because the
// RESOURCE column was fixed too. Every variable column is now sized to its
// widest value, so a 20-character GCP project ID and a long resource name are
// rendered in full and can be copied straight into a gcloud command.
func TestTableProjectAndResourceNamesNeverTruncated(t *testing.T) {
	report := Report{
		Scope:      "organizations/506691140800",
		Provider:   "gcp",
		WindowDays: 14,
		Findings: []rules.Finding{
			{RuleID: "old_snapshot", Resource: "snapshot/alpha-desktop", Project: "alpha-data-storage", MonthlyWasteUSD: 5.20},
			{RuleID: "old_snapshot", Resource: "snapshot/ib-test", Project: "ib-testing-playground", MonthlyWasteUSD: 0.26},
		},
		TotalMonthlyWasteUSD: 5.46,
		FindingCount:         2,
		MultiProject:         true,
		ResourcesScanned:     17,
		RulesEvaluated:       5,
	}

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		"alpha-data-storage",
		"ib-testing-playground",
		"snapshot/alpha-desktop",
		"snapshot/ib-test",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("table must render the full %q (variable columns are sized to their widest value, never truncated):\n%s", want, got)
		}
	}
	if strings.Contains(got, "…") {
		t.Errorf("table must not contain an ellipsis — no displayed value may be truncated:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// Column separation under content-sized widths
// ---------------------------------------------------------------------------

// TestTableColumnsNeverTouch_MultiProject is the boundary regression for the
// multi-project layout under content-sized columns. Because every variable
// column is sized to its widest value, the widest value fills its cell
// exactly — so the literal separator space after every variable cell is what
// keeps it from running into the next column. This is the case that used to
// collide when the separator was missing.
func TestTableColumnsNeverTouch_MultiProject(t *testing.T) {
	report := Report{
		Scope:      "projects/alpha-proj",
		Provider:   "gcp",
		WindowDays: 14,
		Findings: []rules.Finding{
			// "a-very-long-project-name" is the widest project, so the PROJECT
			// column is sized to it: this value fills its cell exactly and must
			// not touch RULE.
			{RuleID: "detached_disk", Resource: "disk/disk-a", Project: "a-very-long-project-name", MonthlyWasteUSD: 8.00},
			{RuleID: "detached_disk", Resource: "disk/d2", Project: "abcdefghi", MonthlyWasteUSD: 8.00},
			{RuleID: "detached_disk", Resource: "disk/d1", Project: "ab", MonthlyWasteUSD: 8.00},
		},
		TotalMonthlyWasteUSD: 24.00,
		FindingCount:         3,
		MultiProject:         true,
		ResourcesScanned:     3,
		RulesEvaluated:       1,
	}

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 7 { // header + 3 data + separator + TOTAL + Summary
		t.Fatalf("expected 7 lines, got %d:\n%s", len(lines), buf.String())
	}

	display := tableFindings(report)
	l := layoutTable(report, display)

	// Header + data rows: every variable boundary must hold a space, and every
	// row must be exactly one data-row wide.
	for _, line := range lines[:4] {
		assertRowSeparated(t, line, l)
	}

	// TOTAL row: label in the resource cell, summary spanning the columns left
	// of the money cell, money right-aligned.
	assertTotalRow(t, lines[len(lines)-2], l, "3 findings", "$24.00")

	// The scan summary is the last line.
	if got := lines[len(lines)-1]; !strings.Contains(got, "Summary: ") {
		t.Errorf("last line must be the scan summary, got %q", got)
	}
}

// assertRowSeparated asserts the row is exactly one data-row wide and that a
// literal space sits at every variable-column boundary, so no value in a
// RESOURCE, PROJECT or RULE cell can touch the cell to its right — even when
// the value fills its cell exactly (a column is sized to its widest value).
func assertRowSeparated(t *testing.T, line string, l tableLayout) {
	t.Helper()
	runes := []rune(line)
	if len(runes) != l.separatorWidth() {
		t.Errorf("row rune length = %d, want %d: %q", len(runes), l.separatorWidth(), line)
		return
	}
	// Boundary after RESOURCE: rune at offset l.resource must be a space.
	if runes[l.resource] != ' ' {
		t.Errorf("resource cell runs into the next cell (offset %d not a space): %q", l.resource, line)
	}
	if l.project > 0 {
		// Boundary after PROJECT: rune at offset l.resource+1+l.project must be a space.
		if runes[l.resource+1+l.project] != ' ' {
			t.Errorf("project cell runs into rule cell (offset %d not a space): %q", l.resource+1+l.project, line)
		}
	}
}

// assertTotalRow asserts the TOTAL row's exact cell contents: the label in the
// resource cell, the finding summary left-aligned in the full span of the
// columns left of the money cell, and the total right-aligned in the money
// cell. The summary assertion is EXACT — a truncated form must fail.
func assertTotalRow(t *testing.T, row string, l tableLayout, wantSummary, wantMoney string) {
	t.Helper()
	runes := []rune(row)
	if len(runes) != l.separatorWidth() {
		t.Errorf("TOTAL row rune length = %d, want %d: %q", len(runes), l.separatorWidth(), row)
	}
	labelCell := strings.TrimSpace(runeSlice(row, 0, l.resource))
	if labelCell != "TOTAL" {
		t.Errorf("TOTAL row label cell = %q, want \"TOTAL\"", labelCell)
	}
	summaryCell := strings.TrimSpace(runeSlice(row, l.resource+1, l.totalSummaryWidth()))
	if summaryCell != wantSummary {
		t.Errorf("TOTAL row summary cell = %q, want %q — the finding count must span the columns left of the money cell and never be truncated", summaryCell, wantSummary)
	}
	moneyCell := strings.TrimSpace(runeSlice(row, l.resource+1+l.totalSummaryWidth(), colMoney))
	if moneyCell != wantMoney {
		t.Errorf("TOTAL row money cell = %q, want %q", moneyCell, wantMoney)
	}
}

// ---------------------------------------------------------------------------
// Defect 2 — the TOTAL row must not truncate its own summary
// ---------------------------------------------------------------------------

// TestTableTotalRow_SummaryNeverTruncated reproduces the exact real-scan shape
// that used to render "TOTAL   9 findin…   $7.28": nine findings across two
// projects whose IDs exceed the old 9-rune PROJECT column. The finding count
// is not a project, so it must span the columns left of the money cell and the
// summary cell must contain the exact, untruncated text "9 findings".
func TestTableTotalRow_SummaryNeverTruncated(t *testing.T) {
	findings := make([]rules.Finding, 0, 9)
	for i := 0; i < 9; i++ {
		waste := 0.26
		if i == 0 {
			waste = 5.20 // 5.20 + 8×0.26 = 7.28, the real scan's total
		}
		project := "alpha-data-storage"
		if i%2 == 1 {
			project = "ib-testing-playground"
		}
		findings = append(findings, rules.Finding{
			RuleID:          "old_snapshot",
			Resource:        "snapshot/alpha-desktop",
			Project:         project,
			MonthlyWasteUSD: waste,
		})
	}
	report := Report{
		Scope:                "organizations/506691140800",
		Provider:             "gcp",
		WindowDays:           14,
		Findings:             findings,
		TotalMonthlyWasteUSD: 7.28,
		FindingCount:         9,
		MultiProject:         true,
		ResourcesScanned:     17,
		RulesEvaluated:       5,
	}

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")

	display := tableFindings(report)
	l := layoutTable(report, display)
	// The TOTAL row is second-to-last: the scan summary is the last line.
	totalRow := lines[len(lines)-2]

	// THE assertion that replaces the relaxed one ("findings" OR "findin"): the
	// summary cell must equal the exact text. If the summary were squeezed back
	// into the PROJECT column's width, this would render "9 findin…" and fail.
	summaryCell := strings.TrimSpace(runeSlice(totalRow, l.resource+1, l.totalSummaryWidth()))
	if summaryCell != "9 findings" {
		t.Errorf("TOTAL row summary cell = %q, want %q — the finding count must span the columns left of the money cell and never be truncated", summaryCell, "9 findings")
	}
	moneyCell := strings.TrimSpace(runeSlice(totalRow, l.resource+1+l.totalSummaryWidth(), colMoney))
	if moneyCell != "$7.28" {
		t.Errorf("TOTAL row money cell = %q, want $7.28", moneyCell)
	}
	if !strings.Contains(got, "TOTAL") {
		t.Errorf("TOTAL row missing the TOTAL label:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// Feature — top 10 findings + link to the full report
// ---------------------------------------------------------------------------

// TestTableTop10_TotalReflectsAllFindingsNotJustShown is the acceptance test
// for the top-10 limit. A report with 12 findings renders exactly ten data
// rows (the largest by monthly waste), states how many were omitted and where
// the full HTML report is, and — the invariant that matters — the TOTAL row
// sums EVERY finding ($78.00), not the ten shown ($75.00). A total that
// silently summed only the visible rows would be a worse bug than the one this
// fixes.
func TestTableTop10_TotalReflectsAllFindingsNotJustShown(t *testing.T) {
	findings := make([]rules.Finding, 0, 12)
	var total float64
	for i := 1; i <= 12; i++ {
		findings = append(findings, rules.Finding{
			RuleID:          "old_snapshot",
			Resource:        fmt.Sprintf("snapshot/snap-%02d", i),
			Project:         "alpha-data-storage",
			MonthlyWasteUSD: float64(i),
		})
		total += float64(i) // 78.00
	}
	report := Report{
		Scope:                "organizations/506691140800",
		Provider:             "gcp",
		WindowDays:           14,
		Findings:             findings,
		TotalMonthlyWasteUSD: total,
		FindingCount:         12,
		MultiProject:         true,
		ResourcesScanned:     17,
		RulesEvaluated:       5,
		ReportPath:           "/tmp/tellury-out/report.html",
	}

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()

	// header + 10 rows + separator + TOTAL + omitted + Summary = 15.
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 15 {
		t.Fatalf("expected 15 lines (header + 10 rows + separator + TOTAL + omitted + Summary), got %d:\n%s", len(lines), got)
	}

	// The ten largest by monthly waste are shown; the two smallest are not.
	for i := 3; i <= 12; i++ {
		if !strings.Contains(got, fmt.Sprintf("$%d.00", i)) {
			t.Errorf("table must show the $%d.00 finding (a top-10-by-waste row):\n%s", i, got)
		}
	}
	for _, small := range []string{"$1.00", "$2.00", "snap-01", "snap-02"} {
		if strings.Contains(got, small) {
			t.Errorf("table must omit the smallest findings (%s); they are below the top 10:\n%s", small, got)
		}
	}

	// The omitted line states the count and gives the clickable file:// URL.
	if !strings.Contains(got, "2 of 12 findings omitted") {
		t.Errorf("omitted line must state that 2 of 12 findings were omitted:\n%s", got)
	}
	if !strings.Contains(got, "full report: file:///tmp/tellury-out/report.html") {
		t.Errorf("omitted line must give the HTML report as a clickable file:// URL:\n%s", got)
	}

	// THE invariant: the TOTAL row sums ALL 12 findings, not the ten shown.
	display := tableFindings(report)
	l := layoutTable(report, display)
	assertTotalRow(t, lines[len(lines)-3], l, "12 findings", "$78.00")
}

// TestJSONAndCSV_CarryEveryFinding pins that the top-10 limit applies to the
// human-facing table only: JSON and CSV are consumed by other tools, so they
// must carry all 12 findings, and the JSON total must be the full sum — never
// a sum of only the top ten.
func TestJSONAndCSV_CarryEveryFinding(t *testing.T) {
	findings := make([]rules.Finding, 0, 12)
	var total float64
	for i := 1; i <= 12; i++ {
		findings = append(findings, rules.Finding{
			RuleID:          "old_snapshot",
			Resource:        fmt.Sprintf("snapshot/snap-%02d", i),
			Project:         "alpha-data-storage",
			MonthlyWasteUSD: float64(i),
		})
		total += float64(i) // 78.00
	}
	report := Report{
		Scope:                "organizations/506691140800",
		Provider:             "gcp",
		WindowDays:           14,
		Findings:             findings,
		TotalMonthlyWasteUSD: total,
		FindingCount:         12,
		MultiProject:         true,
		ResourcesScanned:     17,
		RulesEvaluated:       5,
	}

	var jb, cb bytes.Buffer
	if err := (jsonRenderer{}).Render(&jb, report); err != nil {
		t.Fatalf("json Render: %v", err)
	}
	if err := (csvRenderer{}).Render(&cb, report); err != nil {
		t.Fatalf("csv Render: %v", err)
	}

	for i := 1; i <= 12; i++ {
		res := fmt.Sprintf("snapshot/snap-%02d", i)
		if !strings.Contains(cb.String(), res) {
			t.Errorf("CSV must carry every finding (missing %s); the top-10 limit applies to the table only", res)
		}
		if !strings.Contains(jb.String(), res) {
			t.Errorf("JSON must carry every finding (missing %s); the top-10 limit applies to the table only", res)
		}
	}
	// The JSON total must be the full sum of all 12 findings.
	if !strings.Contains(jb.String(), `"total_monthly_waste_usd": 78`) {
		t.Errorf("JSON total must sum every finding (78.00), not just the top ten:\n%s", jb.String())
	}
}

// TestTableTop10_NoOmittedLineAtOrBelowLimit: with ten or fewer findings the
// table shows them all and must not print an omitted note.
func TestTableTop10_NoOmittedLineAtOrBelowLimit(t *testing.T) {
	findings := make([]rules.Finding, 0, 10)
	var total float64
	for i := 1; i <= 10; i++ {
		findings = append(findings, rules.Finding{
			RuleID:          "old_snapshot",
			Resource:        fmt.Sprintf("snapshot/snap-%02d", i),
			Project:         "alpha-data-storage",
			MonthlyWasteUSD: float64(i),
		})
		total += float64(i)
	}
	report := Report{
		Scope:                "organizations/506691140800",
		Provider:             "gcp",
		WindowDays:           14,
		Findings:             findings,
		TotalMonthlyWasteUSD: total,
		FindingCount:         10,
		MultiProject:         true,
		ResourcesScanned:     17,
		RulesEvaluated:       5,
		ReportPath:           "/tmp/tellury-out/report.html",
	}

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "omitted") {
		t.Errorf("with 10 findings (at the limit) there must be no omitted note:\n%s", got)
	}
	if !strings.Contains(got, "10 findings") {
		t.Errorf("TOTAL row must state 10 findings:\n%s", got)
	}
}

// TestTableSingleProject_LayoutAndTotal: the single-project table has no
// PROJECT column; the TOTAL row's summary spans the RULE column (the only
// variable column left of the money cell) and is never truncated. Column
// widths are content-sized here too, so a long resource name is shown in full.
func TestTableSingleProject_LayoutAndTotal(t *testing.T) {
	report := Report{
		Scope:      "projects/my-project",
		Provider:   "gcp",
		WindowDays: 14,
		Findings: []rules.Finding{
			{RuleID: "detached_disk", Resource: "disk/pd-standard-01", Project: "my-project", MonthlyWasteUSD: 8.00},
			{RuleID: "old_snapshot", Resource: "snapshot/backup-2023-01-01", Project: "my-project", MonthlyWasteUSD: 1.50},
		},
		TotalMonthlyWasteUSD: 9.50,
		FindingCount:         2,
		MultiProject:         false,
		ResourcesScanned:     2,
		RulesEvaluated:       3,
	}

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 6 { // header + 2 data + separator + TOTAL + Summary
		t.Fatalf("expected 6 lines, got %d:\n%s", len(lines), buf.String())
	}

	display := tableFindings(report)
	l := layoutTable(report, display)
	for _, line := range lines[:3] {
		assertRowSeparated(t, line, l)
	}
	assertTotalRow(t, lines[len(lines)-2], l, "2 findings", "$9.50")

	if !strings.Contains(buf.String(), "snapshot/backup-2023-01-01") {
		t.Errorf("single-project table must render the full resource name:\n%s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Feature — the scan summary (what the scan looked at)
// ---------------------------------------------------------------------------

// TestTableSummary_AfterTableCarriesEveryField pins the summary line a scan
// prints after the table. The duration is the REPORT's fixed value — the
// scan's own clock — so the same Report always renders the same line; a
// renderer that re-measured time.Now() would make this assertion flaky and is
// exactly what the design forbids.
func TestTableSummary_AfterTableCarriesEveryField(t *testing.T) {
	report := Report{
		Scope:      "organizations/506691140800",
		Provider:   "gcp",
		WindowDays: 14,
		Findings: []rules.Finding{
			{RuleID: "old_snapshot", Resource: "snapshot/alpha-desktop", Project: "alpha-data-storage", MonthlyWasteUSD: 5.20},
			{RuleID: "old_snapshot", Resource: "snapshot/ib-test", Project: "ib-testing-playground", MonthlyWasteUSD: 0.26},
		},
		TotalMonthlyWasteUSD: 5.46,
		FindingCount:         2,
		MultiProject:         true,
		ResourcesScanned:     17,
		RulesEvaluated:       5,
		ProjectsAnalyzed:     2,
		ResourcesSkipped:     3,
		Duration:             1500 * time.Millisecond,
	}

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()

	want := "Summary: 2 projects analyzed, 17 resources scanned, 5 rules evaluated, 2 findings, 3 resources skipped, 1.5s"
	if !strings.Contains(got, want) {
		t.Errorf("table summary must read %q:\n%s", want, got)
	}
	// The summary is the LAST line: context after the table, not the headline.
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if !strings.HasPrefix(lines[len(lines)-1], "Summary: ") {
		t.Errorf("the summary must be the final line, got %q", lines[len(lines)-1])
	}
}

// TestTableSummary_NoFindingsStillReportsProjects is the reason the summary
// exists at all: a scan with zero findings must still say how many projects it
// analyzed and resources it scanned, so an operator can tell "nothing
// wasteful" (projects > 0, resources > 0, findings 0) from "nothing scanned"
// (projects 0, resources 0). The project count comes from the graph's project
// container nodes — never from the findings — so this report carries it even
// though every count the table would have shown is zero.
func TestTableSummary_NoFindingsStillReportsProjects(t *testing.T) {
	report := Report{
		Scope:                "projects/my-project",
		Provider:             "gcp",
		WindowDays:           14,
		Findings:             nil,
		TotalMonthlyWasteUSD: 0,
		FindingCount:         0,
		ResourcesScanned:     17,
		RulesEvaluated:       5,
		ProjectsAnalyzed:     1,
		ResourcesSkipped:     0,
		Duration:             4 * time.Millisecond,
	}

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()

	if !strings.Contains(got, "No waste found in projects/my-project (17 resources, 5 rules).") {
		t.Errorf("no-findings table must keep its headline line:\n%s", got)
	}
	want := "Summary: 1 project analyzed, 17 resources scanned, 5 rules evaluated, 0 findings, 0 resources skipped, 4ms"
	if !strings.Contains(got, want) {
		t.Errorf("a scan with no findings must still report the projects/resources it analyzed (%q):\n%s", want, got)
	}
}

// TestTableSummary_BrokenScopeReportsZeroProjects is the other half of the
// same distinction: a scan whose graph carried no project container nodes —
// nothing was scanned at all — reports 0 projects, so "0 projects analyzed, 0
// resources scanned" reads as a broken scope, never as a clean bill of health.
func TestTableSummary_BrokenScopeReportsZeroProjects(t *testing.T) {
	report := Report{
		Scope:        "projects/does-not-exist",
		Provider:     "gcp",
		WindowDays:   14,
		Findings:     nil,
		FindingCount: 0,
	}

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "Summary: 0 projects analyzed, 0 resources scanned, 0 rules evaluated, 0 findings, 0 resources skipped, 0s") {
		t.Errorf("a broken scope must report zero projects analyzed, never a silent empty result:\n%s", got)
	}
}

// TestJSON_CarriesSummaryFields pins that every field the table's summary
// prints also appears in the JSON: a JSON consumer needs the denominators
// (projects analyzed, resources skipped) as much as a human does. Duration is
// serialized as an integer count of nanoseconds.
func TestJSON_CarriesSummaryFields(t *testing.T) {
	report := Report{
		Scope:            "projects/my-project",
		Provider:         "gcp",
		WindowDays:       14,
		Findings:         nil,
		FindingCount:     0,
		ResourcesScanned: 17,
		RulesEvaluated:   5,
		ProjectsAnalyzed: 1,
		ResourcesSkipped: 3,
		Duration:         1500 * time.Millisecond,
	}

	var buf bytes.Buffer
	if err := (jsonRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		`"projects_analyzed": 1`,
		`"resources_skipped": 3`,
		`"duration": 1500000000`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON must carry %s:\n%s", want, got)
		}
	}
}

// runeSlice returns runes[s:s+n] of line as a string; never panics on a short
// line, returning what is available.
func runeSlice(line string, s, n int) string {
	r := []rune(line)
	if s > len(r) {
		return ""
	}
	e := s + n
	if e > len(r) {
		e = len(r)
	}
	return string(r[s:e])
}
