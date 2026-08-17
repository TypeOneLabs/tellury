package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/cloud/aws"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

func TestTableCoverage_AccountRegionCoverageIsVisible(t *testing.T) {
	report := Report{
		Provider:   "aws",
		ScanStatus: StatusOK,
		WindowDays: 14,
		AccountStatuses: []aws.AccountStatus{
			{
				ID:                "111122223333",
				Name:              "AcctOne",
				Status:            "scanned",
				RegionsEnabled:    17,
				RegionsSearchable: 1,
			},
		},
		AccountsAnalyzed: 1,
		ResourcesScanned: 2,
		RulesEvaluated:   1,
	}

	got := renderTable(t, false, report)
	for _, want := range []string{
		"COVERAGE",
		"Account outcomes: 1 scanned",
		"scanned: 111122223333 (AcctOne) — 1 of 17 regions searchable",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("COVERAGE table must contain %q:\n%s", want, got)
		}
	}
}

func TestJSON_CarriesAccountRegionCoverage(t *testing.T) {
	report := Report{
		Provider: "aws",
		AccountStatuses: []aws.AccountStatus{
			{
				ID:                "111122223333",
				Name:              "AcctOne",
				Status:            "scanned",
				RegionsEnabled:    17,
				RegionsSearchable: 1,
			},
		},
	}

	var buf bytes.Buffer
	if err := (jsonRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		`"account_statuses": [`,
		`"account_id": "111122223333"`,
		`"regions_enabled": 17`,
		`"regions_searchable": 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON must carry %s:\n%s", want, got)
		}
	}
}

// TestScanStatus_DoesNotDegradeForPartialRegionCoverage pins the stop
// condition from the defect write-up: Resource Explorer first-run gaps are
// opt-in and expected, so a partially searchable account must stay "ok".
// Only an unreachable account/subscription degrades the scan status.
func TestScanStatus_DoesNotDegradeForPartialRegionCoverage(t *testing.T) {
	r := NewReport(rules.Result{}, Meta{
		ResourcesScanned: 2,
		AccountStatuses: []aws.AccountStatus{
			{
				ID:                "111122223333",
				Status:            "scanned",
				RegionsEnabled:    17,
				RegionsSearchable: 1,
			},
		},
	})
	if r.ScanStatus != StatusOK {
		t.Errorf("scan_status = %q, want %q (partial region coverage is expected under --aws-use-resource-explorer)", r.ScanStatus, StatusOK)
	}
}
