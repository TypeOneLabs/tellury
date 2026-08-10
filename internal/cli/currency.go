package cli

import (
	"context"
	"log/slog"

	"github.com/TypeOneLabs/tellury/internal/config"
	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	pricinggcp "github.com/TypeOneLabs/tellury/pkg/pricing/gcp"
)

// currencyResolution is the outcome of deciding which currency a scan prices
// in, before rule evaluation: the requested code and how it was decided. The
// effective currency — what the figures are ACTUALLY in — is derived later
// from the pricer (reportCurrency), after rule evaluation has exercised it,
// because only the pricer knows whether the live catalogue answered in the
// requested currency or the embedded USD table had to.
type currencyResolution struct {
	requested string // ISO 4217 code the operator asked for or the tool detected; "" = default USD
	source    string // "flag" | "detected" | "default"
}

// resolveScanCurrency decides the scan's currency in precedence order —
// explicit --currency/TELLURY_CURRENCY, then best-effort billing-account
// detection, then USD — and applies the decision to the pricer (via
// pricing.CurrencySetter) so the live catalogue is fetched in that currency.
// Detection is skipped entirely for offline scans (no cloud client exists)
// and whenever the operator passed an explicit currency.
//
// Detection needs the ingested graph for a folder/organization scope (there
// is no single project to ask), so this runs after buildGraph and before rule
// evaluation. The pricer's catalogue loads lazily on the first UnitPrice call
// — which happens during rule evaluation — so setting the currency here still
// reaches every ListSkus request.
func resolveScanCurrency(ctx context.Context, cfg config.Scan, pricer pricing.Pricer, gr *graph.Graph, log *slog.Logger, offline bool) currencyResolution {
	if cfg.Currency != "" {
		log.Info("currency", "code", cfg.Currency, "source", "flag")
		return currencyResolution{requested: cfg.Currency, source: "flag"}
	}
	if offline {
		// A fixture/cache replay prices everything from the embedded USD
		// table; there is no cloud client to ask, so USD is the only answer.
		log.Info("currency", "code", "USD", "source", "default", "reason", "offline scan (no billing account to ask)")
		return currencyResolution{requested: "", source: "default"}
	}

	candidates := projectsInScope(cfg, gr)
	code, project := pricinggcp.DetectCurrency(ctx, log, candidates)
	if code == "" {
		// Best effort only: a missing billing role is a normal state for an
		// otherwise-healthy scan, exactly like absent Monitoring or Billing
		// access. Fall back to USD quietly (the report stays silent about it,
		// matching the pre-currency default output).
		log.Info("currency", "code", "USD", "source", "default",
			"reason", "no billing account answered for the scan's projects")
		return currencyResolution{requested: "", source: "default"}
	}

	if setter, ok := pricer.(pricing.CurrencySetter); ok {
		setter.SetCurrency(code)
	}
	// Say what we chose — and from which project — so an operator reading EUR
	// figures knows the tool determined them rather than assumed them.
	log.Info("currency", "code", code, "source", "detected", "project", project)
	return currencyResolution{requested: code, source: "detected"}
}

// reportCurrency derives the effective currency (what the figures are actually
// in) and whether USD fallback prices contaminated a non-USD scan, from the
// pricer after rule evaluation has exercised it. A pricer without a
// pricing.CurrencyReporter is a bare embedded-table pricer (an offline scan
// or a --price-file-only build) and always answers in USD.
func reportCurrency(pricer pricing.Pricer, state currencyResolution) (effective string, mixed bool) {
	if rep, ok := pricer.(pricing.CurrencyReporter); ok {
		info := rep.CurrencyInfo()
		effective = info.Effective
		mixed = info.Mixed
		if effective == "" {
			effective = "USD"
		}
		return effective, mixed
	}
	effective = "USD"
	// The embedded table answered everything. If a non-USD currency was
	// requested, every figure is in the wrong currency — that is the trap,
	// and it must be disclosed loudly.
	mixed = state.requested != "" && state.requested != "USD"
	return effective, mixed
}

// projectsInScope returns the project IDs currency detection can ask about:
// the explicit --gcp-project for a project scope, or the distinct projects
// present in the ingested graph for a folder/organization scope (there is no
// single project to ask). Detection stops at the first project that answers.
func projectsInScope(cfg config.Scan, gr *graph.Graph) []string {
	if cfg.Project != "" {
		return []string{cfg.Project}
	}
	if gr == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	gr.Nodes(func(n *graph.Node) bool {
		if !n.Container() && n.Project != "" && !seen[n.Project] {
			seen[n.Project] = true
			out = append(out, n.Project)
		}
		return true
	})
	return out
}
