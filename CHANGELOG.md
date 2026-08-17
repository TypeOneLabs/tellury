# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and `tellury`
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the major
version is `0`, the CLI surface and the rule interface may change between minor releases.

## [Unreleased]

## [0.3.0] — 2026-08-17

Region discovery is now a choice, organization scans work, and every documented
permission set is verified against a real identity.

### Added

- `--aws-use-resource-explorer`. Resource Explorer region narrowing is now opt-in.
  Three modes, in precedence order: `--aws-regions` (exact regions, fastest), the flag
  (fast, may under-report on early runs, creates indexes), and the default (every enabled
  region, complete on the first run, **no Resource Explorer calls and no writes**).
- Per-account region coverage in the summary and the JSON (`regions_searchable` /
  `regions_enabled`), so a brand-new account is distinguishable from a genuinely clean one.

### Changed

- **The default AWS scan sweeps every enabled region again.** Slower than 0.2.1 — about 70s
  versus 15s on a 17-region account — and complete on the first run. Pass `--aws-regions`
  for speed, or `--aws-use-resource-explorer` for the 0.2.1 behaviour.

### Fixed

- **Organization scans never reached a member account, in any release.** The org tree and
  `sts:AssumeRole` both succeeded; every member account then failed on its first API call
  with "Invalid Configuration: Missing Region", because the per-account client factories
  omitted the empty-region fallback the own-account factories have. Both now share one
  helper.
- The organization summary reported the last account's region count instead of the union
  across accounts.

### Documentation

- `docs/aws-setup.md`: which permissions each **scope** needs, and that the per-account read
  permissions belong in each **member account** through the assumed role, not in the caller.
  Which permissions each **region mode** needs, that the Resource Explorer flag writes
  (searching creates an index), and that coverage under it builds over several runs.
- An ARN-scoped `sts:AssumeRole` must match `--aws-role-name`. The example policy hardcodes
  `OrganizationAccountAccessRole`, so a custom role name is reported as an unreachable member
  account when the caller's own policy is what blocked it.
- Every documented permission set is now verified by scanning with a principal holding
  exactly that set: AWS account, organization and member-account roles; GCP with and without
  `roles/compute.viewer`; Azure at subscription, management-group and tenant scope.

## [0.2.1] — 2026-08-14

Region narrowing on AWS, which had never worked.

### Fixed

- **Every AWS scan swept every enabled region.** Resource Explorer was meant to
  narrow the list and never did — ~70s against an account whose resources sat in
  one region, now ~15s. Three defects, each measured against the live API:
  - Resource Explorer has no `OR`. Several types were asked for as
    `resourcetype:A OR resourcetype:B`; terms are ANDed and `OR` is treated as a
    literal one, so the combined query matched **nothing** — not A, not B. It is
    now one query per type.
  - The resource type strings were CloudFormation aliases, not the values Search
    returns. Some resolve and some silently do not (`AWS::EC2::Image` → 0 where
    `ec2:image` → 1). `aws.ec2.instance` and `aws.ec2.snapshot` were not mapped
    at all.
  - A zero-result discovery fell through to the sweep with no log line, so the
    fallback was invisible even at `--log-level debug`.

### Changed

- **The EC2 `DescribeRegions` sweep is gone.** With an aggregator index, one
  query per type covers every indexed region; without one — most accounts — each
  enabled region is asked directly. No setup is required either way.
- **Un-indexed regions are now named in the output.** Resource Explorer only
  knows a region that has an index, and an un-indexed region answers
  `0 results, Complete: true` — a confident empty — while being searched creates
  its index as a side effect. So a first scan of a region reports nothing there
  and a later scan does. Rather than leave that silent, every enabled region
  Resource Explorer cannot yet answer for is listed by name.
- `--aws-regions` bypasses Resource Explorer entirely: nothing is missed, and
  nothing is created. That last point matters because `Search` creating an index
  is the one write a scan can perform; `docs/aws-setup.md` says so.

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

[Unreleased]: https://github.com/TypeOneLabs/tellury/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/TypeOneLabs/tellury/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/TypeOneLabs/tellury/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/TypeOneLabs/tellury/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/TypeOneLabs/tellury/releases/tag/v0.1.0
