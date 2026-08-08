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
//
// When a scan spans more than one project (MultiProject), a PROJECT column is
// inserted between RESOURCE and RULE so an operator can tell where each
// resource lives. The resource column cedes cells to fund the new PROJECT
// width AND a hard separator after each variable column. That hard separator
// is the point of this layout: a value that fills its column (padded exactly
// at the boundary, or truncated down to it) must never run straight into the
// next column. Total width stays constant at 69 cells across single- and
// multi-project scans, so the table never grows past its single-project width
// even on an organization-wide scan.
const (
	colResource       = 30
	colResourceNarrow = 19
	colProject        = 9
	colRule           = 26
	colMoney          = 13
	sepWidth          = 66
)

type tableRenderer struct{}

func (tableRenderer) Format() string { return "table" }

func (t tableRenderer) Render(w io.Writer, r Report) error {
	if len(r.Findings) == 0 {
		if _, err := fmt.Fprintf(w, "No waste found in %s (%d resources, %d rules).\n",
			r.Scope, r.ResourcesScanned, r.RulesEvaluated); err != nil {
			return err
		}
	} else {
		if r.MultiProject {
			if err := writeRowProject(w, "RESOURCE", "PROJECT", "RULE", "MONTHLY WASTE"); err != nil {
				return err
			}
			for _, f := range r.Findings {
				if err := writeRowProject(w, f.Resource, f.Project, f.RuleID, money(f.MonthlyWasteUSD)); err != nil {
					return err
				}
			}
		} else {
			if err := writeRow(w, "RESOURCE", "RULE", "MONTHLY WASTE"); err != nil {
				return err
			}
			for _, f := range r.Findings {
				if err := writeRow(w, f.Resource, f.RuleID, money(f.MonthlyWasteUSD)); err != nil {
					return err
				}
			}
		}
		if _, err := fmt.Fprintln(w, strings.Repeat("-", sepWidth)); err != nil {
			return err
		}
		// The TOTAL row mirrors whatever column layout the header used, so a
		// four-column header gets a four-column total: RESOURCE, PROJECT (the
		// finding count), RULE (blank — project is not a summing dimension),
		// and MONTHLY WASTE.
		if r.MultiProject {
			if err := writeRowProject(w, "TOTAL",
				fmt.Sprintf("%d findings", r.FindingCount),
				"",
				money(r.TotalMonthlyWasteUSD)); err != nil {
				return err
			}
		} else if err := writeRow(w, "TOTAL",
			fmt.Sprintf("%d findings", r.FindingCount),
			money(r.TotalMonthlyWasteUSD)); err != nil {
			return err
		}
	}

	// Offline honesty: when the scan's data carried no metrics for some rules,
	// "no waste" would be a lie — those rules simply could not evaluate. State
	// which ones explicitly so a fixture run does not look like a clean bill of
	// health when it was actually "could not check".
	if len(r.MetricsBlocked) > 0 {
		if _, err := fmt.Fprintf(w,
			"\n%d rule(s) could not be evaluated for lack of metric data: %s\n"+
				"(use --cache-file from a live `scan` or an enriched `graph export` to evaluate them)\n",
			len(r.MetricsBlocked), strings.Join(r.MetricsBlocked, ", ")); err != nil {
			return err
		}
	}
	return nil
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

// writeRowProject emits one row of the multi-project layout. A literal space
// separates each left-aligned variable column (resource, project, rule), so a
// value that fills or truncates to its width can never touch its right-hand
// neighbour; colResourceNarrow and colProject already cede those cells from
// the content widths to keep the total width at 69. The total row leaves the
// rule cell empty (project is not a summing dimension).
func writeRowProject(w io.Writer, resource, project, rule, amount string) error {
	_, err := fmt.Fprintf(w, "%-*s %-*s %-*s%*s\n",
		colResourceNarrow, truncate(resource, colResourceNarrow),
		colProject, truncate(project, colProject),
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
