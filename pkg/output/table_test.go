package output

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

func renderTable(t *testing.T, color bool, report Report) string {
	t.Helper()
	var buf bytes.Buffer
	if err := TableRenderer(color).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return buf.String()
}

func linesOf(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func lineContaining(lines []string, needle string) int {
	for i, line := range lines {
		if strings.Contains(line, needle) {
			return i
		}
	}
	return -1
}

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

	got := renderTable(t, false, report)

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
// keeps it from running into the next column.
func TestTableColumnsNeverTouch_MultiProject(t *testing.T) {
	report := Report{
		Scope:      "projects/alpha-proj",
		Provider:   "gcp",
		WindowDays: 14,
		Findings: []rules.Finding{
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

	got := renderTable(t, false, report)
	lines := linesOf(got)
	if len(lines) != 19 {
		t.Fatalf("expected 19 rendered lines, got %d:\n%s", len(lines), got)
	}

	display := tableFindings(report)
	l := layoutTable(report, display)

	// Header + data rows are lines 2..5; the first two lines are the FINDINGS
	// header and the separator above the table.
	for _, line := range lines[2:6] {
		assertRowSeparated(t, line, l)
	}

	assertTotalRow(t, lines[7], l, "3 findings", "$24.00")

	if lineContaining(lines, "SUMMARY") != 9 {
		t.Errorf("SUMMARY header must follow the blank line after FINDINGS:\n%s", got)
	}
}

// assertRowSeparated asserts the row is exactly one data-row wide and that a
// literal space sits at every variable-column boundary, so no value in a
// RESOURCE, PROJECT or RULE cell can touch the cell to its right — even when
// the value fills its cell exactly.
func assertRowSeparated(t *testing.T, line string, l tableLayout) {
	t.Helper()
	runes := []rune(line)
	if len(runes) != l.separatorWidth() {
		t.Errorf("row rune length = %d, want %d: %q", len(runes), l.separatorWidth(), line)
		return
	}
	if runes[l.resource] != ' ' {
		t.Errorf("resource cell runs into the next cell (offset %d not a space): %q", l.resource, line)
	}
	if l.project > 0 {
		if runes[l.resource+1+l.project] != ' ' {
			t.Errorf("project cell runs into rule cell (offset %d not a space): %q", l.resource+1+l.project, line)
		}
	}
}

// assertTotalRow asserts the TOTAL row's exact cell contents: the label in the
// resource cell, the finding summary left-aligned in the full span of the
// columns left of the money cell, and the total right-aligned in the money
// cell.
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

func TestTableTotalRow_SummaryNeverTruncated(t *testing.T) {
	findings := make([]rules.Finding, 0, 9)
	for i := 0; i < 9; i++ {
		waste := 0.26
		if i == 0 {
			waste = 5.20
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

	got := renderTable(t, false, report)
	lines := linesOf(got)
	totalIdx := lineContaining(lines, "TOTAL")
	if totalIdx < 0 {
		t.Fatalf("TOTAL row missing:\n%s", got)
	}
	totalRow := lines[totalIdx]

	display := tableFindings(report)
	l := layoutTable(report, display)
	summaryCell := strings.TrimSpace(runeSlice(totalRow, l.resource+1, l.totalSummaryWidth()))
	if summaryCell != "9 findings" {
		t.Errorf("TOTAL row summary cell = %q, want %q — the finding count must span the columns left of the money cell and never be truncated", summaryCell, "9 findings")
	}
	moneyCell := strings.TrimSpace(runeSlice(totalRow, l.resource+1+l.totalSummaryWidth(), colMoney))
	if moneyCell != "$7.28" {
		t.Errorf("TOTAL row money cell = %q, want $7.28", moneyCell)
	}
}

// ---------------------------------------------------------------------------
// Feature — top 10 findings + link to the full report
// ---------------------------------------------------------------------------

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
		total += float64(i)
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

	got := renderTable(t, false, report)
	lines := linesOf(got)
	if len(lines) != 27 {
		t.Fatalf("expected 27 lines (FINDINGS + table + TOTAL + omitted + SUMMARY), got %d:\n%s", len(lines), got)
	}

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

	if !strings.Contains(lines[15], "2 of 12 findings omitted") {
		t.Errorf("omitted line must state that 2 of 12 findings were omitted:\n%s", got)
	}
	if !strings.Contains(lines[15], "full report: file:///tmp/tellury-out/report.html") {
		t.Errorf("omitted line must give the HTML report as a clickable file:// URL:\n%s", got)
	}

	display := tableFindings(report)
	l := layoutTable(report, display)
	assertTotalRow(t, lines[14], l, "12 findings", "$78.00")
}

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

	got := renderTable(t, false, report)
	if strings.Contains(got, "omitted") {
		t.Errorf("with 10 findings (at the limit) there must be no omitted note:\n%s", got)
	}
	if !strings.Contains(got, "10 findings") {
		t.Errorf("TOTAL row must state 10 findings:\n%s", got)
	}
}

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

	got := renderTable(t, false, report)
	lines := linesOf(got)
	if len(lines) != 18 {
		t.Fatalf("expected 18 lines, got %d:\n%s", len(lines), got)
	}

	display := tableFindings(report)
	l := layoutTable(report, display)
	for _, line := range lines[2:5] {
		assertRowSeparated(t, line, l)
	}
	assertTotalRow(t, lines[6], l, "2 findings", "$9.50")

	if !strings.Contains(got, "snapshot/backup-2023-01-01") {
		t.Errorf("single-project table must render the full resource name:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// Feature — the sectioned SUMMARY block
// ---------------------------------------------------------------------------

func TestTableSummaryBlock_CarriesEveryField(t *testing.T) {
	report := Report{
		Scope:      "organizations/506691140800",
		Provider:   "gcp",
		WindowDays: 14,
		ScanStatus: StatusOK,
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

	got := renderTable(t, false, report)
	for _, want := range []string{
		"SUMMARY",
		"Scope          506691140800",
		"Scope ID       organizations/506691140800",
		"Status         ok",
		"Scanned        17 resources (3 skipped) across 2 projects",
		"Evaluated      5 rules",
		"Total Waste    $5.46 / month",
		"Duration       1.5s",
		"Artifacts      -",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("SUMMARY block must contain %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "FINDINGS") {
		t.Errorf("FINDINGS header missing:\n%s", got)
	}
}

func TestTableSummaryBlock_NoFindingsStillReportsProjects(t *testing.T) {
	report := Report{
		Scope:            "projects/my-project",
		Provider:         "gcp",
		WindowDays:       14,
		ScanStatus:       StatusOK,
		Findings:         nil,
		FindingCount:     0,
		ResourcesScanned: 17,
		RulesEvaluated:   5,
		ProjectsAnalyzed: 1,
		ResourcesSkipped: 0,
		Duration:         4 * time.Millisecond,
	}

	got := renderTable(t, false, report)
	if !strings.Contains(got, "No waste found.") {
		t.Errorf("no-findings table must keep its headline line:\n%s", got)
	}
	for _, want := range []string{
		"Status         ok",
		"Scanned        17 resources across 1 project",
		"Evaluated      5 rules",
		"Total Waste    $0.00 / month",
		"Duration       4ms",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a no-findings scan must report %q:\n%s", want, got)
		}
	}
}

func TestTableSummaryBlock_BrokenScopeReportsZeroProjects(t *testing.T) {
	report := Report{
		Scope:            "projects/does-not-exist",
		Provider:         "gcp",
		WindowDays:       14,
		ScanStatus:       StatusNoResources,
		Findings:         nil,
		FindingCount:     0,
		ResourcesScanned: 0,
		RulesEvaluated:   3,
		ProjectsAnalyzed: 0,
	}

	got := renderTable(t, false, report)
	if !strings.Contains(got, "No resources scanned") {
		t.Errorf("a broken scope must use the no-resources empty state:\n%s", got)
	}
	for _, want := range []string{
		"Status         no_resources",
		"Scanned        0 resources across 0 projects",
		"Evaluated      3 rules",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a broken scope must report %q:\n%s", want, got)
		}
	}
}

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

// ---------------------------------------------------------------------------
// Feature — exact rendered output for the four required cases
// ---------------------------------------------------------------------------

func TestTableRenderedOutput_PopulatedScan(t *testing.T) {
	report := Report{
		Scope:                 "subscriptions/000e62f0-1fd2-4e70-b300-6f147b0a687a/resourceGroups/rg-tellury-test",
		Provider:              "azure",
		ScanStatus:            StatusOK,
		Findings:              []rules.Finding{{Resource: "address/tellury-orphan-ip", RuleID: "unassociated_public_ip", Severity: rules.SeverityMedium, MonthlyWasteUSD: 3.65}},
		TotalMonthlyWasteUSD:  3.65,
		FindingCount:          1,
		ResourcesScanned:      2,
		ResourcesSkipped:      1,
		RulesEvaluated:        3,
		SubscriptionsAnalyzed: 1,
		Duration:              3340 * time.Millisecond,
		ReportPath:            "/home/you/tellury-out/scan-123/report.html",
	}

	got := renderTable(t, false, report)
	want := "FINDINGS\n" +
		"-----------------------------------------------------------------------\n" +
		"RESOURCE                  RULE                   SEVERITY MONTHLY WASTE\n" +
		"address/tellury-orphan-ip unassociated_public_ip MEDIUM           $3.65\n" +
		"-----------------------------------------------------------------------\n" +
		"TOTAL                     1 finding                               $3.65\n" +
		"\n" +
		"SUMMARY\n" +
		"-----------------------------------------------------------------------\n" +
		"Scope          rg-tellury-test\n" +
		"Scope ID       subscriptions/000e62f0-1fd2-4e70-b300-6f147b0a687a/resourceGroups/rg-tellury-test\n" +
		"Status         ok\n" +
		"Scanned        2 resources (1 skipped) across 1 subscription\n" +
		"Evaluated      3 rules\n" +
		"Total Waste    $3.65 / month\n" +
		"Duration       3.34s\n" +
		"Artifacts      /home/you/tellury-out/scan-123\n" +
		"               file:///home/you/tellury-out/scan-123/report.html\n"

	if got != want {
		t.Errorf("populated scan output differs:\n got:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(got, "1 finding") || strings.Contains(got, "1 findings") {
		t.Errorf("TOTAL row must use countPhrase singular: got %q", got)
	}
}

func TestTableRenderedOutput_NoFindings(t *testing.T) {
	report := Report{
		Scope:            "projects/my-project",
		Provider:         "gcp",
		ScanStatus:       StatusOK,
		ResourcesScanned: 17,
		RulesEvaluated:   5,
		ProjectsAnalyzed: 1,
		Duration:         4 * time.Millisecond,
		ReportPath:       "/home/you/tellury-out/scan-124/report.html",
	}

	got := renderTable(t, false, report)
	want := "FINDINGS\n" +
		"No waste found.\n" +
		"\n" +
		"SUMMARY\n" +
		"--------------------------------------------------------------------------------\n" +
		"Scope          my-project\n" +
		"Scope ID       projects/my-project\n" +
		"Status         ok\n" +
		"Scanned        17 resources across 1 project\n" +
		"Evaluated      5 rules\n" +
		"Total Waste    $0.00 / month\n" +
		"Duration       4ms\n" +
		"Artifacts      /home/you/tellury-out/scan-124\n" +
		"               file:///home/you/tellury-out/scan-124/report.html\n"

	if got != want {
		t.Errorf("no-findings output differs:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableRenderedOutput_NoResources(t *testing.T) {
	report := Report{
		Scope:            "projects/does-not-exist",
		Provider:         "gcp",
		ScanStatus:       StatusNoResources,
		ResourcesScanned: 0,
		RulesEvaluated:   3,
		ProjectsAnalyzed: 0,
		Duration:         0,
		ReportPath:       "/home/you/tellury-out/scan-125/report.html",
	}

	got := renderTable(t, false, report)
	want := "FINDINGS\n" +
		"No resources scanned — nothing was found to evaluate. Check the scope and the identity's permissions.\n" +
		"\n" +
		"SUMMARY\n" +
		"--------------------------------------------------------------------------------\n" +
		"Scope          does-not-exist\n" +
		"Scope ID       projects/does-not-exist\n" +
		"Status         no_resources\n" +
		"Scanned        0 resources across 0 projects\n" +
		"Evaluated      3 rules\n" +
		"Total Waste    $0.00 / month\n" +
		"Duration       0s\n" +
		"Artifacts      /home/you/tellury-out/scan-125\n" +
		"               file:///home/you/tellury-out/scan-125/report.html\n"

	if got != want {
		t.Errorf("no-resources output differs:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableGlyphFallback(t *testing.T) {
	report := Report{
		Scope:            "projects/my-project",
		Provider:         "gcp",
		ScanStatus:       StatusOK,
		ResourcesScanned: 17,
		RulesEvaluated:   5,
		ProjectsAnalyzed: 1,
		Duration:         4 * time.Millisecond,
	}

	plain := renderTable(t, false, report)
	decorated := renderTable(t, true, report)

	if strings.Contains(plain, unicodeRule) {
		t.Errorf("plain/ASCII mode must not contain Unicode section rules:\n%s", plain)
	}
	if !strings.Contains(plain, strings.Repeat(asciiRule, defaultSummaryWidth)) {
		t.Errorf("plain/ASCII mode must use %q section rules:\n%s", asciiRule, plain)
	}
	if strings.Contains(decorated, strings.Repeat(asciiRule, defaultSummaryWidth)) {
		t.Errorf("decorated mode must not use ASCII section rules at the summary width:\n%s", decorated)
	}
	if !strings.Contains(decorated, strings.Repeat(unicodeRule, defaultSummaryWidth)) {
		t.Errorf("decorated mode must use %q section rules:\n%s", unicodeRule, decorated)
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

func TestTable_OwnerColumnIsProviderAware(t *testing.T) {
	base := Report{
		Scope: "organizations/o-1", WindowDays: 14, MultiProject: true,
		Findings: []rules.Finding{
			{RuleID: "unattached_ebs_volume", Resource: "disk/vol-a", Project: "111122223333", MonthlyWasteUSD: 8},
			{RuleID: "unattached_ebs_volume", Resource: "disk/vol-b", Project: "444455556666", MonthlyWasteUSD: 4},
		},
		TotalMonthlyWasteUSD: 12, FindingCount: 2, ResourcesScanned: 2, RulesEvaluated: 1,
	}

	for _, tc := range []struct{ provider, want, notWant string }{
		{"aws", "ACCOUNT", "PROJECT"},
		{"gcp", "PROJECT", "ACCOUNT"},
	} {
		r := base
		r.Provider = tc.provider
		got := renderTable(t, false, r)
		lines := linesOf(got)
		header := lines[2]
		if !strings.Contains(header, tc.want) {
			t.Errorf("%s header = %q, want it to contain %q", tc.provider, header, tc.want)
		}
		if strings.Contains(header, tc.notWant) {
			t.Errorf("%s header = %q, must not contain %q", tc.provider, header, tc.notWant)
		}
	}
}
