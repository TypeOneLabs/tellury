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
// (--gcp-project, --aws-account, --azure-subscription) and environment
// variable (TELLURY_GCP_PROJECT, TELLURY_AWS_ACCOUNT,
// TELLURY_AZURE_SUBSCRIPTION) via cloud.RegisterScopes. config never hardcodes
// a surface name or a provider-shaped scope set — it only knows these
// dimensions the CLI exposes, and asks the selected provider (through
// cloud.ScopeFlag/ScopeEnvVar, keyed by provider) what each one's flag and
// variable are actually called. "organization" is shared by both GCP and AWS:
// each owns its own surface names for it, resolved by provider.
const (
	scopeProject            = "project"
	scopeFolder             = "folder"
	scopeOrganization       = "organization"
	scopeAccount            = "account"
	scopeOrganizationalUnit = "organizational_unit"
	scopeTenant             = "tenant"
	scopeManagementGroup    = "management_group"
	scopeSubscription       = "subscription"
	scopeResourceGroup      = "resource_group"
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
	// GCP scope dimensions. Exactly one is set for a GCP scan (the offline
	// replay paths are exempt: a cache file or fixture names its own scope).
	Project      string
	Folder       string
	Organization string

	// AWS scope dimensions, mirroring GCP's three levels structurally.
	// Account and OrganizationalUnit parallel Project and Folder.
	// AWSOrganization is AWS's own organization field: it is deliberately a
	// DISTINCT field rather than sharing GCP's Organization, so the CLI never
	// loses which provider's --*-organization flag set it — which is what lets
	// the provider conflict check name both flags.
	Account            string
	OrganizationalUnit string
	AWSOrganization    string

	// AWSRoleName is the name of the IAM role to assume in each member
	// account during an organization or OU scan. Defaults to
	// OrganizationAccountAccessRole (the AWS Organizations convention). A
	// single-account scan (--aws-account) ignores it — the caller's own
	// credentials are used directly.
	AWSRoleName string

	// AWSRegions narrows an AWS scan to an explicit region list (--aws-regions).
	// Empty means the default: every region enabled for the account, resolved
	// by ec2:DescribeRegions (or an offline fixture's regions on a replay).
	// The values are canonicalised by the AWS provider — an availability-zone
	// form like "us-east-1a" becomes its region "us-east-1" — so an operator
	// can pass either spelling.
	AWSRegions []string

	// Azure scope dimensions. Tenant, ManagementGroup and Subscription are
	// the three top-level dimensions (exactly one is set). ResourceGroup is
	// not a dimension by itself: it is an optional filter that is meaningful
	// only with Subscription.
	AzureTenant          string
	AzureManagementGroup string
	AzureSubscription    string
	AzureResourceGroup   string

	Provider string

	Rules     []string
	SkipRules []string

	Format string
	Sort   string

	WindowDays    int
	MinWaste      float64
	MinConfidence float64

	CacheFile string
	Fixture   []string

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

// Scope renders the cloud scope, building the provider-specific block that
// matches the selected provider. Callers must run Validate first so Provider
// is resolved (explicit, inferred from scope flags, or the gcp default).
func (c Scan) Scope() cloud.Scope {
	switch c.Provider {
	case "gcp":
		return cloud.Scope{
			Provider: "gcp",
			GCP: &cloud.GCPScope{
				Project:      c.Project,
				Folder:       c.Folder,
				Organization: c.Organization,
			},
		}
	case "aws":
		return cloud.Scope{
			Provider: "aws",
			AWS: &cloud.AWSScope{
				Account:            c.Account,
				OrganizationalUnit: c.OrganizationalUnit,
				Organization:       c.AWSOrganization,
			},
		}
	case "azure":
		return cloud.Scope{
			Provider: "azure",
			Azure: &cloud.AzureScope{
				Tenant:          c.AzureTenant,
				ManagementGroup: c.AzureManagementGroup,
				Subscription:    c.AzureSubscription,
				ResourceGroup:   c.AzureResourceGroup,
			},
		}
	}
	return cloud.Scope{Provider: c.Provider}
}

// Offline reports whether the scan can run without cloud credentials.
func (c Scan) Offline() bool { return c.CacheFile != "" || len(c.Fixture) > 0 }

// Validate applies environment fallbacks and enforces every invariant the
// pipeline downstream is allowed to assume.
func (c *Scan) Validate() error {
	// Provider resolution happens first: the scope flags (and, failing them,
	// the scope environment variables) can name the provider, and a scan that
	// mixes one provider's scope flags with another's must fail here — before
	// any rule selection, any directory creation, any cloud client.
	if err := c.resolveProvider(); err != nil {
		return err
	}
	if !cloud.ProviderRegistered(c.Provider) {
		return cloud.UnknownProviderError(c.Provider)
	}

	// Progress mode is a flag/usage error and is checked before anything
	// else, so a bad --progress value is rejected even when the scan scope
	// would also be invalid.
	c.Progress = resolveProgress(c.Progress)
	if !validProgressMode(c.Progress) {
		return fmt.Errorf("invalid --progress %q (want auto|on|off)", c.Progress)
	}

	// Scope environment fallbacks, resolved only for the selected provider: a
	// scope belonging to a different provider is never consulted.
	switch c.Provider {
	case "gcp":
		c.Project = resolveScope(c.Provider, scopeProject, c.Project)
		c.Folder = resolveScope(c.Provider, scopeFolder, c.Folder)
		c.Organization = resolveScope(c.Provider, scopeOrganization, c.Organization)
	case "aws":
		c.Account = resolveScope(c.Provider, scopeAccount, c.Account)
		c.OrganizationalUnit = resolveScope(c.Provider, scopeOrganizationalUnit, c.OrganizationalUnit)
		c.AWSOrganization = resolveScope(c.Provider, scopeOrganization, c.AWSOrganization)
	case "azure":
		c.AzureTenant = resolveScope(c.Provider, scopeTenant, c.AzureTenant)
		c.AzureManagementGroup = resolveScope(c.Provider, scopeManagementGroup, c.AzureManagementGroup)
		c.AzureSubscription = resolveScope(c.Provider, scopeSubscription, c.AzureSubscription)
		c.AzureResourceGroup = resolveScope(c.Provider, scopeResourceGroup, c.AzureResourceGroup)
	}

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
	// scan surfaces plainly (naming the currency).
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

// resolveProvider settles which provider a scan targets, and rejects a
// multi-provider ambiguity as a usage error before any work is done.
// Precedence:
//
//  1. An explicit --provider value (non-empty) is authoritative. Scope flags
//     belonging to another provider are then a hard conflict.
//  2. Otherwise the scope FLAGS infer the provider. Flags from two or more
//     providers are a conflict.
//  3. Otherwise the scope ENVIRONMENT variables infer the provider, with the
//     same logic.
//  4. Otherwise the historical default: gcp.
//
// Mixed evidence is never guessed at. An explicit flag always wins over a
// passive environment variable, so an operator whose shell carries
// TELLURY_GCP_PROJECT can still run `tellury scan --aws-account 123` — the
// AWS flag selects AWS, and the unrelated GCP variable is never consulted.
//
// The set of providers is discovered from cloud.Providers(), not from a
// literal gcp/aws/azure chain, so adding a fourth cloud that registers scope
// vocabulary widens conflict detection without a code change here.
func (c *Scan) resolveProvider() error {
	if c.Provider != "" {
		if !cloud.ProviderRegistered(c.Provider) {
			// Let Validate report the unknown provider; there are no
			// registered scope flags against which to check a conflict.
			return nil
		}
		others := c.otherProvidersSetByFlags(c.Provider)
		switch len(others) {
		case 0:
			return nil
		case 1:
			return providerConflictError(c.Provider, others[0])
		default:
			return providerConflictListError(append([]string{c.Provider}, others...))
		}
	}

	flags := c.providersSetByFlags()
	switch len(flags) {
	case 0:
	case 1:
		c.Provider = flags[0]
		return nil
	default:
		return providerConflictErrorForSet(flags)
	}

	envs := c.providersSetByEnv()
	switch len(envs) {
	case 0:
	case 1:
		c.Provider = envs[0]
		return nil
	default:
		return providerConflictErrorForSet(envs)
	}

	c.Provider = "gcp"
	return nil
}

// providersSetByFlags returns every registered provider with at least one
// non-empty scope flag value on the config struct, sorted by provider name.
func (c *Scan) providersSetByFlags() []string {
	return c.providersWhere(func(provider string) bool { return c.providerFlagsSet(provider) })
}

// providersSetByEnv returns every registered provider with at least one
// declared scope environment variable set, sorted by provider name.
func (c *Scan) providersSetByEnv() []string {
	return c.providersWhere(func(provider string) bool { return c.providerEnvSet(provider) })
}

// providersWhere filters the registered provider list through pred.
func (c *Scan) providersWhere(pred func(string) bool) []string {
	providers := cloud.Providers()
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		if pred(provider) {
			out = append(out, provider)
		}
	}
	return out
}

// otherProvidersSetByFlags returns the registered providers, other than
// provider, that have a scope flag set. The explicit-provider conflict check
// uses this so `--provider azure --gcp-project p` names both AZURE and GCP.
func (c *Scan) otherProvidersSetByFlags(provider string) []string {
	providers := c.providersSetByFlags()
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		if p != provider {
			out = append(out, p)
		}
	}
	return out
}

// providerFlagsSet reports whether any of the provider's scope dimensions
// carries a non-empty FLAG value on the config struct. Environment variables
// are deliberately not consulted here: they are the lower inference tier and
// must never be contradicted by a flag from the other provider.
func (c *Scan) providerFlagsSet(provider string) bool {
	switch provider {
	case "gcp":
		return c.Project != "" || c.Folder != "" || c.Organization != ""
	case "aws":
		return c.Account != "" || c.OrganizationalUnit != "" || c.AWSOrganization != ""
	case "azure":
		return c.AzureTenant != "" || c.AzureManagementGroup != "" || c.AzureSubscription != "" || c.AzureResourceGroup != ""
	}
	return false
}

// providerEnvSet reports whether any of the provider's declared scope
// environment variables is set. It resolves the variable names from the
// registry, keyed by provider, so one cloud's variables can never be
// misread as another's.
func (c *Scan) providerEnvSet(provider string) bool {
	for _, sv := range cloud.ScopesFor(provider) {
		if os.Getenv(sv.EnvVar) != "" {
			return true
		}
	}
	return false
}

// providerConflictErrorForSet renders a two-provider conflict in the
// historical "both X and Y scope flags" form, and a three-or-more provider
// conflict in the general multi-provider form.
func providerConflictErrorForSet(providers []string) error {
	if len(providers) == 2 {
		return providerConflictError(providers[0], providers[1])
	}
	return providerConflictListError(providers)
}

// providerConflictError renders the two-provider usage error. It names both
// providers' scope flag groups — resolved from the registry, never a literal
// GCP-shaped list — and tells the operator to pick one provider.
func providerConflictError(providerA, providerB string) error {
	return fmt.Errorf("both %s and %s scope flags are set (%s and %s); pick one provider",
		strings.ToUpper(providerA), strings.ToUpper(providerB),
		scopeFlagList(providerA), scopeFlagList(providerB))
}

// providerConflictListError renders a three-or-more provider conflict. It is
// deliberately distinct from the two-provider message so the common case
// keeps its historical wording while a three-way conflict still names every
// provider involved.
func providerConflictListError(providers []string) error {
	groups := make([]string, 0, len(providers))
	for _, provider := range providers {
		groups = append(groups, scopeFlagList(provider))
	}
	return fmt.Errorf("scope flags from multiple providers are set (%s); pick one provider",
		strings.Join(groups, " and "))
}

// scopeFlagList renders the "--flag/--flag/--flag" group a provider declares
// for its scope dimensions, resolved from the registry and sorted by
// dimension name for determinism.
func scopeFlagList(provider string) string {
	scopes := cloud.ScopesFor(provider)
	names := make([]string, 0, len(scopes))
	for _, sv := range scopes {
		names = append(names, "--"+sv.Flag)
	}
	return strings.Join(names, "/")
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
// usable scope dimension, so an error message tells the operator what to
// actually type. It prefers the dimension each provider treats as its
// day-to-day owner: GCP's "project", AWS's "account", Azure's
// "subscription". If the provider does not declare that dimension, it falls
// back to the provider's first declared dimension. Names come from the
// registry, never a literal list.
func scopeHint(provider string) string {
	name := preferredScopeName(provider)
	if _, ok := cloud.ScopeFlag(provider, name); !ok {
		scopes := cloud.ScopesFor(provider)
		if len(scopes) == 0 {
			return "--" + "?"
		}
		name = scopes[0].Name
	}
	flag, ok := cloud.ScopeFlag(provider, name)
	if !ok {
		return "--" + "?"
	}
	hint := "--" + flag
	if envVar, ok := cloud.ScopeEnvVar(provider, name); ok {
		hint += " or " + envVar
	}
	return hint
}

// preferredScopeName is the dimension a missing-scope hint should prefer for
// each provider. GCP keeps its historical project hint; AWS resolves to
// account; Azure resolves to subscription (the design's scopeHint preference).
func preferredScopeName(provider string) string {
	switch provider {
	case "gcp":
		return scopeProject
	case "aws":
		return scopeAccount
	case "azure":
		return scopeSubscription
	default:
		return ""
	}
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
