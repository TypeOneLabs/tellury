package cloud

import (
	"fmt"
	"sort"
	"sync"
)

// ScopeVar declares one scope dimension a provider accepts as scan input,
// along with the two names it presents to the outside world: the flag the CLI
// exposes and the environment variable consulted as a fallback when that flag
// is empty.
//
// Name is the provider-agnostic identity internal/config and the CLI resolve
// against (e.g. "project", "folder", "organization" for GCP; "account",
// "region" for a future AWS provider). Flag and EnvVar are the provider-owned
// surface names for that dimension: "gcp-project" / "TELLURY_GCP_PROJECT" now,
// "aws-account" / "TELLURY_AWS_ACCOUNT" later.
type ScopeVar struct {
	Name   string // provider-agnostic dimension key (never shown to the user)
	Flag   string // CLI flag name, e.g. "gcp-project"
	EnvVar string // environment variable fallback, e.g. "TELLURY_GCP_PROJECT"
}

type scopeEntry struct {
	flag   string
	envVar string
}

var (
	scopeMu       sync.RWMutex
	scopeRegistry = map[string]map[string]scopeEntry{} // provider -> scope name -> {flag, env}
)

// RegisterScopes declares the scope vocabulary for a single provider.
// Providers call this once, normally from their package's init(), so that
// they — not internal/config and not the CLI — own the mapping between a
// scope dimension, its CLI flag, and its environment variable. This is the
// seam that lets both TELLURY_<PROVIDER>_* and --<provider>-<scope> names be
// added for a new cloud (e.g. AWS's TELLURY_AWS_ACCOUNT / --aws-account) with
// no change anywhere outside that provider's own package.
//
// Panics on an empty provider name, an invalid ScopeVar, or a second
// registration for the same provider: those are build-time programming
// errors, not conditions a caller should need to handle at runtime.
func RegisterScopes(provider string, vars ...ScopeVar) {
	if provider == "" {
		panic("cloud: RegisterScopes with empty provider")
	}
	scopeMu.Lock()
	defer scopeMu.Unlock()
	if _, dup := scopeRegistry[provider]; dup {
		panic("cloud: duplicate scope registration for provider " + provider)
	}
	m := make(map[string]scopeEntry, len(vars))
	for _, v := range vars {
		if v.Name == "" || v.Flag == "" || v.EnvVar == "" {
			panic(fmt.Sprintf("cloud: invalid ScopeVar %+v for provider %s", v, provider))
		}
		m[v.Name] = scopeEntry{flag: v.Flag, envVar: v.EnvVar}
	}
	scopeRegistry[provider] = m
}

// ScopeEnvVar returns the environment variable a provider has declared for a
// named scope dimension, e.g. ScopeEnvVar("gcp", "project") ->
// ("TELLURY_GCP_PROJECT", true).
//
// ok is false when the provider is unknown, or is known but never declared
// that scope. Either way the caller must not fall back to guessing: this is
// exactly what keeps TELLURY_GCP_PROJECT from leaking into, say, an "aws"
// scan — the lookup is keyed by provider first, so another provider's
// variable is never even consulted.
func ScopeEnvVar(provider, name string) (string, bool) {
	scopeMu.RLock()
	defer scopeMu.RUnlock()
	e, ok := scopeRegistry[provider][name]
	if !ok {
		return "", false
	}
	return e.envVar, true
}

// ScopeFlag returns the CLI flag name a provider has declared for a named
// scope dimension, e.g. ScopeFlag("gcp", "project") -> ("gcp-project", true).
func ScopeFlag(provider, name string) (string, bool) {
	scopeMu.RLock()
	defer scopeMu.RUnlock()
	e, ok := scopeRegistry[provider][name]
	if !ok {
		return "", false
	}
	return e.flag, true
}

// Providers returns every provider that has registered a scope vocabulary,
// sorted by name. The CLI and config call this to discover, rather than
// hardcode, which scope flags a given build understands.
func Providers() []string {
	scopeMu.RLock()
	defer scopeMu.RUnlock()
	if len(scopeRegistry) == 0 {
		return nil
	}
	out := make([]string, 0, len(scopeRegistry))
	for p := range scopeRegistry {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ScopesFor returns the scope vocabulary a provider has declared, sorted by
// Name. Returns nil for an unregistered provider.
func ScopesFor(provider string) []ScopeVar {
	scopeMu.RLock()
	defer scopeMu.RUnlock()
	m := scopeRegistry[provider]
	if len(m) == 0 {
		return nil
	}
	out := make([]ScopeVar, 0, len(m))
	for name, e := range m {
		out = append(out, ScopeVar{Name: name, Flag: e.flag, EnvVar: e.envVar})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
