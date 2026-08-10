// Renderers for the self-contained HTML report.
//
// The HTML report is a single self-contained file (inline CSS, one inline
// <script> at the end of <body>, no CDN, no network fetch at runtime) written
// into the artifact directory alongside the graph and findings JSON. It is
// structured so the reader answers the questions in order:
//
//   - Header: the hero number (total monthly waste, or "$0.00"/"No data" for
//     the two degenerate states), the scan meta line, and any currency,
//     metrics-blocked or rule-error warnings.
//   - Summary: a waste-by-project horizontal bar chart (only for multi-project
//     scans, where a single bar would be information-free) and a compact
//     waste-by-rule table. Both are derived by aggregating r.Findings — no
//     graph traversal, no fabricated data.
//   - Findings: every finding in one table, with client-side text search,
//     severity toggles, a sort control and a 50-row default limit with a
//     "Show all N findings" button. The table always contains every finding;
//     filtering hides rows without removing them.
//   - Scan details (collapsed by default): the denominators that let an
//     operator judge whether to trust the headline — resources scanned, rules
//     evaluated, resources skipped (with the per (rule, code) breakdown), and
//     duration.
//
// Determinism: every aggregation is sorted under a stable total ordering, the
// single timestamp lives in exactly one clearly-marked place in the header,
// and map-backed diagnostics (rule errors) are emitted in sorted key order.
// The same scan therefore renders a byte-identical report.
//
// Escaping: every string that originates from a cloud API (resource names,
// project IDs, rule IDs, evidence values, error messages) is passed through
// html.EscapeString before it is written — element text, attributes, and SVG
// text alike — so a bucket named with a <script> tag cannot execute. The
// inline <script> element carries NO interpolated data: the filter and sort
// logic read the already-escaped text content of DOM nodes (.textContent), so
// a resource named "</script><script>alert(1)</script>" is rendered as literal
// text in the cell and matched as literal text by the filter — never parsed as
// markup, and never able to break out of the script element (the dangerous
// sequence cannot appear inside a script because nothing is interpolated into
// it).
package output

import (
	"fmt"
	"html"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// maxTableRows is the default number of finding rows the HTML table shows
// before the "Show all N findings" button is needed.
const maxTableRows = 50

// maxSummaryRows caps the waste-by-project bars and the waste-by-rule table
// at the top rows by value; the remainder folds into one explicit
// "Other"/"and N more" row so the aggregate always totals the findings.
const maxSummaryRows = 8

// maxResourceRunes is the point past which a resource name is truncated with
// an ellipsis in the table cell; the full name travels in the title
// attribute.
const maxResourceRunes = 60

// maxBarLabelRunes caps an on-chart project label; the full name travels in
// an SVG <title> child.
const maxBarLabelRunes = 30

// wasteAgg is one row of a waste aggregation: the total monthly waste
// attributed to one project or one rule, summed from the report's findings
// (never fabricated). Key distinguishes the capped "other" remainder row.
type wasteAgg struct {
	Key   string
	Label string
	Total float64
}

// sumGrouped groups findings by the label pick returns, sums
// MonthlyWasteUSD per group, sorts by (total desc, label asc) for
// determinism, keeps the top topN rows and folds the remainder into one
// "other" row whose label is rendered by remainderLabel. Because capping only
// sums existing values, the sum of every returned row always equals the
// findings total — nothing is dropped and nothing is invented.
func sumGrouped(fs []rules.Finding, pick func(rules.Finding) string, topN int, remainderLabel func(n int) string) []wasteAgg {
	byKey := map[string]*wasteAgg{}
	for _, f := range fs {
		k := pick(f)
		g, ok := byKey[k]
		if !ok {
			g = &wasteAgg{Key: k, Label: k}
			byKey[k] = g
		}
		g.Total += f.MonthlyWasteUSD
	}
	groups := make([]wasteAgg, 0, len(byKey))
	for _, g := range byKey {
		groups = append(groups, *g)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.Label < b.Label
	})
	if len(groups) <= topN {
		return groups
	}
	kept := groups[:topN]
	other := wasteAgg{Key: "other"}
	for _, g := range groups[topN:] {
		other.Total += g.Total
	}
	other.Label = remainderLabel(len(groups) - topN)
	return append(kept, other)
}

// wasteByProject aggregates findings by project. A finding with no project
// groups under "(unknown project)" so the row labels stay honest.
func wasteByProject(fs []rules.Finding) []wasteAgg {
	return sumGrouped(fs, func(f rules.Finding) string {
		if f.Project == "" {
			return "(unknown project)"
		}
		return f.Project
	}, maxSummaryRows, func(n int) string {
		return fmt.Sprintf("Other (%d projects)", n)
	})
}

// wasteByRule aggregates findings by rule ID.
func wasteByRule(fs []rules.Finding) []wasteAgg {
	return sumGrouped(fs, func(f rules.Finding) string { return f.RuleID }, maxSummaryRows,
		func(n int) string { return fmt.Sprintf("and %d more rules", n) })
}

// distinctRuleCount returns the number of distinct rule IDs among the
// findings.
func distinctRuleCount(fs []rules.Finding) int {
	seen := map[string]bool{}
	for _, f := range fs {
		seen[f.RuleID] = true
	}
	return len(seen)
}

// RenderHTML writes a complete, self-contained HTML document for the report.
// The timestamp is emitted in exactly one place — the header — so the
// document is byte-identical for the same scan.
//
// Currency: a default USD scan renders exactly as before (every figure
// "$…"). A non-USD scan names its currency in a header disclosure paragraph
// and renders every figure as "12.40 EUR"; when USD embedded-fallback prices
// contaminated the scan, the disclosure is a loud amber warning so an
// operator reading EUR figures is never silently handed USD numbers.
func RenderHTML(w io.Writer, r Report) error {
	sb := &strings.Builder{}
	sb.WriteString("<!DOCTYPE html>\n")
	sb.WriteString("<html lang=\"en\">\n")
	sb.WriteString("<head>\n")
	sb.WriteString("<meta charset=\"utf-8\">\n")
	fmt.Fprintf(sb, "<title>Tellury waste report — %s</title>\n", esc(r.Scope))
	sb.WriteString(htmlCSS)
	sb.WriteString("</head>\n")
	sb.WriteString("<body>\n")

	// Header: the ONE clearly-marked timestamp, the hero number that answers
	// "how much am I wasting?" at a glance, and the scan meta line.
	sb.WriteString("<header>\n")
	sb.WriteString("<h1>Tellury waste report</h1>\n")
	writeHero(sb, r)
	writeHeaderMeta(sb, r)
	sb.WriteString("</header>\n")

	// Warnings between the header and the summary: metrics-blocked and
	// rule-errors are scan-integrity facts the operator must see before
	// trusting any number below them.
	writeWarningBanners(sb, r)

	// Summary: how much → where.
	writeSummary(sb, r)

	// Findings: why (evidence) and what to do (remediation), per finding.
	writeFindingsSection(sb, r)

	// Scan details: how reliable is this scan.
	writeScanDetails(sb, r)

	sb.WriteString(htmlJS)
	sb.WriteString("</body>\n")
	sb.WriteString("</html>\n")
	_, err := io.WriteString(w, sb.String())
	return err
}

// writeHero renders the headline number. Three states:
//
//   - findings exist: the total monthly waste, large, alone.
//   - no findings but the scan ran (projects analyzed > 0): an honest $0.00
//     hero — a different signal from "the scan broke".
//   - no findings and nothing scanned (projects analyzed == 0): the words
//     "No data" replace the number entirely.
func writeHero(sb *strings.Builder, r Report) {
	switch {
	case len(r.Findings) > 0:
		fmt.Fprintf(sb, "<div class=\"hero\">\n<div class=\"hero-number\">%s</div>\n<div class=\"hero-label\">total monthly waste</div>\n</div>\n",
			moneyHTML(r.TotalMonthlyWasteUSD, r.Currency))
	case r.ProjectsAnalyzed > 0:
		fmt.Fprintf(sb, "<div class=\"hero\">\n<div class=\"hero-number\">%s</div>\n<div class=\"hero-label\">total monthly waste</div>\n</div>\n",
			moneyHTML(0, r.Currency))
	default:
		sb.WriteString("<div class=\"hero\">\n<div class=\"hero-no-data\">No data</div>\n</div>\n")
	}
}

// writeHeaderMeta renders the scope/provider/window line, the generated-at
// line, and — for a non-USD scan — the currency disclosure (amber when USD
// fallback prices contaminated the figures).
func writeHeaderMeta(sb *strings.Builder, r Report) {
	fmt.Fprintf(sb, "<p class=\"meta\">Scope: <code>%s</code> · Provider: %s · %d-day window</p>\n",
		esc(r.Scope), esc(r.Provider), r.WindowDays)
	fmt.Fprintf(sb, "<p class=\"meta\">Generated at <time datetime=\"%s\">%s</time> (UTC)</p>\n",
		esc(r.GeneratedAt.UTC().Format(time.RFC3339)), esc(r.GeneratedAt.UTC().Format(time.RFC3339)))
	if lines := currencyDisclosure(r); len(lines) > 0 {
		cls := "currency"
		if r.CurrencyMixed {
			cls = "currency currency-mixed"
		}
		fmt.Fprintf(sb, "<p class=\"%s\">\n", cls)
		for i, line := range lines {
			if i > 0 {
				sb.WriteString("<br>\n")
			}
			fmt.Fprintf(sb, "%s", esc(line))
		}
		sb.WriteString("\n</p>\n")
	}
}

// writeWarningBanners renders the two always-visible scan-integrity banners:
// metric-dependent rules that could not evaluate (metrics blocked) and rules
// that errored. Both name every affected rule so a fixture replay cannot look
// like a clean bill of health when it was actually "could not check".
func writeWarningBanners(sb *strings.Builder, r Report) {
	if len(r.MetricsBlocked) > 0 {
		sb.WriteString("<div class=\"warning-banner warning-blocked\" role=\"note\">\n")
		fmt.Fprintf(sb, "⚠️ %d rule%s could not be evaluated because the scan data carried no metric series for their required keys: ",
			len(r.MetricsBlocked), pluralS(len(r.MetricsBlocked)))
		quoted := make([]string, 0, len(r.MetricsBlocked))
		for _, id := range r.MetricsBlocked {
			quoted = append(quoted, "<code>"+esc(id)+"</code>")
		}
		sb.WriteString(strings.Join(quoted, ", "))
		sb.WriteString(". If you ran from a raw CAI export, re-run with a cached snapshot or live API access to get full metric coverage.")
		sb.WriteString("\n</div>\n")
	}
	if len(r.RuleErrors) > 0 {
		ids := make([]string, 0, len(r.RuleErrors))
		for id := range r.RuleErrors {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		sb.WriteString("<div class=\"warning-banner warning-errors\" role=\"alert\">\n")
		fmt.Fprintf(sb, "⚠️ %d rule%s failed during evaluation:\n", len(r.RuleErrors), pluralS(len(r.RuleErrors)))
		sb.WriteString("<ul>\n")
		for _, id := range ids {
			fmt.Fprintf(sb, "<li><code>%s</code>: %s</li>\n", esc(id), esc(r.RuleErrors[id]))
		}
		sb.WriteString("</ul>\n</div>\n")
	}
}

// writeSummary renders the "where the waste is" section: the waste-by-project
// bar chart (only for a multi-project scan with more than one project in the
// findings) and the waste-by-rule table (only when more than one rule fired —
// a single-row summary table is decoration). If neither renders, no section
// is emitted at all.
func writeSummary(sb *strings.Builder, r Report) {
	if len(r.Findings) == 0 {
		return
	}
	open := false
	defer func() {
		if open {
			sb.WriteString("</section>\n")
		}
	}()

	if r.MultiProject {
		rows := wasteByProject(r.Findings)
		// A single bar at 100% is information-free; skip it entirely.
		if len(rows) > 1 {
			sb.WriteString("<section id=\"summary\">\n")
			open = true
			sb.WriteString("<h2>Where the waste is</h2>\n")
			renderProjectBars(sb, rows, r.Currency)
		}
	}
	if distinctRuleCount(r.Findings) > 1 {
		if !open {
			sb.WriteString("<section id=\"summary\">\n")
			open = true
		}
		sb.WriteString("<h2>Waste by rule</h2>\n")
		writeRuleSummary(sb, r)
	}
}

// writeRuleSummary renders the compact waste-by-rule table: two columns (rule
// ID, total monthly waste), sorted descending, capped to maxSummaryRows with
// an "and N more rules" remainder row that still carries the remainder total.
func writeRuleSummary(sb *strings.Builder, r Report) {
	rows := wasteByRule(r.Findings)
	sb.WriteString("<table class=\"summary-rules\">\n<tbody>\n")
	for _, row := range rows {
		if row.Key == "other" {
			fmt.Fprintf(sb, "<tr class=\"more-rules\"><td>%s</td><td class=\"col-waste\">%s</td></tr>\n",
				esc(row.Label), moneyHTML(row.Total, r.Currency))
			continue
		}
		fmt.Fprintf(sb, "<tr><td class=\"col-rule\">%s</td><td class=\"col-waste\">%s</td></tr>\n",
			esc(row.Label), moneyHTML(row.Total, r.Currency))
	}
	sb.WriteString("</tbody>\n</table>\n")
}

// renderProjectBars emits the waste-by-project chart as inline SVG: one row
// per project (capped to maxSummaryRows plus an "Other" row), the label on
// the left, a bar whose width is proportional to the project's share of the
// total waste, and the amount just past the bar end. Single colour, no axis,
// no ticks, no gridlines. The SVG scales with its container via the viewBox.
func renderProjectBars(sb *strings.Builder, rows []wasteAgg, currency string) {
	const (
		rowHeight = 28.0
		barHeight = 20.0
		labelW    = 240.0
		barMaxW   = 620.0
		viewW     = 1000.0
	)
	var total float64
	for _, row := range rows {
		total += row.Total
	}
	height := rowHeight * float64(len(rows))
	sb.WriteString("<div class=\"summary-bars\">\n")
	fmt.Fprintf(sb, "<svg viewBox=\"0 0 %s %s\" role=\"img\" aria-label=\"Monthly waste by project, bar widths proportional to monthly waste\" width=\"100%%\">\n",
		f2(viewW), f2(height))
	for i, row := range rows {
		y := float64(i) * rowHeight
		cy := y + rowHeight/2
		barW := 0.0
		if total > 0 {
			barW = barMaxW * row.Total / total
		}
		isOther := row.Key == "other"
		label := truncateRunes(row.Label, maxBarLabelRunes)
		labelCls := "bar-label"
		rectCls := "summary-bar-rect"
		if isOther {
			labelCls += " bar-label-other"
			rectCls += " summary-bar-rect-other"
		}
		sb.WriteString("<g class=\"summary-bar\">\n")
		// An SVG <title> child carries the FULL label so a truncated on-chart
		// label never loses information.
		if label != row.Label {
			fmt.Fprintf(sb, "<text x=\"0\" y=\"%s\" dominant-baseline=\"central\" class=\"%s\"><title>%s</title>%s</text>\n",
				f2(cy), labelCls, esc(row.Label), esc(label))
		} else {
			fmt.Fprintf(sb, "<text x=\"0\" y=\"%s\" dominant-baseline=\"central\" class=\"%s\">%s</text>\n",
				f2(cy), labelCls, esc(label))
		}
		fmt.Fprintf(sb, "<rect x=\"%s\" y=\"%s\" width=\"%s\" height=\"%s\" rx=\"2\" class=\"%s\"/>\n",
			f2(labelW+8), f2(y+4), f2(barW), f2(barHeight), rectCls)
		fmt.Fprintf(sb, "<text x=\"%s\" y=\"%s\" dominant-baseline=\"central\" class=\"bar-amount\">%s</text>\n",
			f2(labelW+8+barW+8), f2(cy), esc(moneyHTML(row.Total, currency)))
		sb.WriteString("</g>\n")
	}
	sb.WriteString("</svg>\n</div>\n")
}

// writeFindingsSection renders the findings table and its controls. The table
// always contains EVERY finding; rows past the first maxTableRows carry the
// beyond-limit class (hidden by CSS, so pagination works even without
// JavaScript) and a "Show all N findings" button removes the limit.
func writeFindingsSection(sb *strings.Builder, r Report) {
	sb.WriteString("<section id=\"findings\">\n<h2>Findings</h2>\n")
	n := len(r.Findings)
	if n == 0 {
		writeEmptyFindings(sb, r)
		sb.WriteString("</section>\n")
		return
	}

	if n > 1 {
		writeFilterBar(sb)
	}

	limit := n
	if limit > maxTableRows {
		limit = maxTableRows
	}
	fmt.Fprintf(sb, "<p class=\"showing\">Showing <span id=\"showing-count\">%d</span> of %d finding%s</p>\n", limit, n, pluralS(n))

	sb.WriteString("<table class=\"findings-table\">\n")
	writeFindingsHeader(sb, r.MultiProject)
	sb.WriteString("<tbody id=\"findings-body\">\n")
	for i, f := range orderByWaste(r.Findings) {
		writeFindingRow(sb, f, r.Currency, r.MultiProject, i)
	}
	sb.WriteString("</tbody>\n</table>\n")

	if n > maxTableRows {
		// The button is the scripted path. <noscript> reveals the rows outright,
		// because the limit is a convenience for a long page and must never be
		// the reason a finding cannot be read at all.
		fmt.Fprintf(sb, "<span class=\"show-all-wrap\"><button type=\"button\" id=\"show-all-btn\" class=\"show-all\">Show all %d findings</button></span>\n", n)
		sb.WriteString("<noscript><style>tr.beyond-limit { display: table-row; } " +
			".show-all-wrap, p.showing { display: none; }</style>" +
			"<p class=\"showing\">Showing all findings (JavaScript is off, so sorting and " +
			"filtering are unavailable).</p></noscript>\n")
	}
	sb.WriteString("</section>\n")
}

// writeEmptyFindings renders the two distinct empty states: the scan ran and
// found nothing wasteful (green check, honest "$0.00" hero above), or the
// scope resolved zero resources (warning naming the problem, with the scope
// and the zero denominator so the operator can judge whether to trust the
// zero).
func writeEmptyFindings(sb *strings.Builder, r Report) {
	if r.ProjectsAnalyzed > 0 {
		// A green check claims a clean bill of health. It is only honest when
		// every rule actually ran: with rules blocked for want of metrics, the
		// truthful statement is "nothing found among what could be checked".
		// The banner above already names the blocked rules, but the checkmark
		// is what a reader takes away, and an unqualified tick over a partial
		// scan is exactly the mistake this tool exists not to make.
		status, mark := "ok", "✅"
		qualifier := "."
		if len(r.MetricsBlocked) > 0 {
			status, mark = "warn", "⚠️"
			qualifier = fmt.Sprintf(" — but %d rule%s could not be evaluated, so this is not a clean bill of health.",
				len(r.MetricsBlocked), pluralS(len(r.MetricsBlocked)))
		}
		fmt.Fprintf(sb, "<p class=\"hero-status %s\">%s No waste found among the %d project%s and %d resource%s checked%s</p>\n",
			status, mark, r.ProjectsAnalyzed, pluralS(r.ProjectsAnalyzed),
			r.ResourcesScanned, pluralS(r.ResourcesScanned), qualifier)
		return
	}
	sb.WriteString("<p class=\"hero-status warn\">⚠️ The scan resolved zero resources. Check that the scope is correct and the project or folder exists.</p>\n")
	fmt.Fprintf(sb, "<p class=\"meta\">Scope: <code>%s</code></p>\n<p class=\"meta\">Resources scanned: 0</p>\n", esc(r.Scope))
}

// writeFilterBar renders the client-side filter controls: text search,
// severity toggles and the sort select. It is only rendered when the table
// has more than one row — the controls serve no purpose on a single row.
func writeFilterBar(sb *strings.Builder) {
	sb.WriteString("<div class=\"filter-bar\">\n")
	sb.WriteString("<input type=\"search\" id=\"filter-search\" placeholder=\"Filter findings…\" aria-label=\"Filter findings\">\n")
	sb.WriteString("<div class=\"sev-toggles\" role=\"group\" aria-label=\"Filter by severity\">\n")
	for _, s := range []struct{ label, sev string }{
		{"High", "high"}, {"Medium", "medium"}, {"Low", "low"},
	} {
		fmt.Fprintf(sb, "<button type=\"button\" class=\"sev-toggle\" data-severity=\"%s\" aria-pressed=\"true\">%s</button>\n", s.sev, s.label)
	}
	sb.WriteString("</div>\n")
	sb.WriteString("<label class=\"sort-label\" for=\"sort-select\">Sort\n<select id=\"sort-select\">\n")
	sb.WriteString("<option value=\"waste-desc\" selected>Waste (high first)</option>\n")
	sb.WriteString("<option value=\"waste-asc\">Waste (low first)</option>\n")
	sb.WriteString("<option value=\"resource-az\">Resource (A–Z)</option>\n")
	sb.WriteString("<option value=\"rule-az\">Rule (A–Z)</option>\n")
	sb.WriteString("<option value=\"confidence-asc\">Confidence (low first)</option>\n")
	sb.WriteString("</select>\n</label>\n")
	sb.WriteString("</div>\n")
}

// writeScanDetails renders the collapsed scan-details section: the
// denominators (resources scanned, rules evaluated, resources skipped,
// duration), the skip breakdown table when any skips exist, and the rule-error
// count (the always-visible banner above carries the per-rule detail).
func writeScanDetails(sb *strings.Builder, r Report) {
	sb.WriteString("<section id=\"scan-details\" class=\"scan-details\">\n<details>\n")
	sb.WriteString("<summary>Scan details")
	if len(r.Skipped) > 0 {
		fmt.Fprintf(sb, " · %d resources skipped across %d rule%s",
			r.ResourcesSkipped, distinctSkipRules(r.Skipped), pluralS(distinctSkipRules(r.Skipped)))
	}
	sb.WriteString("</summary>\n")
	sb.WriteString("<dl class=\"scan-facts\">\n")
	fmt.Fprintf(sb, "<dt>Scope</dt><dd><code>%s</code></dd>\n", esc(r.Scope))
	fmt.Fprintf(sb, "<dt>Provider</dt><dd>%s</dd>\n", esc(r.Provider))
	fmt.Fprintf(sb, "<dt>Window</dt><dd>%d days</dd>\n", r.WindowDays)
	fmt.Fprintf(sb, "<dt>Projects analyzed</dt><dd>%d</dd>\n", r.ProjectsAnalyzed)
	fmt.Fprintf(sb, "<dt>Resources scanned</dt><dd>%d</dd>\n", r.ResourcesScanned)
	fmt.Fprintf(sb, "<dt>Rules evaluated</dt><dd>%d</dd>\n", r.RulesEvaluated)
	fmt.Fprintf(sb, "<dt>Resources skipped</dt><dd>%d</dd>\n", r.ResourcesSkipped)
	fmt.Fprintf(sb, "<dt>Duration</dt><dd>%s</dd>\n", esc(formatDuration(r.Duration)))
	if len(r.RuleErrors) > 0 {
		fmt.Fprintf(sb, "<dt>Rule errors</dt><dd>%d</dd>\n", len(r.RuleErrors))
	}
	sb.WriteString("</dl>\n")
	if len(r.Skipped) > 0 {
		renderSkipTable(sb, r.Skipped)
	}
	sb.WriteString("</details>\n</section>\n")
}

// renderSkipTable renders the per (rule, code) skip breakdown. Skipped arrives
// already sorted by (RuleID, Code) from rules.Result.SkipTotals, so the rows
// are deterministic.
func renderSkipTable(sb *strings.Builder, skipped []rules.SkipTally) {
	sb.WriteString("<table class=\"skip-table\">\n<thead><tr><th>Rule</th><th>Code</th><th>Count</th></tr></thead>\n<tbody>\n")
	for _, t := range skipped {
		fmt.Fprintf(sb, "<tr><td><code>%s</code></td><td><code>%s</code></td><td class=\"col-num\">%d</td></tr>\n",
			esc(t.RuleID), esc(string(t.Code)), t.Count)
	}
	sb.WriteString("</tbody>\n</table>\n")
}

// distinctSkipRules counts the distinct rule IDs that tallied a skip.
func distinctSkipRules(tallies []rules.SkipTally) int {
	seen := map[string]bool{}
	for _, t := range tallies {
		seen[t.RuleID] = true
	}
	return len(seen)
}

// writeFindingsHeader emits the findings-table header row. The Project column
// only exists for a multi-project scan, matching the table renderer.
func writeFindingsHeader(sb *strings.Builder, multi bool) {
	sb.WriteString("<thead>\n<tr>")
	sb.WriteString("<th>Resource</th>")
	if multi {
		sb.WriteString("<th>Project</th>")
	}
	sb.WriteString("<th>Rule</th><th>Severity</th><th>Confidence</th><th>Monthly waste</th><th>Evidence &amp; remediation</th>")
	sb.WriteString("</tr>\n</thead>\n")
}

// writeFindingRow emits one finding row. Every attacker-controlled value is
// escaped; the numeric data attributes (waste, confidence) are Go floats,
// and severity/resource/rule are still escaped for uniformity before landing
// in attributes.
func writeFindingRow(sb *strings.Builder, f rules.Finding, currency string, multi bool, idx int) {
	sb.WriteString("<tr")
	if idx >= maxTableRows {
		sb.WriteString(" class=\"finding-row beyond-limit\"")
	} else {
		sb.WriteString(" class=\"finding-row\"")
	}
	fmt.Fprintf(sb, " data-waste=\"%s\"", strconv.FormatFloat(f.MonthlyWasteUSD, 'f', 2, 64))
	fmt.Fprintf(sb, " data-confidence=\"%s\"", strconv.FormatFloat(f.Confidence, 'f', 2, 64))
	fmt.Fprintf(sb, " data-severity=\"%s\"", esc(string(f.Severity)))
	fmt.Fprintf(sb, " data-resource=\"%s\"", esc(f.Resource))
	fmt.Fprintf(sb, " data-rule=\"%s\"", esc(f.RuleID))
	sb.WriteString(">\n")

	// Resource — truncated with an ellipsis past maxResourceRunes; the title
	// attribute carries the full name so truncation never loses information.
	display, needsTitle := truncateRunesSafe(f.Resource, maxResourceRunes)
	sb.WriteString("<td class=\"col-resource\">")
	if needsTitle {
		fmt.Fprintf(sb, "<span class=\"resource-name\" title=\"%s\">%s</span>", esc(f.Resource), esc(display))
	} else {
		fmt.Fprintf(sb, "<span class=\"resource-name\">%s</span>", esc(f.Resource))
	}
	sb.WriteString("</td>\n")

	if multi {
		fmt.Fprintf(sb, "<td class=\"col-project\">%s</td>\n", esc(f.Project))
	}

	fmt.Fprintf(sb, "<td class=\"col-rule\"><code>%s</code></td>\n", esc(f.RuleID))
	fmt.Fprintf(sb, "<td class=\"col-severity\"><span class=\"sev-pill sev-pill-%s\">%s</span></td>\n",
		esc(string(f.Severity)), strings.ToUpper(esc(string(f.Severity))))

	// Confidence — a horizontal fill bar coloured by threshold plus a tiny
	// numeric label.
	conf := f.Confidence
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}
	pct := int(math.Round(conf * 100))
	fmt.Fprintf(sb, "<td class=\"col-confidence\"><span class=\"conf-bar\"><span class=\"conf-bar-fill %s\" style=\"width:%d%%\"></span></span><span class=\"conf-num\">%d%%</span></td>\n",
		confClass(conf), pct, pct)

	fmt.Fprintf(sb, "<td class=\"col-waste\">%s</td>\n", moneyHTML(f.MonthlyWasteUSD, currency))

	// Evidence (why) then remediation (what to do), both always visible.
	sb.WriteString("<td class=\"col-evidence\">")
	writeEvidenceCell(sb, f)
	if f.Remediation != "" {
		fmt.Fprintf(sb, "<p class=\"finding-remediation\"><span class=\"fix-label\">Fix:</span>%s</p>", esc(f.Remediation))
	}
	sb.WriteString("</td>\n")
	sb.WriteString("</tr>\n")
}

// writeEvidenceCell renders a finding's evidence list. price_-prefixed keys
// are grouped under a subtle "Pricing" label so cost provenance reads apart
// from behavioural facts. An empty evidence slice renders an explicit "No
// evidence recorded." line — never nothing.
func writeEvidenceCell(sb *strings.Builder, f rules.Finding) {
	if len(f.Evidence) == 0 {
		sb.WriteString("<p class=\"no-evidence\">No evidence recorded.</p>")
		return
	}
	sb.WriteString("<ul class=\"finding-evidence\">\n")
	var price []rules.Evidence
	for _, ev := range f.Evidence {
		if strings.HasPrefix(ev.Key, "price_") {
			price = append(price, ev)
			continue
		}
		fmt.Fprintf(sb, "<li><code>%s</code>: %s</li>\n", esc(ev.Key), esc(ev.Value))
	}
	if len(price) > 0 {
		sb.WriteString("<li class=\"evidence-price-label\">Pricing</li>\n")
		for _, ev := range price {
			fmt.Fprintf(sb, "<li><code>%s</code>: %s</li>\n", esc(ev.Key), esc(ev.Value))
		}
	}
	sb.WriteString("</ul>")
}

// orderByWaste returns a stable copy of the findings ordered by (waste desc,
// resource asc, rule asc) — the default table order, which matches the
// default "Waste (high first)" sort option. It is decoupled from whatever
// --sort the CLI applied.
func orderByWaste(fs []rules.Finding) []rules.Finding {
	out := append([]rules.Finding(nil), fs...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.MonthlyWasteUSD != b.MonthlyWasteUSD:
			return a.MonthlyWasteUSD > b.MonthlyWasteUSD
		case a.Resource != b.Resource:
			return a.Resource < b.Resource
		default:
			return a.RuleID < b.RuleID
		}
	})
	return out
}

// confClass maps a confidence value onto the fill-bar colour class.
func confClass(c float64) string {
	switch {
	case c >= 0.8:
		return "conf-good"
	case c >= 0.5:
		return "conf-mid"
	default:
		return "conf-low"
	}
}

// truncateRunesSafe returns the display form of s — truncated to max runes
// with an ellipsis when longer — and whether the full string must travel in a
// title attribute.
func truncateRunesSafe(s string, max int) (display string, needsTitle bool) {
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]) + "…", true
}

// truncateRunes truncates s to max runes with an ellipsis when longer.
func truncateRunes(s string, max int) string {
	display, _ := truncateRunesSafe(s, max)
	return display
}

// pluralS returns "s" for a non-1 count — the tiny English plural helper for
// "N rule(s)", "N project(s)" and friends.
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func esc(s string) string { return html.EscapeString(s) }

// f2 formats a coordinate/measure as a fixed 2-decimal string.
func f2(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// moneyHTML renders an amount in the report's currency. USD (including the
// empty default) keeps the historical "$12.40" form so a default scan renders
// byte-identically; any other currency appends its code — "12.40 EUR" — so a
// EUR figure can never be mistaken for dollars.
func moneyHTML(v float64, currency string) string {
	v = pricing.Round2(v)
	if currency == "" || currency == "USD" {
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("%.2f %s", v, currency)
}

// htmlCSS is the inline stylesheet. No external resource is referenced (no
// @import, no url(), no web font), so the report is legible on an air-gapped
// machine and when printed to PDF. The colour palette lives in custom
// properties on :root so the SVG chart can reference the same colours.
const htmlCSS = `<style>
:root {
  color-scheme: light;
  --font-body: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  --font-mono: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  --color-text-primary: #1a1d21;
  --color-text-secondary: #57606a;
  --color-border: #e1e4e8;
  --color-accent: #0969da;
  --color-warning-bg: #fff7eb;
  --color-warning-border: #b35900;
  --color-blocked-bg: #fff0f0;
  --color-blocked-border: #d73a49;
  --color-sev-high: #d73a49;
  --color-sev-medium: #d4a72c;
  --color-sev-low: #6f7378;
  --color-conf-high: #2da44e;
  --color-conf-medium: #d4a72c;
  --color-conf-low: #cf222e;
  --color-chart: #4E79A7;
  --color-chart-other: #a1b7d4;
  --color-fix: #1a6b3c;
}
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
body {
  margin: 0 auto; max-width: 960px; padding: 1.5rem 2rem 3rem;
  font-family: var(--font-body); font-size: 15px; line-height: 1.5;
  color: var(--color-text-primary); background: #fff;
}
h1 { font-size: 1.4rem; margin: 0; }
h2 { font-size: 1.1rem; font-weight: 600; color: var(--color-text-primary); margin: 0 0 0.75rem; }
p { margin: 0.5rem 0; }
section { margin-top: 2rem; }
code {
  font-family: var(--font-mono); background: #f6f8fa; padding: 0 0.25em;
  border-radius: 3px; font-size: 0.85em;
}

/* Hero */
.hero { margin: 2rem 0; }
.hero-number {
  font-family: var(--font-mono); font-size: 3rem; font-weight: 700;
  font-variant-numeric: tabular-nums; color: var(--color-text-primary);
  line-height: 1.1; margin: 0 0 0.5rem;
}
.hero-label { color: var(--color-text-secondary); font-size: 0.9rem; }
.hero-no-data { font-size: 1.5rem; font-weight: 600; color: var(--color-text-secondary); }
.hero-status.ok { color: var(--color-conf-high); font-weight: 600; }
.hero-status.warn { color: var(--color-warning-border); font-weight: 600; }

/* Header meta + currency */
p.meta { color: var(--color-text-secondary); font-size: 0.85rem; margin: 0.15rem 0; }
p.currency {
  color: var(--color-accent); font-size: 0.85rem; margin: 0.5rem 0 0;
  border-left: 3px solid var(--color-accent); padding: 0.35rem 0.6rem; background: #f6f8fa;
}
p.currency.currency-mixed {
  color: var(--color-warning-border); border-left-color: var(--color-warning-border);
  background: var(--color-warning-bg);
}

/* Scan-integrity warning banners */
.warning-banner {
  margin: 1rem 0 0; padding: 0.6rem 0.8rem; border-left: 4px solid;
  font-size: 0.9rem; color: var(--color-text-primary);
}
.warning-banner ul { margin: 0.35rem 0 0; padding-left: 1.2rem; }
.warning-blocked { border-left-color: var(--color-blocked-border); background: var(--color-blocked-bg); }
.warning-errors { border-left-color: var(--color-blocked-border); background: var(--color-blocked-bg); }

/* Waste-by-project bar chart */
.summary-bars { margin: 0.25rem 0 0; }
.summary-bars svg { width: 100%; height: auto; display: block; }
.bar-label { font-size: 12.5px; fill: var(--color-text-primary); }
.bar-label-other { font-style: italic; }
.bar-amount {
  font-size: 12.5px; fill: var(--color-text-primary);
  font-family: var(--font-mono); font-variant-numeric: tabular-nums;
}
.summary-bar-rect { fill: var(--color-chart); }
.summary-bar-rect-other { fill: var(--color-chart-other); }

/* Waste-by-rule summary table */
table.summary-rules { width: 100%; border-collapse: collapse; margin: 0.25rem 0 0; }
table.summary-rules td { padding: 0.5rem 0.75rem; border-bottom: 1px solid var(--color-border); vertical-align: top; }
table.summary-rules td.col-rule { font-family: var(--font-mono); font-size: 0.85rem; }
table.summary-rules td.col-waste {
  text-align: right; font-family: var(--font-mono); font-variant-numeric: tabular-nums; white-space: nowrap;
}
tr.more-rules td { color: var(--color-text-secondary); font-style: italic; }

/* Filter bar */
.filter-bar { display: flex; flex-wrap: wrap; align-items: center; gap: 0.75rem; margin: 0.5rem 0 0; }
.filter-bar input[type="search"] {
  flex: 1 1 200px; min-width: 160px; padding: 0.35rem 0.6rem; font-size: 14px;
  border: 1px solid var(--color-border); border-radius: 4px; background: #fff;
}
.sev-toggles { display: inline-flex; gap: 0.4rem; }
button.sev-toggle {
  font-size: 0.8rem; font-weight: 600; padding: 0.3rem 0.7rem;
  border: 1px solid var(--color-border); border-radius: 4px;
  background: #fff; color: var(--color-text-primary); cursor: pointer;
}
button.sev-toggle.off {
  color: var(--color-text-secondary); background: #f6f8fa;
  text-decoration: line-through; border-color: #d0d7de;
}
.sort-label { font-size: 0.85rem; color: var(--color-text-secondary); display: inline-flex; align-items: center; gap: 0.4rem; }
.sort-label select {
  font-size: 14px; padding: 0.3rem 0.4rem; border: 1px solid var(--color-border);
  border-radius: 4px; background: #fff;
}

p.showing { color: var(--color-text-secondary); font-size: 0.85rem; margin: 0.75rem 0 0; }

/* Findings table */
table.findings-table { width: 100%; border-collapse: collapse; margin: 0.5rem 0 0; }
table.findings-table th, table.findings-table td {
  text-align: left; padding: 0.5rem 0.75rem; border-bottom: 1px solid var(--color-border);
  vertical-align: top; font-size: 13.5px;
}
table.findings-table thead th {
  background: #f6f8fa; font-size: 0.75rem; text-transform: uppercase;
  letter-spacing: 0.05em; color: var(--color-text-secondary);
}
table.findings-table tbody tr:hover { background: #f6f8fa; }
tr.beyond-limit { display: none; }
table.findings-table td.col-resource { font-family: var(--font-mono); font-size: 0.85em; }
table.findings-table td.col-project { font-size: 13px; }
table.findings-table td.col-rule { font-family: var(--font-mono); font-size: 0.85em; white-space: nowrap; }
table.findings-table td.col-waste {
  text-align: right; font-family: var(--font-mono); font-variant-numeric: tabular-nums; white-space: nowrap;
}
table.findings-table td.col-confidence { width: 7rem; }
table.findings-table td.col-evidence { min-width: 16rem; }

/* Severity pill */
.sev-pill {
  display: inline-block; font-size: 0.7rem; font-weight: 700; text-transform: uppercase;
  letter-spacing: 0.03em; color: #fff; border-radius: 3px; padding: 0.15em 0.5em;
}
.sev-pill-high { background: var(--color-sev-high); }
.sev-pill-medium { background: var(--color-sev-medium); }
.sev-pill-low { background: var(--color-sev-low); }

/* Confidence bar */
.conf-bar { display: block; width: 100%; height: 4px; background: #eaeef2; border-radius: 2px; overflow: hidden; margin-bottom: 3px; }
.conf-bar-fill { display: block; height: 100%; }
.conf-good { background: var(--color-conf-high); }
.conf-mid { background: var(--color-conf-medium); }
.conf-low { background: var(--color-conf-low); }
.conf-num { font-size: 0.75rem; color: var(--color-text-secondary); font-variant-numeric: tabular-nums; }

/* Evidence and remediation */
ul.finding-evidence { list-style: none; margin: 0; padding: 0; }
ul.finding-evidence li { margin: 0.25rem 0 0; }
ul.finding-evidence li.evidence-price-label {
  margin-top: 0.5rem; color: var(--color-text-secondary);
  font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.05em;
}
p.no-evidence { color: var(--color-text-secondary); font-style: italic; font-size: 13px; margin: 0; }
p.finding-remediation { font-size: 13px; color: var(--color-fix); margin: 0.5rem 0 0; }
span.fix-label { color: var(--color-text-secondary); font-weight: 700; margin-right: 0.35em; }

button.show-all { margin: 0.75rem 0 0; font-size: 0.9rem; color: var(--color-accent); background: none; border: none; cursor: pointer; padding: 0; }
button.show-all:hover { text-decoration: underline; }

/* Scan details */
section.scan-details { margin-top: 2rem; }
section.scan-details details { margin-top: 0.25rem; }
section.scan-details summary { cursor: pointer; font-size: 0.95rem; color: var(--color-accent); }
.scan-facts { display: grid; grid-template-columns: max-content 1fr; gap: 0.25rem 1rem; margin: 0.75rem 0 0; }
.scan-facts dt { color: var(--color-text-secondary); font-size: 0.85rem; }
.scan-facts dd { font-size: 0.9rem; margin: 0; }
table.skip-table { margin: 0.75rem 0 0; width: 100%; border-collapse: collapse; }
table.skip-table th, table.skip-table td {
  text-align: left; padding: 0.4rem 0.6rem; border-bottom: 1px solid var(--color-border); font-size: 13px;
}
table.skip-table thead th { text-transform: uppercase; font-size: 0.7rem; letter-spacing: 0.05em; color: var(--color-text-secondary); }
table.skip-table td.col-num { text-align: right; font-variant-numeric: tabular-nums; }

/* Narrow viewport: keep the report complete, prevent horizontal scroll */
@media (max-width: 700px) {
  .hero-number { font-size: 2rem; }
  .filter-bar { flex-direction: column; align-items: stretch; }
  .sev-toggles { justify-content: flex-start; }
  table.findings-table td.col-resource { max-width: 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  table.findings-table td.col-evidence { max-width: 200px; overflow: hidden; text-overflow: ellipsis; }
}

@media print {
  body { max-width: none; padding: 0; color: #000; background: #fff; }
  /* The row limit is a screen affordance, not an edit. Printing must not drop
     findings: a report that silently omits rows 51+ on paper is worse than one
     that is simply long, because nothing on the page says anything is missing. */
  tr.beyond-limit { display: table-row !important; }
  .show-all-wrap { display: none; }
  details > *:not(summary) { display: block !important; }
  tr { break-inside: avoid; }
  .summary-bars { break-inside: avoid; }
  table.findings-table tbody tr:hover { background: none; }
}
</style>
`

// htmlJS is the single inline script. It carries NO interpolated data: the
// filter and sort logic operate on the already-escaped text content of DOM
// nodes, so a hostile resource name can only ever match as literal text.
const htmlJS = `<script>
"use strict";
(function () {
  var LIMIT = 50;
  var body = document.getElementById('findings-body');
  if (!body) { return; }
  var all = body.querySelectorAll('tr.finding-row');
  var rows = [];
  var i;
  for (i = 0; i < all.length; i++) { rows.push(all[i]); }
  var search = document.getElementById('filter-search');
  var sortSel = document.getElementById('sort-select');
  var toggles = document.querySelectorAll('button.sev-toggle');
  var count = document.getElementById('showing-count');
  var showAllBtn = document.getElementById('show-all-btn');
  var debounceTimer = null;

  function cmp(a, b) {
    if (a < b) { return -1; }
    if (a > b) { return 1; }
    return 0;
  }

  function filterTable() {
    var q = search ? search.value.toLowerCase() : '';
    var limited = showAllBtn !== null && !showAllBtn.hidden;
    var shown = 0;
    var i, j;
    for (i = 0; i < rows.length; i++) {
      var tr = rows[i];
      var sev = tr.getAttribute('data-severity');
      var sevOn = true;
      for (j = 0; j < toggles.length; j++) {
        if (toggles[j].getAttribute('data-severity') === sev &&
            toggles[j].classList.contains('off')) {
          sevOn = false;
          break;
        }
      }
      var matches = q === '' || tr.textContent.toLowerCase().indexOf(q) !== -1;
      var beyond = limited && i >= LIMIT;
      var show = sevOn && matches && !beyond;
      tr.style.display = show ? '' : 'none';
      if (show) { shown++; }
    }
    if (count) { count.textContent = String(shown); }
  }

  function sortTable() {
    var mode = sortSel ? sortSel.value : 'waste-desc';
    var sorted = rows.slice();
    sorted.sort(function (a, b) {
      var r;
      switch (mode) {
        case 'waste-asc':
          r = parseFloat(a.getAttribute('data-waste')) - parseFloat(b.getAttribute('data-waste'));
          break;
        case 'resource-az':
          r = cmp((a.getAttribute('data-resource') || '').toLowerCase(), (b.getAttribute('data-resource') || '').toLowerCase());
          break;
        case 'rule-az':
          r = cmp((a.getAttribute('data-rule') || '').toLowerCase(), (b.getAttribute('data-rule') || '').toLowerCase());
          break;
        case 'confidence-asc':
          r = parseFloat(a.getAttribute('data-confidence')) - parseFloat(b.getAttribute('data-confidence'));
          break;
        default:
          r = parseFloat(b.getAttribute('data-waste')) - parseFloat(a.getAttribute('data-waste'));
          break;
      }
      if (r !== 0) { return r; }
      r = cmp((a.getAttribute('data-resource') || '').toLowerCase(), (b.getAttribute('data-resource') || '').toLowerCase());
      if (r !== 0) { return r; }
      return cmp((a.getAttribute('data-rule') || '').toLowerCase(), (b.getAttribute('data-rule') || '').toLowerCase());
    });
    for (i = 0; i < sorted.length; i++) { body.appendChild(sorted[i]); }
    rows = sorted;
    filterTable();
  }

  function toggleSev() {
    var off = this.classList.contains('off');
    if (off) {
      this.classList.remove('off');
      this.setAttribute('aria-pressed', 'true');
    } else {
      this.classList.add('off');
      this.setAttribute('aria-pressed', 'false');
    }
    filterTable();
  }

  function showAll() {
    for (i = 0; i < rows.length; i++) { rows[i].classList.remove('beyond-limit'); }
    if (showAllBtn) { showAllBtn.hidden = true; }
    filterTable();
  }

  if (search) {
    search.addEventListener('input', function () {
      if (debounceTimer) { window.clearTimeout(debounceTimer); }
      debounceTimer = window.setTimeout(filterTable, 200);
    });
  }
  if (sortSel) { sortSel.addEventListener('change', sortTable); }
  for (i = 0; i < toggles.length; i++) { toggles[i].addEventListener('click', toggleSev); }
  if (showAllBtn) { showAllBtn.addEventListener('click', showAll); }
})();
</script>
`
