package output

import (
	"bytes"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

const (
	ansiESC     = "\x1b["
	ansiRedT    = "\x1b[31m"
	ansiGreenT  = "\x1b[32m"
	ansiYellowT = "\x1b[33m"
)

func colourReport() Report {
	return Report{
		Scope:      "projects/my-project",
		Provider:   "gcp",
		WindowDays: 14,
		ScanStatus: StatusOK,
		Findings: []rules.Finding{
			{RuleID: "detached_disk", Resource: "disk/pd-standard-01", Severity: rules.SeverityHigh, MonthlyWasteUSD: 72.00},
			{RuleID: "old_snapshot", Resource: "snapshot/backup-2023", Severity: rules.SeverityLow, MonthlyWasteUSD: 1.50},
			{RuleID: "underutilized_instance", Resource: "vm/instance-a", Severity: rules.SeverityMedium, MonthlyWasteUSD: 18.20},
		},
		TotalMonthlyWasteUSD: 91.70,
		FindingCount:         3,
		ResourcesScanned:     41,
		RulesEvaluated:       6,
		ProjectsAnalyzed:     1,
	}
}

// TestTableColourPaintsOnlySeverity is the presence half of the colour
// contract: with colour enabled, the ONLY bytes that change from the plain
// table are SGR wrappers around the already-padded HIGH and MEDIUM severity
// cells. LOW stays plain. Resource, rule, money, TOTAL, separator and summary
// are untouched.
func TestTableColourPaintsOnlySeverity(t *testing.T) {
	report := colourReport()

	var plain, coloured bytes.Buffer
	if err := TableRenderer(false).Render(&plain, report); err != nil {
		t.Fatalf("plain Render: %v", err)
	}
	if err := TableRenderer(true).Render(&coloured, report); err != nil {
		t.Fatalf("coloured Render: %v", err)
	}

	// The plain table is the monochrome baseline: no escapes anywhere.
	if bytes.Contains(plain.Bytes(), []byte(ansiESC)) {
		t.Fatalf("plain table must contain no ANSI escapes:\n%q", plain.String())
	}

	// Presence: red wraps the padded HIGH cell and yellow wraps the padded
	// MEDIUM cell. Padding happens before SGR, so the escape is never inside a
	// width argument.
	if !bytes.Contains(coloured.Bytes(), []byte(ansiRedT+"HIGH    \x1b[0m")) {
		t.Errorf("HIGH severity cell must be red and already-padded:\n%q", coloured.String())
	}
	if !bytes.Contains(coloured.Bytes(), []byte(ansiYellowT+"MEDIUM  \x1b[0m")) {
		t.Errorf("MEDIUM severity cell must be yellow and already-padded:\n%q", coloured.String())
	}
	if !bytes.Contains(coloured.Bytes(), []byte("LOW     ")) {
		t.Errorf("LOW severity cell must stay plain text:\n%q", coloured.String())
	}

	// Absence is as important as presence: only the two elevated severities
	// carry SGR. One HIGH and one MEDIUM means exactly four escape sequences
	// (start + reset for each).
	if got := bytes.Count(coloured.Bytes(), []byte(ansiESC)); got != 4 {
		t.Fatalf("coloured table must contain exactly 4 escape sequences (red start/reset and yellow start/reset), got %d:\n%q",
			got, coloured.String())
	}
	if bytes.Contains(coloured.Bytes(), []byte(ansiRedT+"LOW")) {
		t.Errorf("LOW must not be red:\n%q", coloured.String())
	}
	if bytes.Contains(coloured.Bytes(), []byte(ansiYellowT+"LOW")) {
		t.Errorf("LOW must not be yellow:\n%q", coloured.String())
	}
}

// TestTableColourPaintsMultiProjectSeverity exercises the owner-column branch:
// a multi-owner table writes through writeRowProject and must still colour
// only the severity cell.
func TestTableColourPaintsMultiProjectSeverity(t *testing.T) {
	report := Report{
		Scope:        "organizations/o",
		Provider:     "gcp",
		WindowDays:   14,
		ScanStatus:   StatusOK,
		MultiProject: true,
		Findings: []rules.Finding{
			{RuleID: "old_snapshot", Resource: "snapshot/backup-2023", Project: "alpha-data-storage", Severity: rules.SeverityHigh, MonthlyWasteUSD: 5.20},
		},
		TotalMonthlyWasteUSD: 5.20,
		FindingCount:         1,
		ResourcesScanned:     17,
		RulesEvaluated:       5,
		ProjectsAnalyzed:     2,
	}

	var coloured bytes.Buffer
	if err := TableRenderer(true).Render(&coloured, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Contains(coloured.Bytes(), []byte(ansiRedT+"HIGH    \x1b[0m")) {
		t.Errorf("multi-project HIGH severity cell must be red:\n%q", coloured.String())
	}
	if got := bytes.Count(coloured.Bytes(), []byte(ansiESC)); got != 2 {
		t.Errorf("multi-project coloured table must contain exactly 2 escape sequences (one red severity), got %d:\n%q",
			got, coloured.String())
	}
}

// TestTableNonTerminalWriterIsByteIdentical pins the monochrome table's raw
// bytes. A table rendered to a buffer (the same writer tests, README examples
// and the HTML report all use) carries the new SEVERITY column but no ANSI.
func TestTableNonTerminalWriterIsByteIdentical(t *testing.T) {
	var buf bytes.Buffer
	if err := TableRenderer(false).Render(&buf, colourReport()); err != nil {
		t.Fatalf("Render: %v", err)
	}

	want := "RESOURCE             RULE                   SEVERITY MONTHLY WASTE\n" +
		"disk/pd-standard-01  detached_disk          HIGH            $72.00\n" +
		"snapshot/backup-2023 old_snapshot           LOW              $1.50\n" +
		"vm/instance-a        underutilized_instance MEDIUM          $18.20\n" +
		"------------------------------------------------------------------\n" +
		"TOTAL                3 findings                             $91.70\n" +
		"Summary: projects/my-project — 1 project analyzed, 41 resources scanned, 6 rules evaluated, 3 findings, 0 resources skipped, 0s\n"

	if !bytes.Equal(buf.Bytes(), []byte(want)) {
		t.Errorf("non-terminal table bytes changed:\n got %q\nwant %q", buf.String(), want)
	}
}

// TestJSONAndCSV_NeverColoured pins the machine-output contract on raw bytes:
// jsonRenderer and csvRenderer have no colour field and no colour code path,
// so their output is exactly the same bytes with or without a coloured table
// renderer in the same process.
func TestJSONAndCSV_NeverColoured(t *testing.T) {
	report := colourReport()

	var jb, cb bytes.Buffer
	jsonRenderer, err := For("json")
	if err != nil {
		t.Fatalf("For(json): %v", err)
	}
	if err := jsonRenderer.Render(&jb, report); err != nil {
		t.Fatalf("json Render: %v", err)
	}
	csvRenderer, err := For("csv")
	if err != nil {
		t.Fatalf("For(csv): %v", err)
	}
	if err := csvRenderer.Render(&cb, report); err != nil {
		t.Fatalf("csv Render: %v", err)
	}

	if bytes.Contains(jb.Bytes(), []byte(ansiESC)) {
		t.Fatalf("JSON must never contain ANSI escapes:\n%q", jb.String())
	}
	if bytes.Contains(cb.Bytes(), []byte(ansiESC)) {
		t.Fatalf("CSV must never contain ANSI escapes:\n%q", cb.String())
	}

	wantJSON := `{
  "schema_version": 0,
  "scan_status": "ok",
  "scope": "projects/my-project",
  "provider": "gcp",
  "generated_at": "0001-01-01T00:00:00Z",
  "window_days": 14,
  "findings": [
    {
      "rule_id": "detached_disk",
      "resource_id": "",
      "resource": "disk/pd-standard-01",
      "kind": "",
      "project": "",
      "location": "",
      "severity": "high",
      "monthly_waste_usd": 72,
      "confidence": 0
    },
    {
      "rule_id": "old_snapshot",
      "resource_id": "",
      "resource": "snapshot/backup-2023",
      "kind": "",
      "project": "",
      "location": "",
      "severity": "low",
      "monthly_waste_usd": 1.5,
      "confidence": 0
    },
    {
      "rule_id": "underutilized_instance",
      "resource_id": "",
      "resource": "vm/instance-a",
      "kind": "",
      "project": "",
      "location": "",
      "severity": "medium",
      "monthly_waste_usd": 18.2,
      "confidence": 0
    }
  ],
  "total_monthly_waste_usd": 91.7,
  "finding_count": 3,
  "resources_scanned": 41,
  "rules_evaluated": 6,
  "projects_analyzed": 1,
  "resources_skipped": 0,
  "duration": 0
}
`
	if !bytes.Equal(jb.Bytes(), []byte(wantJSON)) {
		t.Errorf("JSON bytes changed:\n got %q\nwant %q", jb.String(), wantJSON)
	}

	wantCSV := "resource,rule,monthly_waste_usd,severity,confidence,kind,project,location,resource_id,evidence\n" +
		"disk/pd-standard-01,detached_disk,72.00,high,0.00,,,,,\n" +
		"snapshot/backup-2023,old_snapshot,1.50,low,0.00,,,,,\n" +
		"vm/instance-a,underutilized_instance,18.20,medium,0.00,,,,,\n"
	if !bytes.Equal(cb.Bytes(), []byte(wantCSV)) {
		t.Errorf("CSV bytes changed:\n got %q\nwant %q", cb.String(), wantCSV)
	}
}

// TestTableEmptyStatusColoursOnlyTheHeadline pins the one non-table place
// colour is used: the empty-result line. The summary and the metrics-blocked
// note that follow stay plain.
func TestTableEmptyStatusColoursOnlyTheHeadline(t *testing.T) {
	cases := []struct {
		name     string
		report   Report
		wantCode string
		wantText string
	}{
		{
			name:     "clean zero",
			report:   Report{ScanStatus: StatusOK, ResourcesScanned: 4, ProjectsAnalyzed: 1},
			wantCode: ansiGreenT,
			wantText: "No waste found.",
		},
		{
			name:     "degraded zero is qualified",
			report:   Report{ScanStatus: StatusDegraded, ResourcesScanned: 4, ProjectsAnalyzed: 1},
			wantCode: ansiYellowT,
			wantText: "No waste found.",
		},
		{
			name:     "metrics-blocked zero is qualified",
			report:   Report{ScanStatus: StatusOK, ResourcesScanned: 4, ProjectsAnalyzed: 1, MetricsBlocked: []string{"underutilized_instance"}},
			wantCode: ansiYellowT,
			wantText: "No waste found.",
		},
		{
			name:     "nothing scanned",
			report:   Report{ScanStatus: StatusNoResources, ResourcesScanned: 0},
			wantCode: ansiYellowT,
			wantText: "No resources scanned — nothing was found to evaluate. Check the scope and the identity's permissions.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TableRenderer(true).Render(&buf, tc.report); err != nil {
				t.Fatalf("Render: %v", err)
			}
			got := buf.String()
			want := tc.wantCode + tc.wantText + "\x1b[0m"
			if !bytes.Contains(buf.Bytes(), []byte(want)) {
				t.Errorf("empty status line = %q, want it to contain %q", got, want)
			}
			// The summary line is plain context, not part of the headline.
			if bytes.Contains(buf.Bytes(), []byte("Summary: \x1b[")) {
				t.Errorf("summary line must stay plain:\n%q", got)
			}
			if bytes.Contains(buf.Bytes(), []byte("could not be evaluated\x1b[")) {
				t.Errorf("metrics-blocked note must stay plain:\n%q", got)
			}
		})
	}
}

// TestTableRendererFalseNeverPaintsEmptyStatus guards the colour gate on the
// empty-result path: no TTY / --no-color / NO_COLOR / TERM=dumb all resolve to
// a plain renderer before this point, and a plain renderer emits no escapes.
func TestTableRendererFalseNeverPaintsEmptyStatus(t *testing.T) {
	for _, report := range []Report{
		{ScanStatus: StatusOK, ResourcesScanned: 4, ProjectsAnalyzed: 1},
		{ScanStatus: StatusDegraded, ResourcesScanned: 4, ProjectsAnalyzed: 1},
		{ScanStatus: StatusNoResources, ResourcesScanned: 0},
		{ScanStatus: StatusOK, ResourcesScanned: 4, ProjectsAnalyzed: 1, MetricsBlocked: []string{"underutilized_instance"}},
	} {
		var buf bytes.Buffer
		if err := TableRenderer(false).Render(&buf, report); err != nil {
			t.Fatalf("Render: %v", err)
		}
		if bytes.Contains(buf.Bytes(), []byte(ansiESC)) {
			t.Errorf("plain table must not colour the empty status:\n%q", buf.String())
		}
	}
}
