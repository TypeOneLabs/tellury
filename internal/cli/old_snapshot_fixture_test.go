package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/internal/config"
)

// oldSnapshotFixture is the fixture the `old_snapshot` rule ships with, one
// level down from the rule package it lives beside. The scan below runs the
// exact command shape docs/writing-a-rule.md documents against it, through the
// real runScan pipeline (rule selection -> offline provider -> ingest ->
// rules -> table render + --explain-skips), so the guide's worked example is
// not help text: it is this test.
const oldSnapshotFixture = "../../pkg/rules/gcp/compute/old_snapshot/testdata/old-snapshot.json"

// TestSkillWorkedExample_OldSnapshotScan pins the worked example the
// rule-writing guide ships: at a fixed evaluation instant (--at) the old
// snapshot fires at its exact flat cost, the young snapshot is skipped as
// too_young, and the size-less snapshot is skipped as missing_attribute. It
// also logs the real stdout/stderr the guide's commands produce.
func TestSkillWorkedExample_OldSnapshotScan(t *testing.T) {
	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{oldSnapshotFixture},
		Format:         "table",
		Rules:          []string{"old_snapshot"},
		FailOnFindings: false,
		ExplainSkips:   true,
		OutDir:         filepath.Join(t.TempDir(), "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out, errOut bytes.Buffer
	// The exact evaluation instant the guide documents:
	// 2024-01-20T00:00:00Z, making every age deterministic.
	scanAt := readmeNow // 2024-01-20T00:00:00Z
	if err := runScan(context.Background(), &out, &errOut, g, cfg, scanAt); err != nil {
		t.Fatalf("runScan (old_snapshot fixture): %v", err)
	}

	t.Logf("=== `tellury scan --fixture ... --rules old_snapshot --at 2024-01-20T00:00:00Z --explain-skips` (stdout) ===\n%s", out.String())
	t.Logf("=== same command (stderr: --explain-skips) ===\n%s", errOut.String())

	got := out.String()
	if !strings.Contains(got, "snapshot/backup-2023-01-01") {
		t.Errorf("table output missing the old snapshot resource:\n%s", got)
	}
	if !strings.Contains(got, "old_snapshot") {
		t.Errorf("table output missing the old_snapshot rule column:\n%s", got)
	}
	// The snapshot bills on storageBytes (30 GiB incremental), NOT on the 250
	// GiB source disk: 30 x $0.050/GiB-month = $1.50/month. Pricing the source
	// disk instead gives $12.50 — the defect this asserts against, which
	// overstated a real organization's snapshot waste by ~9x.
	if !strings.Contains(got, "$1.50") {
		t.Errorf("table output missing the $1.50 monthly waste (30 GiB billable x $0.050/GiB-month):\n%s", got)
	}

	skips := errOut.String()
	if !strings.Contains(skips, "too_young") || !strings.Contains(skips, "1") {
		t.Errorf("--explain-skips missing the too_young tally for the young snapshot:\n%s", skips)
	}
	if !strings.Contains(skips, "missing_attribute") {
		t.Errorf("--explain-skips missing the missing_attribute tally for the size-less snapshot:\n%s", skips)
	}
}

// TestSkillWorkedExample_RulesList pins the registration half of the worked
// example: `tellury rules list` renders the new rule among the shipped set. It
// exercises the exact command implementation the CLI runs.
func TestSkillWorkedExample_RulesList(t *testing.T) {
	cmd := newRulesListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rules list: %v", err)
	}

	t.Logf("=== `tellury rules list` ===\n%s", buf.String())

	got := buf.String()
	if !strings.Contains(got, "old_snapshot") {
		t.Errorf("rules list missing the new rule:\n%s", got)
	}
	if !strings.Contains(got, "gcp") || !strings.Contains(got, "compute") {
		t.Errorf("rules list should attribute old_snapshot to gcp/compute:\n%s", got)
	}
}
