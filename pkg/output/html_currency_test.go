package output

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// htmlCurrencyReport renders a Report to a string for currency assertions.
func htmlCurrencyReport(t *testing.T, r Report) string {
	t.Helper()
	var buf bytes.Buffer
	if err := RenderHTML(&buf, r); err != nil {
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
// in the hero AND the findings table — so a figure can never be mistaken for
// dollars.
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
	// Hero figure and findings-table figure.
	if strings.Count(got, "12.40 EUR") < 2 {
		t.Errorf("HTML must render 12.40 EUR in the hero and the findings table:\n%s", got)
	}
	if strings.Contains(got, "$12.40") {
		t.Errorf("EUR scan must not render a $-prefixed amount:\n%s", got)
	}
}

// TestRenderHTML_MixedUSDFallbackWarnsLoudly: when USD embedded-fallback
// prices contaminated a non-USD request, the HTML must say so loudly — the
// header carries the amber warning and the figures stay $-prefixed (they
// really are USD, and the report must not pretend otherwise).
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
	if !strings.Contains(got, "currency-mixed") {
		t.Errorf("mixed-currency disclosure must carry the amber warning styling:\n%s", got)
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
