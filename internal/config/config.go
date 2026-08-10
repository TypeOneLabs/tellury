// Package config turns flags plus environment fallbacks into a validated
// struct. Twenty lines of os.Getenv beats a configuration framework.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/output"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// Scope dimension names. These are provider-agnostic identity keys, NOT
// surface names: a provider maps each one it accepts to its own CLI flag
// (--gcp-project) and environment variable (TELLURY_GCP_PROJECT) via
// cloud.RegisterScopes. config never hardcodes a surface name or a
// GCP-shaped scope set — it only knows these three dimensions the CLI
// exposes, and asks the selected provider (through cloud.ScopeFlag/
// ScopeEnvVar, keyed by provider) what each one's flag and variable are
// actually called.
const (
	scopeProject      = "project"
	scopeFolder       = "folder"
	scopeOrganization = "organization"
)

// Metric window bounds. Below 7 days the p95 sample floor cannot be met, above
// 30 days Cloud Monitoring's hourly retention starts to thin out.
const (
	MinWindowDays     = 7
	MaxWindowDays     = 30
	DefaultWindowDays = 14
)

// DefaultOutDir is the directory a scan writes its artifacts into when
// --out-dir is not given. Created on demand; a scan chooses not to scatter
// files across the working directory.
const DefaultOutDir = "tellury-out"

// CurrencyEnvVar is the environment-variable fallback for the --currency
// flag, following the flag-beats-environment convention every other scan
// option uses. Currency is provider-agnostic (not a cloud scope), so it is
// read directly here rather than through the provider scope registry.
const CurrencyEnvVar = "TELLURY_CURRENCY"

// ProgressEnvVar is the environment-variable fallback for the --progress
// flag, following the same flag-beats-environment convention as every other
// scan option. Progress is provider-agnostic, so it is read directly here
// rather than through the provider scope registry.
const ProgressEnvVar = "TELLURY_PROGRESS"

// Scan is the fully-resolved `tellury scan` configuration.
type Scan struct {
	Project      string
	Folder       string
	Organization string
	Provider     string

	Rules     []string
	SkipRules []string

	Format string
	Sort   string

	WindowDays    int
	MinWaste      float64
	MinConfidence float64

	CacheFile string
	Fixture   []string
	PriceFile string

	OutDir         string
	FailOnFindings bool
	ExplainSkips   bool

	// Progress is the scan progress mode: "auto" (default — report phase
	// progress only on an interactive terminal), "on" (always, degrading to
	// plain periodic lines off a terminal), or "off" (never). It is resolved
	// from --progress with TELLURY_PROGRESS as the environment fallback, and
	// controls a status channel on stderr that is independent of --log-level
	// and never touches stdout. See internal/cli/progress.go for the
	// reporter itself.
	Progress string

	// Currency is the ISO 4217 code the scan's prices are expressed in. ""
	// means "not requested": the tool auto-detects a billing account's
	// currency, and falls back to USD when detection cannot answer. A
	// non-empty value (--currency or TELLURY_CURRENCY) overrides detection.
	Currency string
}

// Scope renders the cloud scope.
func (c Scan) Scope() cloud.Scope {
	return cloud.Scope{Project: c.Project, Folder: c.Folder, Organization: c.Organization}
}

// Offline reports whether the scan can run without cloud credentials.
func (c Scan) Offline() bool { return c.CacheFile != "" || len(c.Fixture) > 0 }

// Validate applies environment fallbacks and enforces every invariant the
// pipeline downstream is allowed to assume.
func (c *Scan) Validate() error {
	if c.Provider == "" {
		c.Provider = "gcp"
	}
	if c.Provider != "gcp" {
		return cloud.UnknownProviderError(c.Provider)
	}

	// Progress mode is a flag/usage error and is checked before anything
	// else, so a bad --progress value is rejected even when the scan scope
	// would also be invalid.
	c.Progress = resolveProgress(c.Progress)
	if !validProgressMode(c.Progress) {
		return fmt.Errorf("invalid --progress %q (want auto|on|off)", c.Progress)
	}

	c.Project = resolveScope(c.Provider, scopeProject, c.Project)
	c.Folder = resolveScope(c.Provider, scopeFolder, c.Folder)
	c.Organization = resolveScope(c.Provider, scopeOrganization, c.Organization)

	// A cache file or fixture already names its own scope; a live scan does not.
	if err := c.Scope().Validate(); err != nil {
		if !c.Offline() {
			return fmt.Errorf("%w (set %s)", err, scopeHint(c.Provider))
		}
	}

	if _, err := output.For(c.Format); err != nil {
		return err
	}
	if c.Sort == "" {
		c.Sort = string(rules.SortWaste)
	}
	if _, err := rules.ParseSortOrder(c.Sort); err != nil {
		return err
	}

	if c.WindowDays == 0 {
		c.WindowDays = DefaultWindowDays
	}
	if c.WindowDays < MinWindowDays || c.WindowDays > MaxWindowDays {
		return fmt.Errorf("invalid --window %d (valid range %d-%d days)",
			c.WindowDays, MinWindowDays, MaxWindowDays)
	}

	if c.MinConfidence < 0 || c.MinConfidence > 1 {
		return fmt.Errorf("invalid --min-confidence %.2f (want 0..1)", c.MinConfidence)
	}
	if c.MinWaste < 0 {
		return fmt.Errorf("invalid --min-waste %.2f (want >= 0)", c.MinWaste)
	}

	// Currency: flag beats TELLURY_CURRENCY, then the value is normalized to
	// an uppercase 3-letter ISO 4217 code. A malformed code fails here, before
	// the scan starts — never after ingestion. A well-formed but unsupported
	// code passes this check and fails at the Cloud Billing API, which the
	// scan surfaces plainly (naming the currency) rather than silently
	// falling back to the USD embedded table.
	c.Currency = resolveCurrency(c.Currency)
	if c.Currency != "" && !validCurrencyCode(c.Currency) {
		return fmt.Errorf("invalid --currency %q: want a 3-letter ISO 4217 currency code such as EUR or USD", c.Currency)
	}

	if c.OutDir == "" {
		c.OutDir = DefaultOutDir
	}

	c.Rules = cleanList(c.Rules)
	c.SkipRules = cleanList(c.SkipRules)
	return nil
}

// resolveProgress applies the flag-beats-environment convention to the
// --progress flag: a non-empty flag wins, otherwise TELLURY_PROGRESS is read,
// and an empty result defaults to "auto" (report only on an interactive
// terminal). The value is normalized to lowercase.
func resolveProgress(flagValue string) string {
	v := flagValue
	if v == "" {
		v = os.Getenv(ProgressEnvVar)
	}
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "auto"
	}
	return v
}

// validProgressMode reports whether mode is one of the supported progress
// modes: "auto" (interactive terminals only), "on" (always, plain off a
// terminal), or "off" (never).
func validProgressMode(mode string) bool {
	switch mode {
	case "auto", "on", "off":
		return true
	}
	return false
}

// resolveCurrency applies the flag-beats-environment convention to the
// --currency flag: a non-empty flag wins, otherwise TELLURY_CURRENCY is read.
// The result is normalized (trimmed, uppercased) so "eur" and " EUR " both
// resolve to "EUR" before validation.
func resolveCurrency(flagValue string) string {
	v := flagValue
	if v == "" {
		v = os.Getenv(CurrencyEnvVar)
	}
	return strings.ToUpper(strings.TrimSpace(v))
}

// validCurrencyCode reports whether code is a well-formed ISO 4217 currency
// code: exactly three ASCII uppercase letters. It deliberately does NOT check
// that the code exists — that is the Cloud Billing API's job, and an
// unsupported-but-well-formed code must surface as an API error naming the
// currency rather than being rejected here.
func validCurrencyCode(code string) bool {
	if len(code) != 3 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return false
		}
	}
	return true
}

// scopeHint renders the flag/env pair the provider publishes for its first
// scope dimension, so an error message tells the operator what to actually
// type. It resolves the names from the registry rather than guessing.
func scopeHint(provider string) string {
	hint := "--" + "?"
	if flag, ok := cloud.ScopeFlag(provider, scopeProject); ok {
		hint = "--" + flag
	}
	if envVar, ok := cloud.ScopeEnvVar(provider, scopeProject); ok {
		hint += " or " + envVar
	}
	return hint
}

// resolveScope returns flagValue if set, otherwise the value of the
// environment variable the given provider declares for the named scope
// dimension. If the provider is unregistered, or is registered but does not
// accept this scope dimension at all, there is no variable to fall back to
// and the empty flag value is returned unchanged — a scope belonging to a
// different provider is never consulted, because the lookup is keyed by
// provider first.
func resolveScope(provider, name, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	envVar, ok := cloud.ScopeEnvVar(provider, name)
	if !ok {
		return ""
	}
	return os.Getenv(envVar)
}

// SortOrder returns the validated sort order.
func (c Scan) SortOrder() rules.SortOrder {
	o, err := rules.ParseSortOrder(c.Sort)
	if err != nil {
		return rules.SortWaste
	}
	return o
}

func cleanList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
