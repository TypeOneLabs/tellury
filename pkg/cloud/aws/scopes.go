// Package aws is the AWS provider for tellury. It ingests a single AWS account
// (--aws-account) through the EC2 API — EBS volumes and Elastic IPs, per
// region — and normalizes them into the provider-neutral graph under an
// account container node and per-account region nodes, exactly as GCP hangs
// resources off a region under a project. Credentials come from the standard
// AWS chain (config.LoadDefaultConfig); an offline fixture path replays
// captured EC2 data with no credentials at all.
//
// Its scope vocabulary — --aws-account, --aws-organizational-unit and
// --aws-organization, plus the TELLURY_AWS_ACCOUNT,
// TELLURY_AWS_ORGANIZATIONAL_UNIT and TELLURY_AWS_ORGANIZATION environment
// variables — registers through cloud.RegisterScopes, so the CLI and config
// carry AWS alongside GCP with no shared code change. This build implements
// --aws-account only; organizational-unit and organization scans arrive with
// Organizations traversal in a later step.
package aws

import "github.com/TypeOneLabs/tellury/pkg/cloud"

// ProviderName is the --provider value this package implements.
const ProviderName = "aws"

// init registers AWS's scope vocabulary into the shared provider registry. The
// scope dimension NAME "organization" intentionally mirrors GCP's — it is a
// provider-agnostic identity key — while the flag and environment-variable
// surface names stay AWS-owned (--aws-organization / TELLURY_AWS_ORGANIZATION),
// resolved by provider through the registry so neither cloud's vocabulary
// leaks into the other.
func init() {
	cloud.RegisterScopes(ProviderName,
		cloud.ScopeVar{Name: "account", Flag: "aws-account", EnvVar: "TELLURY_AWS_ACCOUNT"},
		cloud.ScopeVar{Name: "organizational_unit", Flag: "aws-organizational-unit", EnvVar: "TELLURY_AWS_ORGANIZATIONAL_UNIT"},
		cloud.ScopeVar{Name: "organization", Flag: "aws-organization", EnvVar: "TELLURY_AWS_ORGANIZATION"},
	)
}
