package cli

import (
	"testing"

	"github.com/spf13/pflag"

	"github.com/TypeOneLabs/tellury/internal/config"
	"github.com/TypeOneLabs/tellury/pkg/cloud"
	_ "github.com/TypeOneLabs/tellury/pkg/cloud/gcp" // registers GCP scopes via init()
)

// TestScopeFlagsRegisterFromRegistry asserts that the CLI's scope flag
// surface is driven entirely by cloud.ScopesFor — the same registry that owns
// scope environment variables — and not by a literal list hardcoded in the
// CLI. It holds the invariant this task exists for: flag names come from the
// provider package, so a future AWS registration (--aws-account,
// --aws-region) shows up here with no shared code change.
func TestScopeFlagsRegisterFromRegistry(t *testing.T) {
	cfg := &config.Scan{}
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	got := addScopeFlags(fs, "gcp", cfg)
	scopes := cloud.ScopesFor("gcp")
	if got != len(scopes) {
		t.Fatalf("addScopeFlags registered %d flags; want %d (one per declared GCP scope)", got, len(scopes))
	}

	// Every registry flag must be present on the flag set, with a matching
	// config field to bind to. The registry is the single source of truth for
	// the flag NAMES; this asserts the (dimension -> field) binding table
	// stays in lockstep with it.
	for _, sv := range scopes {
		fl := fs.Lookup(sv.Flag)
		if fl == nil {
			t.Fatalf("scope %q did not register flag --%s from the registry", sv.Name, sv.Flag)
		}
		switch sv.Name {
		case "project":
			_ = cfg.Project
		case "folder":
			_ = cfg.Folder
		case "organization":
			_ = cfg.Organization
		default:
			t.Fatalf("scope %q has no config field binding; add one alongside its registry entry", sv.Name)
		}
	}

	// The flag names must match the registry, never a hardcoded GCP shape.
	assertFlag(t, fs, "gcp-project")
	assertFlag(t, fs, "gcp-folder")
	assertFlag(t, fs, "gcp-organization")
	assertNoFlag(t, fs, "project")
	assertNoFlag(t, fs, "folder")
	assertNoFlag(t, fs, "organization")
}

// assertFlag fails unless the flag set exposes exactly the given flag.
func assertFlag(t *testing.T, fs *pflag.FlagSet, name string) {
	t.Helper()
	if fs.Lookup(name) == nil {
		t.Fatalf("expected --%s to be registered from the scope registry", name)
	}
}

// assertNoFlag fails if the flag set exposes a flag that should not exist.
// This is what stops the old un-prefixed, GCP-shaped flag set from creeping
// back in.
func assertNoFlag(t *testing.T, fs *pflag.FlagSet, name string) {
	t.Helper()
	if fs.Lookup(name) != nil {
		t.Fatalf("--%s must not be registered; scope flags must be provider-prefixed", name)
	}
}
