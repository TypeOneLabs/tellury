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

# Offline, no credentials. --at pins the clock so this reproduces on any day.
./tellury scan --gcp-project my-project \
  --fixture internal/cli/testdata/readme-assets.json \
  --rules detached_disk --at 2024-01-20T00:00:00Z
```

```
RESOURCE            RULE         MONTHLY WASTE
disk/pd-standard-01 detached_disk        $8.00
----------------------------------------------
TOTAL               1 findings           $8.00
Summary: projects/my-project — 1 project analyzed, 1 resource scanned, 1 rule evaluated, 1 finding, 0 resources skipped, 2ms
```

Against a real project:

```bash
gcloud auth application-default login
./tellury scan --gcp-project my-project
```

Read-only access is enough — see [GCP setup](docs/gcp-setup.md).

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
| [CONTRIBUTING.md](CONTRIBUTING.md) | Writing a rule |
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

Rules are the point — see [CONTRIBUTING.md](CONTRIBUTING.md). The contributor skill at
`.claude/skills/write-a-tellury-rule/` walks the `NodeRule` interface, the skip vocabulary,
the registration step and its silent-failure trap, and the required mutation check, with
`old_snapshot` as a worked example in the repository. No CLA.

## License

[Apache License 2.0](LICENSE). Copyright 2026 TypeOne Labs.
