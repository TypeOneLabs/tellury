package config

import (
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/TypeOneLabs/tellury/pkg/cloud/aws" // registers TELLURY_AWS_* via init()
	_ "github.com/TypeOneLabs/tellury/pkg/cloud/gcp" // registers TELLURY_GCP_* via init()
)

// TestValidate_ProviderInferredFromAWSFlag is the acceptance test for provider
// inference: `tellury scan --aws-account 123456789012` (no --provider) must
// select the AWS provider.
func TestValidate_ProviderInferredFromAWSFlag(t *testing.T) {
	cfg := &Scan{Account: "123456789012"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(--aws-account alone) failed: %v", err)
	}
	if cfg.Provider != "aws" {
		t.Fatalf("Provider = %q, want aws (inferred from --aws-account)", cfg.Provider)
	}
}

// TestValidate_ProviderInferredFromAWSFlags covers the other two AWS
// dimensions, including --aws-organization, which must NOT be misread as
// GCP's organization.
func TestValidate_ProviderInferredFromAWSFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Scan
	}{
		{"account", Scan{Account: "123456789012"}},
		{"organizational_unit", Scan{OrganizationalUnit: "ou-abc"}},
		{"organization", Scan{AWSOrganization: "o-abc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate failed: %v", err)
			}
			if cfg.Provider != "aws" {
				t.Fatalf("Provider = %q, want aws (inferred from the AWS scope flag)", cfg.Provider)
			}
		})
	}
}

// TestValidate_ProviderInferredFromGCPFlag asserts a --gcp-* flag alone still
// selects GCP, exactly as the historical default did.
func TestValidate_ProviderInferredFromGCPFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Scan
	}{
		{"project", Scan{Project: "my-project"}},
		{"folder", Scan{Folder: "folders/123"}},
		{"organization", Scan{Organization: "organizations/456"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate failed: %v", err)
			}
			if cfg.Provider != "gcp" {
				t.Fatalf("Provider = %q, want gcp", cfg.Provider)
			}
		})
	}
}

// TestValidate_ProviderDefaultsToGCP pins the historical default: no scope
// flags anywhere, no scope environment variables, no --provider — the
// provider is gcp, exactly as it always was. The config is offline (a cache
// file names its own scope), so the empty scope is allowed and the whole
// validation path resolves the provider rather than failing on scope.
func TestValidate_ProviderDefaultsToGCP(t *testing.T) {
	cfg := &Scan{Format: "table", OutDir: filepath.Join(t.TempDir(), "out"), CacheFile: "snap.json"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with no scope flags failed: %v", err)
	}
	if cfg.Provider != "gcp" {
		t.Fatalf("Provider = %q, want the historical gcp default", cfg.Provider)
	}
}

// TestValidate_ProviderInferredFromEnv asserts the scope ENVIRONMENT variables
// also infer the provider when no flag does — TELLURY_AWS_ACCOUNT alone makes
// an AWS scan, TELLURY_GCP_PROJECT alone makes a GCP scan.
func TestValidate_ProviderInferredFromEnv(t *testing.T) {
	t.Run("aws env", func(t *testing.T) {
		t.Setenv("TELLURY_AWS_ACCOUNT", "123456789012")
		cfg := &Scan{Format: "table", OutDir: filepath.Join(t.TempDir(), "out")}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate with TELLURY_AWS_ACCOUNT failed: %v", err)
		}
		if cfg.Provider != "aws" || cfg.Account != "123456789012" {
			t.Fatalf("Provider/Account = %q/%q, want aws/123456789012", cfg.Provider, cfg.Account)
		}
	})
	t.Run("gcp env", func(t *testing.T) {
		t.Setenv("TELLURY_GCP_PROJECT", "my-project")
		cfg := &Scan{Format: "table", OutDir: filepath.Join(t.TempDir(), "out")}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate with TELLURY_GCP_PROJECT failed: %v", err)
		}
		if cfg.Provider != "gcp" || cfg.Project != "my-project" {
			t.Fatalf("Provider/Project = %q/%q, want gcp/my-project", cfg.Provider, cfg.Project)
		}
	})
}

// TestValidate_AWSFlagBeatsPassiveGCPEnv pins the precedence rule that keeps
// a passive environment variable from contradicting an explicit flag: with
// TELLURY_GCP_PROJECT in the shell and --aws-account on the command line, the
// flag selects AWS and the unrelated GCP variable is never consulted.
func TestValidate_AWSFlagBeatsPassiveGCPEnv(t *testing.T) {
	t.Setenv("TELLURY_GCP_PROJECT", "my-project")
	cfg := &Scan{Account: "123456789012", Format: "table", OutDir: filepath.Join(t.TempDir(), "out")}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(--aws-account with TELLURY_GCP_PROJECT set) failed: %v", err)
	}
	if cfg.Provider != "aws" {
		t.Fatalf("Provider = %q, want aws — the explicit AWS flag must win over a passive GCP env var", cfg.Provider)
	}
	if cfg.Project != "" {
		t.Fatalf("Project = %q, want empty — a GCP env var must never populate an AWS scan", cfg.Project)
	}
}

// TestValidate_TwoProviderConflictNamesBothFlags is the acceptance test for
// the two-provider failure: a --gcp-* flag and an --aws-* flag together fail,
// naming both providers' flag groups and telling the operator to pick one.
func TestValidate_TwoProviderConflictNamesBothFlags(t *testing.T) {
	cfg := &Scan{Project: "my-project", Account: "123456789012"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate with --gcp-project and --aws-account must fail")
	}
	msg := err.Error()
	for _, want := range []string{
		"both",
		"GCP",
		"AWS",
		"--gcp-project",
		"--gcp-folder",
		"--gcp-organization",
		"--aws-account",
		"--aws-organizational-unit",
		"--aws-organization",
		"pick one provider",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("conflict error must name %q; got: %s", want, msg)
		}
	}
}

// TestValidate_TwoProviderConflict_OrganizationDimensions is the case the
// shared-field design would have missed: --gcp-organization and
// --aws-organization are distinct flags bound to distinct config fields, so
// giving both must be caught as a two-provider conflict rather than one value
// silently overwriting the other.
func TestValidate_TwoProviderConflict_OrganizationDimensions(t *testing.T) {
	cfg := &Scan{Organization: "organizations/gcp-org", AWSOrganization: "o-aws-org"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate with both --gcp-organization and --aws-organization must fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--gcp-organization") || !strings.Contains(msg, "--aws-organization") {
		t.Errorf("conflict error must name both organization flags; got: %s", msg)
	}
	if !strings.Contains(msg, "pick one provider") {
		t.Errorf("conflict error must tell the operator to pick one provider; got: %s", msg)
	}
}

// TestValidate_TwoProviderConflict_ExplicitProviderContradicted asserts an
// explicit --provider contradicted by the other provider's scope flags is the
// same hard conflict: `--provider gcp --aws-account 123` must fail, never
// silently run a GCP scan.
func TestValidate_TwoProviderConflict_ExplicitProviderContradicted(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Scan
	}{
		{"provider gcp + aws flag", Scan{Provider: "gcp", Account: "123456789012"}},
		{"provider aws + gcp flag", Scan{Provider: "aws", Project: "my-project"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate(%+v) must fail as a two-provider conflict", tc.cfg)
			}
			if !strings.Contains(err.Error(), "pick one provider") {
				t.Errorf("error must tell the operator to pick one provider; got: %s", err)
			}
		})
	}
}

// TestValidate_TwoProvidersFromEnvConflict asserts the same conflict when both
// providers' scope environment variables are set and no flag decides.
func TestValidate_TwoProvidersFromEnvConflict(t *testing.T) {
	t.Setenv("TELLURY_GCP_PROJECT", "my-project")
	t.Setenv("TELLURY_AWS_ACCOUNT", "123456789012")
	cfg := &Scan{}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate with both TELLURY_GCP_PROJECT and TELLURY_AWS_ACCOUNT set must fail")
	} else if !strings.Contains(err.Error(), "pick one provider") {
		t.Errorf("error must tell the operator to pick one provider; got: %s", err)
	}
}

// TestValidate_AWSScopeRequiresExactlyOne asserts exactly one AWS scope
// dimension within the provider, mirroring the GCP rule: two AWS dimensions
// fail, none fail on the live path, and the offline (cache/fixture) paths
// stay exempt because a replay names its own scope.
func TestValidate_AWSScopeRequiresExactlyOne(t *testing.T) {
	if err := (&Scan{Provider: "aws", Account: "123", OrganizationalUnit: "ou-1"}).Validate(); err == nil {
		t.Fatal("two AWS scope dimensions must fail")
	}
	if err := (&Scan{Provider: "aws", Format: "table", OutDir: filepath.Join(t.TempDir(), "out")}).Validate(); err == nil {
		t.Fatal("a live AWS scan with no scope must fail")
	}
	if err := (&Scan{Provider: "aws", CacheFile: "snap.json", Format: "table", OutDir: filepath.Join(t.TempDir(), "out")}).Validate(); err != nil {
		t.Fatalf("an offline AWS replay with no scope must validate (the snapshot names its own scope): %v", err)
	}
	if err := (&Scan{Provider: "aws", Account: "123456789012"}).Validate(); err != nil {
		t.Fatalf("a single --aws-account must validate: %v", err)
	}
}

// TestValidate_UnknownProviderRejected pins the widening of the provider gate:
// "gcp" and "aws" are accepted, anything else is still an unknown provider.
func TestValidate_UnknownProviderRejected(t *testing.T) {
	for _, provider := range []string{"mars", "azure", "other-test-provider"} {
		cfg := &Scan{Provider: provider, Project: "p"}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate(--provider %s) must fail", provider)
		} else if !strings.Contains(err.Error(), "unknown provider") {
			t.Errorf("error for %q must say unknown provider; got: %s", provider, err)
		}
	}
}

// TestScan_ScopeRendersGCPUnchanged asserts config.Scan.Scope() still renders
// a GCP scope exactly as before the provider-neutral redesign.
func TestScan_ScopeRendersGCPUnchanged(t *testing.T) {
	cfg := &Scan{Provider: "gcp", Project: "my-project"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	sc := cfg.Scope()
	if got := sc.String(); got != "projects/my-project" {
		t.Errorf("Scope().String() = %q, want projects/my-project (unchanged)", got)
	}
	if got := sc.Parent(); got != "projects/my-project" {
		t.Errorf("Scope().Parent() = %q, want projects/my-project (unchanged)", got)
	}
}

// TestScan_ScopeRendersAWS asserts the AWS scope renders in AWS vocabulary:
// "accounts/<id>" for display, and "" for Parent (the AWS provider drives its
// API calls from the scope fields directly).
func TestScan_ScopeRendersAWS(t *testing.T) {
	cfg := &Scan{Provider: "aws", Account: "123456789012"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	sc := cfg.Scope()
	if got := sc.String(); got != "accounts/123456789012" {
		t.Errorf("Scope().String() = %q, want accounts/123456789012", got)
	}
	if got := sc.Parent(); got != "" {
		t.Errorf("Scope().Parent() = %q, want \"\" for AWS", got)
	}
}

// TestValidate_GCPScopeHintUnchanged pins the exactly-one-scope error message
// a live GCP scan with no scope produces — byte for byte the historical text.
func TestValidate_GCPScopeHintUnchanged(t *testing.T) {
	cfg := &Scan{Provider: "gcp", Format: "table", OutDir: filepath.Join(t.TempDir(), "out")}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a live GCP scan with no scope must fail")
	}
	want := "cloud: scope requires exactly one of project/folder/organization (set --gcp-project or TELLURY_GCP_PROJECT)"
	if err.Error() != want {
		t.Errorf("scope error = %q, want %q", err, want)
	}
}
