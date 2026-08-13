package output

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/TypeOneLabs/tellury/pkg/cloud/aws"
	"github.com/TypeOneLabs/tellury/pkg/cloud/azure"
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
// edge. Unlike the variable columns (resource, project, rule, severity), which
// are sized to their widest displayed value for each scan, the money column is
// fixed so the table's right edge never shifts with the amounts.
const colMoney = 13

// summaryLabelWidth is the width of the label column in the SUMMARY key-value
// block. Labels are left-justified in this many runes, followed by one space,
// so values start at rune 15 (column 16 for a human counting from one).
const summaryLabelWidth = 14

// defaultSummaryWidth is the SUMMARY block width when the scan has no findings
// table. With a table, SUMMARY borrows the table's computed separator width so
// every section rule lines up; without one, 80 is the stable full-width block.
const defaultSummaryWidth = 80

// glyphs for section rules. The Unicode glyph is the only new glyph in the
// redesign; ASCII fallback is the historical dash. Which one is used is
// controlled by the same colourEnabled boolean that controls ANSI severity
// colour: decorated output gets Unicode rules and ANSI, plain output gets
// dashes and no ANSI.
const (
	unicodeRule = "─"
	asciiRule   = "-"
)

// ANSI SGR sequences used by the table renderer. They are the 8 basic
// foreground colours only, deliberately not bright, 256-colour or truecolour:
// basic colours are remapped by terminal themes and stay legible on light and
// dark backgrounds.
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

// tableLayout is the per-scan column layout. The variable columns (resource,
// project, rule, severity) are sized to their widest displayed value, so a
// 30-character GCP project ID or a long resource name is rendered in full —
// never truncated — and can be copied straight into a gcloud command. A
// literal space separates every variable column, so a value that fills its
// cell (padded exactly at the boundary) can never run into the column to its
// right.
type tableLayout struct {
	resource int
	project  int // 0 for a single-project table (no PROJECT column)
	rule     int
	severity int
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
		severity: runeLen("SEVERITY"),
		money:    colMoney,
	}
	if r.MultiProject {
		l.project = runeLen(r.ownerLabel())
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
		if n := runeLen(severityLabel(f.Severity)); n > l.severity {
			l.severity = n
		}
	}
	return l
}

// separatorWidth is the width of the dashed separator row — one full data
// row: the variable columns, their separator spaces, and the money column.
func (l tableLayout) separatorWidth() int {
	w := l.resource + 1 + l.rule + 1 + l.severity + 1 + l.money
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
	w := l.rule + 1 + l.severity + 1
	if l.project > 0 {
		w += 1 + l.project
	}
	return w
}

// tableRenderer is the human table renderer. color is the only place colour
// capability lives; jsonRenderer and csvRenderer have no colour field and no
// colour code path. The same boolean also selects the section-rule glyph:
// decorated output uses Unicode "─", plain output uses ASCII "-".
type tableRenderer struct {
	color bool
}

func (tableRenderer) Format() string { return "table" }

func (t tableRenderer) Render(w io.Writer, r Report) error {
	rule := asciiRule
	if t.color {
		rule = unicodeRule
	}

	// Currency disclosure, before anything else: an operator reading non-USD
	// figures must see which currency they are in and how it was decided
	// before they read a single number. The default USD scan emits nothing,
	// keeping its output byte-identical to the pre-currency build.
	if lines := currencyDisclosure(r); len(lines) > 0 {
		if _, err := fmt.Fprintln(w, "CURRENCY"); err != nil {
			return err
		}
		for _, line := range lines {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintln(w, "FINDINGS"); err != nil {
		return err
	}

	var summaryWidth int
	if len(r.Findings) == 0 {
		if err := t.writeFindingsEmpty(w, r); err != nil {
			return err
		}
		summaryWidth = defaultSummaryWidth
	} else {
		// The table shows at most the ten largest findings by monthly waste
		// (tableFindings); the TOTAL row below still sums every finding.
		display := tableFindings(r)
		layout := layoutTable(r, display)
		summaryWidth = layout.separatorWidth()

		if _, err := fmt.Fprintln(w, strings.Repeat(rule, layout.separatorWidth())); err != nil {
			return err
		}

		severityHeader := fmt.Sprintf("%-*s", layout.severity, "SEVERITY")

		if r.MultiProject {
			if err := writeRowProject(w, layout, "RESOURCE", r.ownerLabel(), "RULE", severityHeader, "MONTHLY WASTE"); err != nil {
				return err
			}
			for _, f := range display {
				if err := writeRowProject(w, layout, f.Resource, f.Project, f.RuleID, t.paintSeverity(layout, f.Severity), r.money(f.MonthlyWasteUSD)); err != nil {
					return err
				}
			}
		} else {
			if err := writeRow(w, layout, "RESOURCE", "RULE", severityHeader, "MONTHLY WASTE"); err != nil {
				return err
			}
			for _, f := range display {
				if err := writeRow(w, layout, f.Resource, f.RuleID, t.paintSeverity(layout, f.Severity), r.money(f.MonthlyWasteUSD)); err != nil {
					return err
				}
			}
		}

		if _, err := fmt.Fprintln(w, strings.Repeat(rule, layout.separatorWidth())); err != nil {
			return err
		}

		// TOTAL agrees with the JSON finding_count and with the SUMMARY
		// pluralisation: "1 finding", not "1 findings".
		summary := countPhrase(r.FindingCount, "finding", "findings")
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

	// One blank line separates FINDINGS from SUMMARY.
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	if err := t.renderSummary(w, r, rule, summaryWidth); err != nil {
		return err
	}

	if hasCoverage(r) {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "COVERAGE"); err != nil {
			return err
		}
		if err := writeCoverage(w, r); err != nil {
			return err
		}
	}
	return nil
}

func (t tableRenderer) writeFindingsEmpty(w io.Writer, r Report) error {
	// SCANNING NOTHING IS NOT THE SAME AS FINDING NOTHING. On Azure, an
	// identity missing read access to a resource TYPE gets an empty result set
	// from Resource Graph rather than a denial, so a permissions gap and a
	// clean account produce identical data. Printing "No waste found" over
	// zero scanned resources states a conclusion the scan did not reach.
	msg := "No waste found."
	code := ""
	if r.ResourcesScanned == 0 {
		msg = "No resources scanned — nothing was found to evaluate. " +
			"Check the scope and the identity's permissions."
		code = ansiYellow
	} else if r.ScanStatus == StatusOK && len(r.MetricsBlocked) == 0 {
		code = ansiGreen
	} else {
		code = ansiYellow
	}
	if !t.color {
		code = ""
	}
	_, err := fmt.Fprintln(w, paint(code, msg))
	return err
}

// renderSummary writes the always-on SUMMARY key-value block. The separator
// width follows the findings table when one exists and defaults to 80 runes
// for an empty FINDINGS section.
func (t tableRenderer) renderSummary(w io.Writer, r Report, rule string, width int) error {
	if _, err := fmt.Fprintln(w, "SUMMARY"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, strings.Repeat(rule, width)); err != nil {
		return err
	}

	type field struct {
		label string
		value string
	}
	fields := []field{
		{"Scope", orDash(scopeName(r.Scope))},
		{"Scope ID", orDash(r.Scope)},
		{"Status", orDash(r.ScanStatus)},
		{"Scanned", scannedValue(r)},
	}
	if r.Provider == "aws" {
		fields = append(fields, field{"Regions", regionsValue(r)})
	}
	fields = append(fields,
		field{"Evaluated", countPhrase(r.RulesEvaluated, "rule", "rules")},
		field{"Total Waste", r.money(r.TotalMonthlyWasteUSD) + " / month"},
		field{"Duration", formatDuration(r.Duration)},
		field{"Artifacts", artifactsValue(r)},
	)

	for _, f := range fields {
		if err := writeSummaryField(w, f.label, f.value, width); err != nil {
			return err
		}
	}
	return nil
}

// writeSummaryField writes one SUMMARY key-value row, wrapping values longer
// than the summary block at slash, middle-dot or space boundaries.
func writeSummaryField(w io.Writer, label, value string, blockWidth int) error {
	valueWidth := blockWidth - summaryLabelWidth - 1
	if valueWidth < 1 {
		valueWidth = 1
	}
	lines := wrapSummaryValue(value, valueWidth)
	if len(lines) == 0 {
		lines = []string{""}
	}
	if _, err := fmt.Fprintf(w, "%-*s %s\n", summaryLabelWidth, label, lines[0]); err != nil {
		return err
	}
	indent := strings.Repeat(" ", summaryLabelWidth+1)
	for _, line := range lines[1:] {
		if _, err := fmt.Fprintln(w, indent+line); err != nil {
			return err
		}
	}
	return nil
}

// wrapSummaryValue wraps a SUMMARY value at the available value width. Spaces
// are the preferred break; slashes and middle dots are used when no space is
// available. A single unbreakable token longer than the width is emitted whole
// and may overflow — values are never truncated and never split inside a
// token.
func wrapSummaryValue(value string, width int) []string {
	if width <= 0 || value == "" {
		return []string{value}
	}

	// WRAP AT SPACES, NEVER INSIDE A TOKEN. A token here is a scope ID, a
	// filesystem path or a file:// URL, and breaking one across lines makes it
	// unselectable by double-click, useless to copy-paste and — for a URL —
	// unclickable, which was the entire reason for rendering it as a URL.
	//
	// A token wider than the column therefore overflows on a line of its own.
	// That is deliberate: the terminal soft-wraps it and the value survives
	// intact, where a hard break would destroy it. Splitting on "/" produced a
	// six-line artifacts row whose URL no terminal would open.
	var (
		out  []string
		line string
	)
	for _, tok := range strings.Fields(value) {
		switch {
		case line == "":
			line = tok
		case len([]rune(line))+1+len([]rune(tok)) <= width:
			line += " " + tok
		default:
			out = append(out, line)
			line = tok
		}
	}
	if line != "" {
		out = append(out, line)
	}
	if len(out) == 0 {
		return []string{value}
	}
	return out
}

// lastSummaryBreak returns the index of the preferred break within width:
// the rightmost space, falling back to the rightmost slash or middle dot.
func lastSummaryBreak(runes []rune, width int) int {
	limit := width - 1
	if limit >= len(runes) {
		limit = len(runes) - 1
	}
	for i := limit; i >= 0; i-- {
		if runes[i] == ' ' {
			return i
		}
	}
	for i := limit; i >= 0; i-- {
		if runes[i] == '/' || runes[i] == '·' {
			return i
		}
	}
	return -1
}

// nextSummaryBreak finds the first break at or after width. It is used when a
// single token is too long for the width: the over-long token is emitted whole
// and the next break becomes the wrap point.
func nextSummaryBreak(runes []rune, width int) int {
	if width >= len(runes) {
		return -1
	}
	for i := width; i < len(runes); i++ {
		if runes[i] == ' ' {
			return i
		}
	}
	for i := width; i < len(runes); i++ {
		if runes[i] == '/' || runes[i] == '·' {
			return i
		}
	}
	return -1
}

// hasCoverage reports whether the optional COVERAGE section has anything to
// say: AWS account outcomes, Azure subscription outcomes, or offline
// metric-blocked rules.
func hasCoverage(r Report) bool {
	return len(r.AccountStatuses) > 0 || len(r.SubscriptionStatuses) > 0 || len(r.MetricsBlocked) > 0
}

// writeCoverage renders the COVERAGE section content.
func writeCoverage(w io.Writer, r Report) error {
	if len(r.AccountStatuses) > 0 {
		if _, err := fmt.Fprintln(w, accountStatusLines(r.AccountStatuses)); err != nil {
			return err
		}
	}
	if len(r.SubscriptionStatuses) > 0 {
		if _, err := fmt.Fprintln(w, subscriptionStatusLines(r.SubscriptionStatuses)); err != nil {
			return err
		}
	}
	if len(r.MetricsBlocked) > 0 {
		if _, err := fmt.Fprintf(w,
			"%d rule(s) could not be evaluated for lack of metric data: %s\n"+
				"(use --cache-file from a live `scan` or an enriched `graph export` to evaluate them)\n",
			len(r.MetricsBlocked), strings.Join(r.MetricsBlocked, ", ")); err != nil {
			return err
		}
	}
	return nil
}

// scopeName returns the last path segment of a scope ID: the human-readable
// short name kept separate from the full Scope ID in SUMMARY.
func scopeName(scope string) string {
	scope = strings.TrimRight(scope, "/")
	if i := strings.LastIndex(scope, "/"); i >= 0 {
		return scope[i+1:]
	}
	return scope
}

// scannedValue renders the SUMMARY Scanned row: resources, optional skipped
// parenthetical, and the provider denominator that says how much ground the
// scan actually covered.
func scannedValue(r Report) string {
	v := countPhrase(r.ResourcesScanned, "resource", "resources")
	if r.ResourcesSkipped > 0 {
		v += fmt.Sprintf(" (%d skipped)", r.ResourcesSkipped)
	}
	v += " across " + providerDenominator(r)
	return v
}

// providerDenominator renders the container count a scan's resource total is
// measured against. The field is provider-owned: GCP reports projects, AWS
// accounts, Azure subscriptions.
func providerDenominator(r Report) string {
	switch r.Provider {
	case "aws":
		return countPhrase(r.AccountsAnalyzed, "account", "accounts")
	case "azure":
		return countPhrase(r.SubscriptionsAnalyzed, "subscription", "subscriptions")
	default:
		return countPhrase(r.ProjectsAnalyzed, "project", "projects")
	}
}

// regionsValue renders the SUMMARY Regions row for an AWS scan, including the
// source annotation that says whether discovery narrowed the sweep.
func regionsValue(r Report) string {
	v := countPhrase(r.RegionsAnalyzed, "region", "regions")
	if r.RegionSource != "" {
		v += " (" + r.RegionSource + ")"
	}
	return v
}

// artifactsValue names the artifact directory and the HTML report as a
// clickable file:// URL. The middle dot is the visual separator; the wrapping
// logic breaks on the spaces around it when the value exceeds the SUMMARY
// width.
// artifactsValue is the scan's output directory, shortened to a path relative
// to the working directory when it lives under it — which is the common case,
// since --out-dir defaults to ./tellury-out. The absolute form is 100+
// characters and tells a reader standing in the directory nothing they did not
// know.
//
// The directory and the report are one value separated by a space, so the
// wrapper breaks between them rather than through either: each stays a single
// token, selectable by double-click and clickable as a URL.
//
// Both are shown because they answer different questions — where everything
// was written, and what to open. Neither was mentioned anywhere before unless
// a scan produced more than ten findings.
func artifactsValue(r Report) string {
	if r.ReportPath == "" {
		return "-"
	}
	// Separated by a space, not a glyph: the wrapper breaks between tokens, so
	// a "·" joiner became a line containing nothing but the joiner.
	return shortenDir(filepath.Dir(r.ReportPath)) + " " + reportURL(r.ReportPath)
}

// shortenDir renders a path relative to the working directory when it lives
// under it, which is the common case: --out-dir defaults to ./tellury-out. The
// absolute form runs past 100 characters and tells a reader standing in that
// directory nothing they did not already know.
func shortenDir(dir string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return dir
	}
	rel, err := filepath.Rel(cwd, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return dir
	}
	return rel
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// severityLabel renders a finding severity as the uppercase text the SEVERITY
// column displays in both colour and monochrome modes. Colour is redundant
// with this text, never a replacement for it.
func severityLabel(s rules.Severity) string {
	switch s {
	case rules.SeverityHigh:
		return "HIGH"
	case rules.SeverityMedium:
		return "MEDIUM"
	case rules.SeverityLow:
		return "LOW"
	default:
		return strings.ToUpper(string(s))
	}
}

// severityCode maps severity to its basic ANSI foreground colour. LOW and any
// unknown severity are deliberately plain: the absence of colour is part of
// the severity signal.
func severityCode(s rules.Severity) string {
	switch s {
	case rules.SeverityHigh:
		return ansiRed
	case rules.SeverityMedium:
		return ansiYellow
	default:
		return ""
	}
}

// paint wraps text in an SGR colour when code is non-empty, resetting
// afterwards. A plain string passes through byte-for-byte.
func paint(code, s string) string {
	if code == "" {
		return s
	}
	return code + s + ansiReset
}

// paintSeverity renders one severity cell. The plain text is padded FIRST, and
// only the already-padded cell is wrapped in SGR: SGR sequences are zero-width
// on a terminal but not zero-rune to fmt's width calculations, so an escape
// sequence must never be placed inside a %-*s width argument.
func (t tableRenderer) paintSeverity(l tableLayout, s rules.Severity) string {
	cell := fmt.Sprintf("%-*s", l.severity, severityLabel(s))
	if !t.color {
		return cell
	}
	return paint(severityCode(s), cell)
}

// accountStatusLines renders the account outcome report below the COVERAGE
// header. When an organization scan skipped some accounts — because the role
// could not be assumed, the account was suspended, or ingestion failed — the
// total is incomplete and the operator must know exactly which accounts were
// affected and why.
func accountStatusLines(statuses []aws.AccountStatus) string {
	scanned, unreachable, suspended := 0, 0, 0
	for _, s := range statuses {
		switch s.Status {
		case "scanned":
			scanned++
		case "unreachable":
			unreachable++
		case "suspended":
			suspended++
		}
	}

	// Build a compact report: counts first, then the per-account breakdown
	// only when something was not scanned.
	counts := fmt.Sprintf("Account outcomes: %d scanned", scanned)
	if unreachable > 0 {
		counts += fmt.Sprintf(", %d unreachable", unreachable)
	}
	if suspended > 0 {
		counts += fmt.Sprintf(", %d suspended", suspended)
	}

	var b strings.Builder
	b.WriteString(counts)

	// List unreachable accounts with reasons.
	for _, s := range statuses {
		if s.Status == "unreachable" {
			b.WriteString("\n  unreachable: ")
			b.WriteString(s.ID)
			if s.Name != "" && s.Name != s.ID {
				b.WriteString(" (")
				b.WriteString(s.Name)
				b.WriteString(")")
			}
			if s.Reason != "" {
				b.WriteString(" — ")
				b.WriteString(s.Reason)
			}
		}
	}

	// List suspended accounts.
	for _, s := range statuses {
		if s.Status == "suspended" {
			b.WriteString("\n  suspended: ")
			b.WriteString(s.ID)
			if s.Name != "" && s.Name != s.ID {
				b.WriteString(" (")
				b.WriteString(s.Name)
				b.WriteString(")")
			}
		}
	}

	return b.String()
}

// subscriptionStatusLines renders the subscription outcome report below the
// COVERAGE header. When a tenant or management-group scan found subscriptions
// it could not query, the total is incomplete and the operator must know
// exactly which subscriptions were affected and why.
func subscriptionStatusLines(statuses []azure.SubscriptionStatus) string {
	scanned, unreachable, noResources := 0, 0, 0
	for _, s := range statuses {
		switch s.Status {
		case "scanned":
			scanned++
		case "unreachable":
			unreachable++
		case "no_resources":
			noResources++
		}
	}

	counts := fmt.Sprintf("Subscription outcomes: %d scanned", scanned)
	if unreachable > 0 {
		counts += fmt.Sprintf(", %d unreachable", unreachable)
	}
	if noResources > 0 {
		counts += fmt.Sprintf(", %d no_resources", noResources)
	}

	var b strings.Builder
	b.WriteString(counts)

	for _, s := range statuses {
		if s.Status == "unreachable" {
			b.WriteString("\n  unreachable: ")
			b.WriteString(s.ID)
			if s.Reason != "" {
				b.WriteString(" — ")
				b.WriteString(s.Reason)
			}
		}
	}
	return b.String()
}

// countPhrase renders "N singular" or "N plural" for the summary line.
func countPhrase(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// writeRow emits one single-project row. A literal space separates the three
// variable columns so a value that fills its cell (padded exactly at the
// boundary) can never touch its neighbour; the money column is right-aligned
// at its fixed width.
func writeRow(w io.Writer, l tableLayout, resource, rule, severity, amount string) error {
	_, err := fmt.Fprintf(w, "%-*s %-*s %s %*s\n",
		l.resource, resource,
		l.rule, rule,
		severity,
		l.money, amount)
	return err
}

// writeRowProject emits one multi-project row, with a literal space between
// every variable column so no cell can touch its right-hand neighbour.
func writeRowProject(w io.Writer, l tableLayout, resource, project, rule, severity, amount string) error {
	_, err := fmt.Fprintf(w, "%-*s %-*s %-*s %s %*s\n",
		l.resource, resource,
		l.project, project,
		l.rule, rule,
		severity,
		l.money, amount)
	return err
}

// writeTotalRow emits the TOTAL row: the "TOTAL" label in the resource cell,
// the finding summary left-aligned in the full width of the columns left of
// the money cell, and the total right-aligned in the money column. The summary
// width (totalSummaryWidth) spans the project, rule and severity columns, so
// "N findings" is never truncated the way it was when squeezed into the
// PROJECT column.
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
