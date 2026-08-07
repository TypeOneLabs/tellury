# tellury

Finds unused and overprovisioned cloud resources and tells you what each one costs you
per month.

A single-binary CLI that ingests your cloud inventory, builds an in-memory resource
graph, and evaluates deterministic rules against it. Every finding carries the evidence
behind it and a dollar figure.

> **Status: early development (v0.1.0).** Live GCP scanning works — Cloud Asset Inventory,
> Cloud Monitoring, and Cloud Billing are wired against the official SDKs. Four rules ship.
> Expect rough edges, and see [What works today](#what-works-today) for the boundary.

## Quick start

```bash
git clone https://github.com/TypeOneLabs/tellury.git && cd tellury
go build -o tellury ./cmd/tellury

gcloud auth application-default login
./tellury scan --gcp-project my-project
```

```
RESOURCE                      RULE                      MONTHLY WASTE
disk/pd-standard-01           detached_disk                     $8.00
address/idle-ip               unused_reserved_ip                $7.30
------------------------------------------------------------------
TOTAL                         2 findings                       $15.30
```

Read-only access is enough: `roles/cloudasset.viewer` and `roles/monitoring.viewer`.
No credentials at hand? `./tellury scan --gcp-project demo --fixture assets.json` runs the
whole pipeline offline.

## Vision

Most cloud cost tools are SaaS platforms: you hand over credentials, they ingest
everything into their warehouse, and you rent a dashboard. `tellury` is the opposite bet —
a tool you run yourself, that finishes in seconds and prints an answer.

Three principles shape the design:

**Deterministic, not heuristic.** Every rule resolves to true or false against data the
provider actually returns. A finding carries the evidence that produced it and the exact
formula behind its dollar figure. No ML, no "confidence score" standing in for a real
threshold. Run it twice on the same input and get the same answer — `--at` even pins the
evaluation instant so age-based predicates are reproducible.

**No infrastructure.** No database, no daemon, no agent to deploy. The resource graph
lives in memory for a scan and is thrown away, or written to a single JSON file if you
want to replay it. Dependencies are chosen on merit — the official provider SDKs handle
auth, retries, pagination, and regional endpoints properly, and reimplementing that
badly isn't a virtue.

**Rules as a compilable surface.** Rules are ordinary Go packages behind a small
interface, and there's a rule-compiler layer (`pkg/rules/compiler`) designed so that
rules can eventually be expressed declaratively and compiled into execution passes —
rather than every new check requiring a code change and a release.

The long-term target is multi-cloud. GCP is implemented first because Cloud Asset
Inventory gives a whole-scope inventory in one API surface.

## What works today

Working:

- **Live GCP ingestion** — Cloud Asset Inventory via `cloud.google.com/go/asset`, scoped to
  a project, folder, or organization. It reads through the asset-inventory module's
  `SearchAllResources` RPC, not `ListAssets`. That distinction matters: `ListAssets` is
  fronted by an eventually-consistent snapshot that can lag reality by hours (a reserved
  address can still report `RESERVING` for a while after it is `RESERVED`) and can omit
  resources entirely. `SearchAllResources` is the current, realtime view, and it is what
  tellury prices — a waste figure is only honest if it reflects what the scope looks like
  right now. Authenticates with Application Default Credentials.
- **Live metrics** — Cloud Monitoring via `cloud.google.com/go/monitoring`, fetched
  concurrently with bounded parallelism. Percentiles are computed client-side, so the
  aggregation is reproducible rather than whatever the server decided to align.
- **Live pricing** — Cloud Billing Catalog via `cloud.google.com/go/billing`, cached per
  scan. Precedence: `--price-file` override, then the live API, then an embedded fallback
  table. Every figure records which of the three produced it.
- **Resource graph** — compute instances, disks, snapshots, addresses, networks, and GCS
  buckets, with edges linking related resources. The graph also carries the resource
  hierarchy as container nodes — organization, folder, and project — joined by
  containment edges, so a finding can be attributed up to a folder or organization rather
  than stopping at the individual resource. Those containers are never rule targets and
  never count toward the scan's "N resources" figure.
- **Rule engine** — bounded worker pool, deterministic ordering, per-rule skip accounting.
- **Four GCP rules** (see below).
- **Offline scanning** — `--fixture` replays Cloud Asset Inventory JSON and `--cache-file`
  replays a saved graph. Both make no API calls and need no credentials.
- **Output** — `table`, `json`, `csv`, with filtering and sorting.

Not there yet:

- **AWS and Azure.** The provider seam exists — a provider declares its own scope flags
  and environment variables — but only GCP is implemented.
- **Rules beyond the initial four.** The engine is built for many more.
- **Declarative rules.** `pkg/rules/compiler` is the seam for compiling rules from a
  specification rather than writing Go; it isn't finished.

### Rules

| ID | Service | Severity | Detects |
|---|---|---|---|
| `detached_disk` | compute | medium | Persistent disks in a non-attached state, priced by type and size |
| `underutilized_instance` | compute | high | Instances significantly overprovisioned for their CPU load |
| `no_lifecycle_policy` | gcs | low | Buckets with no lifecycle management rules |
| `unused_reserved_ip` | compute | medium | External IP addresses reserved but attached to nothing; an unattached reserved address bills the full hourly rate while contributing nothing, so its entire monthly cost is waste |

`underutilized_instance` and `no_lifecycle_policy` read Cloud Monitoring series. Without
monitoring access they skip and say why — they never guess a value from missing data.
`detached_disk` and `unused_reserved_ip` are pure graph/attribute checks and need no
metrics. `tellury rules list` shows the catalogue; `tellury rules explain <id>` prints a
rule's full declaration.

## Build

Requires **Go 1.22+**.

```bash
go build -o tellury ./cmd/tellury
```

That is a development build: `tellury version` reports `dev` for the version, and the commit
and build time come from the Go toolchain's own VCS stamps. For a release build, stamp the
version too:

```bash
go build -ldflags "-X github.com/TypeOneLabs/tellury/internal/buildinfo.Version=v0.1.0" \
  -o tellury ./cmd/tellury
```

No code generation, no build tags, no Makefile.

## Run

```bash
tellury --help                 # top-level commands
tellury rules list             # the rule catalogue
tellury rules explain detached_disk
tellury version
tellury graph export --gcp-project my-project --out graph.json   # dump the graph snapshot
```

### Scanning a real project

Authenticate once with Application Default Credentials — `tellury` reads no key files and
takes no credentials flag:

```bash
gcloud auth application-default login
tellury scan --gcp-project my-project
```

Scope with `--gcp-project`, `--gcp-folder`, or `--gcp-organization`, or the matching
environment variables `TELLURY_GCP_PROJECT`, `TELLURY_GCP_FOLDER`, `TELLURY_GCP_ORGANIZATION`.
Flags win. The `gcp-` prefix keeps the CLI honest on a multi-cloud surface: AWS will bring
`--aws-account` and `--aws-region` later, registered the same way through the scope
registry rather than hardcoded in the CLI.

The scope is the SearchAllResources parent: `projects/<id>`, `folders/<n>`, or
`organizations/<n>`. Scanning a folder or organization pulls every resource under it and
builds the full hierarchy up from the SearchAllResources result — no separate Cloud
Resource Manager calls are made.

Read-only roles are enough: `roles/cloudasset.viewer` and `roles/monitoring.viewer`, plus
the `cloudasset`, `monitoring`, and `cloudbilling` APIs enabled. Without billing access the
scan still runs and prices from the embedded table. Without monitoring access it still
runs; only metric-dependent rules skip, each explaining why.

### Scanning offline

Both offline paths make no API calls and need no credentials — useful for testing rules, CI,
and reproducing a scan someone else ran. `--fixture` takes Cloud Asset Inventory JSON shaped
`{"assets": [ ... ]}`:

```bash
tellury scan --gcp-project my-project --fixture assets.json

# --cache-file writes a snapshot on a miss and replays it on a hit. A graph
# already written by an earlier run carries its own scope, so a pure replay
# needs no --gcp-project/--gcp-folder/--gcp-organization at all.
tellury scan --cache-file snapshot.json
```

```
RESOURCE                      RULE                      MONTHLY WASTE
disk/pd-standard-01           detached_disk                     $8.00
------------------------------------------------------------------
TOTAL                         1 findings                        $8.00
```

Machine-readable output, with per-finding evidence and the inputs behind each cost:

```bash
tellury scan --gcp-project my-project --fixture assets.json --format json
tellury scan --gcp-project my-project --fixture assets.json --format csv
```

Useful flags:

| Flag | Purpose |
|---|---|
| `--gcp-project` / `--gcp-folder` / `--gcp-organization` | Scope the scan (GCP's vocabulary: project, folder, or organization) |
| `--rules` / `--skip-rules` | Run or exclude specific rule IDs |
| `--min-waste` / `--min-confidence` | Hide findings below a threshold |
| `--sort` | `waste` (default), `resource`, or `rule` |
| `--explain-skips` | Print why each resource was skipped, per rule |
| `--at` | Pin the evaluation instant (RFC3339) so age predicates are reproducible |
| `--window` | Metric lookback in days (7–30, default 14) |
| `--cache-file` | Replay a graph from disk, or write one on first run — zero API calls on a hit |
| `--price-file` | Override the embedded price table |

Exit codes: `0` clean, `3` findings present (disable with `--fail-on-findings=false`),
non-zero otherwise on error.

## Layout

```
cmd/tellury/        entrypoint
internal/cli/       command wiring (cobra)
internal/config/    scan configuration
pkg/graph/          in-memory resource graph (leaf resources + hierarchy containers)
pkg/cloud/          provider seam
pkg/cloud/gcp/      GCP ingestion, normalization, asset types, hierarchy
pkg/metrics/        metric series + client-side percentiles
pkg/pricing/        price tables, machine catalogue, cost math
pkg/rules/          engine, registry, findings, skip accounting
pkg/rules/compiler/ declarative-rule compiler layer
pkg/rules/gcp/      the GCP rule implementations
pkg/output/         table, JSON, and CSV renderers
```

## Known issues

None affect a scan's results.

- **Multi-failure error text is dense.** When more than one metric fetch fails, the failures
  are joined with `errors.Join` — each stays individually inspectable via `errors.Is` — but
  they render as a single flat line rather than one per failure.
- **Only the first `--fixture` shape is supported.** Fixtures must match Cloud Asset
  Inventory's resource JSON. A fixture hand-written from documentation rather than captured
  from the API will normalize to a node with empty attributes rather than failing loudly.

## Changelog

Release history is in [CHANGELOG.md](CHANGELOG.md). `tellury` follows
[semantic versioning](https://semver.org); while the major version is `0`, the CLI surface
and rule interface may change between minor releases.

## Contributing

Rules are the point, and community rules are the reason this is open source — see
[CONTRIBUTING.md](CONTRIBUTING.md) for what a good rule looks like. No CLA, nothing to sign.

## License

[Apache License 2.0](LICENSE). Copyright 2026 TypeOne Labs.

`tellury` is open source and stays that way. TypeOne Labs also builds commercial software
on this engine; that does not change the licence of anything in this repository.
