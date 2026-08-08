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
}

// Meta carries the scan context that is not derivable from the findings.
type Meta struct {
	Scope            string
	Provider         string
	GeneratedAt      time.Time
	WindowDays       int
	ResourcesScanned int
	RulesEvaluated   int
	MultiProject     bool
}

// NewReport assembles a Report and computes the totals exactly once. The sum is
// over unrounded finding values and is rounded a single time (invariant I3).
func NewReport(res rules.Result, m Meta) Report {
	r := Report{
		Scope:            m.Scope,
		Provider:         m.Provider,
		GeneratedAt:      m.GeneratedAt.UTC(),
		WindowDays:       m.WindowDays,
		Findings:         res.Findings,
		FindingCount:     len(res.Findings),
		ResourcesScanned: m.ResourcesScanned,
		RulesEvaluated:   m.RulesEvaluated,
		MultiProject:     m.MultiProject,
		Skipped:          res.SkipTotals(),
	}
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

// money formats a USD amount with the report's fixed 2-dp convention.
func money(v float64) string { return fmt.Sprintf("$%.2f", pricing.Round2(v)) }
