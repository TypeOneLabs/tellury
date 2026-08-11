# tellury

Finds unused and overprovisioned cloud resources and prices them per month.

A single-binary CLI that reads your cloud inventory, builds an in-memory resource graph,
and evaluates deterministic rules against it. Each finding carries the evidence behind it,
the arithmetic that produced its figure, and which price source answered.

> **Early development.** GCP only. Five rules. The CLI surface and the rule interface may
> change between minor releases.

## Quick start

```bash
git clone https://github.com/TypeOneLabs/tellury.git && cd tellury
go build -o tellury ./cmd/tellury

gcloud auth application-default login
```

Read-only access is enough. See [GCP setup](docs/gcp-setup.md) for the four roles and what
each one buys you.

Scan a project:

```bash
./tellury scan --gcp-project my-project
```

```
RESOURCE               RULE              MONTHLY WASTE
disk/pd-standard-01    detached_disk             $8.00
address/reserved-ip-01 unused_reserved_ip        $7.30
------------------------------------------------------
TOTAL                  2 findings               $15.30
Summary: projects/my-project — 1 project analyzed, 2 resources scanned, 5 rules evaluated, 2 findings, 0 resources skipped, 2ms
```

Or a whole organization, which adds a `PROJECT` column and rolls the total up across every
project beneath it:

```bash
./tellury scan --gcp-organization 123456789012
```

```
RESOURCE               PROJECT       RULE              MONTHLY WASTE
disk/old-cache         ml-training   detached_disk            $20.00
disk/pd-standard-01    data-platform detached_disk             $8.00
disk/scratch-disk      web-frontend  detached_disk             $8.00
address/reserved-ip-01 data-platform unused_reserved_ip        $7.30
--------------------------------------------------------------------
TOTAL                  4 findings                             $43.30
Summary: organizations/123456789012 — 3 projects analyzed, 4 resources scanned, 5 rules evaluated, 4 findings, 0 resources skipped, 2ms
```

`--gcp-folder` scopes to a folder. Each scan also writes a graph snapshot, findings JSON and
an HTML report into `tellury-out/`.

No credentials to hand? `tellury` runs the whole pipeline offline from a captured inventory
or a saved snapshot — see [Offline scanning](docs/offline.md).

## What it does

- Reads GCP inventory from Cloud Asset Inventory, metrics from Cloud Monitoring, prices
  from the Cloud Billing Catalog.
- Builds a resource graph: instances, disks, snapshots, addresses, networks and buckets,
  plus the organization/folder/project hierarchy.
- Evaluates rules against it and prices each finding, in USD or your billing account's own
  currency.
- Writes a directory of artifacts per scan: a replayable graph snapshot, findings JSON, and
  a self-contained HTML report.
- Runs fully offline from a fixture or a saved snapshot.

Not there yet: AWS and Azure. The provider seam exists; only GCP is implemented.

## Rules

| ID | Service | Severity | Detects |
|---|---|---|---|
| `detached_disk` | compute | medium | Persistent disks attached to nothing |
| `underutilized_instance` | compute | high | Instances overprovisioned for their CPU load |
| `old_snapshot` | compute | low | Snapshots past the retention window |
| `unused_reserved_ip` | compute | medium | Reserved external IPs attached to nothing |
| `no_lifecycle_policy` | gcs | low | Buckets with no lifecycle rules |

`underutilized_instance` and `no_lifecycle_policy` read Cloud Monitoring. Without metric
access they skip and say so — they never guess a value from missing data.

`tellury rules list` shows the catalogue; `tellury rules explain <id>` prints one rule's full
declaration.

## Design

**Deterministic.** Every rule resolves to true or false against data the provider returned.
A finding carries its evidence and the arithmetic behind its figure. Run it twice on the
same input and get the same answer; `--at` pins the evaluation instant so age-based
predicates are reproducible too.

**No infrastructure.** No database, no daemon, no agent. The graph lives in memory for a
scan, or in one JSON file if you want to replay it.

**Rules are Go.** A rule implements `NodeRule` — metadata, target kind, ordered guards,
cost, evidence — and the engine owns the rest: the exempt-label check, typed skip
accounting, the noise floor, finding construction. Compiled by the ordinary Go toolchain,
no code generation.

**Honest about gaps.** A rule that cannot evaluate a resource records a typed reason rather
than guessing, and a scan reports what it scanned, not only what it found. An empty table
means nothing wasteful, never nothing scanned.

## Documentation

| | |
|---|---|
| [CLI reference](docs/cli.md) | Every command, flag, environment variable and exit code |
| [GCP setup](docs/gcp-setup.md) | Authentication, IAM roles, APIs, currency detection |
| [Offline scanning](docs/offline.md) | Fixtures, snapshots, `graph export`, scan artifacts |
| [Writing a rule](docs/writing-a-rule.md) | The `NodeRule` interface end to end, with a worked example |
| [AGENTS.md](AGENTS.md) | Repository conventions, for people and coding agents |
| [CHANGELOG.md](CHANGELOG.md) | Release history |

## Build

Requires Go 1.22 or newer.

```bash
go build -o tellury ./cmd/tellury
go test ./...
```

Version information comes from the Go toolchain's VCS stamps, so `tellury version` reports
the commit a binary was built from.

## Layout

```
cmd/tellury/       CLI entry point
internal/cli/     command wiring, flags, output selection
pkg/graph/        in-memory resource graph
pkg/cloud/gcp/    Cloud Asset Inventory ingestion and normalization
pkg/metrics/      metric registry and GCP backends
pkg/pricing/      price sources: override, live catalogue, embedded table
pkg/rules/        rule engine, NodeRule interface, skip vocabulary
pkg/rules/gcp/    the shipped rules
pkg/output/       table, JSON, CSV and HTML renderers
```

## Known issues

Neither affects a scan's results.

- A raw CAI fixture can only evaluate topology rules. By design, and stated at the end of
  every fixture run — see [Offline scanning](docs/offline.md).
- When several metric fetches fail, the failures are joined and render as one dense line,
  though each stays individually inspectable via `errors.Is`.

## Contributing

Rules are the point. [docs/writing-a-rule.md](docs/writing-a-rule.md) walks the `NodeRule`
interface, the skip vocabulary, registration and its silent-failure trap, and the required
mutation check, with `old_snapshot` as a worked example that is real code in this
repository. It reads the same whether you are a person or a coding agent.

[AGENTS.md](AGENTS.md) carries the repository conventions in one place; see
[CONTRIBUTING.md](CONTRIBUTING.md) for the process. No CLA.

## License

[Apache License 2.0](LICENSE). Copyright 2026 TypeOne Labs.
