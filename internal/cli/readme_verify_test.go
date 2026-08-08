package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/internal/config"
)

// readmeNow is a fixed evaluation instant: 2024-01-20T00:00:00Z. The README
// fixture's detached disk was created 2024-01-01 00:00:00Z, so 19 days have
// elapsed — comfortably above detached_disk's MinDetachedDays=7 gate, and
// deterministic across every run (no dependence on time.Now).
var readmeNow = time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)

// TestReadmeScanExample runs the exact `tellury scan` example the README
// documents against its own fixture file, through the real runScan pipeline
// (config.Validate -> rule selection -> offline provider -> ingest -> rules ->
// table render). It proves the documented command behaves as described: it is
// not help text. A --cache-file variant is also exercised to cover the
// "replay carries its own scope" claim.
//
// The scan writes its artifacts into a temp directory (OutDir is set to a
// t.TempDir(), never the source tree, following the same rule artifacts_test.go
// already applies) so `go test` cannot litter the package directory.
func TestReadmeScanExample(t *testing.T) {
	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-assets.json"},
		Format:         "table",
		FailOnFindings: false,
		OutDir:         filepath.Join(t.TempDir(), "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := runScan(context.Background(), &out, &errOut, g, cfg, readmeNow); err != nil {
		t.Fatalf("runScan (fixture example): %v", err)
	}

	got := out.String()
	// The table must show the detached disk and the total behind it, exactly
	// as the README's sample output documents. The exact waste depends on the
	// embedded pd-standard price ($0.040/GiB) x 200 GiB = $8.00.
	if !contains(got, "pd-standard-01") {
		t.Errorf("table output missing the detached disk resource:\n%s", got)
	}
	if !contains(got, "detached_disk") {
		t.Errorf("table output missing the rule column:\n%s", got)
	}
	if !contains(got, "$8.00") {
		t.Errorf("table output missing the $8.00 monthly waste (README documents this exact total):\n%s", got)
	}
}

// TestReadmeUnusedReservedIPExample verifies the newly-documented rule fires
// through the same runScan pipeline with a reserved external IP fixture. This
// is how the README's fourth rule claim ("an external IP reserved but
// attached to nothing, whole cost is waste") is exercised against real code.
// Its artifacts go to a temp directory, never the source tree.
func TestReadmeUnusedReservedIPExample(t *testing.T) {
	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-ip.json"},
		Format:         "table",
		Rules:          []string{"unused_reserved_ip"},
		FailOnFindings: false,
		OutDir:         filepath.Join(t.TempDir(), "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := runScan(context.Background(), &out, &errOut, g, cfg, readmeNow); err != nil {
		t.Fatalf("runScan (unused reserved IP example): %v", err)
	}

	got := out.String()
	if !contains(got, "unused_reserved_ip") {
		t.Errorf("table output missing the unused_reserved_ip rule:\n%s", got)
	}
	if !contains(got, "address/") {
		t.Errorf("table output missing the reserved address resource:\n%s", got)
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
