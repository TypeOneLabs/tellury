# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and `tellury`
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the major
version is `0`, the CLI surface and the rule interface may change between minor releases.

## [Unreleased]

## [0.1.0] — 2026-08-14

First release. `tellury` finds cloud resources nothing is using and prices each one per
month, across AWS, Azure and GCP.

### What it does

- **Eleven rules** across three clouds: unattached disks and volumes, unassociated
  addresses, snapshots past their retention window, buckets with no lifecycle policy, and
  overprovisioned instances on all three providers.
- **Scans any scope**: one account, subscription or project, or a whole organization.
  Findings roll up through the hierarchy each cloud actually has — organizations, folders
  and projects on GCP; organizations, OUs and accounts on AWS; tenants, management groups
  and subscriptions on Azure.
- **Live pricing only.** Cloud Billing Catalog for GCP, Price List for AWS, Retail Prices
  for Azure. There is no embedded price table: a rate that cannot be resolved makes the rule
  skip the resource rather than report a guessed figure.
- **Metrics where the cloud publishes them** — Cloud Monitoring, CloudWatch and Azure
  Monitor. A rule that cannot read a metric says so instead of assuming.
- **Deterministic and auditable.** Every rule resolves to true or false against data the
  provider returned; every finding carries its evidence and the arithmetic behind its
  figure; every resource that was not reported has a typed reason. `--at` pins the
  evaluation instant so age-based predicates reproduce.
- **Machine-readable output.** `--format json` carries `schema_version` and `scan_status`
  (`ok`, `no_resources`, `degraded`), so a CI step or an AI agent can tell "found nothing"
  from "could not look" — a distinction that is otherwise undecidable on Azure, where
  Resource Graph returns an empty result for resource types an identity cannot read.
- **Artifacts per scan**: a replayable graph snapshot, the findings as JSON, and a
  self-contained HTML report.

### Notes

- Everything `tellury` does is read-only. See the setup guide for each cloud for the
  minimum grant.
- Azure pricing needs no credentials at all — the Retail Prices API is public. Discovery
  does.
- Not included: Azure memory-aware rightsizing. Azure publishes guest memory as a platform
  metric, which neither AWS nor GCP do without an agent, and the backend reads it — but no
  rule declares it yet.

[Unreleased]: https://github.com/TypeOneLabs/tellury/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/TypeOneLabs/tellury/releases/tag/v0.1.0
