# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and `tellury`
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the major
version is `0`, the CLI surface and the rule interface may change between minor releases.

## [Unreleased]

## [0.2.0] — 2026-08-14

Five image rules, and the eight defects that validating them against real cloud
resources uncovered.

### Added

- **Five image rules**, bringing the catalogue to sixteen. Machine and custom images are a
  classic accumulation: a pipeline creates them, nothing deletes them, and the storage bills
  forever.
  - `unused_ami` (AWS) — an AMI no instance, launch template, launch configuration or fleet
    references.
  - `orphaned_ami_snapshot` (AWS) — the trap behind it. Deregistering an AMI does **not**
    delete its EBS snapshots; they stop appearing under AMIs in the console and keep billing.
  - `unused_custom_image` (GCP) — a custom image no instance template references.
  - `old_machine_image` (GCP) — a machine image past the retention window.
  - `unused_gallery_image_version` (Azure) — a Compute Gallery version no VM or scale set
    references.
- `pricing.SourceEquivalentSKU`, a `price_source` provenance for a figure answered by an
  equivalent SKU rather than the exact one asked for. It appears today only where GCP
  publishes no multi-region custom-image SKU.
- The README's rule tables are now per-provider, and a test pins them to the registry — they
  had drifted five rules behind it.

### Fixed

- **GCP image SKU tokens matched nothing.** The catalogue's resource groups are
  `StorageImage` and `MachineImage`; the code queried `ImageStorage` and
  `MachineImageStorage`. A wrong SKU token does not fail — every image resolved no price and
  skipped silently. Now pinned against a recorded catalogue response.
- **GCP custom images could not be priced or sized.** `archiveSizeBytes` is published only
  for images imported from an archive, so disk-sourced images had no size; and `StorageImage`
  publishes no multi-region SKU, while GCP defaults new images to a multi-region. Both are
  handled, and both bases are stated in the finding's evidence.
- **A live price could report itself as coming from a fixture.** Provenance was keyed by the
  region asked for while rules render evidence with the region returned; where those differ
  the lookup missed. Fixed in all three pricers.
- **Azure gallery image versions were priced against managed-disk tiers** (which match no
  meter) instead of snapshot meters, and their size was read from a field Resource Graph
  never returns.
- **An empty Azure compute inventory was treated as zero references** rather than as an
  untrusted one. Resource Graph omits rows an identity cannot read without failing, so "no
  VMs" and "no access to VMs" were indistinguishable — and an in-use gallery image could
  have been reported as unused.
- **AWS snapshot pricing indexed five GB-month products**, last one winning. Only the
  standard EBS snapshot rate is indexed now.
- `orphaned_ami_snapshot` emitted three evidence keys twice and dropped `price_source`
  entirely.

### Documentation

- Azure's least-privilege custom role was missing `virtualMachines/read`, `skus/read` and
  `Insights/metrics/read`. GCP's setup guide now documents `roles/compute.viewer`, needed by
  `unused_custom_image` for instance-template references. Tests now fail when the setup
  guides omit a permission the code calls — a missing permission is otherwise invisible,
  surfacing as a clean scan rather than an error.

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

[Unreleased]: https://github.com/TypeOneLabs/tellury/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/TypeOneLabs/tellury/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/TypeOneLabs/tellury/releases/tag/v0.1.0
