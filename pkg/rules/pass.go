package rules

import (
	"log/slog"
	"sort"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// Pass is the per-rule evaluation context. Every rule receives one; it carries
// the frozen graph, the pricer, sizing catalog, scan scope/window, the
// evaluation instant, and a logger. It is copied by the engine per rule (the
// copy swaps in a rule-scoped logger and a rule-scoped skip tally callback),
// so a rule must treat the whole struct as read-only except for the skip
// callback.
type Pass struct {
	Graph *graph.Graph
	Price pricing.Pricer
	Scope string
	// Window is the metric lookback window in days (7-30).
	Window int
	Log    *slog.Logger
	// Now is the fixed evaluation instant; age predicates derive from it so
	// a scan with --at reruns identically.
	Now time.Time
	// Sizer resolves machine-type specs; nil means no catalog is available
	// and shape/candidate rules must skip.
	Sizer pricing.Sizer

	// Skip is called by SkipNode to tally one skipped resource. The engine
	// replaces it per rule so each skip lands in the shared Result under the
	// correct rule ID. A rule never writes here directly; it calls SkipNode.
	Skip func(ruleID string, ref graph.Ref, code SkipCode)
}

// SkipNode tallies one resource the current rule declined to report. It is
// the single place a rule records a skip; the engine aggregates these into
// Result.Skipped which `--explain-skips` renders. A nil Skip (a Pass built
// directly in a pure unit test with no engine) is a silent no-op, so tests
// that only care about findings do not need a tally harness.
func (p *Pass) SkipNode(ruleID string, ref graph.Ref, code SkipCode) {
	if p.Skip == nil {
		return
	}
	p.Skip(ruleID, ref, code)
}

// SkipTally is one (rule, code) skip count, aggregated across all evaluated
// nodes. It is the machine-readable form of `tellury scan --explain-skips`.
type SkipTally struct {
	RuleID string   `json:"rule_id"`
	Code   SkipCode `json:"code"`
	Count  int      `json:"count"`
}

// SkipTotals flattens Result.Skipped into a deterministically sorted slice
// (by RuleID, then Code) so renderers never need map iteration.
func (r Result) SkipTotals() []SkipTally {
	if len(r.Skipped) == 0 {
		return nil
	}
	out := make([]SkipTally, 0, len(r.Skipped))
	for ruleID, byCode := range r.Skipped {
		for code, count := range byCode {
			out = append(out, SkipTally{RuleID: ruleID, Code: code, Count: count})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuleID != out[j].RuleID {
			return out[i].RuleID < out[j].RuleID
		}
		return out[i].Code < out[j].Code
	})
	return out
}
