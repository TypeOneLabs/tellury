package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/internal/config"
)

// TestReadmeOutDirAndHTMLReport exercises the two artifact additions that
// shipped after v0.1.0:
//
//  1. --out-dir (default tellury-out/) receives, per scan, a timestamped
//     subdirectory holding the replayable graph snapshot, the findings JSON,
//     and the self-contained HTML report.
//  2. The HTML report is self-contained (no network fetch), leads with the
//     hero number, and carries the findings table with price provenance and
//     remediation, plus the collapsed scan-details section.
//
// It runs the exact command shapes the README documents against a temp out
// dir — never the source tree — and asserts the three artifacts exist and the
// HTML is stand-alone (no src= or href= to an external resource).
func TestReadmeOutDirAndHTMLReport(t *testing.T) {
	gcpPriceFixtureEnv(t)

	outDir := filepath.Join(t.TempDir(), "tellury-out")
	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-assets.json"},
		Format:         "table",
		Rules:          []string{"detached_disk"},
		FailOnFindings: false,
		OutDir:         outDir,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out, errOut bytes.Buffer
	if err := runScan(context.Background(), &out, &errOut, g, cfg, readmeNow); err != nil {
		t.Fatalf("runScan (--out-dir): %v", err)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", outDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one timestamped scan subdirectory under --out-dir, got %d", len(entries))
	}
	dir := filepath.Join(outDir, entries[0].Name())

	// The three artifacts a scan leaves behind.
	for name := range map[string]bool{
		"graph-projects-my-project.json":    true,
		"findings-projects-my-project.json": true,
		"report-projects-my-project.html":   true,
	} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Size() == 0 {
			t.Fatalf("artifact %s missing or empty: %v (err=%v)", name, st, err)
		}
	}

	// The HTML must be self-contained: no src= or href= pointing anywhere.
	html, err := os.ReadFile(filepath.Join(dir, "report-projects-my-project.html"))
	if err != nil {
		t.Fatalf("read HTML report: %v", err)
	}
	doc := string(html)
	for _, needle := range []string{"<script src=", "href=\"http", "src=\"http", "cdn"} {
		if strings.Contains(doc, needle) {
			t.Errorf("HTML report must be self-contained; found %q", needle)
		}
	}
	// It must lead with the hero number and carry the findings table with the
	// price source provenance on the row, plus the collapsed scan-details
	// section (the README fixture is a single-project, single-rule scan, so no
	// project chart and no waste-by-rule summary render — by design).
	for _, want := range []string{
		"<details>",           // collapsed scan-details section
		"Scan details",        // scan-details summary
		"total monthly waste", // hero label
		"Findings",            // findings table section
		"price_source",        // price provenance on a finding
		"pd-standard-01",      // the fixture resource
		"$8.00",               // the documented monthly waste
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("HTML report missing %q", want)
		}
	}
}

// TestReadmeCacheFileReplay verifies the "graph is replayable via --cache-file"
// claim end to end: a scan that first writes a cache file (live ingest path
// with fixtures) then replays it — the replay carrying its own scope so no
// --gcp-project is needed — and both runs agree on the finding.
func TestReadmeCacheFileReplay(t *testing.T) {
	gcpPriceFixtureEnv(t)

	dir := t.TempDir()
	cache := filepath.Join(dir, "snap.json")
	outDir := filepath.Join(dir, "out")

	// First run: fixture ingest + metrics-less graph written to --cache-file on
	// the miss. This is the "first run" half of the README example.
	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-assets.json"},
		Format:         "json",
		Rules:          []string{"detached_disk"},
		CacheFile:      cache,
		FailOnFindings: false,
		OutDir:         outDir,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate (first run): %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out1, errOut1 bytes.Buffer
	if err := runScan(context.Background(), &out1, &errOut1, g, cfg, readmeNow); err != nil {
		t.Fatalf("runScan (cache miss): %v", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("--cache-file not written on cache miss: %v", err)
	}

	// Second run: pure replay, same rules, no scope flag, no fixture — the
	// cache file carries its own scope (projects/my-project).
	cfg2 := config.Scan{
		Provider:       "gcp",
		CacheFile:      cache,
		Format:         "json",
		Rules:          []string{"detached_disk"},
		FailOnFindings: false,
		OutDir:         filepath.Join(dir, "out2"),
	}
	// Offline replay must pass Validate without a scope.
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("Validate (replay): %v", err)
	}
	var out2, errOut2 bytes.Buffer
	if err := runScan(context.Background(), &out2, &errOut2, g, cfg2, readmeNow); err != nil {
		t.Fatalf("runScan (cache replay): %v", err)
	}

	for _, item := range []string{out1.String(), out2.String()} {
		if !strings.Contains(item, "pd-standard-01") {
			t.Errorf("expected the fixture disk in JSON output, got:\n%s", item)
		}
		if !strings.Contains(item, `"detached_disk"`) {
			t.Errorf("expected the detached_disk rule in JSON output, got:\n%s", item)
		}
	}
}
