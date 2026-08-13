package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/output"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// TestDocumentedTableOutputsMatchRenderer guards the two most valuable human
// outputs in the repository: the Quick Start examples in README.md and the
// fixture examples in docs/offline.md. Every block below is rendered by the
// real table renderer (plain/ASCII mode, the same mode a redirect or CI
// capture sees) and the documentation file must contain that exact rendered
// text. A table layout change that is not pasted into these files fails here,
// instead of being noticed by hand during release preparation.
func TestDocumentedTableOutputsMatchRenderer(t *testing.T) {
	blocks := []struct {
		file string
		text string
	}{
		{"../../README.md", renderDocTable(t, awsDocReport())},
		{"../../README.md", renderDocTable(t, azureDocReport())},
		{"../../README.md", renderDocTable(t, gcpDocReport())},
		// docs/writing-a-rule.md is the contributor guide, and it is the file that
		// rotted furthest: its worked example priced a snapshot on the source disk
		// at a stale rate, teaching a defect that had already reached a real invoice.
		// It is guarded here so the same thing cannot happen quietly again.
		{"../../docs/writing-a-rule.md", findingsSectionOnly(renderDocTable(t, oldSnapshotDocReport()))},
		{"../../README.md", renderDocTable(t, orgDocReport())},
		{"../../docs/offline.md", renderDocTable(t, offlineNoPriceDocReport())},
		{"../../docs/offline.md", renderDocTable(t, offlinePriceDocReport())},
	}

	for _, block := range blocks {
		path := filepath.Clean(block.file)
		doc, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		// Compare with LEADING whitespace stripped per line. A fenced block
		// inside a markdown list item must be indented to stay in the list, so
		// an exact substring match would fail on formatting rather than on
		// content. Only the indent is removed: the table's own padding is
		// interior, so column alignment is still compared exactly.
		if !strings.Contains(deindent(string(doc)), deindent(block.text)) {
			t.Errorf("%s does not contain the renderer's current table output:\n%s", path, block.text)
		}
	}
}

func renderDocTable(t *testing.T, r output.Report) string {
	t.Helper()
	var buf bytes.Buffer
	if err := output.TableRenderer(false).Render(&buf, r); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return buf.String()
}

func awsDocReport() output.Report {
	return output.Report{
		Scope:      "accounts/123456789012",
		Provider:   "aws",
		ScanStatus: output.StatusOK,
		Findings: []rules.Finding{
			{Resource: "volume/vol-0a1b2c3d", RuleID: "unattached_ebs_volume", Severity: rules.SeverityMedium, MonthlyWasteUSD: 17.60},
			{Resource: "address/203.0.113.42", RuleID: "unassociated_eip", Severity: rules.SeverityMedium, MonthlyWasteUSD: 3.65},
		},
		TotalMonthlyWasteUSD: 21.25,
		FindingCount:         2,
		ResourcesScanned:     5,
		RulesEvaluated:       3,
		AccountsAnalyzed:     1,
		RegionsAnalyzed:      1,
		RegionSource:         "explicit",
		ResourcesSkipped:     3,
		Duration:             8100 * time.Millisecond,
		ReportPath:           "/home/you/tellury-out/scan-aws/report.html",
	}
}

func azureDocReport() output.Report {
	return output.Report{
		Scope:      "subscriptions/000e62f0-1fd2-4e70-b300-6f147b0a687a/resourceGroups/rg-tellury-test",
		Provider:   "azure",
		ScanStatus: output.StatusOK,
		Findings: []rules.Finding{
			{Resource: "address/tellury-orphan-ip", RuleID: "unassociated_public_ip", Severity: rules.SeverityMedium, MonthlyWasteUSD: 3.65},
		},
		TotalMonthlyWasteUSD:  3.65,
		FindingCount:          1,
		ResourcesScanned:      2,
		ResourcesSkipped:      1,
		RulesEvaluated:        3,
		SubscriptionsAnalyzed: 1,
		Duration:              3340 * time.Millisecond,
		ReportPath:            "/home/you/tellury-out/scan-123/report.html",
	}
}

func gcpDocReport() output.Report {
	return output.Report{
		Scope:      "projects/my-project",
		Provider:   "gcp",
		ScanStatus: output.StatusOK,
		Findings: []rules.Finding{
			{Resource: "disk/pd-standard-01", RuleID: "detached_disk", Severity: rules.SeverityMedium, MonthlyWasteUSD: 8.00},
			{Resource: "address/reserved-ip-01", RuleID: "unused_reserved_ip", Severity: rules.SeverityMedium, MonthlyWasteUSD: 7.30},
		},
		TotalMonthlyWasteUSD: 15.30,
		FindingCount:         2,
		ResourcesScanned:     2,
		RulesEvaluated:       5,
		ProjectsAnalyzed:     1,
		Duration:             2 * time.Millisecond,
		ReportPath:           "/home/you/tellury-out/scan-gcp/report.html",
	}
}

func orgDocReport() output.Report {
	return output.Report{
		Scope:        "organizations/123456789012",
		Provider:     "gcp",
		ScanStatus:   output.StatusOK,
		MultiProject: true,
		Findings: []rules.Finding{
			{Resource: "disk/old-cache", Project: "ml-training", RuleID: "detached_disk", Severity: rules.SeverityMedium, MonthlyWasteUSD: 20.00},
			{Resource: "disk/pd-standard-01", Project: "data-platform", RuleID: "detached_disk", Severity: rules.SeverityMedium, MonthlyWasteUSD: 8.00},
			{Resource: "disk/scratch-disk", Project: "web-frontend", RuleID: "detached_disk", Severity: rules.SeverityMedium, MonthlyWasteUSD: 8.00},
			{Resource: "address/reserved-ip-01", Project: "data-platform", RuleID: "unused_reserved_ip", Severity: rules.SeverityMedium, MonthlyWasteUSD: 7.30},
		},
		TotalMonthlyWasteUSD: 43.30,
		FindingCount:         4,
		ResourcesScanned:     4,
		RulesEvaluated:       5,
		ProjectsAnalyzed:     3,
		Duration:             2 * time.Millisecond,
		ReportPath:           "/home/you/tellury-out/scan-org/report.html",
	}
}

func offlineNoPriceDocReport() output.Report {
	return output.Report{
		Scope:            "projects/my-project",
		Provider:         "gcp",
		ScanStatus:       output.StatusOK,
		ResourcesScanned: 1,
		RulesEvaluated:   5,
		ProjectsAnalyzed: 1,
		ResourcesSkipped: 1,
		Duration:         2 * time.Millisecond,
		MetricsBlocked:   []string{"no_lifecycle_policy", "underutilized_instance"},
		ReportPath:       "/home/you/tellury-out/scan-offline/report.html",
	}
}

func offlinePriceDocReport() output.Report {
	return output.Report{
		Scope:      "projects/my-project",
		Provider:   "gcp",
		ScanStatus: output.StatusOK,
		Findings: []rules.Finding{
			{Resource: "disk/pd-standard-01", RuleID: "detached_disk", Severity: rules.SeverityMedium, MonthlyWasteUSD: 8.00},
		},
		TotalMonthlyWasteUSD: 8.00,
		FindingCount:         1,
		ResourcesScanned:     1,
		RulesEvaluated:       5,
		ProjectsAnalyzed:     1,
		Duration:             2 * time.Millisecond,
		MetricsBlocked:       []string{"no_lifecycle_policy", "underutilized_instance"},
		ReportPath:           "/home/you/tellury-out/report.html",
	}
}

// oldSnapshotDocReport is the worked example in docs/writing-a-rule.md: one
// snapshot billing on its stored bytes, not on the disk it came from.
// 30 GiB x $0.050/GiB-month = $1.50, the figure old_snapshot_fixture_test.go
// asserts against the real fixture.
func oldSnapshotDocReport() output.Report {
	return output.Report{
		Findings: []rules.Finding{{
			Resource:        "snapshot/backup-2023-01-01",
			RuleID:          "old_snapshot",
			Severity:        rules.SeverityLow,
			MonthlyWasteUSD: 1.50,
		}},
		FindingCount:         1,
		TotalMonthlyWasteUSD: 1.50,
		ResourcesScanned:     1,
	}
}

// deindent removes leading whitespace from every line, leaving interior
// spacing — and therefore column alignment — untouched.
func deindent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimLeft(l, " \t")
	}
	return strings.Join(lines, "\n")
}

// findingsSectionOnly keeps the FINDINGS block and drops the SUMMARY that
// follows. The contributor guide illustrates what a rule produces, not what a
// whole scan reports, so it shows the table alone — and a guard that demanded
// the summary too would push noise into the guide to satisfy a test.
func findingsSectionOnly(s string) string {
	if i := strings.Index(s, "\n\nSUMMARY"); i > 0 {
		return s[:i]
	}
	return s
}
