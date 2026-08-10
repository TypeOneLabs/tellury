package output

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// htmlCurrencyReport builds a minimal report with one finding plus a
// project->disk containment tree, so the rollup hierarchy and the findings
// table both render.
func htmlCurrencyReport(t *testing.T, r Report) string {
	t.Helper()
	g := graph.New()
	project := &graph.Node{ID: "projects/my-project", Kind: graph.KindProject, Name: "my-project", Project: "my-project"}
	leaf := &graph.Node{ID: "//…/disks/pd-01", Kind: graph.KindDisk, Name: "pd-01", Project: "my-project"}
	for _, n := range []*graph.Node{project, leaf} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := g.AddEdge(graph.Edge{From: leaf.ID, To: project.ID, Kind: graph.EdgeContains}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.Freeze()

	var buf bytes.Buffer
	if err := RenderHTML(&buf, r, BuildHierarchy(g, r.Findings, "projects/my-project")); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	return buf.String()
}

func baseEURReport() Report {
	return Report{
		Scope:                "projects/my-project",
		Provider:             "gcp",
		GeneratedAt:          time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC),
		WindowDays:           14,
		Findings:             []rules.Finding{findingFixture()},
		TotalMonthlyWasteUSD: 12.40,
		FindingCount:         1,
		ResourcesScanned:     1,
		RulesEvaluated:       3,
	}
}

// TestRenderHTML_NonUSDRendersCurrencyEverywhere: a EUR scan must name its
// currency in the header disclosure and render every figure as "12.40 EUR" —
// in the rollup hierarchy AND the findings table — so a figure can never be
// mistaken for dollars.
func TestRenderHTML_NonUSDRendersCurrencyEverywhere(t *testing.T) {
	r := baseEURReport()
	r.Currency = "EUR"
	r.CurrencySource = "detected"
	r.CurrencyRequested = "EUR"

	got := htmlCurrencyReport(t, r)

	// Header disclosure: which currency, and how it was decided.
	if !strings.Contains(got, "Prices are in EUR (detected from the billing account).") {
		t.Errorf("HTML missing the detected-currency disclosure:\n%s", got)
	}
	// Rollup hierarchy figure and findings-table figure.
	if strings.Count(got, "12.40 EUR") < 2 {
		t.Errorf("HTML must render 12.40 EUR in the hierarchy and the findings table:\n%s", got)
	}
	if strings.Contains(got, "$12.40") {
		t.Errorf("EUR scan must not render a $-prefixed amount:\n%s", got)
	}
}

// TestRenderHTML_MixedUSDFallbackWarnsLoudly: when USD embedded-fallback
// prices contaminated a non-USD request, the HTML must say so loudly — the
// header carries the warning and the figures stay $-prefixed (they really are
// USD, and the report must not pretend otherwise).
func TestRenderHTML_MixedUSDFallbackWarnsLoudly(t *testing.T) {
	r := baseEURReport()
	r.Currency = "USD"
	r.CurrencySource = "flag"
	r.CurrencyRequested = "EUR"
	r.CurrencyMixed = true

	got := htmlCurrencyReport(t, r)

	if !strings.Contains(got, "WARNING: prices are in USD, not the requested EUR.") {
		t.Errorf("HTML missing the loud mixed-currency warning:\n%s", got)
	}
	if !strings.Contains(got, "$12.40") {
		t.Errorf("mixed USD report must render the (real) $-prefixed amounts:\n%s", got)
	}
}

// TestRenderHTML_DefaultHasNoCurrencyParagraph: the default USD scan must not
// emit the currency disclosure paragraph at all, keeping the document
// byte-identical to the pre-currency build.
func TestRenderHTML_DefaultHasNoCurrencyParagraph(t *testing.T) {
	got := htmlCurrencyReport(t, baseEURReport())
	if strings.Contains(got, `<p class="currency">`) {
		t.Errorf("default scan must not render a currency paragraph:\n%s", got)
	}
}
