package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/internal/config"
)

// awsScanNow is a fixed evaluation instant for the AWS fixture scan: the
// fixture's vol-0aaa was created 2024-01-15, so 2024-02-15 is 31 days later —
// comfortably past unattached_ebs_volume's MinDetachedDays=7 gate, and
// deterministic across every run (no dependence on time.Now).
var awsScanNow = time.Date(2024, 2, 15, 0, 0, 0, 0, time.UTC)

// TestAwsFixtureScanFiresBothRules is the end-to-end proof that the two AWS
// native rules fire through the real scan pipeline (config.Validate -> rule
// selection -> offline AWS provider -> fixture ingest -> rules -> table
// render) on the shipped AWS EC2 fixture
// (pkg/cloud/aws/testdata/aws-ec2-fixture.json). The offline provider prices
// the replay from the embedded static table, so the expected figures are
// exact and deterministic:
//
//   - vol-0aaa: available gp3, 100 GiB, 3000 IOPS, 250 MB/s, created 31 days
//     before now → unattached_ebs_volume fires at
//     100*0.08 + 3000*0.005 + 250*0.04 = $33.00;
//   - eipalloc-0d1: VPC-domain EIP with no association →
//     unassociated_eip fires at 0.005 * 730 = $3.65;
//   - vol-0bbb (in-use), vol-0ccc (in-use) and eipalloc-0d2 (associated) all
//     skip, so the total is exactly $36.65 over 2 findings.
func TestAwsFixtureScanFiresBothRules(t *testing.T) {
	cfg := config.Scan{
		Provider:       "aws",
		Account:        "123456789012",
		Fixture:        []string{filepath.Join("..", "..", "pkg", "cloud", "aws", "testdata", "aws-ec2-fixture.json")},
		Format:         "table",
		Rules:          []string{"unattached_ebs_volume", "unassociated_eip"},
		FailOnFindings: false,
		OutDir:         filepath.Join(t.TempDir(), "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := runScan(context.Background(), &out, &errOut, g, cfg, awsScanNow); err != nil {
		t.Fatalf("runScan (AWS fixture): %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"unattached_ebs_volume",
		"unassociated_eip",
		"$13.00", // vol-0aaa: capacity + iops + throughput
		"$3.65",  // eipalloc-0d1: 0.005/hr x 730
		"2 findings",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("AWS fixture scan output missing %q:\n%s", want, got)
		}
	}
}
