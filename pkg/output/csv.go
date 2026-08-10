package output

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

type csvRenderer struct{}

func (csvRenderer) Format() string { return "csv" }

// csvDiscloses reports whether a scan's CSV must carry the currency columns.
// A default scan — USD, nothing requested or detected (NewReport clears every
// currency field) — is silent, keeping the historical header byte-for-byte so
// the pre-currency baseline guardrail holds. Every non-default scan (the
// --currency flag or billing-account detection chose a currency) and every
// scan whose figures were contaminated by USD fallback prices names the
// currency, because an operator reading EUR figures must never be handed a
// CSV whose numbers could be dollars without the file saying so.
func csvDiscloses(r Report) bool {
	return r.CurrencySource != "" || r.CurrencyMixed || (r.Currency != "" && r.Currency != "USD")
}

// csvHeader returns the CSV header. A default scan keeps the exact historical
// header. A non-USD scan renames the money column from the historical
// "monthly_waste_usd" to "monthly_waste_<code>" (e.g. "monthly_waste_eur").
// Any non-default scan appends four currency columns that name the effective
// currency, how it was decided, what was requested, and whether USD
// embedded-fallback prices contaminated the figures — so a machine reader can
// always tell what a number is in without reading a README.
func csvHeader(r Report) []string {
	h := []string{
		"resource", "rule", "monthly_waste_usd", "severity", "confidence",
		"kind", "project", "location", "resource_id", "evidence",
	}
	if r.Currency != "" && r.Currency != "USD" {
		h[2] = "monthly_waste_" + strings.ToLower(r.Currency)
	}
	if csvDiscloses(r) {
		h = append(h, "currency", "currency_source", "currency_requested", "currency_mixed")
	}
	return h
}

func (csvRenderer) Render(w io.Writer, r Report) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader(r)); err != nil {
		return err
	}
	currencyColumns := csvDiscloses(r)
	for _, f := range r.Findings {
		row := []string{
			f.Resource,
			f.RuleID,
			fmt.Sprintf("%.2f", f.MonthlyWasteUSD),
			string(f.Severity),
			fmt.Sprintf("%.2f", f.Confidence),
			string(f.Kind),
			f.Project,
			f.Location,
			string(f.ResourceID),
			flattenEvidence(f.Evidence),
		}
		if currencyColumns {
			// The money cell stays a plain 2-dp number; the renamed column
			// and these explicit columns carry the unit, so a spreadsheet
			// never has to parse "12.40 EUR" out of a numeric cell.
			row = append(row,
				csvCurrency(r.Currency),
				csvCurrencySource(r.CurrencySource),
				csvRequested(r.CurrencyRequested),
				csvBool(r.CurrencyMixed),
			)
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// csvCurrency renders the effective currency code, defaulting to USD so a
// disclosed scan never leaves the cell empty (a bare Report built without a
// currency still discloses its figures as USD).
func csvCurrency(currency string) string {
	if currency == "" {
		return "USD"
	}
	return currency
}

// csvCurrencySource renders how the currency was decided. For a non-default
// scan the source is "flag" or "detected"; a defensive fallback keeps the
// cell non-empty.
func csvCurrencySource(source string) string {
	if source == "" {
		return "default"
	}
	return source
}

// csvRequested renders the requested currency code, defaulting to USD so a
// flag/detected source always carries a requested code.
func csvRequested(requested string) string {
	if requested == "" {
		return "USD"
	}
	return requested
}

func csvBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func flattenEvidence(ev []rules.Evidence) string {
	if len(ev) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ev))
	for _, e := range ev {
		parts = append(parts, e.Key+"="+e.Value)
	}
	return strings.Join(parts, "; ")
}
