package output

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// maxTableFindings caps the human-facing table at the ten largest findings by
// monthly waste. Organization-wide scans can produce hundreds of rows that
// flood a terminal; the table shows the top ten and points at the HTML report
// the scan already wrote for the rest. The TOTAL row still reflects EVERY
// finding — FindingCount and TotalMonthlyWasteUSD are computed from all
// findings in NewReport, never from the ten displayed — and JSON/CSV remain
// complete: they are consumed by other tools, so the limit applies to the
// table only.
const maxTableFindings = 10

// colMoney is the width of the right-aligned money column — exactly the width
// of its header, "MONTHLY WASTE", so the header and the money share a right
// edge. Unlike the variable columns (resource, project, rule), which are sized
// to their widest displayed value for each scan, the money column is fixed so
// the table's right edge never shifts with the amounts.
const colMoney = 13

// tableLayout is the per-scan column layout. The variable columns (resource,
// project, rule) are sized to their widest displayed value, so a 30-character
// GCP project ID or a long resource name is rendered in full — never
// truncated — and can be copied straight into a gcloud command. A literal
// space separates every variable column, so a value that fills its cell
// (padded exactly at the boundary) can never run into the column to its right.
type tableLayout struct {
	resource int
	project  int // 0 for a single-project table (no PROJECT column)
	rule     int
	money    int
}

// layoutTable computes the column layout for a report from the findings the
// table will actually print (tableFindings) plus the headers. Because every
// variable column is as wide as its widest value, truncation is structurally
// impossible for displayed values.
func layoutTable(r Report, display []rules.Finding) tableLayout {
	l := tableLayout{
		resource: runeLen("RESOURCE"),
		rule:     runeLen("RULE"),
		money:    colMoney,
	}
	if r.MultiProject {
		l.project = runeLen("PROJECT")
	}
	for _, f := range display {
		if n := runeLen(f.Resource); n > l.resource {
			l.resource = n
		}
		if r.MultiProject {
			if n := runeLen(f.Project); n > l.project {
				l.project = n
			}
		}
		if n := runeLen(f.RuleID); n > l.rule {
			l.rule = n
		}
	}
	return l
}

// separatorWidth is the width of the dashed separator row — one full data
// row: the variable columns, their separator spaces, and the money column.
func (l tableLayout) separatorWidth() int {
	w := l.resource + 1 + l.rule + l.money
	if l.project > 0 {
		w += 1 + l.project
	}
	return w
}

// totalSummaryWidth is the width the TOTAL row gives its finding summary:
// every column left of the money cell except the resource label column,
// separator spaces included. The summary is not a project, so it must never
// be squeezed into the PROJECT column's width.
func (l tableLayout) totalSummaryWidth() int {
	if l.project > 0 {
		return 1 + l.project + l.rule
	}
	return l.rule
}

type tableRenderer struct{}

func (tableRenderer) Format() string { return "table" }

func (t tableRenderer) Render(w io.Writer, r Report) error {
	// Currency disclosure, before anything else: an operator reading non-USD
	// figures must see which currency they are in and how it was decided
	// before they read a single number. The default USD scan emits nothing,
	// keeping its output byte-identical to the pre-currency build.
	if lines := currencyDisclosure(r); len(lines) > 0 {
		for _, line := range lines {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	if len(r.Findings) == 0 {
		// Deliberately short: the summary line below already names the scope,
		// the resources scanned and the rules evaluated, and repeating them
		// here made two consecutive lines say the same thing.
		if _, err := fmt.Fprintln(w, "No waste found."); err != nil {
			return err
		}
	} else {
		// The table shows at most the ten largest findings by monthly waste
		// (tableFindings); the TOTAL row below still sums every finding.
		display := tableFindings(r)
		layout := layoutTable(r, display)

		if r.MultiProject {
			if err := writeRowProject(w, layout, "RESOURCE", "PROJECT", "RULE", "MONTHLY WASTE"); err != nil {
				return err
			}
			for _, f := range display {
				if err := writeRowProject(w, layout, f.Resource, f.Project, f.RuleID, r.money(f.MonthlyWasteUSD)); err != nil {
					return err
				}
			}
		} else {
			if err := writeRow(w, layout, "RESOURCE", "RULE", "MONTHLY WASTE"); err != nil {
				return err
			}
			for _, f := range display {
				if err := writeRow(w, layout, f.Resource, f.RuleID, r.money(f.MonthlyWasteUSD)); err != nil {
					return err
				}
			}
		}

		if _, err := fmt.Fprintln(w, strings.Repeat("-", layout.separatorWidth())); err != nil {
			return err
		}

		// The TOTAL row's finding summary spans the columns left of the money
		// cell: it is not a project, so it must not inherit the PROJECT column's
		// width (the old fixed 9-rune width truncated it to "9 findin…"). The
		// summary is left-aligned in that full span and is therefore never
		// truncated.
		summary := fmt.Sprintf("%d findings", r.FindingCount)
		if err := writeTotalRow(w, layout, "TOTAL", summary, r.money(r.TotalMonthlyWasteUSD)); err != nil {
			return err
		}

		// The table shows only the top ten: say plainly how many findings were
		// omitted and where the complete report lives, as a file:// URL so a
		// terminal makes it clickable. The TOTAL above already summed every
		// finding, not just the ten shown.
		if omitted := len(r.Findings) - len(display); omitted > 0 {
			note := fmt.Sprintf("%d of %d findings omitted", omitted, len(r.Findings))
			if r.ReportPath != "" {
				note += "; full report: " + reportURL(r.ReportPath)
			}
			if _, err := fmt.Fprintln(w, note); err != nil {
				return err
			}
		}
	}

	// Scan summary — printed after the table (or after the no-findings line
	// when the scan produced no table), in every case. It is the "what did
	// the scan actually look at" context, not the headline: the denominators
	// that tell an operator whether an empty findings table means "nothing
	// wasteful" (projects analyzed > 0, resources scanned > 0) or "nothing
	// scanned" (a broken scope with zero projects). Every number here is
	// carried by the Report, never measured at render time, so a replayed or
	// fixture-driven scan reports its real counts and its real duration and
	// the output stays deterministic for a given Report.
	if _, err := fmt.Fprintln(w, summaryLine(r)); err != nil {
		return err
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

// summaryLine renders the one-line scan summary: how much ground the scan
// covered (projects analyzed, resources scanned, rules evaluated) and what it
// produced (findings, resources skipped, duration). It is context, not the
// headline, so it is a single compact line. The duration comes from the
// Report — the scan's own clock — so rendering a Report twice always prints
// the same line.
//
// An AWS scan reports the account and the regions it actually covered — "1
// account analyzed, 2 regions analyzed, ..." — in place of the GCP projects
// figure. The branch keys on AccountsAnalyzed, which only an AWS report ever
// sets, so a GCP report renders byte-identically to the pre-AWS build.
func summaryLine(r Report) string {
	parts := make([]string, 0, 7)
	if r.AccountsAnalyzed > 0 {
		parts = append(parts, countPhrase(r.AccountsAnalyzed, "account analyzed", "accounts analyzed"))
		if r.RegionsAnalyzed > 0 {
			parts = append(parts, countPhrase(r.RegionsAnalyzed, "region analyzed", "regions analyzed"))
		}
	} else {
		parts = append(parts, countPhrase(r.ProjectsAnalyzed, "project analyzed", "projects analyzed"))
	}
	parts = append(parts,
		countPhrase(r.ResourcesScanned, "resource scanned", "resources scanned"),
		countPhrase(r.RulesEvaluated, "rule evaluated", "rules evaluated"),
		countPhrase(r.FindingCount, "finding", "findings"),
		countPhrase(r.ResourcesSkipped, "resource skipped", "resources skipped"),
		formatDuration(r.Duration),
	)
	// The scope leads the line. Without it a findings table says nothing about
	// what it is a scan OF: the PROJECT column only appears when findings span
	// more than one project, so a single-project run named the project nowhere
	// at all, and a table pasted into a ticket or scrolled past in CI could not
	// be attributed. The empty-result path has always named the scope
	// ("No waste found in projects/x"); this makes the two consistent.
	if r.Scope != "" {
		return "Summary: " + r.Scope + " — " + strings.Join(parts, ", ")
	}
	return "Summary: " + strings.Join(parts, ", ")
}

// countPhrase renders "N singular" or "N plural" for the summary line.
func countPhrase(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// writeRow emits one single-project row. A literal space separates the two
// variable columns so a value that fills its cell (padded exactly at the
// boundary) can never touch its neighbour; the money column is right-aligned
// at its fixed width.
func writeRow(w io.Writer, l tableLayout, resource, rule, amount string) error {
	_, err := fmt.Fprintf(w, "%-*s %-*s%*s\n",
		l.resource, resource,
		l.rule, rule,
		l.money, amount)
	return err
}

// writeRowProject emits one multi-project row, with a literal space between
// every variable column so no cell can touch its right-hand neighbour.
func writeRowProject(w io.Writer, l tableLayout, resource, project, rule, amount string) error {
	_, err := fmt.Fprintf(w, "%-*s %-*s %-*s%*s\n",
		l.resource, resource,
		l.project, project,
		l.rule, rule,
		l.money, amount)
	return err
}

// writeTotalRow emits the TOTAL row: the "TOTAL" label in the resource cell,
// the finding summary left-aligned in the full width of the columns left of
// the money cell, and the total right-aligned in the money column. The summary
// width (totalSummaryWidth) spans the project and rule columns, so "N
// findings" is never truncated the way it was when squeezed into the PROJECT
// column.
func writeTotalRow(w io.Writer, l tableLayout, label, summary, amount string) error {
	_, err := fmt.Fprintf(w, "%-*s %-*s%*s\n",
		l.resource, label,
		l.totalSummaryWidth(), summary,
		l.money, amount)
	return err
}

// tableFindings returns the findings the table prints: the full list when it
// fits in maxTableFindings, otherwise the ten largest by monthly waste. The
// report's FindingCount and TotalMonthlyWasteUSD — which the TOTAL row uses —
// are computed from ALL findings in NewReport, never from this slice, so the
// limit can never silently shrink the total.
func tableFindings(r Report) []rules.Finding {
	if len(r.Findings) <= maxTableFindings {
		return r.Findings
	}
	fs := append([]rules.Finding(nil), r.Findings...)
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		switch {
		case a.MonthlyWasteUSD != b.MonthlyWasteUSD:
			return a.MonthlyWasteUSD > b.MonthlyWasteUSD
		case a.Resource != b.Resource:
			return a.Resource < b.Resource
		default:
			return a.RuleID < b.RuleID
		}
	})
	return fs[:maxTableFindings]
}

// reportURL renders an absolute HTML report path as a file:// URL so a
// terminal can make it clickable. A Windows drive path ("C:\...") gets the
// scheme's required extra leading slash.
func reportURL(path string) string {
	p := filepath.ToSlash(filepath.Clean(path))
	if len(p) >= 2 && p[1] == ':' {
		return "file:///" + p
	}
	return "file://" + p
}

// runeLen returns the display width of s in runes, so a multi-byte character
// counts as one cell and columns are sized consistently with rune rendering.
func runeLen(s string) int { return utf8.RuneCountInString(s) }
