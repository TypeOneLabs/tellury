package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/cloud/aws"
	"github.com/TypeOneLabs/tellury/pkg/cloud/azure"
)

// TestScanStatus pins the machine-readable scan outcome.
//
// It exists because an empty findings list is ambiguous and, on Azure,
// undecidable: Resource Graph returns an empty result set for resource types
// the identity cannot read, so a permissions gap and a genuinely clean
// subscription produce identical data. Before this field a machine consumer
// had to guess from a combination of counts, and on Azure it could not guess
// correctly at all.
func TestScanStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta Meta
		want string
	}{
		{"resources scanned", Meta{ResourcesScanned: 4}, StatusOK},
		{"nothing scanned", Meta{ResourcesScanned: 0}, StatusNoResources},
		{
			"an account could not be reached",
			Meta{ResourcesScanned: 4, AccountStatuses: []aws.AccountStatus{
				{ID: "1", Status: "scanned"}, {ID: "2", Status: "unreachable"},
			}},
			StatusDegraded,
		},
		{
			// "suspended" and "no_resources" are answers the scan reached, not
			// failures to reach them. Reporting them as degraded would make
			// every organization with a dormant account look broken.
			"a suspended account is an answer, not a failure",
			Meta{ResourcesScanned: 4, AccountStatuses: []aws.AccountStatus{{ID: "1", Status: "suspended"}}},
			StatusOK,
		},
		{
			"a subscription could not be reached",
			Meta{ResourcesScanned: 4, SubscriptionStatuses: []azure.SubscriptionStatus{
				{ID: "s", Status: "unreachable"},
			}},
			StatusDegraded,
		},
		{
			"nothing scanned outranks unreachable: there is nothing to act on",
			Meta{ResourcesScanned: 0, AccountStatuses: []aws.AccountStatus{{ID: "1", Status: "unreachable"}}},
			StatusNoResources,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanStatus(tc.meta); got != tc.want {
				t.Errorf("scanStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEmptyResultWordingDistinguishesScannedFromFound pins the human-facing
// half of the same problem. "No waste found" over zero scanned resources
// states a conclusion the scan never reached.
func TestEmptyResultWordingDistinguishesScannedFromFound(t *testing.T) {
	for _, tc := range []struct {
		scanned int
		want    string
	}{
		{4, "No waste found."},
		{0, "No resources scanned"},
	} {
		var buf bytes.Buffer
		if err := (tableRenderer{}).Render(&buf, Report{ResourcesScanned: tc.scanned}); err != nil {
			t.Fatalf("render: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, tc.want) {
			t.Errorf("with %d resources scanned, output %q does not contain %q", tc.scanned, out, tc.want)
		}
	}
}
