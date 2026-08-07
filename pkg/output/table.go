package output

import (
	"fmt"
	"io"
	"strings"
)

// Column widths reproduce the mandated layout exactly:
//
//	RESOURCE                      RULE                      MONTHLY WASTE
//	disk/pd-standard-01           detached_disk                    $12.40
//	------------------------------------------------------------------
//	TOTAL                         3 findings                      $128.25
//
// RESOURCE is left-aligned in 30, RULE left-aligned in 26, MONTHLY WASTE
// right-aligned in 13 — which is exactly the width of the header text, so the
// header and the money column share a right edge.
const (
	colResource = 30
	colRule     = 26
	colMoney    = 13
	sepWidth    = 66
)

type tableRenderer struct{}

func (tableRenderer) Format() string { return "table" }

func (t tableRenderer) Render(w io.Writer, r Report) error {
	if len(r.Findings) == 0 {
		_, err := fmt.Fprintf(w, "No waste found in %s (%d resources, %d rules).\n",
			r.Scope, r.ResourcesScanned, r.RulesEvaluated)
		return err
	}

	if err := writeRow(w, "RESOURCE", "RULE", "MONTHLY WASTE"); err != nil {
		return err
	}
	for _, f := range r.Findings {
		if err := writeRow(w, f.Resource, f.RuleID, money(f.MonthlyWasteUSD)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, strings.Repeat("-", sepWidth)); err != nil {
		return err
	}
	return writeRow(w, "TOTAL",
		fmt.Sprintf("%d findings", r.FindingCount),
		money(r.TotalMonthlyWasteUSD))
}

// writeRow emits one fixed-width row. Fixed widths (rather than tabwriter's
// content-derived widths) are what make the layout stable across scans: a long
// resource name must not shift the money column.
func writeRow(w io.Writer, resource, rule, amount string) error {
	_, err := fmt.Fprintf(w, "%-*s%-*s%*s\n",
		colResource, truncate(resource, colResource),
		colRule, truncate(rule, colRule),
		colMoney, amount)
	return err
}

// truncate keeps the column intact, reserving one cell for the ellipsis. It is
// rune-safe so a multi-byte name cannot be cut mid-character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}
