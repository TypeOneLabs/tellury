// Package output renders a scan Report. Every renderer is deterministic: the
// findings are pre-sorted by the engine and the totals are computed once, in
// NewReport, so the table, JSON and CSV forms can never disagree about money.
package output

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// Report is the render input.
type Report struct {
	Scope       string    `json:"scope"`
	Provider    string    `json:"provider"`
	GeneratedAt time.Time `json:"generated_at"`
	WindowDays  int       `json:"window_days"`

	Findings []rules.Finding `json:"findings"`

	TotalMonthlyWasteUSD float64 `json:"total_monthly_waste_usd"`
	FindingCount         int     `json:"finding_count"`
	ResourcesScanned     int     `json:"resources_scanned"`
	RulesEvaluated       int     `json:"rules_evaluated"`

	// ProjectsAnalyzed is the number of project container nodes the scan's
	// graph carried — the denominator that tells an operator whether an empty
	// findings table means "nothing wasteful" (projects analyzed > 0) or
	// "nothing scanned" (a broken scope with zero projects). It is derived
	// from the graph's project container nodes, never from the findings, so a
	// scan with no findings still reports the projects it analyzed.
	ProjectsAnalyzed int `json:"projects_analyzed"`

	// AccountsAnalyzed is the AWS analog of ProjectsAnalyzed: the number of
	// account container nodes the scan's graph carried. It is set only for an
	// AWS scan (GCP never sets it, so the GCP report is byte-identical), and
	// the table summary shows it in place of the projects figure.
	AccountsAnalyzed int `json:"accounts_analyzed,omitempty"`

	// RegionsAnalyzed is the number of region container nodes the scan's
	// graph carried — the "which regions did the scan actually cover" answer
	// an AWS operator needs to tell "nothing wasteful" from "nothing
	// scanned". It is set only for an AWS scan (GCP never sets it, so the GCP
	// report is byte-identical) and the table summary shows it right after
	// the account figure.
	RegionsAnalyzed int `json:"regions_analyzed,omitempty"`

	// ResourcesSkipped is the total number of resource-rule skips recorded
	// during evaluation — the sum of every entry in Skipped, which is exactly
	// the count `--explain-skips` breaks down per (rule, code). It is the
	// denominator beside finding_count: a machine reader that sees 5 findings
	// out of 400 resources skipped needs both to judge the scan's coverage.
	ResourcesSkipped int `json:"resources_skipped"`

	// Duration is the wall-clock time the scan took, measured by the scan's
	// own clock and threaded through Meta. It is NEVER re-measured inside a
	// renderer, so a replayed or fixture-driven scan reports its real
	// duration and the value is testable with a fixed clock rather than being
	// whatever the machine felt like at render time. It is serialized as an
	// integer count of nanoseconds.
	Duration time.Duration `json:"duration"`

	RuleErrors map[string]string `json:"rule_errors,omitempty"`
	Skipped    []rules.SkipTally `json:"skipped,omitempty"`

	// MultiProject reports whether the scan's findings span more than one
	// project. The table renderer surfaces a PROJECT column only in that
	// case, so a single-project scan keeps its compact width while an
	// organization-wide scan gives the operator a way to tell where each
	// resource lives. CSV always writes the project column per finding.
	MultiProject bool `json:"multi_project,omitempty"`

	// MetricsBlocked lists the rule IDs that were NOT evaluated because the
	// scan's data carried no metric series for the rules' required keys. This
	// is the offline summary's honesty mechanism: a raw CAI fixture carries no
	// metrics, so every metric-dependent rule lands here and the operator can
	// see "could not check" instead of mistaking an empty table for "no waste".
	// A cached-snapshot replay usually carries full metric fidelity and so has
	// no blocked rules.
	MetricsBlocked []string `json:"metrics_blocked,omitempty"`

	// Currency is the ISO 4217 code the findings' money figures are actually
	// in. It is empty for the default case (no --currency, no detection, USD),
	// which keeps the default scan's JSON and table byte-identical to the
	// pre-currency build. When non-empty, every renderer names it and amounts
	// are rendered with the code instead of a "$" prefix.
	Currency string `json:"currency,omitempty"`
	// CurrencySource says how Currency was decided: "flag" (explicit
	// --currency/TELLURY_CURRENCY) or "detected" (from a billing account).
	// Empty for the default USD case.
	CurrencySource string `json:"currency_source,omitempty"`
	// CurrencyRequested is the code the operator asked for (flag) or the tool
	// detected, before fallback. It differs from Currency only when the
	// embedded USD table answered a non-USD request (the currency trap).
	CurrencyRequested string `json:"currency_requested,omitempty"`
	// CurrencyMixed reports that USD embedded-fallback prices were used while
	// a non-USD currency was requested — the scan's figures are partly or
	// wholly USD although the operator asked for another currency. Human
	// renderers surface this as a loud warning.
	CurrencyMixed bool `json:"currency_mixed,omitempty"`

	// ReportPath is the absolute path of the self-contained HTML report the
	// scan wrote for this run. The table renderer uses it in the "N of M
	// findings omitted; full report: file://..." footer so an operator can
	// open the complete report from the terminal. It is a human-facing hint
	// only and is excluded from the JSON serialization (json:"-"), so the
	// machine-readable findings stay byte-identical to the pre-report-path
	// build.
	ReportPath string `json:"-"`
}

// Meta carries the scan context that is not derivable from the findings.
type Meta struct {
	Scope            string
	Provider         string
	GeneratedAt      time.Time
	WindowDays       int
	ResourcesScanned int
	RulesEvaluated   int

	// ProjectsAnalyzed is the count of project container nodes in the scan's
	// graph (graph.Graph.ProjectContainerCount). It is carried here rather
	// than derived from the findings so a scan with no findings still reports
	// the projects it looked at.
	ProjectsAnalyzed int

	// AccountsAnalyzed / RegionsAnalyzed are the AWS analogs (account and
	// region container nodes). Both stay zero for a GCP scan, which keeps the
	// GCP report byte-identical to the pre-AWS build.
	AccountsAnalyzed int
	RegionsAnalyzed  int

	// Duration is the scan's wall-clock duration, measured by the scan's own
	// clock and never re-measured inside a renderer.
	Duration time.Duration

	MultiProject bool

	// Currency fields describe how the scan's money figures were decided and
	// what currency they are actually in; see Report for the exact meanings.
	// NewReport clears all four when Currency is empty or the source is the
	// default, so a default USD scan's output stays byte-identical to the
	// pre-currency build.
	Currency          string
	CurrencySource    string
	CurrencyRequested string
	CurrencyMixed     bool
}

// NewReport assembles a Report and computes the totals exactly once. The sum is
// over unrounded finding values and is rounded a single time (invariant I3).
func NewReport(res rules.Result, m Meta) Report {
	// A default scan (no currency requested, nothing detected) must render
	// exactly as it did before currency existed: no currency fields, "$"
	// amounts, the historical JSON shape. The CLI sets Currency="" and
	// CurrencySource="default" for that case; the guard here also covers a
	// direct NewReport caller that left the fields zero.
	if m.Currency == "" || m.CurrencySource == "" || m.CurrencySource == "default" {
		m.Currency, m.CurrencySource, m.CurrencyRequested, m.CurrencyMixed = "", "", "", false
	}

	r := Report{
		Scope:             m.Scope,
		Provider:          m.Provider,
		GeneratedAt:       m.GeneratedAt.UTC(),
		WindowDays:        m.WindowDays,
		Findings:          res.Findings,
		FindingCount:      len(res.Findings),
		ResourcesScanned:  m.ResourcesScanned,
		RulesEvaluated:    m.RulesEvaluated,
		ProjectsAnalyzed:  m.ProjectsAnalyzed,
		AccountsAnalyzed:  m.AccountsAnalyzed,
		RegionsAnalyzed:   m.RegionsAnalyzed,
		Duration:          m.Duration,
		MultiProject:      m.MultiProject,
		Skipped:           res.SkipTotals(),
		Currency:          m.Currency,
		CurrencySource:    m.CurrencySource,
		CurrencyRequested: m.CurrencyRequested,
		CurrencyMixed:     m.CurrencyMixed,
	}
	// ResourcesSkipped is the sum of the skip tallies the report already
	// carries, so the two can never disagree: the summary's "N resources
	// skipped" is exactly what `--explain-skips` breaks down per (rule, code).
	r.ResourcesSkipped = skipTotal(r.Skipped)

	total := 0.0
	for _, f := range res.Findings {
		total += f.MonthlyWasteUSD
	}
	r.TotalMonthlyWasteUSD = pricing.Round2(total)

	if len(res.Errors) > 0 {
		r.RuleErrors = make(map[string]string, len(res.Errors))
		for id, err := range res.Errors {
			r.RuleErrors[id] = err.Error()
		}
	}
	return r
}

// skipTotal sums a report's skip tallies into the single "resources skipped"
// figure the scan summary prints. It is the exact total `--explain-skips`
// breaks down per (rule, code), so the summary and the breakdown can never
// disagree.
func skipTotal(tallies []rules.SkipTally) int {
	n := 0
	for _, t := range tallies {
		n += t.Count
	}
	return n
}

// formatDuration renders the scan's duration compactly for human renderers:
// rounded to milliseconds once the scan reaches a millisecond (so a real scan
// reads "1.235s", not "1.234567891s"), and to microseconds below that so a
// sub-millisecond fixture replay still reads as a real, non-zero duration
// instead of "0s". A zero duration — a Report built directly, or a scan that
// finished within one microsecond — reads "0s".
func formatDuration(d time.Duration) string {
	switch {
	case d >= time.Second:
		return d.Round(time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(time.Millisecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}

// Renderer writes a Report. Implementations MUST be deterministic.
type Renderer interface {
	Format() string
	Render(w io.Writer, r Report) error
}

// Formats lists the supported --format values.
var Formats = []string{"table", "json", "csv"}

// For returns the renderer for a --format value.
func For(format string) (Renderer, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "table":
		return tableRenderer{}, nil
	case "json":
		return jsonRenderer{}, nil
	case "csv":
		return csvRenderer{}, nil
	default:
		return nil, fmt.Errorf("invalid --format %q (want %s)", format, strings.Join(Formats, "|"))
	}
}

// money formats an amount in the report's currency with the fixed 2-dp
// convention. USD (including the empty default) keeps the historical "$"
// prefix — "$12.40" — so a default scan renders byte-identically; any other
// currency appends its code — "12.40 EUR" — so a EUR figure can never be
// mistaken for dollars.
func (r Report) money(v float64) string {
	v = pricing.Round2(v)
	if r.Currency == "" || r.Currency == "USD" {
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("%.2f %s", v, r.Currency)
}

// currencyDisclosure renders the scan's currency disclosure for the human
// table and HTML outputs. It returns nothing for the default USD scan, so
// that output stays byte-identical to the pre-currency build. For a non-USD
// scan it states the effective currency and how it was decided; when USD
// fallback prices contaminated the scan it returns a loud warning naming the
// requested currency so an operator reading EUR figures is never silently
// handed USD numbers.
func currencyDisclosure(r Report) []string {
	if r.Currency == "" {
		return nil
	}
	how := "assumed"
	switch r.CurrencySource {
	case "flag":
		how = "requested via --currency"
	case "detected":
		how = "detected from the billing account"
	}
	if !r.CurrencyMixed {
		return []string{fmt.Sprintf("Prices are in %s (%s).", r.Currency, how)}
	}
	if r.Currency == r.CurrencyRequested || r.CurrencyRequested == "" {
		// Partial contamination: the catalogue answered in the requested
		// currency, but some prices fell back to the USD table.
		return []string{
			fmt.Sprintf("WARNING: some prices came from the embedded USD fallback table, not the %s catalogue.", r.Currency),
			"Those figures are USD and were NOT converted; reconcile them against the " + r.Currency + " bill by hand.",
		}
	}
	// Full fallback: the requested currency priced nothing.
	return []string{
		fmt.Sprintf("WARNING: prices are in %s, not the requested %s.", r.Currency, r.CurrencyRequested),
		"The Cloud Billing catalogue did not answer in " + r.CurrencyRequested + " and the embedded fallback table is USD-only.",
		"These figures were NOT converted; reconcile them against the " + r.CurrencyRequested + " bill by hand.",
	}
}
