package gcp

import "github.com/TypeOneLabs/tellury/pkg/cloud"

// ProviderName is the --provider value this package implements.
const ProviderName = "gcp"

// Scope dimension names, as accepted internally by the CLI and config. These
// are provider-agnostic identity keys, NOT surface names: GCP renders each
// one as both a --gcp-<scope> flag and a TELLURY_GCP_<SCOPE> environment
// variable, and a future AWS provider will own its own surface names for the
// same dimensions. Neither internal/config nor internal/cli hardcodes a
// surface name — both resolve them through cloud.RegisterScopes/ScopeFlag/
// ScopeEnvVar by provider.
const (
	ScopeProject      = "project"
	ScopeFolder       = "folder"
	ScopeOrganization = "organization"
)

// CLI flag names. GCP owns this vocabulary; internal/cli resolves them
// through cloud.ScopeFlag by provider rather than hardcoding a GCP-shaped
// flag set. The --gcp- prefix keeps this tool's CLI honest on a multi-cloud
// surface: AWS will later add --aws-account/--aws-region without touching
// shared code.
const (
	FlagProject      = "gcp-project"
	FlagFolder       = "gcp-folder"
	FlagOrganization = "gcp-organization"
)

// Environment variables consulted as flag fallbacks. GCP owns this
// vocabulary; internal/config never hardcodes these names, it resolves them
// through cloud.RegisterScopes/ScopeEnvVar by provider.
const (
	EnvProject = "TELLURY_GCP_PROJECT"
	EnvFolder  = "TELLURY_GCP_FOLDER"
	EnvOrg     = "TELLURY_GCP_ORGANIZATION"
)

func init() {
	cloud.RegisterScopes(ProviderName,
		cloud.ScopeVar{Name: ScopeProject, Flag: FlagProject, EnvVar: EnvProject},
		cloud.ScopeVar{Name: ScopeFolder, Flag: FlagFolder, EnvVar: EnvFolder},
		cloud.ScopeVar{Name: ScopeOrganization, Flag: FlagOrganization, EnvVar: EnvOrg},
	)
}
