package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// projectRowWidth is the multi-project row's fixed width: RESOURCE (19) + one
// separator + PROJECT (9) + one separator + RULE (26) + MONTHLY WASTE (13).
// Because every variable column is right-padded to exactly its width and a
// literal space follows each one, a value that fills its cell (padded exactly
// at the boundary) or that is truncated down to it can never run straight
// into the next column. The single-project layout is 30+26+13 = 69 too, so
// an organization-wide scan never grows past the single-project width.
const projectRowWidth = colResourceNarrow + 1 + colProject + 1 + colRule + colMoney

// offsetProjectStart is the rune offset where the PROJECT cell begins: after
// the resource field and its single separator space.
const offsetProjectStart = colResourceNarrow + 1

// offsetRuleStart is the rune offset where the RULE cell begins: after the
// project field and its single separator space.
const offsetRuleStart = colResourceNarrow + 1 + colProject + 1

// TestTableColumnsNeverTouch_MultiProject is the boundary regression for the
// multi-project layout. GCP project IDs are routinely longer than the column
// width, so a value that fills its cell (padded exactly at the boundary — a
// 9-rune project) or a value that is truncated down to it (a 10+-rune project)
// must still be separated from the RULE column by at least one space. This is
// the case that used to collide when the separator was missing.
func TestTableColumnsNeverTouch_MultiProject(t *testing.T) {
	if projectRowWidth != 69 {
		t.Fatalf("project row width = %d, want 69; layout drift must be caught here", projectRowWidth)
	}

	report := Report{
		Scope:      "projects/alpha-proj",
		Provider:   "gcp",
		WindowDays: 14,
		Findings: []rules.Finding{
			// "abcdefghi" is exactly colProject (9): the padded-exactly boundary.
			{RuleID: "detached_disk", Resource: "disk/disk-a", Project: "abcdefghi", MonthlyWasteUSD: 8.00},
			// Longer than the 9-rune width: truncates down to a 9-rune cell.
			{RuleID: "detached_disk", Resource: "disk/d2", Project: "a-very-long-project-name", MonthlyWasteUSD: 8.00},
			// Short: left-padding must not collide either.
			{RuleID: "detached_disk", Resource: "disk/d1", Project: "ab", MonthlyWasteUSD: 8.00},
		},
		TotalMonthlyWasteUSD: 24.00,
		FindingCount:         3,
		MultiProject:         true,
		ResourcesScanned:     3,
		RulesEvaluated:       1,
	}

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, report); err != nil {
		t.Fatalf("Render: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 6 { // header + 3 data + separator + TOTAL
		t.Fatalf("expected 6 lines, got %d:\n%s", len(lines), buf.String())
	}

	// Header + the three data rows: every variable boundary must hold a space.
	for _, line := range lines[:4] {
		assertRowSeparated(t, line)
	}
	assertRowSeparated(t, lines[1]) // exact-width project
	assertRowSeparated(t, lines[2]) // truncated project
	assertRowSeparated(t, lines[3]) // short project (padding)

	// The TOTAL row mirrors the header's four-column layout and is likewise
	// separated at every variable boundary.
	totalRow := lines[len(lines)-1]
	assertRowSeparated(t, totalRow)
	if !strings.Contains(totalRow, "TOTAL") {
		t.Errorf("TOTAL row missing TOTAL in the resource cell: %q", totalRow)
	}
	// The finding count lands in the PROJECT cell and the money lands in the
	// far-right MONTHLY WASTE cell — proving the total row uses four columns
	// and not the three-column layout.
	projectCell := strings.TrimSpace(runeSlice(totalRow, offsetProjectStart, colProject))
	if !strings.Contains(projectCell, "findings") && !strings.Contains(projectCell, "findin") {
		t.Errorf("TOTAL row PROJECT cell = %q, want the finding count; the total must use the four-column layout", projectCell)
	}
	moneyCell := strings.TrimSpace(runeSlice(totalRow, offsetRuleStart+colRule, colMoney))
	if moneyCell != "$24.00" {
		t.Errorf("TOTAL row MONTHLY WASTE cell = %q, want $24.00", moneyCell)
	}
}

// assertRowSeparated asserts the row is exactly projectRowWidth runes wide and
// that a literal space sits at both variable-column boundaries, so no value in
// a RESOURCE, PROJECT or RULE cell can touch the cell to its right.
func assertRowSeparated(t *testing.T, line string) {
	t.Helper()
	runes := []rune(line)
	if len(runes) != projectRowWidth {
		t.Errorf("row rune length = %d, want %d: %q", len(runes), projectRowWidth, line)
		return
	}
	// Boundary after RESOURCE: rune at offset colResourceNarrow must be a space.
	if runes[colResourceNarrow] != ' ' {
		t.Errorf("resource cell runs into project cell (offset %d not a space): %q", colResourceNarrow, line)
	}
	// Boundary after PROJECT: rune at offsetProjectStart+colProject must be a space.
	if runes[offsetRuleStart-1] != ' ' {
		t.Errorf("project cell runs into rule cell (offset %d not a space): %q", offsetRuleStart-1, line)
	}
}

// runeSlice returns runes[s:s+n] of line as a string; never panics on a short
// line, returning what is available.
func runeSlice(line string, s, n int) string {
	r := []rune(line)
	if s > len(r) {
		return ""
	}
	e := s + n
	if e > len(r) {
		e = len(r)
	}
	return string(r[s:e])
}
