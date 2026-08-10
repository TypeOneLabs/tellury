package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveCurrency_FlagBeatsEnv follows the flag-beats-environment
// convention every other scan option uses: --currency wins over
// TELLURY_CURRENCY.
func TestResolveCurrency_FlagBeatsEnv(t *testing.T) {
	t.Setenv(CurrencyEnvVar, "GBP")
	if got := resolveCurrency("EUR"); got != "EUR" {
		t.Fatalf("resolveCurrency(flag=EUR, env=GBP) = %q, want EUR (flag wins)", got)
	}
}

// TestResolveCurrency_EnvUsedWhenFlagAbsent: with no --currency the
// TELLURY_CURRENCY variable is read and normalized.
func TestResolveCurrency_EnvUsedWhenFlagAbsent(t *testing.T) {
	t.Setenv(CurrencyEnvVar, "eur")
	if got := resolveCurrency(""); got != "EUR" {
		t.Fatalf("resolveCurrency(flag empty, env=eur) = %q, want normalized EUR", got)
	}
}

// TestResolveCurrency_NormalizesTrimsAndUppercases: " eur " must resolve to
// "EUR" so a sloppy flag value still reaches the Cloud Billing API in the
// exact form the API expects.
func TestResolveCurrency_NormalizesTrimsAndUppercases(t *testing.T) {
	t.Setenv(CurrencyEnvVar, "")
	if got := resolveCurrency(" eur "); got != "EUR" {
		t.Fatalf("resolveCurrency(\" eur \") = %q, want EUR", got)
	}
}

// TestResolveCurrency_EmptyFlagAndEnvYieldsEmpty: neither flag nor env set
// means "not requested" — the empty string, which the scan treats as
// detect-then-default-to-USD, never as a malformed code.
func TestResolveCurrency_EmptyFlagAndEnv(t *testing.T) {
	t.Setenv(CurrencyEnvVar, "")
	if got := resolveCurrency(""); got != "" {
		t.Fatalf("resolveCurrency(\"\") = %q, want empty (not requested)", got)
	}
}

// TestValidCurrencyCode_WellFormedOnly: validation checks the SHAPE (exactly
// three ASCII uppercase letters) and deliberately does NOT check that the code
// exists in ISO 4217 — a well-formed but unsupported code must reach the Cloud
// Billing API and fail there, naming the currency, rather than being rejected
// here as if it were malformed.
func TestValidCurrencyCode_WellFormedOnly(t *testing.T) {
	for code, want := range map[string]bool{
		"EUR":  true,
		"USD":  true,
		"GBP":  true,
		"JPY":  true,
		"XYZ":  true, // well-formed, unsupported: the API's job to reject
		"":     false,
		"E":    false,
		"EU":   false,
		"EURO": false,
		"Eu1":  false,
		"E12":  false,
		"eur":  false, // lower-case fails here; Validate uppercases first
	} {
		if got := validCurrencyCode(code); got != want {
			t.Errorf("validCurrencyCode(%q) = %v, want %v", code, got, want)
		}
	}
}

// TestValidate_CurrencyNormalizedAndMalformedRejected: Validate applies the
// environment fallback, normalizes the value, and rejects a malformed code
// BEFORE the scan starts with a message naming --currency. A well-formed but
// unsupported code passes validation (the API rejects it during the scan).
func TestValidate_CurrencyNormalizedAndMalformedRejected(t *testing.T) {
	base := func() Scan {
		return Scan{
			Provider: "gcp",
			Project:  "my-project",
			Format:   "table",
			OutDir:   filepath.Join(t.TempDir(), "out"),
		}
	}

	// Well-formed code is normalized, never rejected.
	cfg := base()
	cfg.Currency = "eur"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with a well-formed currency must pass: %v", err)
	}
	if cfg.Currency != "EUR" {
		t.Fatalf("Validate left Currency = %q, want normalized EUR", cfg.Currency)
	}

	// A well-formed but unsupported code passes validation — the API rejects
	// it later, and the scan surfaces that error naming the currency.
	cfg = base()
	cfg.Currency = "XYZ"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with an unsupported-but-well-formed code must pass (the API rejects it): %v", err)
	}

	// A malformed code fails BEFORE the scan starts, naming --currency.
	for _, bad := range []string{"E", "EUR1", "eur$", "12"} {
		cfg = base()
		cfg.Currency = bad
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("Validate with malformed currency %q must fail", bad)
		}
		if !strings.Contains(err.Error(), "--currency") {
			t.Errorf("malformed-currency error %q must name the --currency flag", err)
		}
	}
}

// TestValidate_CurrencyEnvFallback: with no --currency flag, TELLURY_CURRENCY
// is read and normalized by Validate, and the flag still wins when both are
// set.
func TestValidate_CurrencyEnvFallback(t *testing.T) {
	cfg := Scan{
		Provider: "gcp",
		Project:  "my-project",
		Format:   "table",
		OutDir:   filepath.Join(t.TempDir(), "out"),
	}
	t.Setenv(CurrencyEnvVar, "gbp")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Currency != "GBP" {
		t.Fatalf("Validate read TELLURY_CURRENCY = %q, want normalized GBP", cfg.Currency)
	}

	cfg2 := Scan{
		Provider: "gcp",
		Project:  "my-project",
		Format:   "table",
		OutDir:   filepath.Join(t.TempDir(), "out"),
		Currency: "JPY",
	}
	t.Setenv(CurrencyEnvVar, "GBP")
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("Validate (flag + env): %v", err)
	}
	if cfg2.Currency != "JPY" {
		t.Fatalf("Validate kept Currency = %q, want JPY (flag beats env)", cfg2.Currency)
	}
}
