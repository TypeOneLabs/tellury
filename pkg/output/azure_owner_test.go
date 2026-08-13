package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// TestTable_OwnerColumnIsAzureAware pins the Azure owner column: Azure
// resources belong to a subscription, so a multi-owner Azure report must head
// its owner column SUBSCRIPTION, never PROJECT or ACCOUNT.
func TestTable_OwnerColumnIsAzureAware(t *testing.T) {
	report := Report{
		Scope:        "tenants/t",
		Provider:     "azure",
		WindowDays:   14,
		MultiProject: true,
		Findings: []rules.Finding{
			{RuleID: "unattached_managed_disk", Resource: "disk/disk-a", Project: "sub-a", MonthlyWasteUSD: 8},
			{RuleID: "unattached_managed_disk", Resource: "disk/disk-b", Project: "sub-b", MonthlyWasteUSD: 4},
		},
		TotalMonthlyWasteUSD: 12,
		FindingCount:         2,
		ResourcesScanned:     2,
		RulesEvaluated:       1,
	}

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render(azure): %v", err)
	}
	header := strings.SplitN(buf.String(), "\n", 2)[0]
	if !strings.Contains(header, "SUBSCRIPTION") {
		t.Errorf("azure header = %q, want it to contain SUBSCRIPTION", header)
	}
	if strings.Contains(header, "PROJECT") || strings.Contains(header, "ACCOUNT") {
		t.Errorf("azure header = %q, must not contain PROJECT or ACCOUNT", header)
	}
}
