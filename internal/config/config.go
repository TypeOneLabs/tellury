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

	FailOnFindings bool
	ExplainSkips   bool
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

	c.Rules = cleanList(c.Rules)
	c.SkipRules = cleanList(c.SkipRules)
	return nil
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
