# tellury

Finds unused and overprovisioned cloud resources and prices them per month.

A single-binary CLI that reads your cloud inventory, builds an in-memory resource graph,
and evaluates deterministic rules against it. Each finding carries the evidence behind it,
the arithmetic that produced its figure, and which price source answered.

> **Early development.** GCP and AWS. Eight rules. The CLI surface and the rule interface
> may change between minor releases.

## Quick start

Download a binary from the [latest release](https://github.com/TypeOneLabs/tellury/releases/latest)
— Linux, macOS and Windows, amd64 and arm64:

```bash
# Linux amd64; swap linux/amd64 for darwin/arm64 etc.
curl -sSL https://github.com/TypeOneLabs/tellury/releases/latest/download/tellury_linux_amd64.tar.gz \
  | tar xz tellury
./tellury version
```

Each release also carries a `checksums.txt`. Or build from source, which needs Go 1.22+:

```bash
git clone https://github.com/TypeOneLabs/tellury.git && cd tellury
go build -o tellury ./cmd/tellury
```

Read-only credentials are enough on either cloud. **Pricing requires live API access** — a
scan that cannot reach the pricing API finds your resources and reports them as skipped
(unpriced) rather than inventing a dollar figure.

### AWS

```bash
export AWS_PROFILE=my-profile
./tellury scan --aws-account 123456789012 --aws-regions eu-west-1
```

```
RESOURCE              RULE               MONTHLY WASTE
volume/vol-0a1b2c3d   unattached_ebs_volume     $17.60
address/203.0.113.42  unassociated_eip           $3.65
------------------------------------------------------
TOTAL                 2 findings                $21.25
Summary: accounts/123456789012 — 1 account analyzed, 1 region analyzed (explicit), 5 resources scanned, 3 rules evaluated, 2 findings, 3 resources skipped, 8.1s
```

Without `--aws-regions` every enabled region is swept, which is thorough and slow; narrowing
it is the single biggest lever on scan time. See [AWS setup](docs/aws-setup.md) for the six
read-only permissions.

### GCP

```bash
gcloud auth application-default login
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

See [GCP setup](docs/gcp-setup.md) for the four roles and what each one buys you.

### Scanning a whole organization

Both clouds take an organization scope, which adds an owner column and rolls the total up
across everything beneath it:

```bash
./tellury scan --gcp-organization 123456789012
./tellury scan --aws-organization o-abc123 --aws-regions eu-west-1
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

`--gcp-folder` and `--aws-organizational-unit` scope to a subtree. On AWS, member accounts
are reached by assuming a role in each; an account that cannot be reached is reported in the
account outcomes rather than failing the scan.

A scan runs one provider at a time: passing both `--gcp-*` and `--aws-*` flags fails before
doing any work. Each scan writes a graph snapshot, findings JSON and an HTML report into
`tellury-out/`.

## What it does

- Reads GCP inventory from Cloud Asset Inventory, metrics from Cloud Monitoring, prices from
  the Cloud Billing Catalog; and AWS inventory from the EC2 API, metrics from CloudWatch,
  prices from the Price List API (pricing:GetProducts).
- Builds a resource graph: instances, disks, snapshots, addresses, networks and buckets,
  plus the hierarchy above them — organization, folder and project on GCP, account on AWS,
  with a region tier beneath, so waste rolls up by place as well as by owner.
- Evaluates rules against it and prices each finding, in USD or your billing account's own
  currency.
- Writes a directory of artifacts per scan: a replayable graph snapshot, findings JSON, and
  a self-contained HTML report.
- Replays a captured inventory or a saved snapshot without the network, for reproducing a
  scan — though pricing always needs the live API. See [Offline scanning](docs/offline.md).

On AWS, organization and organizational-unit scopes work through Organizations traversal
with cross-account role assumption, and Resource Explorer narrows the region sweep to where
resources actually are. Prices come from the live Price List API — EBS capacity, IOPS and
throughput, the hourly address charge, and On-Demand instance rates.

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
| `underutilized_ec2` | aws / ec2 | high | EC2 instances overprovisioned for their CPU load |

`underutilized_instance` and `no_lifecycle_policy` read Cloud Monitoring; `underutilized_ec2`
reads CloudWatch. Without metric access they skip and say so — they never guess a value from
missing data. Neither cloud publishes guest memory without an agent installed in the VM, so
every rule here judges CPU only.

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
| [AWS setup](docs/aws-setup.md) | Credentials, IAM permissions, regions, pricing caveats |
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

Version information comes from the Go toolchain's VCS stamps, so `tellury version` reports the
commit a binary was built from. A locally built binary reports its version as `dev`; released
binaries are stamped with their tag by the release workflow.

## Layout

```
cmd/tellury/       CLI entry point
scripts/          release helpers
internal/cli/     command wiring, flags, output selection
pkg/graph/        in-memory resource graph
pkg/cloud/gcp/    Cloud Asset Inventory ingestion and normalization
pkg/cloud/aws/    EC2, Organizations and Resource Explorer ingestion
pkg/metrics/      metric registry, with GCP and AWS backends
pkg/pricing/      pricing interfaces, live API clients, test fixtures
pkg/rules/        rule engine, NodeRule interface, skip vocabulary
pkg/rules/gcp/    the shipped GCP rules
pkg/rules/aws/    the shipped AWS rules
pkg/output/       table, JSON, CSV and HTML renderers
```

## Known issues

- When several metric fetches fail, the failures are joined and render as one dense line,
  though each stays individually inspectable via `errors.Is`.
- `underutilized_ec2` recommends a smaller size only within the same instance family and only
  when that family has a member with fewer vCPUs. Several AWS families (t3 below `xlarge`,
  for instance) hold vCPU count constant and vary only memory, so instances there can only be
  reported as stop/delete candidates. Sizing on memory would need a guest agent neither cloud
  requires.
- EC2 instances are not priced on the offline path, so `underutilized_ec2` produces no
  findings from a fixture. See [Offline scanning](docs/offline.md).

## Contributing

Rules are the point. [docs/writing-a-rule.md](docs/writing-a-rule.md) walks the `NodeRule`
interface, the skip vocabulary, registration and its silent-failure trap, and the required
mutation check, with `old_snapshot` as a worked example that is real code in this
repository. It reads the same whether you are a person or a coding agent.

[AGENTS.md](AGENTS.md) carries the repository conventions in one place; see
[CONTRIBUTING.md](CONTRIBUTING.md) for the process. No CLA.

## License

[Apache License 2.0](LICENSE). Copyright 2026 TypeOne Labs.
