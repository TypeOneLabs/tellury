package cli

import (
	"testing"

	"github.com/spf13/pflag"

	"github.com/TypeOneLabs/tellury/internal/config"
	"github.com/TypeOneLabs/tellury/pkg/cloud"
	_ "github.com/TypeOneLabs/tellury/pkg/cloud/azure" // registers Azure scopes via init()
	_ "github.com/TypeOneLabs/tellury/pkg/cloud/gcp"   // registers GCP scopes via init()
)

// TestScopeFlagsRegisterFromRegistry asserts that the CLI's scope flag
// surface is driven entirely by cloud.ScopesFor — the same registry that owns
// scope environment variables — and not by a literal list hardcoded in the
// CLI. It holds the invariant this task exists for: flag names come from the
// provider package, so a future provider registration shows up here with no
// shared code change.
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

// TestAddAllScopeFlagsRegistersEveryProvider asserts that the CLI's full
// scope flag surface is driven by cloud.Providers(): GCP's --gcp-* flags,
// AWS's --aws-* flags and Azure's --azure-* flags are registered in one call,
// each bound to the provider's own config fields, with no literal flag list in
// the CLI. AWS and Azure are registered in this test binary through package
// imports (root.go and scan.go).
func TestAddAllScopeFlagsRegistersEveryProvider(t *testing.T) {
	cfg := &config.Scan{}
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	n := addAllScopeFlags(fs, cfg)
	wantFlags := map[string]bool{
		"gcp-project":             false,
		"gcp-folder":              false,
		"gcp-organization":        false,
		"aws-account":             false,
		"aws-organizational-unit": false,
		"aws-organization":        false,
		"azure-tenant":            false,
		"azure-management-group":  false,
		"azure-subscription":      false,
		"azure-resource-group":    false,
	}
	if n != len(wantFlags) {
		t.Fatalf("addAllScopeFlags registered %d flags; want %d (3 GCP + 3 AWS + 4 Azure)", n, len(wantFlags))
	}
	for name := range wantFlags {
		if fs.Lookup(name) == nil {
			t.Errorf("addAllScopeFlags must register --%s", name)
		}
	}

	// The AWS flags bind to AWS-owned config fields; --aws-organization is
	// deliberately distinct from GCP's --gcp-organization, so the CLI never
	// loses which provider's organization flag was set.
	if err := fs.Set("aws-account", "123456789012"); err != nil {
		t.Fatalf("Set(--aws-account): %v", err)
	}
	if cfg.Account != "123456789012" {
		t.Errorf("--aws-account must bind to cfg.Account; got %q", cfg.Account)
	}
	if err := fs.Set("aws-organizational-unit", "ou-abc"); err != nil {
		t.Fatalf("Set(--aws-organizational-unit): %v", err)
	}
	if cfg.OrganizationalUnit != "ou-abc" {
		t.Errorf("--aws-organizational-unit must bind to cfg.OrganizationalUnit; got %q", cfg.OrganizationalUnit)
	}
	if err := fs.Set("aws-organization", "o-abc"); err != nil {
		t.Fatalf("Set(--aws-organization): %v", err)
	}
	if cfg.AWSOrganization != "o-abc" {
		t.Errorf("--aws-organization must bind to cfg.AWSOrganization; got %q", cfg.AWSOrganization)
	}
	if cfg.Organization != "" {
		t.Errorf("--aws-organization must not write cfg.Organization; got %q", cfg.Organization)
	}
	if err := fs.Set("gcp-organization", "organizations/456"); err != nil {
		t.Fatalf("Set(--gcp-organization): %v", err)
	}
	if cfg.Organization != "organizations/456" {
		t.Errorf("--gcp-organization must bind to cfg.Organization; got %q", cfg.Organization)
	}
	if cfg.AWSOrganization != "o-abc" {
		t.Errorf("--gcp-organization must not clobber cfg.AWSOrganization; got %q", cfg.AWSOrganization)
	}

	// The Azure flags bind to Azure-owned config fields.
	if err := fs.Set("azure-tenant", "11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatalf("Set(--azure-tenant): %v", err)
	}
	if cfg.AzureTenant != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("--azure-tenant must bind to cfg.AzureTenant; got %q", cfg.AzureTenant)
	}
	if err := fs.Set("azure-management-group", "mg-abc"); err != nil {
		t.Fatalf("Set(--azure-management-group): %v", err)
	}
	if cfg.AzureManagementGroup != "mg-abc" {
		t.Errorf("--azure-management-group must bind to cfg.AzureManagementGroup; got %q", cfg.AzureManagementGroup)
	}
	if err := fs.Set("azure-subscription", "22222222-2222-2222-2222-222222222222"); err != nil {
		t.Fatalf("Set(--azure-subscription): %v", err)
	}
	if cfg.AzureSubscription != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("--azure-subscription must bind to cfg.AzureSubscription; got %q", cfg.AzureSubscription)
	}
	if err := fs.Set("azure-resource-group", "rg-1"); err != nil {
		t.Fatalf("Set(--azure-resource-group): %v", err)
	}
	if cfg.AzureResourceGroup != "rg-1" {
		t.Errorf("--azure-resource-group must bind to cfg.AzureResourceGroup; got %q", cfg.AzureResourceGroup)
	}
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
