package cli

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/internal/config"
)

// TestScanCurrencyEUROfflineWarnsLoudly drives the currency-mix trap through
// the REAL runScan pipeline: an offline scan (--fixture, so the pricer is the
// embedded USD table) with an explicit --currency EUR. Detection cannot run
// offline and the embedded table is USD-only, so every figure is USD while the
// operator asked for EUR — and the scan's output must say so loudly, in every
// format, never silently. This is the acceptance test for the trap.
func TestScanCurrencyEUROfflineWarnsLoudly(t *testing.T) {
	for _, format := range []string{"table", "json"} {
		t.Run(format, func(t *testing.T) {
			cfg := config.Scan{
				Provider:       "gcp",
				Project:        "my-project",
				Fixture:        []string{"testdata/readme-assets.json"},
				Format:         format,
				Rules:          []string{"detached_disk"},
				Currency:       "EUR",
				FailOnFindings: false,
				OutDir:         filepath.Join(t.TempDir(), "out"),
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			g := &globalFlags{LogLevel: "warn"}
			var out, errOut strings.Builder
			if err := runScan(context.Background(), &out, &errOut, g, cfg, readmeNow); err != nil {
				t.Fatalf("runScan (EUR offline): %v", err)
			}
			got := out.String()

			if format == "table" {
				// The embedded USD table answered an EUR request: the table must
				// carry the loud warning and render the (real) USD amounts with
				// a "$" — never "8.00 EUR" invented from a USD number.
				if !strings.Contains(got, "WARNING: prices are in USD, not the requested EUR.") {
					t.Errorf("EUR offline table must warn loudly about the USD fallback:\n%s", got)
				}
				if !strings.Contains(got, "$8.00") {
					t.Errorf("EUR offline table must render the real USD amount $8.00:\n%s", got)
				}
				if strings.Contains(got, "8.00 EUR") {
					t.Errorf("EUR offline table must never render a fake EUR amount:\n%s", got)
				}
			} else {
				// JSON names the effective currency, the requested one, and the
				// mix — the machine-readable disclosure of the same trap.
				for _, want := range []string{
					`"currency": "USD"`,
					`"currency_source": "flag"`,
					`"currency_requested": "EUR"`,
					`"currency_mixed": true`,
				} {
					if !strings.Contains(got, want) {
						t.Errorf("EUR offline JSON missing %s:\n%s", want, got)
					}
				}
			}
		})
	}
}

// TestScanCurrencyDefaultOfflineOutputUnchanged pins the default guardrail
// through the real pipeline: with no --currency, an offline scan's output is
// byte-identical to the pre-currency shape — no currency disclosure, no
// currency fields, "$" amounts — so the findings guardrail that replays
// fixtures against the pre-change baseline stays green.
func TestScanCurrencyDefaultOfflineOutputUnchanged(t *testing.T) {
	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{"testdata/readme-assets.json"},
		Format:         "json",
		Rules:          []string{"detached_disk"},
		FailOnFindings: false,
		OutDir:         filepath.Join(t.TempDir(), "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out, errOut strings.Builder
	if err := runScan(context.Background(), &out, &errOut, g, cfg, readmeNow); err != nil {
		t.Fatalf("runScan (default offline): %v", err)
	}
	got := out.String()

	for _, forbidden := range []string{`"currency"`, `"currency_mixed"`, `"currency_requested"`} {
		if strings.Contains(got, forbidden) {
			t.Errorf("default offline JSON must not carry currency fields (%s):\n%s", forbidden, got)
		}
	}
	// The finding still names the historical field and the total carries the
	// same name — the pre-currency schema, untouched.
	if !strings.Contains(got, `"monthly_waste_usd"`) {
		t.Errorf("default offline JSON must keep the historical monthly_waste_usd field:\n%s", got)
	}
	if !strings.Contains(got, `"total_monthly_waste_usd"`) {
		t.Errorf("default offline JSON must keep the historical total_monthly_waste_usd field:\n%s", got)
	}
}

var _ = slog.Default // keep the slog import honest if the helpers above change
