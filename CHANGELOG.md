# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and `tellury`
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the major
version is `0`, the CLI surface and the rule interface may change between minor releases.

## [Unreleased]

## [0.1.0] — 2026-08-07

First release. GCP only.

### Added

**Scanning**

- `tellury scan` against a GCP project, folder, or organization, scoped with
  `--gcp-project`, `--gcp-folder`, or `--gcp-organization` (or `TELLURY_GCP_PROJECT`,
  `TELLURY_GCP_FOLDER`, `TELLURY_GCP_ORGANIZATION`). Flags take precedence.
- Authentication via Application Default Credentials only. `tellury` reads no key files and
  takes no credentials flag.
- Live ingestion from Cloud Asset Inventory's `SearchAllResources`, which reflects current
  state. The alternative `ListAssets` surface is fronted by a snapshot that can lag by hours
  and omit resources entirely.
- Live metric enrichment from Cloud Monitoring, fetched concurrently with bounded
  parallelism. Percentiles are computed client-side so aggregation is reproducible.
- Live pricing from the Cloud Billing Catalog, cached per scan. Precedence is `--price-file`
  override, then the live API, then an embedded fallback table; every figure records which
  source produced it.
- Graceful degradation: without billing access, prices come from the embedded table; without
  monitoring access, metric-dependent rules skip and say why. Neither fails the scan.

**Rules**

- `detached_disk` — persistent disks in a non-attached state, priced by type and size.
- `underutilized_instance` — instances significantly overprovisioned for their CPU load.
- `no_lifecycle_policy` — buckets with no lifecycle management rules.
- `unused_reserved_ip` — external IP addresses reserved but attached to nothing, where the
  entire monthly cost is waste.
- Per-rule skip accounting with distinct reasons, surfaced by `--explain-skips`. A rule that
  cannot evaluate a resource says so rather than guessing a value.
- `tellury-exempt=true` as a resource label to exclude a resource from every rule.

**Graph**

- In-memory resource graph covering compute instances, disks, snapshots, addresses,
  networks, and GCS buckets, with edges linking related resources.
- Resource hierarchy as container nodes — organization, folder, project — joined by
  containment edges. Containers are never rule targets and never counted as resources.
- `tellury graph export` to dump a graph snapshot as JSON.

**Offline operation**

- `--fixture` replays Cloud Asset Inventory JSON; `--cache-file` writes a graph snapshot on
  a miss and replays it on a hit. Both make no API calls and need no credentials.

**Output**

- `table`, `json`, and `csv` formats. JSON findings carry per-finding evidence, the inputs
  behind each cost, the SKU and source each price came from, and a remediation command.
- Filtering and sorting via `--rules`, `--skip-rules`, `--min-waste`, `--min-confidence`,
  and `--sort`.
- `--at` pins the evaluation instant so age-based predicates are reproducible.
- Exit codes: `0` clean, `3` findings present (disable with `--fail-on-findings=false`),
  non-zero otherwise on error.

**Other**

- `tellury rules list` and `tellury rules explain <id>` for the rule catalogue.
- `tellury version`, reporting the stamped version plus the commit and build time from the
  Go toolchain's VCS stamps.

### Known limitations

- GCP only. The provider seam exists — a provider declares its own scope flags and
  environment variables — but AWS and Azure are not implemented.
- Rules are Go packages. `pkg/rules/compiler` is the seam for declarative rules; it is not
  finished.
- Fixtures must match Cloud Asset Inventory's real resource JSON. A fixture written from
  documentation rather than captured from the API normalizes to a node with empty
  attributes rather than failing loudly.

[Unreleased]: https://github.com/TypeOneLabs/tellury/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/TypeOneLabs/tellury/releases/tag/v0.1.0
