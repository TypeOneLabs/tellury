// Package azure is the Azure provider for tellury. This build owns the Azure
// scope surface and the provider shell: it registers the four Azure scope
// dimensions, validates them, and dispatches through the provider registry.
// Inventory ingestion, management-group traversal and live pricing arrive in
// later tasks; no cloud SDK client is constructed here.
//
// Its scope vocabulary is:
//
//	--azure-tenant / TELLURY_AZURE_TENANT
//	--azure-management-group / TELLURY_AZURE_MANAGEMENT_GROUP
//	--azure-subscription / TELLURY_AZURE_SUBSCRIPTION
//	--azure-resource-group / TELLURY_AZURE_RESOURCE_GROUP
//
// The names register through cloud.RegisterScopes, so the CLI and config carry
// Azure alongside GCP and AWS with no shared hardcoded flag list.
package azure

import "github.com/TypeOneLabs/tellury/pkg/cloud"

// ProviderName is the --provider value this package implements.
const ProviderName = "azure"

// Scope dimension names, as accepted internally by the CLI and config. These
// are provider-agnostic identity keys, NOT surface names: Azure renders each
// one as both a --azure-<scope> flag and a TELLURY_AZURE_<SCOPE> environment
// variable, resolved by provider through the registry so no other cloud's
// vocabulary leaks in.
const (
	ScopeTenant          = "tenant"
	ScopeManagementGroup = "management_group"
	ScopeSubscription    = "subscription"
	ScopeResourceGroup   = "resource_group"
)

// CLI flag names. Azure owns this vocabulary; internal/cli resolves them
// through cloud.ScopeFlag by provider rather than hardcoding an Azure-shaped
// flag set.
const (
	FlagTenant          = "azure-tenant"
	FlagManagementGroup = "azure-management-group"
	FlagSubscription    = "azure-subscription"
	FlagResourceGroup   = "azure-resource-group"
)

// Environment variables consulted as flag fallbacks. Azure owns this
// vocabulary; internal/config never hardcodes these names, it resolves them
// through cloud.RegisterScopes/ScopeEnvVar by provider.
const (
	EnvTenant          = "TELLURY_AZURE_TENANT"
	EnvManagementGroup = "TELLURY_AZURE_MANAGEMENT_GROUP"
	EnvSubscription    = "TELLURY_AZURE_SUBSCRIPTION"
	EnvResourceGroup   = "TELLURY_AZURE_RESOURCE_GROUP"
)

func init() {
	cloud.RegisterScopes(ProviderName,
		cloud.ScopeVar{Name: ScopeTenant, Flag: FlagTenant, EnvVar: EnvTenant},
		cloud.ScopeVar{Name: ScopeManagementGroup, Flag: FlagManagementGroup, EnvVar: EnvManagementGroup},
		cloud.ScopeVar{Name: ScopeSubscription, Flag: FlagSubscription, EnvVar: EnvSubscription},
		cloud.ScopeVar{Name: ScopeResourceGroup, Flag: FlagResourceGroup, EnvVar: EnvResourceGroup},
	)
}
