package output

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// defaultCSVReport is the report shape a default scan produces: no currency
// fields at all (NewReport clears them), so the renderer must stay
// byte-identical to the pre-currency build.
func defaultCSVReport() Report {
	return Report{
		Scope:                "projects/my-project",
		Provider:             "gcp",
		WindowDays:           14,
		Findings:             []rules.Finding{findingFixture()},
		TotalMonthlyWasteUSD: 12.40,
		FindingCount:         1,
		ResourcesScanned:     1,
		RulesEvaluated:       3,
	}
}

func findingFixture() rules.Finding {
	return rules.Finding{
		RuleID:          "detached_disk",
		ResourceID:      graph.Ref("//…/disks/pd-01"),
		Resource:        "disk/pd-01",
		Kind:            graph.KindDisk,
		Project:         "my-project",
		Location:        "us-central1-a",
		Severity:        rules.SeverityHigh,
		Confidence:      0.95,
		MonthlyWasteUSD: 12.40,
		Evidence:        []rules.Evidence{{Key: "price_source", Value: "embedded_fallback sku=pd-standard"}},
	}
}

// TestCSVRender_DefaultIsByteIdenticalToPreCurrencyOutput pins the default
// path: no currency requested/detected means the exact historical header and
// plain 2-dp number cells — the guardrail that replays fixtures against the
// pre-change baseline without a diff.
func TestCSVRender_DefaultIsByteIdenticalToPreCurrencyOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := (csvRenderer{}).Render(&buf, defaultCSVReport()); err != nil {
		t.Fatalf("Render: %v", err)
	}

	wantHeader := "resource,rule,monthly_waste_usd,severity,confidence,kind,project,location,resource_id,evidence\n"
	lines := buf.String()
	if !strings.HasPrefix(lines, wantHeader) {
		t.Fatalf("default CSV header changed:\n got %q\nwant %q", lines[:len(wantHeader)], wantHeader)
	}
	if strings.Contains(lines, "currency") {
		t.Errorf("default CSV must not carry currency columns:\n%s", lines)
	}
	// The money cell stays a plain number; no $ sign, no code suffix.
	if !strings.Contains(lines, "detached_disk,12.40,high") {
		t.Errorf("default CSV money cell diverged from the historical shape:\n%s", lines)
	}
}

// TestCSVRender_EURRenamesMoneyColumnAndNamesCurrency: a non-USD scan renames
// the money column to monthly_waste_<code> and appends the four currency
// columns, so a machine reader can always tell what a figure is in. The money
// cell itself stays a plain 2-dp number; the column name and the currency
// columns carry the unit.
func TestCSVRender_EURRenamesMoneyColumnAndNamesCurrency(t *testing.T) {
	r := defaultCSVReport()
	r.Currency = "EUR"
	r.CurrencySource = "detected"
	r.CurrencyRequested = "EUR"
	r.CurrencyMixed = false

	var buf bytes.Buffer
	if err := (csvRenderer{}).Render(&buf, r); err != nil {
		t.Fatalf("Render: %v", err)
	}
	lines := buf.String()

	wantHeader := "resource,rule,monthly_waste_eur,severity,confidence,kind,project,location,resource_id,evidence,currency,currency_source,currency_requested,currency_mixed\n"
	if !strings.HasPrefix(lines, wantHeader) {
		t.Fatalf("EUR CSV header = %q, want %q", firstLine(lines), wantHeader)
	}
	if strings.Contains(lines, "monthly_waste_usd") {
		t.Errorf("EUR CSV must not keep the historical monthly_waste_usd column:\n%s", lines)
	}
	// The data row names the currency, how it was decided, what was requested,
	// and that nothing fell back to USD.
	wantRow := "disk/pd-01,detached_disk,12.40,high,0.95,disk,my-project,us-central1-a,//…/disks/pd-01," +
		"price_source=embedded_fallback sku=pd-standard,EUR,detected,EUR,false\n"
	if !strings.Contains(lines, wantRow) {
		t.Errorf("EUR CSV row = missing; want %q in:\n%s", wantRow, lines)
	}
}

// TestCSVRender_MixedFlagsUSDFallbackLoudly: when USD embedded-fallback prices
// contaminated a non-USD scan, the currency_mixed column must say true on
// every row so a machine reader cannot mistake the numbers' currency. The
// money column stays monthly_waste_usd because the figures really are USD.
func TestCSVRender_MixedFlagsUSDFallbackLoudly(t *testing.T) {
	r := defaultCSVReport()
	r.Currency = "USD"
	r.CurrencySource = "flag"
	r.CurrencyRequested = "EUR"
	r.CurrencyMixed = true

	var buf bytes.Buffer
	if err := (csvRenderer{}).Render(&buf, r); err != nil {
		t.Fatalf("Render: %v", err)
	}
	lines := buf.String()
	if !strings.Contains(lines, ",USD,flag,EUR,true\n") {
		t.Errorf("mixed CSV row must end with currency=USD, source=flag, requested=EUR, mixed=true:\n%s", lines)
	}
	// The money column is NOT renamed for a full fallback: the figures are
	// USD, and the header must say so rather than pretend they are EUR.
	if !strings.Contains(lines, "monthly_waste_usd") {
		t.Errorf("mixed CSV must keep monthly_waste_usd (the figures really are USD):\n%s", lines)
	}
}

// TestCSVRender_ExplicitUSDDisclosesWithoutRenaming: --currency USD is USD —
// the money column keeps its historical monthly_waste_usd name (no rename),
// but the currency columns ARE appended because the operator chose the
// currency, exactly matching the JSON report's currency fields for the same
// scan.
func TestCSVRender_ExplicitUSDDisclosesWithoutRenaming(t *testing.T) {
	r := defaultCSVReport()
	r.Currency = "USD"
	r.CurrencySource = "flag"
	r.CurrencyRequested = "USD"

	var buf bytes.Buffer
	if err := (csvRenderer{}).Render(&buf, r); err != nil {
		t.Fatalf("Render: %v", err)
	}
	lines := buf.String()
	if !strings.HasPrefix(lines, "resource,rule,monthly_waste_usd,") {
		t.Fatalf("explicit-USD CSV must keep the historical money column:\n%s", firstLine(lines))
	}
	if !strings.Contains(lines, ",USD,flag,USD,false\n") {
		t.Errorf("explicit-USD CSV must disclose currency=USD, source=flag, requested=USD, mixed=false:\n%s", lines)
	}
}

// TestCSVHeader_IsParseableAsCSV guards the shape against drift: whatever the
// renderer emits must round-trip through encoding/csv with a consistent column
// count on every row (header and data).
func TestCSVHeader_IsParseableAsCSV(t *testing.T) {
	reports := []Report{
		defaultCSVReport(),
		func() Report {
			r := defaultCSVReport()
			r.Currency = "EUR"
			r.CurrencySource = "flag"
			r.CurrencyRequested = "EUR"
			return r
		}(),
		func() Report {
			r := defaultCSVReport()
			r.Currency = "USD"
			r.CurrencySource = "flag"
			r.CurrencyRequested = "EUR"
			r.CurrencyMixed = true
			return r
		}(),
	}
	for _, r := range reports {
		var buf bytes.Buffer
		if err := (csvRenderer{}).Render(&buf, r); err != nil {
			t.Fatalf("Render: %v", err)
		}
		rows, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
		if err != nil {
			t.Fatalf("output is not parseable CSV: %v", err)
		}
		want := len(rows[0])
		for i, row := range rows {
			if len(row) != want {
				t.Fatalf("row %d has %d columns, want %d", i, len(row), want)
			}
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
