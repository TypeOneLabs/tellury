package rules

import (
	"context"
	"fmt"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
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

// EvMoney formats a money value for evidence in the currency the pricer's
// answers are actually in, following the same convention as the report
// renderers: USD (and the empty default) keeps the "$0.0439" form, any other
// currency appends its code — "0.0439 EUR".
//
// It exists because every rule used to hardcode a "$" into its evidence. Once
// --currency landed, a scan priced in EUR rendered its table correctly as
// "1.25 EUR" while its evidence still read "$0.0439" for the same number —
// the figure right, the symbol a lie. Rules must not each re-derive this.
//
// prec is the number of decimal places; unit prices want 4, monthly totals 2.
// CurrencyOf reports the currency a Pass's answers are actually in, or "" when
// the pricer cannot say. Call it in Cost (which has the Pass) and stash the
// result in the NodeContext, because ExtraEvidence has no Pass to ask.
func CurrencyOf(p *Pass) string {
	if r, ok := p.Price.(pricing.CurrencyReporter); ok {
		return r.CurrencyInfo().Effective
	}
	return ""
}

// EvMoneyIn is EvMoney with the currency already resolved, for use inside
// ExtraEvidence via a value CurrencyOf stashed in the NodeContext.
func EvMoneyIn(key, currency string, v float64, prec int) Evidence {
	if currency == "" || currency == "USD" {
		return Evidence{Key: key, Value: fmt.Sprintf("$%.*f", prec, v)}
	}
	return Evidence{Key: key, Value: fmt.Sprintf("%.*f %s", prec, v, currency)}
}

// EvMoney formats a money value where a Pass is in hand.
func EvMoney(key string, p *Pass, v float64, prec int) Evidence {
	return EvMoneyIn(key, CurrencyOf(p), v, prec)
}
