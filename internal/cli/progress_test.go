package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/internal/config"
)

// TestProgress_JSONPipeStillParses is the stdout-purity contract, driven
// through the REAL runScan pipeline with progress FORCED on (--progress on):
// stderr carries the phase lines while stdout stays a single valid JSON
// document — exactly what `tellury scan --format json | jq` must keep parsing
// when the operator enables progress. This is the acceptance test for "progress
// goes to stderr, never stdout".
func TestProgress_JSONPipeStillParses(t *testing.T) {
	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-assets.json"},
		Format:         "json",
		Rules:          []string{"detached_disk"},
		FailOnFindings: false,
		OutDir:         filepath.Join(t.TempDir(), "out"),
		Progress:       "on",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out, errOut bytes.Buffer
	if err := runScan(context.Background(), &out, &errOut, g, cfg, readmeNow); err != nil {
		t.Fatalf("runScan (json + progress on): %v", err)
	}

	t.Logf("=== `tellury scan --format json --progress on` (stderr: progress) ===\n%s", errOut.String())

	// stdout must be ONE valid JSON document — the stream `jq` parses. If a
	// progress line had leaked into it, json.Unmarshal would fail.
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON with progress enabled: %v\n%s", err, out.String())
	}
	for _, field := range []string{"findings", "duration", "projects_analyzed"} {
		if _, ok := doc[field]; !ok {
			t.Errorf("stdout JSON lost the %q field:\n%s", field, out.String())
		}
	}

	// stderr must carry the phase lines — that is the progress stream.
	progress := errOut.String()
	for _, phase := range []string{"asset discovery", "rule evaluation"} {
		if !strings.Contains(progress, "    "+phase+":") {
			t.Errorf("stderr must carry a %q progress phase line:\n%s", phase, progress)
		}
	}
	// and stderr must be plain: no ANSI escapes, no carriage returns, even
	// with progress forced on into a non-terminal writer.
	for _, bad := range []string{"\x1b[", "\r"} {
		if strings.Contains(progress, bad) {
			t.Errorf("stderr must never contain %q (no ANSI off a terminal):\n%q", bad, progress)
		}
	}
}

// TestProgress_DefaultAutoIsSilentOffATerminal pins the "stay silent" branch
// of the non-TTY contract: with the default mode (auto) and a stderr that is
// a buffer/pipe/file rather than a terminal, the scan emits NO progress lines
// at all — CI logs and `2>err.log` files are never scribbled on by default.
func TestProgress_DefaultAutoIsSilentOffATerminal(t *testing.T) {
	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-assets.json"},
		Format:         "table",
		Rules:          []string{"detached_disk"},
		FailOnFindings: false,
		OutDir:         filepath.Join(t.TempDir(), "out"),
		Progress:       "", // default resolves to auto
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out, errOut bytes.Buffer
	if err := runScan(context.Background(), &out, &errOut, g, cfg, readmeNow); err != nil {
		t.Fatalf("runScan (auto, non-terminal): %v", err)
	}
	if got := errOut.String(); strings.Contains(got, "tellury:") {
		t.Errorf("auto progress must be silent when stderr is not a terminal:\n%s", got)
	}
}

// TestProgress_OffSuppressesEntirely pins the suppression control: --progress
// off must leave stderr exactly as it was before progress existed.
func TestProgress_OffSuppressesEntirely(t *testing.T) {
	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-assets.json"},
		Format:         "table",
		Rules:          []string{"detached_disk"},
		FailOnFindings: false,
		OutDir:         filepath.Join(t.TempDir(), "out"),
		Progress:       "off",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out, errOut bytes.Buffer
	if err := runScan(context.Background(), &out, &errOut, g, cfg, readmeNow); err != nil {
		t.Fatalf("runScan (progress off): %v", err)
	}
	if got := errOut.String(); strings.Contains(got, "tellury:") {
		t.Errorf("--progress off must leave stderr free of progress lines:\n%s", got)
	}
}

// TestProgressPhase_ReportsDenominatorAndThrottles is a unit test on the
// reporter itself: a phase with a denominator prints the count when it first
// appears, the Set calls inside the throttle interval are coalesced, and the
// done line carries the final count plus the detail. The writer is a buffer
// (not a terminal), so --progress on degrades to plain lines exactly as it
// would in a CI log.
func TestProgressPhase_ReportsDenominatorAndThrottles(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "on")
	ph := p.Begin("metric enrichment", "fetches")
	if ph == nil {
		t.Fatal("Begin with --progress on must return a phase")
	}
	ph.Set(0, 100) // establishes the denominator; prints "0/100 fetches"
	ph.Set(1, 100) // within the 5s non-terminal interval: throttled
	ph.Set(50, 100)
	ph.End("2 projects")

	got := buf.String()
	for _, want := range []string{
		"    metric enrichment: started\n",
		"    metric enrichment: 0/100 fetches (",
		"metric enrichment: done 50/100 fetches (",
		"2 projects)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("phase output missing %q:\n%s", want, got)
		}
	}
	// The intermediate, throttled Set calls must not each print a line.
	if strings.Contains(got, "1/100") {
		t.Errorf("throttled Set must not print 1/100 within the interval:\n%s", got)
	}
	// The final count 50/100 must appear exactly once — on the done line.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "50/100") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("50/100 must appear exactly once (the done line), got %d:\n%s", n, got)
	}
}

// TestProgress_BeginDisabledReturnsNil pins the nil contract: when progress is
// disabled, Begin returns nil and every Phase method is a safe no-op, so call
// sites can nil-check exactly once.
func TestProgress_BeginDisabledReturnsNil(t *testing.T) {
	p := newProgress(&bytes.Buffer{}, "off")
	if p.Enabled() {
		t.Fatal("--progress off must disable the reporter")
	}
	if ph := p.Begin("rule evaluation", "rules"); ph != nil {
		t.Fatal("Begin with progress disabled must return nil")
	}
	// auto + non-terminal is also disabled: the "stay silent" branch.
	pAuto := newProgress(&bytes.Buffer{}, "auto")
	if pAuto.Enabled() {
		t.Fatal("auto + non-terminal must be disabled")
	}
}
