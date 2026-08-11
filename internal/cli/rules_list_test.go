package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestRulesListIncludesAWSRules is the acceptance test for the stop condition
// "both rules appear in `tellury rules list`": it drives the REAL
// `tellury rules list` command (newRulesListCmd, the same command the root CLI
// registers) and asserts the two AWS native rules are in its output. A rule
// whose blank import was dropped from pkg/rules/all would vanish from the
// registry and from this table while build/vet/test elsewhere stayed green —
// the silent-nothing trap docs/writing-a-rule.md §5 warns about — so this
// test pins the operator-visible catalogue directly.
func TestRulesListIncludesAWSRules(t *testing.T) {
	cmd := newRulesListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rules list: %v", err)
	}
	got := buf.String()

	for _, id := range []string{"unattached_ebs_volume", "unassociated_eip"} {
		if !strings.Contains(got, id) {
			t.Errorf("`tellury rules list` output is missing rule %q:\n%s", id, got)
		}
	}
	// The header must also be present so the table rendered at all.
	if !strings.Contains(got, "ID\tPROVIDER") && !strings.Contains(got, "PROVIDER") {
		t.Errorf("`tellury rules list` output has no header row:\n%s", got)
	}
}
