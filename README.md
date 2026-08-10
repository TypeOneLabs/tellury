# tellury

Finds unused and overprovisioned cloud resources and tells you what each one costs you
per month.

A single-binary CLI that ingests your cloud inventory, builds an in-memory resource
graph, and evaluates deterministic rules against it. Every finding carries the evidence
behind it and a dollar figure.

> **Status: early development.** Live GCP scanning works — Cloud Asset Inventory, Cloud
> Monitoring, and Cloud Billing are wired against the official SDKs. Five rules ship, and a
> scan writes a directory of artifacts (graph snapshot, findings JSON, self-contained HTML
> report). Expect rough edges, and see [What works today](#what-works-today) for the
> boundary.

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

Read-only access is enough: `roles/cloudasset.viewer` and `roles/monitoring.viewer`, plus
`roles/billing.viewer` on the billing account if you want figures in your own currency
rather than USD ([full table](#permissions)).
No credentials at hand? `./tellury scan --gcp-project demo --fixture assets.json` runs the
whole pipeline offline (see [Offline mode](#offline-mode) for exactly what that replay can
and cannot evaluate).

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

**Rules as a compilable surface.** Rules are native Go packages implementing the
`NodeRule` interface — `Meta`, `Kind`, ordered `Guards`, `Cost`, `MinWasteUSD`, and the
evidence hooks — compiled by the ordinary Go toolchain, no code generation. The engine
owns the evaluation skeleton (the exempt-label check, typed skip accounting, the
minimum-waste floor, finding construction), so a rule is just the decisions: what to
detect, how to price it, and how to say why it was skipped. Writing one is a short,
well-trodden path — see [Contributing](#contributing) and the contributor skill at
`.claude/skills/write-a-tellury-rule/`, which walks a complete new rule end to end.

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
- **Pricing currency** — `--currency` (or `TELLURY_CURRENCY`) prices a scan in any ISO 4217
  currency the Cloud Billing Catalog supports: the API converts the whole catalogue
  server-side, so the snapshot SKU price of 0.050000 USD comes back as 0.043890 EUR.
  Without a flag, tellury best-effort detects the billing account's currency (a billing
  account's currency is fixed at creation) from any project in scope and falls back to
  USD; the report always states which currency the figures are actually in and how it was
  decided. See [Pricing currency](#pricing-currency).
- **Resource graph** — compute instances, disks, snapshots, addresses, networks, and GCS
  buckets, with edges linking related resources. The graph also carries the resource
  hierarchy as container nodes — organization, folder, and project — joined by
  containment edges, so a finding can be attributed up to a folder or organization rather
  than stopping at the individual resource. Those containers are never rule targets and
  never count toward the scan's "N resources" figure.
- **Rule engine** — bounded worker pool, deterministic ordering, per-rule skip accounting.
- **Four GCP rules** (see below).
- **Offline scanning** — `--fixture` replays Cloud Asset Inventory JSON and `--cache-file`
  replays a saved graph. Both make no API calls and need no credentials. The distinction
  between the two is explicit and honest: a fixture replay can only ever evaluate the
  topology-only rules, while a cached-snapshot replay drives every rule including the
  metric-dependent ones (see [Offline mode](#offline-mode)).
- **Output** — `table`, `json`, `csv`, with filtering and sorting.
- **Scan artifacts** — every scan leaves a directory of artifacts behind in `--out-dir`
  (default `tellury-out/`): a replayable graph snapshot, the findings JSON, and a
  self-contained HTML report. See [Scan artifacts](#scan-artifacts).

Not there yet:

- **AWS and Azure.** The provider seam exists — a provider declares its own scope flags
  and environment variables — but only GCP is implemented.
- **Rules beyond the initial four.** The engine is built for many more.

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

One exclusion on `underutilized_instance` is worth spelling out: **it skips any instance
that is a managed instance group (MIG) member**, flagged via the instance's `created-by`
metadata pointing at an `instanceGroupManagers` resource. The reason is structural — a MIG
owns its members' size and count, so advice to resize one member is not something an
operator can act on (the group would just roll it back). Rightsizing advice belongs on the
group, which is a separate concern with its own future rules, not on an individual member.
The skip is tallied separately (reason `managed_by_mig`) so `--explain-skips` keeps it
distinct from every other reason. This is not part of `unused_reserved_ip`, which never
looks at instances.

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
tellury graph export --gcp-project my-project --out graph.json   # enrich + dump a graph snapshot
```

### Scanning a real project

Authenticate once with Application Default Credentials — `tellury` reads no key files and
takes no credentials flag:

```bash
gcloud auth application-default login
tellury scan --gcp-project my-project
```

Every real scan writes a directory of artifacts into `--out-dir` (see
[Scan artifacts](#scan-artifacts)).

Scope with `--gcp-project`, `--gcp-folder`, or `--gcp-organization`, or the matching
environment variables `TELLURY_GCP_PROJECT`, `TELLURY_GCP_FOLDER`, `TELLURY_GCP_ORGANIZATION`.
Flags win. The `gcp-` prefix keeps the CLI honest on a multi-cloud surface: AWS will bring
`--aws-account` and `--aws-region` later, registered the same way through the scope
registry rather than hardcoded in the CLI.

The scope is the SearchAllResources parent: `projects/<id>`, `folders/<n>`, or
`organizations/<n>`. Scanning a folder or organization pulls every resource under it and
builds the full hierarchy up from the SearchAllResources result — no separate Cloud
Resource Manager calls are made.

#### Permissions

Everything `tellury` does is read-only. Each role unlocks one capability, and only the first
is required — without any of the others the scan still runs and says what it could not do.

| Role | Granted on | Enables | Without it |
|---|---|---|---|
| `roles/cloudasset.viewer` | the scanned org, folder or project | Resource discovery | Nothing to scan; required |
| `roles/monitoring.viewer` | each project in scope | Metric enrichment | Metric-dependent rules skip, each saying why |
| `roles/billing.viewer` | the billing account | Reading the account's currency | Figures are reported in USD |

Live pricing needs **no** billing role: the Cloud Billing Catalog is readable by any
authenticated caller. `roles/billing.viewer` buys only the ability to read which currency
your billing account is denominated in, so figures come back in the currency you are
actually invoiced in. Without it, pricing is still live — just quoted in USD.

Enable the `cloudasset`, `monitoring` and `cloudbilling` APIs on the project the credentials
belong to.

If you authenticate by impersonating a service account, the **caller** additionally needs
`roles/iam.serviceAccountTokenCreator` on that service account. This one catches people out:
the service account can hold every role above and impersonation still fails with a 403 on
`iam.serviceAccounts.getAccessToken`, because that permission governs who may borrow the
identity rather than what the identity is allowed to do.

```bash
gcloud organizations add-iam-policy-binding ORG_ID \
  --member="serviceAccount:SA_EMAIL" --role="roles/cloudasset.viewer"
gcloud organizations add-iam-policy-binding ORG_ID \
  --member="serviceAccount:SA_EMAIL" --role="roles/monitoring.viewer"

# Currency detection.
gcloud billing accounts add-iam-policy-binding BILLING_ACCOUNT_ID \
  --member="serviceAccount:SA_EMAIL" --role="roles/billing.viewer"

# Only when impersonating rather than authenticating as the service account.
gcloud iam service-accounts add-iam-policy-binding SA_EMAIL \
  --member="user:YOU@example.com" --role="roles/iam.serviceAccountTokenCreator"
```

### Scan artifacts

A scan is not just a table on stdout. Every `tellury scan` (online or offline, real or
fixture) leaves a directory of artifacts behind, so a run you cannot watch still records
what it saw and found. The directory defaults to `tellury-out/` and the artifacts land in a
per-scan, timestamped subdirectory, so consecutive runs never silently overwrite each
other:

```
tellury-out/
  projects-my-project-20260807T123456.789012345Z/
    graph-projects-my-project.json       # replayable graph snapshot
    findings-projects-my-project.json    # the scan report, as JSON
    report-projects-my-project.html      # self-contained HTML report
```

Change the base directory with `--out-dir`:

```bash
tellury scan --gcp-project my-project --out-dir ./reports/
```

The three files are:

- **`graph-<scope>.json`** — the enriched graph snapshot. It is written by the exact same
  serializer `--cache-file` uses, so the artifact directory is directly replayable:
  `tellury scan --cache-file tellury-out/<scope>-<stamp>/graph-<scope>.json` reproduces the
  scan's full-fidelity graph, metric-dependent rules included, with zero API calls. The
  snapshot carries its own scope, so a pure replay needs no `--gcp-project`.
- **`findings-<scope>.json`** — the scan report (findings, totals, scope,
  `metrics_blocked`) in the same shape `--format json` prints, saved to disk.
- **`report-<scope>.html`** — a **self-contained** HTML report. All CSS is inlined, there
  is no JavaScript, no CDN reference, and no runtime network fetch: an operator can open it
  on an air-gapped machine, email it, or attach it to a ticket and it renders identically.

The HTML report has two parts:

1. A **collapsible hierarchy** built from the graph's container nodes and containment
   edges. Waste rolls **up**: each organization, folder, and project branch shows the sum
   of everything beneath it, so a folder-sized or organization-wide scan surfaces where
   the money is without scrolling a flat list. Expand a branch to walk down to the
   individual findings.
2. A **top-findings table**, ordered by monthly waste, where each row carries the evidence
   behind its figure — including the **price provenance** (which of `--price-file` / live
   API / embedded table answered each SKU). The same scan renders a byte-identical report,
   and every value that came from a cloud API is HTML-escaped, so a hostile resource name
   cannot turn the report into an attack surface.

### Pricing currency

By default a scan prices in **USD**. Pass `--currency EUR` (or `TELLURY_CURRENCY=EUR`) to
price the whole scan in that currency: the Cloud Billing Catalog API converts the
catalogue on the server side, so every figure — findings, totals, rollups — is expressed
in EUR, not converted afterwards by the tool.

```bash
tellury scan --gcp-project my-project --currency EUR
```

When no flag is given, tellury **auto-detects** the currency from the billing account of a
project in scope (a billing account's currency is fixed at creation, so it is a reliable
answer). Detection is best effort — it needs billing permission that a scan may
legitimately lack — so every detection failure degrades quietly to USD, and the report
always states which currency the figures are actually in and how it was decided
(`requested via --currency` / `detected from the billing account`). Precedence, highest
first: **explicit flag, then detection, then USD**.

A malformed code (not three letters) is rejected before the scan starts. A well-formed but
unsupported code fails at the Cloud Billing API with the currency named — never silently
falls back. The embedded fallback price table is **USD-only**: if it answers a non-USD
request (for example an offline `--fixture` replay asked for EUR), the report says so
loudly with a WARNING in every output format rather than handing you EUR-labelled figures
that are actually dollars.

### Offline mode

Both offline paths make no API calls and need no credentials. **The two paths have
different fidelity, and tellury says so explicitly at the end of every offline scan.**

#### `--fixture`: raw Cloud Asset Inventory (topology only)

`--fixture` takes a captured Cloud Asset Inventory export — raw assets with their resource
payloads, but **no metric series**. It is the closest the offline world gets to a live
ingestion: it runs the full normalization, edge extraction, and hierarchy pass, so every
topology-based rule (`detached_disk`, `unused_reserved_ip`) can evaluate. The
metric-dependent rules (`underutilized_instance`, `no_lifecycle_policy`) **cannot** — they
need Cloud Monitoring series that a raw CAI export does not carry.

tellury does not let this look like "no waste". At the end of a fixture run, the report
states explicitly which rules could not be evaluated for lack of metrics:

```bash
tellury scan --gcp-project my-project --fixture assets.json
```

```
No waste found in projects/my-project (1 resources, 4 rules).

2 rule(s) could not be evaluated for lack of metric data: no_lifecycle_policy, underutilized_instance
(use --cache-file from a live `scan` or an enriched `graph export` to evaluate them)
```

The JSON report carries the same information under `metrics_blocked`. A fixture run still
writes the artifact directory (including the HTML report), and its findings JSON and HTML
carry the same `metrics_blocked` honesty.

#### `--cache-file`: full-fidelity graph snapshot

`--cache-file` reads or writes a **graph snapshot** — the same in-memory graph `scan`
builds, serialized AFTER enrichment, with every node's `Metrics` map (p95 values, sample
counts, coverage, window bounds). A replay therefore has full fidelity: **every rule can
evaluate**, metric-dependent ones included, exactly as if the live scan were rerun.

```bash
# First run: live scan, graph with metrics written back.
tellury scan --gcp-project my-project --cache-file snap.json

# Later runs: graph replayed verbatim — zero API calls, same rules, same data.
tellury scan --cache-file snap.json
```

A graph snapshot written by `scan` also carries its own scope, so a pure replay needs no
`--gcp-project`/`--gcp-folder`/`--gcp-organization` at all. And a replay writes its own
artifact directory the same way an online scan does — the graph goes back out under the
new run's timestamp, alongside fresh findings and HTML.

#### Capturing your own fixture

To produce the exact CAI shape `--fixture` accepts, run `gcloud asset search-all-resources`
from the SDK it ships with (`gcloud` CLI uses the same Cloud Asset Inventory
`SearchAllResources` RPC tellury's live path calls). The command below captures every asset
type the shipped rules request, with the full versioned resource payload, and writes a
plain JSON array that `tellury scan --fixture` consumes **without any hand-editing**:

```bash
gcloud asset search-all-resources \
  --scope=projects/MY_PROJECT \
  --asset-types=compute.googleapis.com/Instance,compute.googleapis.com/Disk,compute.googleapis.com/Snapshot,compute.googleapis.com/Address,compute.googleapis.com/Network,storage.googleapis.com/Bucket \
  --read-mask='*' \
  --format=json \
  > cai-assets.json
```

The captured file is a bare JSON array of `ResourceSearchResult` objects — exactly what
tellury's fixture reader accepts (it folds `versionedResources` onto the `RawAsset` shape
its live `SearchAllResources` path uses, so normalization treats the capture identically).
The `--read-mask='*'` is **mandatory**: without it the payload drops the versioned resource
fields the normalizers read, and every asset would come through as an empty-shell node.

To capture a folder or organization instead of a project, replace
`--scope=projects/MY_PROJECT` with `--scope=folders/123` or
`--scope=organizations/456`.

**Do not confuse a CAI fixture with a graph snapshot.** The `gcloud` command above captures
raw inventory; a `tellury graph export` (below) writes an enriched graph that carries metric
series and drives every rule. Prefer `graph export` when you need full fidelity offline.

### `graph export`: full-fidelity snapshot

`graph export` ingests a scope **exactly as `scan` does** — including Cloud Monitoring
enrichment — and writes the resulting graph as a version-2 JSON snapshot. The output is
what `scan --cache-file` replays verbatim, so **every rule, metric-dependent included, can
fire on the replay**.

```bash
tellury graph export --gcp-project my-project --out graph.json
tellury scan --cache-file graph.json
```

Enrichment is on by default. If you only need topology (you are debugging edges, or the
monitoring API is unreachable), pass `--no-enrich-metrics` to write a snapshot whose replay
will state metric-dependent rules as "could not be evaluated for lack of metric data" —
the same explicit honesty the fixture path uses.

### Machine-readable output

```bash
tellury scan --gcp-project my-project --fixture assets.json --format json
tellury scan --gcp-project my-project --fixture assets.json --format csv
```

Useful flags:

| Flag | Purpose |
|---|---|
| `--gcp-project` / `--gcp-folder` / `--gcp-organization` | Scope the scan (GCP's vocabulary: project, folder, or organization) |
| `--currency` | ISO 4217 code to price the scan in, e.g. `EUR` (or `TELLURY_CURRENCY`); overrides billing-account auto-detection, default USD |
| `--rules` / `--skip-rules` | Run or exclude specific rule IDs |
| `--min-waste` / `--min-confidence` | Hide findings below a threshold |
| `--sort` | `waste` (default), `resource`, or `rule` |
| `--explain-skips` | Print why each resource was skipped, per rule |
| `--at` | Pin the evaluation instant (RFC3339) so age predicates are reproducible |
| `--window` | Metric lookback in days (7–30, default 14) |
| `--cache-file` | Replay a graph from disk, or write one on first run — zero API calls on a hit |
| `--fixture` | Read raw CAI assets from local JSON (topology-only; metric rules stated as unevaluated) |
| `--price-file` | Override the embedded price table |
| `--out-dir` | Directory to write scan artifacts into (created if absent; default `tellury-out/`) |

In machine-readable output the currency is always named: the JSON report carries a
top-level `currency` field (plus `currency_source`, `currency_requested`, and
`currency_mixed` when relevant), the CSV renames its money column to `monthly_waste_<code>`
for non-USD scans and appends `currency`, `currency_source`, `currency_requested`,
`currency_mixed` columns, and amounts keep the code next to the number in the table and
HTML forms.

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
pkg/rules/gcp/      the GCP rule implementations
pkg/output/         table, JSON, CSV, and HTML renderers
```

## Known issues

None affect a scan's results.

- **Multi-failure error text is dense.** When more than one metric fetch fails, the failures
  are joined with `errors.Join` — each stays individually inspectable via `errors.Is` — but
  they render as a single flat line rather than one per failure.
- **A raw CAI fixture can only evaluate topology rules.** This is by design and stated
  explicitly at the end of every fixture run (see [Offline mode](#offline-mode)). Use
  `--cache-file` or `graph export` for full-fidelity offline evaluation.
- **The HTML report needs `--out-dir`.** Terminal-only output still works without it (the
  directory is created only so nothing aborts mid-scan), but the report lives there; a CI
  job that wants the HTML must point `--out-dir` at something it can collect.

## Changelog

Release history is in [CHANGELOG.md](CHANGELOG.md). `tellury` follows
[semantic versioning](https://semver.org); while the major version is `0`, the CLI surface
and rule interface may change between minor releases.

## Contributing

Rules are the point, and community rules are the reason this is open source — see
[CONTRIBUTING.md](CONTRIBUTING.md) for what a good rule looks like. Before you write one,
run the contributor skill at `.claude/skills/write-a-tellury-rule/`: it teaches the
`NodeRule` interface method by method, the typed skip-code vocabulary, the registration
step and its silent-nothing trap, and the mandatory mutation check, with `old_snapshot`
as a complete worked example already in this repo. No CLA, nothing to sign.

## License

[Apache License 2.0](LICENSE). Copyright 2026 TypeOne Labs.

`tellury` is open source and stays that way. TypeOne Labs also builds commercial software
on this engine; that does not change the licence of anything in this repository.
