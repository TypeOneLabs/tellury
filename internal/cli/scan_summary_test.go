package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/internal/config"
	"github.com/TypeOneLabs/tellury/pkg/cloud"
)

// TestScanSummary_ProjectsDerivedFromGraphNodes drives the summary through the
// real runScan pipeline: the "projects analyzed" figure must come from the
// graph's project container nodes, not from the findings. The README fixture
// is a single detached disk in one project: one project container, one
// resource, one finding, zero skips. The duration is the scan's own wall
// clock, so it varies run to run — everything before it is stable and
// asserted verbatim. The logged output is the verbatim `tellury scan
// --fixture ...` stdout this feature is documented against.
func TestScanSummary_ProjectsDerivedFromGraphNodes(t *testing.T) {
	gcpPriceFixtureEnv(t)

	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-assets.json"},
		Format:         "table",
		Rules:          []string{"detached_disk"},
		FailOnFindings: false,
		OutDir:         filepath.Join(t.TempDir(), "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out, errOut bytes.Buffer
	if err := runScan(context.Background(), &out, &errOut, g, cfg, readmeNow); err != nil {
		t.Fatalf("runScan (fixture): %v", err)
	}
	got := out.String()

	t.Logf("=== `tellury scan --fixture testdata/readme-assets.json --rules detached_disk` (stdout) ===\n%s", got)

	want := "1 project analyzed, 1 resource scanned, 1 rule evaluated, 1 finding, 0 resources skipped, "
	if !strings.Contains(got, want) {
		t.Errorf("fixture scan summary must report the project/resource/rule/finding/skip denominators (%q):\n%s", want, got)
	}
}

// TestScanSummary_NoFindingsStillReportsProjects is the exact ambiguity the
// summary exists to resolve, through the real pipeline: the old-snapshot
// fixture fires one finding, but --min-waste hides it, so the findings list is
// empty while the graph still carried one project container and three
// resources (two of which were skipped during evaluation). The summary must
// report the project and resources it looked at even though the table is
// empty — that is how an operator distinguishes "nothing wasteful" from
// "nothing scanned".
func TestScanSummary_NoFindingsStillReportsProjects(t *testing.T) {
	gcpPriceFixtureEnv(t)

	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{oldSnapshotFixture},
		Format:         "table",
		Rules:          []string{"old_snapshot"},
		MinWaste:       100, // hide the $1.50 finding; the scan still looked at everything
		FailOnFindings: false,
		OutDir:         filepath.Join(t.TempDir(), "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out, errOut bytes.Buffer
	if err := runScan(context.Background(), &out, &errOut, g, cfg, readmeNow); err != nil {
		t.Fatalf("runScan (no-findings fixture): %v", err)
	}
	got := out.String()

	t.Logf("=== `tellury scan --fixture old-snapshot.json --rules old_snapshot --min-waste 100` (stdout) ===\n%s", got)

	if !strings.Contains(got, "No waste found.") {
		t.Errorf("a zero-findings scan must keep its no-waste headline:\n%s", got)
	}
	// Projects and resources come from the graph, so a scan whose findings were
	// all filtered away still reports that it analyzed one project and three
	// resources, and skipped two during evaluation.
	want := "1 project analyzed, 3 resources scanned, 1 rule evaluated, 0 findings, 2 resources skipped, "
	if !strings.Contains(got, want) {
		t.Errorf("an empty findings table must still report the ground the scan covered (%q):\n%s", want, got)
	}
}

// TestScanSummary_JSONCarriesSummaryFields pins that the same denominators the
// table's summary prints are in the JSON report too, through the real
// pipeline: a JSON consumer needs projects_analyzed, resources_skipped and the
// duration as much as a human does. The logged document is the verbatim
// `tellury scan --format json` report.
func TestScanSummary_JSONCarriesSummaryFields(t *testing.T) {
	gcpPriceFixtureEnv(t)

	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-assets.json"},
		Format:         "json",
		Rules:          []string{"detached_disk"},
		FailOnFindings: false,
		OutDir:         filepath.Join(t.TempDir(), "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out, errOut bytes.Buffer
	if err := runScan(context.Background(), &out, &errOut, g, cfg, readmeNow); err != nil {
		t.Fatalf("runScan (json fixture): %v", err)
	}
	got := out.String()

	t.Logf("=== `tellury scan --fixture testdata/readme-assets.json --rules detached_disk --format json` (summary fields) ===\n%s", got)

	for _, want := range []string{
		`"projects_analyzed": 1`,
		`"resources_skipped": 0`,
		`"duration": `, // nanoseconds; the value is wall clock, the field is stable
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scan JSON must carry %s:\n%s", want, got)
		}
	}
}

// TestScopeSpansManyOwners pins when the owner column appears.
//
// It used to key off the resources found: an organization scan whose findings
// all landed in one account printed no ACCOUNT column — precisely the case
// where the reader has no other way to know which account a finding is in.
// A single-account, single-project or single-subscription scan needs no
// column, because the owner is named on the command line.
func TestScopeSpansManyOwners(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope cloud.Scope
		want  bool
	}{
		{"aws organization", cloud.Scope{Provider: "aws", AWS: &cloud.AWSScope{Organization: "o-1"}}, true},
		{"aws organizational unit", cloud.Scope{Provider: "aws", AWS: &cloud.AWSScope{OrganizationalUnit: "ou-1"}}, true},
		{"aws single account", cloud.Scope{Provider: "aws", AWS: &cloud.AWSScope{Account: "111122223333"}}, false},
		{"gcp organization", cloud.Scope{Provider: "gcp", GCP: &cloud.GCPScope{Organization: "1234"}}, true},
		{"gcp folder", cloud.Scope{Provider: "gcp", GCP: &cloud.GCPScope{Folder: "5678"}}, true},
		{"gcp single project", cloud.Scope{Provider: "gcp", GCP: &cloud.GCPScope{Project: "p"}}, false},
		{"azure tenant", cloud.Scope{Provider: "azure", Azure: &cloud.AzureScope{Tenant: "t"}}, true},
		{"azure management group", cloud.Scope{Provider: "azure", Azure: &cloud.AzureScope{ManagementGroup: "mg"}}, true},
		{"azure single subscription", cloud.Scope{Provider: "azure", Azure: &cloud.AzureScope{Subscription: "s"}}, false},
		{"azure subscription resource group", cloud.Scope{Provider: "azure", Azure: &cloud.AzureScope{Subscription: "s", ResourceGroup: "rg"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scopeSpansManyOwners(tc.scope); got != tc.want {
				t.Errorf("scopeSpansManyOwners = %v, want %v", got, tc.want)
			}
		})
	}
}
