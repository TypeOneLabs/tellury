package output

import (
	"bytes"
	"strings"
	"testing"
)

// TestMoney_RendersInReportCurrency pins the amount rendering convention: USD
// (including the empty default) keeps the historical "$12.40" form, any other
// currency appends its code so a EUR figure can never be mistaken for dollars.
func TestMoney_RendersInReportCurrency(t *testing.T) {
	cases := []struct {
		currency string
		want     string
	}{
		{"", "$12.40"},    // default: byte-identical to the pre-currency build
		{"USD", "$12.40"}, // explicit USD is still USD
		{"EUR", "12.40 EUR"},
		{"GBP", "12.40 GBP"},
	}
	for _, tc := range cases {
		r := Report{Currency: tc.currency}
		if got := r.money(12.4); got != tc.want {
			t.Errorf("money(12.4) with currency %q = %q, want %q", tc.currency, got, tc.want)
		}
	}
}

// TestCurrencyDisclosure_DefaultIsSilent: the default USD scan emits no
// disclosure, keeping the table output byte-identical to the pre-currency
// build.
func TestCurrencyDisclosure_DefaultIsSilent(t *testing.T) {
	if lines := currencyDisclosure(Report{}); len(lines) != 0 {
		t.Fatalf("default scan must be silent about currency, got %v", lines)
	}
}

// TestCurrencyDisclosure_FlagAndDetectedStateHowDecided: a non-USD scan states
// which currency the figures are in and how it was decided, so an operator
// reading EUR figures knows the tool determined them rather than assumed them.
func TestCurrencyDisclosure_FlagAndDetectedStateHowDecided(t *testing.T) {
	flag := currencyDisclosure(Report{Currency: "EUR", CurrencySource: "flag"})
	if len(flag) != 1 || !strings.Contains(flag[0], "requested via --currency") {
		t.Fatalf("flag disclosure = %v, want a line naming --currency", flag)
	}
	det := currencyDisclosure(Report{Currency: "EUR", CurrencySource: "detected"})
	if len(det) != 1 || !strings.Contains(det[0], "detected from the billing account") {
		t.Fatalf("detected disclosure = %v, want a line naming detection", det)
	}
}

// TestCurrencyDisclosure_MixedWarnsLoudly: USD prices answering a non-USD scan
// is the currency trap, and the disclosure must be a loud warning, never a
// silent degradation.
func TestCurrencyDisclosure_MixedWarnsLoudly(t *testing.T) {
	// Full fallback: the requested currency priced nothing, every figure is
	// USD.
	full := currencyDisclosure(Report{
		Currency: "USD", CurrencySource: "flag", CurrencyRequested: "EUR", CurrencyMixed: true,
	})
	if len(full) == 0 || !strings.Contains(full[0], "WARNING") {
		t.Fatalf("full-fallback disclosure must start with a WARNING, got %v", full)
	}
	if !strings.Contains(full[0], "not the requested EUR") {
		t.Fatalf("full-fallback disclosure must name the requested currency, got %v", full)
	}

	// Partial contamination: the catalogue answered in EUR but some prices
	// could not be resolved in that currency.
	partial := currencyDisclosure(Report{
		Currency: "EUR", CurrencySource: "flag", CurrencyRequested: "EUR", CurrencyMixed: true,
	})
	if len(partial) == 0 || !strings.Contains(partial[0], "WARNING") {
		t.Fatalf("partial-contamination disclosure must start with a WARNING, got %v", partial)
	}
	if !strings.Contains(partial[0], "could not be resolved in EUR") {
		t.Fatalf("partial-contamination disclosure must state prices could not be resolved, got %v", partial)
	}
}

// TestTableRender_EURDisclosesAndRendersCurrency: the table output for a
// non-USD scan states the currency up front (and how it was decided) and
// renders each amount with the code — the operator-facing proof that the tool
// determined the currency rather than assumed it.
func TestTableRender_EURDisclosesAndRendersCurrency(t *testing.T) {
	r := defaultCSVReport()
	r.Currency = "EUR"
	r.CurrencySource = "detected"
	r.CurrencyRequested = "EUR"

	var buf bytes.Buffer
	if err := (tableRenderer{}).Render(&buf, r); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()

	if !strings.Contains(got, "Prices are in EUR (detected from the billing account).") {
		t.Errorf("table output missing the currency disclosure:\n%s", got)
	}
	if !strings.Contains(got, "12.40 EUR") {
		t.Errorf("table output must render the finding amount in EUR:\n%s", got)
	}
	if strings.Contains(got, "$12.40") {
		t.Errorf("EUR table output must not render $-prefixed amounts:\n%s", got)
	}
}
