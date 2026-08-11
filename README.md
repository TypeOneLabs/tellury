# tellury

Finds unused and overprovisioned cloud resources and prices them per month.

A single-binary CLI that reads your cloud inventory, builds an in-memory resource graph,
and evaluates deterministic rules against it. Each finding carries the evidence behind it,
the arithmetic that produced its figure, and which price source answered.

> **Early development.** GCP and AWS. Seven rules. The CLI surface and the rule interface
> may change between minor releases.

## Quick start

```bash
git clone https://github.com/TypeOneLabs/tellury.git && cd tellury
go build -o tellury ./cmd/tellury

gcloud auth application-default login
```

Read-only access is enough. See [GCP setup](docs/gcp-setup.md) for the four roles and what
each one buys you. **Pricing requires API access.** Without a live pricing API connection
(or the `TELLURY_PRICE_FIXTURE` environment variable for tests), resources are found and
reported as skipped (unpriced) rather than carrying a dollar total.

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

AWS works the same way, against one account:

```bash
export AWS_PROFILE=my-profile
./tellury scan --aws-account 123456789012
```

```
RESOURCE             RULE            MONTHLY WASTE
address/203.0.113.42 unassociated_eip        $3.65
--------------------------------------------------
TOTAL                1 findings              $3.65
Summary: accounts/123456789012 — 1 account analyzed, 17 regions analyzed, 2 resources scanned, 2 rules evaluated, 1 finding, 1 resource skipped, 15.285s
```

Three read-only permissions are enough — see [AWS setup](docs/aws-setup.md). A scan runs one
provider at a time: passing both `--gcp-*` and `--aws-*` flags fails before doing any work.

No credentials to hand? `tellury` runs the whole pipeline offline from a captured inventory
or a saved snapshot — see [Offline scanning](docs/offline.md). An offline scan without
pricing will find resources but report them as skipped (unpriced).

## What it does

- Reads GCP inventory from Cloud Asset Inventory, metrics from Cloud Monitoring, prices
  from the Cloud Billing Catalog; and AWS inventory from the EC2 API, prices from the
  Price List API (pricing:GetProducts).
- Builds a resource graph: instances, disks, snapshots, addresses, networks and buckets,
  plus the hierarchy above them — organization, folder and project on GCP, account on AWS,
  with a region tier beneath, so waste rolls up by place as well as by owner.
- Evaluates rules against it and prices each finding, in USD or your billing account's own
  currency.
- Writes a directory of artifacts per scan: a replayable graph snapshot, findings JSON, and
  a self-contained HTML report.
- Runs fully offline from a fixture or a saved snapshot. Inventory replays without the
  network; pricing requires the live API (or a test fixture via `TELLURY_PRICE_FIXTURE`).

On AWS, organization and organizational-unit scopes work through Organizations traversal
with cross-account role assumption, and Resource Explorer narrows the region sweep to where
resources actually are. Prices come from the live Price List API — EBS capacity, IOPS and
throughput, and the hourly address charge.

Not there yet: Azure.

## Rules

| ID | Provider / service | Severity | Detects |
|---|---|---|---|
| `detached_disk` | gcp / compute | medium | Persistent disks attached to nothing |
| `underutilized_instance` | gcp / compute | high | Instances overprovisioned for their CPU load |
| `old_snapshot` | gcp / compute | low | Snapshots past the retention window |
| `unused_reserved_ip` | gcp / compute | medium | Reserved external IPs attached to nothing |
| `no_lifecycle_policy` | gcp / gcs | low | Buckets with no lifecycle rules |
| `unattached_ebs_volume` | aws / ec2 | medium | EBS volumes attached to nothing |
| `unassociated_eip` | aws / ec2 | medium | Elastic IPs associated with nothing |

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
means nothing wasteful, never nothing scanned. An unpriced resource is reported as skipped,
never as free.

## Documentation

| | |
|---|---|
| [CLI reference](docs/cli.md) | Every command, flag, environment variable and exit code |
| [GCP setup](docs/gcp-setup.md) | Authentication, IAM roles, APIs, currency detection |
| [AWS setup](docs/aws-setup.md) | Credentials, the three permissions, regions, pricing caveats |
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
pkg/pricing/      pricing interfaces, live API clients, test fixtures
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
- An offline scan without a price source (no live API, no `TELLURY_PRICE_FIXTURE`) reports
  resources as skipped (unpriced). This is by design: the tool refuses to guess at money.

## Contributing

Rules are the point. [docs/writing-a-rule.md](docs/writing-a-rule.md) walks the `NodeRule`
interface, the skip vocabulary, registration and its silent-failure trap, and the required
mutation check, with `old_snapshot` as a worked example that is real code in this
repository. It reads the same whether you are a person or a coding agent.

[AGENTS.md](AGENTS.md) carries the repository conventions in one place; see
[CONTRIBUTING.md](CONTRIBUTING.md) for the process. No CLA.

## License

[Apache License 2.0](LICENSE). Copyright 2026 TypeOne Labs.
