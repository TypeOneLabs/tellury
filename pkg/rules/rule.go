package rules

import (
	"context"
	"fmt"
)

// Severity is a coarse impact classification for a Finding / Rule.
type Severity string

const (
	SeverityLow    Severity = "low"
	SeverityMedium Severity = "medium"
	SeverityHigh   Severity = "high"
)

// Origin records how a rule came to exist.
const OriginNative = "native"

// Meta is a rule's static, machine-readable declaration.
type Meta struct {
	ID       string
	Provider string
	Service  string

	Title       string
	Description string
	Severity    Severity

	// RequiredAssetTypes are provider asset-type strings pushed to the API.
	RequiredAssetTypes []string
	// RequiredMetrics are metrics.Key values needed by Eval.
	RequiredMetrics []string

	Remediation string
	// Origin records how the rule came to exist: "native".
	Origin string
}

// Rule is the only extension point of the engine.
//
// Contract:
//   - Eval MUST be pure w.r.t. the graph: read-only, no mutation, no I/O.
//   - Eval MUST be safe for concurrent execution alongside other rules.
//   - Eval MUST skip (not guess) when required data is absent.
//   - Eval returns findings in any order; the engine sorts.
type Rule interface {
	Meta() Meta
	Eval(ctx context.Context, p *Pass) ([]Finding, error)
}

// RuleFunc adapts a function to Rule.
type RuleFunc struct {
	M  Meta
	Fn func(context.Context, *Pass) ([]Finding, error)
}

func (r RuleFunc) Meta() Meta { return r.M }
func (r RuleFunc) Eval(ctx context.Context, p *Pass) ([]Finding, error) {
	return r.Fn(ctx, p)
}

// Ev builds one Evidence entry, formatting value with format. It is the
// shared helper native rules use so evidence rendering never drifts between
// the two paths.
func Ev(key, format string, value any) Evidence {
	return Evidence{Key: key, Value: fmt.Sprintf(format, value)}
}
