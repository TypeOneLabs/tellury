# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and `tellury`
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the major
version is `0`, the CLI surface and the rule interface may change between minor releases.

## [Unreleased]

## [0.1.2] — 2026-08-09

Snapshot pricing corrections. `old_snapshot` shipped in 0.1.1 reporting figures that were
wrong in both directions at once; both errors are fixed and validated against a real
organization's bill.

### Fixed

- `old_snapshot` priced snapshots against their source disk's size rather than the
  incremental, deduplicated bytes Google actually bills. Measured against a real
  organization it overstated snapshot waste by about 9x — $7.28/month reported against
  $0.82 of real storage — and the error was not a constant factor that a rate could absorb:
  the billable fraction ranged from 15% of the source disk down to 0%. A fully deduplicated
  snapshot occupies no billable bytes and is now correctly worth nothing rather than being
  reported as waste. Snapshots now carry `storage_bytes` as their billable size, with
  `source_disk_size_gb` kept alongside as evidence because it is the figure the console
  shows.
- Snapshot pricing never resolved from the live Cloud Billing Catalog. The matcher looked
  for resource group `storagesnapshot`; the catalogue calls it `PDSnapshot` and files it
  under resource family `Storage`, not `Compute`. Every snapshot therefore fell back to the
  embedded table, which itself carried $0.026/GiB-month against a real rate of $0.050 —
  so the fallback was wrong too, and the two errors compounded. Snapshot SKUs are now
  matched on resource group alone, since the group name is unique and the family is not
  where a reader would expect it. Early-deletion charges in the same group are excluded:
  they are one-off penalties, not standing rates.

## [0.1.1] — 2026-08-09

Reporting, a rewritten rule interface, and a contributor on-ramp. No breaking changes to
the CLI; scans produce the same findings they did in 0.1.0.

### Added

- A scan writes a directory of artifacts instead of only printing to the terminal.
  `--out-dir` (default `tellury-out/`) receives a timestamped subdirectory holding the graph
  snapshot, findings JSON, and a self-contained HTML report. The graph replays through
  `--cache-file`, so a scan is reproducible offline.
- The HTML report renders the resource hierarchy as a collapsible tree with waste rolled up
  per organization, folder and project, above a table of the largest findings with price
  provenance. No network access is needed to view it.
- `old_snapshot` — persistent disk snapshots older than the retention window.
- A contributor skill at `.claude/skills/write-a-tellury-rule/`, walking through the rule
  interface, registration, the skip-code vocabulary, and a required mutation check: break
  the condition your rule detects, watch the test fail, restore it.
- A `PROJECT` column in table output for scans spanning more than one project, and a
  coverage report naming rules that could not be evaluated for lack of metrics.

### Changed

- Rules implement `NodeRule`, and the engine owns the evaluation skeleton — node iteration,
  the `tellury-exempt` check, ordered guards each carrying a typed skip code, the
  minimum-waste floor, and Finding construction. A rule supplies only what is specific to
  it. `Cost` returns a slice of branches, so a rule can offer a rightsizing delta and a
  stop/delete fallback and let the engine pick; a per-node context carries values computed
  in a guard through to cost and evidence. The previous `Rule` interface remains for rules
  that must reason across nodes.
- All four shipped rules were converted with identical output — same findings, same skip
  codes, same confidence.
- `underutilized_instance` skips instances managed by an instance group. A MIG owns its
  members' sizing, so per-member advice is not actionable.

### Fixed

- Static IP pricing never matched the live Cloud Billing Catalog: the catalog indexed the
  SKU as `external-static` while the rule queried `unattached`, so every static IP silently
  resolved from the embedded fallback table with provenance reading `embedded`.
- The HTML report's hierarchy total disagreed with the findings total in two directions —
  containers outside the scanned scope root were dropped, and a project reachable from two
  folders was counted twice. Both are now pinned by one invariant test.
- Table output no longer collides the `PROJECT` and `RULE` columns when a project ID fills
  or exceeds the column width.

### Removed

- `pkg/rules/compiler`, an unused 914-line scaffold for a declarative rule format that was
  evaluated and not adopted. Nothing imported it and no test covered it.

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
- Rules are native Go packages; there is no declarative rule language.
- Fixtures must match Cloud Asset Inventory's real resource JSON. A fixture written from
  documentation rather than captured from the API normalizes to a node with empty
  attributes rather than failing loudly.

[Unreleased]: https://github.com/TypeOneLabs/tellury/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/TypeOneLabs/tellury/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/TypeOneLabs/tellury/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/TypeOneLabs/tellury/releases/tag/v0.1.0
