package config

import (
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	_ "github.com/TypeOneLabs/tellury/pkg/cloud/gcp" // registers TELLURY_GCP_* via init()
)

// otherProviderName is a synthetic provider registered only inside this test
// file, so we can assert that a scope declared for it is never picked up while
// resolving scopes for "gcp".
const otherProviderName = "other-test-provider"

func init() {
	// Deliberately reuses the same dimension name ("project") that gcp
	// registers, but points it at a different flag and environment variable.
	// If resolveScope ever stopped keying its lookup by provider first, this
	// would leak into the gcp assertions below and the test would fail.
	cloud.RegisterScopes(otherProviderName,
		cloud.ScopeVar{Name: scopeProject, Flag: "other-project", EnvVar: "TELLURY_OTHER_PROJECT"},
	)
}

// A non-empty flag always wins, even when the provider's env var is also set.
func TestResolveScope_FlagBeatsEnv(t *testing.T) {
	t.Setenv("TELLURY_GCP_PROJECT", "env-project")

	if got := resolveScope("gcp", scopeProject, "flag-project"); got != "flag-project" {
		t.Fatalf("resolveScope() = %q, want flag value to win", got)
	}
}

// With the flag empty, the provider's declared env var is read.
func TestResolveScope_EnvUsedWhenFlagAbsent(t *testing.T) {
	t.Setenv("TELLURY_GCP_PROJECT", "env-project")

	if got := resolveScope("gcp", scopeProject, ""); got != "env-project" {
		t.Fatalf("resolveScope() = %q, want env fallback %q", got, "env-project")
	}
}

// Neither flag nor env set yields empty, not a GCP-shaped default.
func TestResolveScope_EmptyFlagAndEnv(t *testing.T) {
	t.Setenv("TELLURY_GCP_PROJECT", "")

	if got := resolveScope("gcp", scopeProject, ""); got != "" {
		t.Fatalf("resolveScope() = %q, want empty", got)
	}
}

// The core seam regression: neither provider's variable may leak into the
// other's resolution, even though both declare a scope named "project".
func TestResolveScope_OtherProviderScopeNotPickedUp(t *testing.T) {
	t.Setenv("TELLURY_GCP_PROJECT", "gcp-value")
	t.Setenv("TELLURY_OTHER_PROJECT", "other-value")

	if got := resolveScope("gcp", scopeProject, ""); got != "gcp-value" {
		t.Fatalf("resolveScope(gcp) = %q, want %q — must not read TELLURY_OTHER_PROJECT", got, "gcp-value")
	}
	if got := resolveScope(otherProviderName, scopeProject, ""); got != "other-value" {
		t.Fatalf("resolveScope(other) = %q, want %q — must not read TELLURY_GCP_PROJECT", got, "other-value")
	}
}

// An unregistered provider has no variable to fall back to; it must not guess
// at a name or borrow another provider's.
func TestResolveScope_UnregisteredProviderYieldsEmpty(t *testing.T) {
	t.Setenv("TELLURY_GCP_PROJECT", "gcp-value")

	if got := resolveScope("does-not-exist", scopeProject, ""); got != "" {
		t.Fatalf("resolveScope(unregistered) = %q, want empty", got)
	}
}

// A registered provider that never declared a given dimension must not fall
// back to another provider's variable for it.
func TestResolveScope_ScopeNotAcceptedByProviderYieldsEmpty(t *testing.T) {
	t.Setenv("TELLURY_GCP_FOLDER", "gcp-folder")

	if got := resolveScope(otherProviderName, scopeFolder, ""); got != "" {
		t.Fatalf("resolveScope(other, folder) = %q, want empty", got)
	}
}

// The --progress flag beats TELLURY_PROGRESS; with the flag empty the env var
// is read; with both empty the default is "auto" (interactive terminals only).
func TestResolveProgress_FlagBeatsEnv(t *testing.T) {
	t.Setenv("TELLURY_PROGRESS", "off")

	if got := resolveProgress("on"); got != "on" {
		t.Fatalf("resolveProgress(flag=on, env=off) = %q, want on (flag wins)", got)
	}
	if got := resolveProgress(""); got != "off" {
		t.Fatalf("resolveProgress(flag=empty, env=off) = %q, want off (env fallback)", got)
	}

	t.Setenv("TELLURY_PROGRESS", "")
	if got := resolveProgress(""); got != "auto" {
		t.Fatalf("resolveProgress(empty) = %q, want auto default", got)
	}
}

// Validate accepts every supported progress mode and rejects anything else as
// a usage error — before any scope/flag check that would otherwise fire first.
func TestScanValidate_ProgressMode(t *testing.T) {
	for _, mode := range []string{"auto", "on", "off"} {
		c := &Scan{Provider: "gcp", Project: "p", Progress: mode}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate(--progress %s) failed: %v", mode, err)
		}
	}
	c := &Scan{Provider: "gcp", Progress: "sometimes"}
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate(--progress sometimes) must fail")
	}
}
