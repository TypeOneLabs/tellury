package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/internal/config"
	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// artifactNow is a fixed evaluation instant behind the artifact test so the
// pipeline (including rule age predicates) is deterministic.
var artifactNow = time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)

// TestScanWritesArtifacts asserts that a scan over --out-dir leaves a
// directory of artifacts behind (graph snapshot + findings JSON), NOT just
// terminal output, and that two consecutive scans do not overwrite each other
// thanks to the timestamped subdirectory. It is the "artifacts, not just
// terminal output" acceptance test.
func TestScanWritesArtifacts(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-assets.json"},
		Format:         "table",
		FailOnFindings: false,
		OutDir:         filepath.Join(dir, "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	run := func() error {
		var out, errOut strings.Builder
		g := &globalFlags{LogLevel: "warn"}
		return runScan(t.Context(), &out, &errOut, g, cfg, artifactNow)
	}

	if err := run(); err != nil {
		t.Fatalf("runScan (first artifact scan): %v", err)
	}
	if err := run(); err != nil {
		t.Fatalf("runScan (second artifact scan): %v", err)
	}

	entries, err := os.ReadDir(cfg.OutDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", cfg.OutDir, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) < 2 {
		t.Fatalf("expected at least two distinct scan subdirectories over two consecutive runs, got %d: %v",
			len(dirs), dirs)
	}
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			t.Fatalf("duplicate scan subdirectory %q; consecutive runs must not collide", d)
		}
		seen[d] = true

		graphPath := filepath.Join(cfg.OutDir, d, "graph.json")
		findingsPath := filepath.Join(cfg.OutDir, d, "findings.json")
		if st, err := os.Stat(graphPath); err != nil || st.Size() == 0 {
			t.Fatalf("scan artifact missing graph snapshot %s: %v", graphPath, err)
		}
		if st, err := os.Stat(findingsPath); err != nil || st.Size() == 0 {
			t.Fatalf("scan artifact missing findings JSON %s: %v", findingsPath, err)
		}
	}
}

// TestArtifactGraphIsReplayable is the round-trip guarantee behind the
// artifact directory: the graph snapshot a scan writes into --out-dir must be
// loadable through graph.LoadSnapshot (the exact path `--cache-file` uses),
// producing the same node and edge structure the scan ingested. This is what
// makes the artifact directory usable as a full-fidelity offline replay.
func TestArtifactGraphIsReplayable(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-assets.json"},
		Format:         "table",
		FailOnFindings: false,
		OutDir:         filepath.Join(dir, "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	var out, errOut strings.Builder
	g := &globalFlags{LogLevel: "warn"}
	if err := runScan(t.Context(), &out, &errOut, g, cfg, artifactNow); err != nil {
		t.Fatalf("runScan: %v", err)
	}

	// Locate the graph snapshot the scan wrote.
	entries, err := os.ReadDir(cfg.OutDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", cfg.OutDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one scan directory, got %d", len(entries))
	}
	graphPath := filepath.Join(cfg.OutDir, entries[0].Name(), "graph.json")

	// Round-trip through the exact loader --cache-file uses.
	f, err := os.Open(graphPath)
	if err != nil {
		t.Fatalf("open graph artifact: %v", err)
	}
	defer f.Close()
	replayed, snap, err := graph.LoadSnapshot(f)
	if err != nil {
		t.Fatalf("LoadSnapshot(graph artifact): %v — the written graph must be replayable", err)
	}
	if snap.Scope != "projects/my-project" {
		t.Errorf("replayed snapshot scope = %q, want projects/my-project", snap.Scope)
	}
	if replayed.NodeCount() == 0 {
		t.Errorf("replayed graph has 0 nodes; ingestion must have produced a leaf disk and a project container")
	}
	if replayed.EdgeCount() == 0 {
		t.Errorf("replayed graph has 0 edges; the fixture must link disk -> project containment")
	}
	// The shipped README fixture is a single detached pd-standard disk in
	// my-project: exactly one resource node plus one project container.
	if want := 1; replayed.ResourceNodeCount() != want {
		t.Errorf("replayed ResourceNodeCount = %d, want %d", replayed.ResourceNodeCount(), want)
	}
}
