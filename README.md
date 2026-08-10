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

# The shipped fixture drives the whole pipeline offline — no credentials needed.
# --at pins the evaluation instant so this example reproduces on any day.
./tellury scan --gcp-project my-project --fixture internal/cli/testdata/readme-assets.json \
  --rules detached_disk --at 2024-01-20T00:00:00Z
```

```
RESOURCE            RULE         MONTHLY WASTE
disk/pd-standard-01 detached_disk        $8.00
----------------------------------------------
TOTAL               1 findings           $8.00
Summary: 1 project analyzed, 1 resource scanned, 1 rule evaluated, 1 finding, 0 resources skipped, 1ms
```

The `Summary:` line is the scan summary: how much ground the scan covered (projects
analyzed, resources scanned, rules evaluated), what it produced (findings, resources
skipped), and the scan's own wall-clock duration — the duration is live and varies run to
run (see [Scan summary](#scan-summary)). Columns are sized to their content, and a
single-project scan prints no `PROJECT` column; an organization-wide scan adds one.

Against a real project the command is the same minus the fixture:

```bash
gcloud auth application-default login
./tellury scan --gcp-project my-project
```

That exits `3` when findings are present — pass `--fail-on-findings=false` to keep the
exit code `0` (see [Exit codes](#exit-codes)). Read-only access is enough:
`roles/cloudasset.viewer` and `roles/monitoring.viewer`, plus `roles/billing.viewer` on the
billing account if you want figures in your own currency rather than USD
([full table](#permissions)). The fixture above replays raw Cloud Asset Inventory JSON
offline; see [Offline mode](#offline-mode) for exactly what that replay can and cannot
evaluate.

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
- **Five GCP rules** (see below).
- **Offline scanning** — `--fixture` replays Cloud Asset Inventory JSON and `--cache-file`
  replays a saved graph. Both make no API calls and need no credentials. The distinction
  between the two is explicit and honest: a fixture replay can only ever evaluate the
  topology-only rules, while a cached-snapshot replay drives every rule including the
  metric-dependent ones (see [Offline mode](#offline-mode)).
- **Output** — `table`, `json`, `csv`, with filtering and sorting.
- **Scan artifacts** — every scan leaves a directory of artifacts behind in `--out-dir`
  (default `tellury-out/`): a replayable graph snapshot, the findings JSON, and a
  self-contained HTML report. See [Scan artifacts](#scan-artifacts).
- **Progress reporting** — a long organization scan reports its phases on stderr
  (`--progress`, default `auto`: interactive terminals only). See
  [Progress reporting](#progress-reporting).

Not there yet:

- **AWS and Azure.** The provider seam exists — a provider declares its own scope flags
  and environment variables — but only GCP is implemented.
- **Rules beyond the initial five.** The engine is built for many more.

### Rules

| ID | Service | Severity | Detects |
|---|---|---|---|
| `detached_disk` | compute | medium | Persistent disks in a non-attached state, priced by type and size |
| `old_snapshot` | compute | low | Persistent disk snapshots at or beyond the 90-day retention window; billed on the snapshot's incremental storage bytes, not the source disk's size |
| `underutilized_instance` | compute | high | Instances significantly overprovisioned for their CPU load |
| `no_lifecycle_policy` | gcs | low | Buckets with no lifecycle management rules |
| `unused_reserved_ip` | compute | medium | External IP addresses reserved but attached to nothing; an unattached reserved address bills the full hourly rate while contributing nothing, so its entire monthly cost is waste |

`underutilized_instance` and `no_lifecycle_policy` read Cloud Monitoring series. Without
monitoring access they skip and say why — they never guess a value from missing data.
`detached_disk`, `old_snapshot`, and `unused_reserved_ip` are pure graph/attribute checks
and need no metrics. `tellury rules list` shows the catalogue; `tellury rules explain <id>`
prints a rule's full declaration.

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
and build time come from the Go toolchain's own VCS stamps:

```
tellury dev
  commit: 800addad7629657a67549cd07884f22bc08245f8
  built:  2026-08-10T08:29:23Z
```

For a release build, stamp the version too:

```bash
go build -ldflags "-X github.com/TypeOneLabs/tellury/internal/buildinfo.Version=v0.1.0" \
  -o tellury ./cmd/tellury
```

No code generation, no build tags, no Makefile.

## Run

```bash
tellury --help
```

```
Find and price cloud waste. Zero bloat.

Usage:
  tellury [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  graph       Inspect the ingested resource graph (debug)
  help        Help about any command
  rules       Inspect the rule catalogue
  scan        Scan a cloud scope for waste
  version     Print build information

Flags:
  -h, --help               help for tellury
      --log-level string   error|warn|info|debug (default "warn")
      --no-color           disable ANSI color
      --timeout duration   overall deadline (default 5m0s)

Use "tellury [command] --help" for more information about a command.
```

```bash
tellury rules list
```

```
ID                      PROVIDER  SERVICE  SEVERITY  METRICS  TITLE
detached_disk           gcp       compute  medium    0        Persistent disk is not attached to any instance
no_lifecycle_policy     gcp       gcs      low       1        Bucket has no lifecycle policy
old_snapshot            gcp       compute  low       0        Persistent disk snapshot is older than the retention window
underutilized_instance  gcp       compute  high      1        Instance is significantly overprovisioned for its CPU load
unused_reserved_ip      gcp       compute  medium    0        Reserved external IP address is not attached to anything
```

```bash
tellury rules explain detached_disk
```

```
detached_disk (gcp/compute)

  title:        Persistent disk is not attached to any instance
  severity:     medium
  origin:       native
  asset types:  compute.googleapis.com/Disk, compute.googleapis.com/Instance
  metrics:      (none)
  remediation:  gcloud compute disks snapshot NAME --zone ZONE --snapshot-names NAME-pre-delete && gcloud compute disks delete NAME --zone ZONE --quiet

A zonal or regional persistent disk with no attached instance still bills at the full provisioned-capacity rate. Snapshot and delete, or attach it.
```

```bash
tellury version
```

```
tellury dev
  commit: 800addad7629657a67549cd07884f22bc08245f8
  built:  2026-08-10T08:29:23Z
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
| `roles/browser` | the org, folder or project | Resolving a project to its billing account | Currency detection stops at its first step; figures are USD |
| `roles/billing.viewer` | the billing account | Reading that account's currency | Figures are reported in USD |

Live pricing needs **no** billing role: the Cloud Billing Catalog is readable by any
authenticated caller. The last two rows buy only the ability to report figures in the
currency you are actually invoiced in. Without them, pricing is still live — just quoted in
USD.

Currency detection takes two hops and each needs a different grant, which is why both rows
are listed: `roles/browser` supplies the `resourcemanager.projects.get` that resolves a
project to its billing account, and `roles/billing.viewer` then reads that account's
currency code. Missing either one falls back to USD. Note that GCP reports a project you
cannot see and a project that does not exist identically, as `PermissionDenied`, so a
fallback to USD is not by itself proof that a role is missing — check the project name too.

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

# Currency detection: hierarchy read, then the billing account's currency.
gcloud organizations add-iam-policy-binding ORG_ID \
  --member="serviceAccount:SA_EMAIL" --role="roles/browser"
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
other. This is the real layout a fixture scan leaves at the repository root:

```
tellury-out/
  projects-my-project-20260810T091503.962418143Z/
    findings-projects-my-project.json
    graph-projects-my-project.json
    report-projects-my-project.html
```

(the timestamp is when that particular scan ran; yours will differ).

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
  `metrics_blocked`, `projects_analyzed`, `resources_skipped`, `duration`) in the same
  shape `--format json` prints, saved to disk.
- **`report-<scope>.html`** — a **self-contained** HTML report. All CSS is inlined, there
  is no CDN reference and no runtime network fetch — the only JavaScript is inlined in the
  document itself. An operator can open it on an air-gapped machine, email it, or attach it
  to a ticket and it renders identically, and it stays readable with scripting disabled.

The HTML report reads in the order the questions occur:

1. **How much** — the total monthly waste as the headline figure, with the scan's scope,
   provider and window beneath it, and a banner for anything that qualifies the number: a
   non-USD currency, rules that could not be evaluated, or rule errors.
2. **Where it is concentrated** — waste by project as proportional bars, and waste by rule
   as a compact table. Both are aggregated from the findings themselves, so what they show
   always sums to the headline. A single-project scan omits the bars, where one full-width
   bar would carry no information.
3. **Why, and what to do** — every finding in one table, carrying its severity, confidence,
   the evidence behind its figure (including **price provenance**: which of `--price-file`,
   the live API, or the embedded table answered each SKU), and the remediation command.
   Text search, severity filters and a sort control are inline JavaScript; the table is
   fully written into the document, so filtering hides rows rather than fetching them.
4. **How much to trust it** — a collapsed scan-details block with the denominators:
   resources scanned, rules evaluated, resources skipped with their reasons, and duration.

Long reports show the first 50 rows with a button to reveal the rest; printing and
`<noscript>` both override that, so a printed or script-less report never silently omits a
finding.

The report header also carries the scan summary denominators (`N projects analyzed · N
resources scanned · N rules evaluated · N findings · N resources skipped · duration`), and
every value that came from a cloud API is HTML-escaped, so a hostile resource name cannot
turn the report into an attack surface. The same scan renders a byte-identical report.

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
(`requested via --currency` / `detected from the billing account`). A live scan that
detected EUR opens its table with a currency line like
`Prices are in EUR (detected from the billing account).`

Precedence, highest first: **explicit flag, then detection, then USD**.

A malformed code (not three letters) is rejected before the scan starts. A well-formed but
unsupported code fails at the Cloud Billing API with the currency named — never silently
falls back. The embedded fallback price table is **USD-only**: if it answers a non-USD
request (for example an offline `--fixture` replay asked for EUR), the report says so
loudly in every output format rather than handing you EUR-labelled figures that are
actually dollars:

```bash
./tellury scan --gcp-project my-project --fixture internal/cli/testdata/readme-assets.json \
  --rules detached_disk --currency EUR --at 2024-01-20T00:00:00Z
```

```
WARNING: prices are in USD, not the requested EUR.
The Cloud Billing catalogue did not answer in EUR and the embedded fallback table is USD-only.
These figures were NOT converted; reconcile them against the EUR bill by hand.

RESOURCE            RULE         MONTHLY WASTE
disk/pd-standard-01 detached_disk        $8.00
----------------------------------------------
TOTAL               1 findings           $8.00
Summary: 1 project analyzed, 1 resource scanned, 1 rule evaluated, 1 finding, 0 resources skipped, 1ms
```

### Offline mode

Both offline paths make no API calls and need no credentials. **The two paths have
different fidelity, and tellury says so explicitly at the end of every offline scan.**

#### `--fixture`: raw Cloud Asset Inventory (topology only)

`--fixture` takes a captured Cloud Asset Inventory export — raw assets with their resource
payloads, but **no metric series**. It is the closest the offline world gets to a live
ingestion: it runs the full normalization, edge extraction, and hierarchy pass, so every
topology-based rule (`detached_disk`, `unused_reserved_ip`, `old_snapshot`) can evaluate.
The metric-dependent rules (`underutilized_instance`, `no_lifecycle_policy`) **cannot** —
they need Cloud Monitoring series that a raw CAI export does not carry.

tellury does not let this look like "no waste". At the end of a fixture run, the report
states explicitly which rules could not be evaluated for lack of metrics:

```bash
./tellury scan --gcp-project my-project --fixture internal/cli/testdata/readme-assets.json \
  --at 2024-01-20T00:00:00Z
```

```
RESOURCE            RULE         MONTHLY WASTE
disk/pd-standard-01 detached_disk        $8.00
----------------------------------------------
TOTAL               1 findings           $8.00
Summary: 1 project analyzed, 1 resource scanned, 5 rules evaluated, 1 finding, 0 resources skipped, 734µs

2 rule(s) could not be evaluated for lack of metric data: no_lifecycle_policy, underutilized_instance
(use --cache-file from a live `scan` or an enriched `graph export` to evaluate them)
```

And when the fixture produces no finding at all, the summary still reports the ground the
scan covered, so an empty table means **nothing wasteful**, never **nothing scanned**:

```bash
./tellury scan --gcp-project my-project \
  --fixture pkg/rules/gcp/compute/old_snapshot/testdata/old-snapshot.json \
  --rules old_snapshot --min-waste 100 --at 2024-01-20T00:00:00Z
```

```
No waste found in projects/my-project (3 resources, 1 rules).
Summary: 1 project analyzed, 3 resources scanned, 1 rule evaluated, 0 findings, 2 resources skipped, 1ms
```

The `--min-waste 100` filtered away the fixture's $1.50 old-snapshot finding; the scan
still analyzed one project and three resources (two skipped: one `too_young`, one
`missing_attribute` — see `--explain-skips`). The JSON report carries the same
information under `metrics_blocked`. A fixture run still writes the artifact directory
(including the HTML report), and its findings JSON and HTML carry the same
`metrics_blocked` honesty.

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

With a fixture the same round-trip is fully offline, and both runs agree on the finding:

```bash
./tellury scan --gcp-project my-project --fixture internal/cli/testdata/readme-assets.json \
  --rules detached_disk --cache-file snap.json --at 2024-01-20T00:00:00Z
```

```
RESOURCE            RULE         MONTHLY WASTE
disk/pd-standard-01 detached_disk        $8.00
----------------------------------------------
TOTAL               1 findings           $8.00
Summary: 1 project analyzed, 1 resource scanned, 1 rule evaluated, 1 finding, 0 resources skipped, 2ms
```

```bash
./tellury scan --cache-file snap.json --rules detached_disk --at 2024-01-20T00:00:00Z
```

```
RESOURCE            RULE         MONTHLY WASTE
disk/pd-standard-01 detached_disk        $8.00
----------------------------------------------
TOTAL               1 findings           $8.00
Summary: 1 project analyzed, 1 resource scanned, 1 rule evaluated, 1 finding, 0 resources skipped, 947µs
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

Against the README fixture the export is fully offline (the fixture carries no metrics, so
nothing enriches) and prints its summary to stderr:

```bash
./tellury graph export --gcp-project my-project \
  --fixture internal/cli/testdata/readme-assets.json --out graph.json
```

```
wrote 2 nodes, 1 edges to graph.json
```

and the snapshot replays with the same finding:

```bash
./tellury scan --cache-file graph.json --rules detached_disk --at 2024-01-20T00:00:00Z
```

```
RESOURCE            RULE         MONTHLY WASTE
disk/pd-standard-01 detached_disk        $8.00
----------------------------------------------
TOTAL               1 findings           $8.00
Summary: 1 project analyzed, 1 resource scanned, 1 rule evaluated, 1 finding, 0 resources skipped, 621µs
```

Enrichment is on by default. If you only need topology (you are debugging edges, or the
monitoring API is unreachable), pass `--no-enrich-metrics` to write a snapshot whose replay
will state metric-dependent rules as "could not be evaluated for lack of metric data" —
the same explicit honesty the fixture path uses.

### Machine-readable output

The table is the human face; `--format json` and `--format csv` are the machine face.
Both are deterministic for a given scan and carry every field the table prints plus the
scan summary denominators. `tellury scan --help` is the authoritative flag reference:

```bash
tellury scan --help
```

```
Scan a cloud scope for waste

Usage:
  tellury scan [flags]

Examples:
  tellury scan --gcp-project my-gcp-project
  tellury scan --gcp-project p --rules detached_disk --format json
  tellury scan --currency EUR --gcp-project my-gcp-project   # price in EUR
  tellury scan --cache-file snap.json   # offline replay (full fidelity)
  tellury scan --fixture cai-assets.json   # offline replay (topology only)

Flags:
      --at string                 evaluation instant (RFC3339); default now. Makes age predicates reproducible
      --cache-file string         read graph from file if it exists, else write it
      --currency string           ISO 4217 currency code to price the scan in, e.g. EUR. Overrides auto-detection from the billing account; the default (also TELLURY_CURRENCY) is USD. A well-formed but unsupported code fails at the Cloud Billing API with the currency named, never silently falling back to USD
      --explain-skips             print the per-rule skip tally to stderr
      --fail-on-findings          exit 3 when findings exist (default true)
      --fixture strings           read assets from CAI JSON fixtures instead of the API
      --format string             table|json|csv (default "table")
      --gcp-folder string         scope: folder
      --gcp-organization string   scope: organization
      --gcp-project string        scope: project
  -h, --help                      help for scan
      --min-confidence float      hide findings below confidence
      --min-waste float           hide findings below $/month
      --out-dir string            directory to write scan artifacts into (created if absent) (default "tellury-out")
      --price-file string         path to a JSON price override file. Price precedence, highest first: --price-file override > live Cloud Billing Catalog API (cached for this scan) > embedded fallback table used when the API is unreachable or billing access is missing
      --progress string           auto|on|off: report scan phase progress to stderr. auto (default, also TELLURY_PROGRESS) reports only on an interactive terminal; on always reports (off a terminal it degrades to plain periodic lines, never ANSI); off silences it. Progress is a stderr status channel, independent of --log-level, and never touches stdout
      --provider string           cloud provider (default "gcp")
      --rules strings             rule IDs to run (default: all)
      --skip-rules strings        rule IDs to exclude
      --sort string               waste|resource|rule (default "waste")
      --window int                metric lookback window in days (7-30) (default 14)

Global Flags:
      --log-level string   error|warn|info|debug (default "warn")
      --no-color           disable ANSI color
      --timeout duration   overall deadline (default 5m0s)
```

```bash
./tellury scan --gcp-project my-project --fixture internal/cli/testdata/readme-assets.json \
  --rules detached_disk --format json --at 2024-01-20T00:00:00Z
```

```json
{
  "scope": "projects/my-project",
  "provider": "gcp",
  "generated_at": "2024-01-20T00:00:00Z",
  "window_days": 14,
  "findings": [
    {
      "rule_id": "detached_disk",
      "resource_id": "//compute.googleapis.com/projects/my-project/zones/us-central1-a/disks/pd-standard-01",
      "resource": "disk/pd-standard-01",
      "kind": "disk",
      "project": "my-project",
      "location": "us-central1-a",
      "severity": "medium",
      "monthly_waste_usd": 8,
      "confidence": 1,
      "evidence": [
        {
          "key": "size_gb",
          "value": "200"
        },
        {
          "key": "disk_type",
          "value": "pd-standard"
        },
        {
          "key": "disk_sku",
          "value": "pd-standard"
        },
        {
          "key": "status",
          "value": "READY"
        },
        {
          "key": "attached_instances",
          "value": "0"
        },
        {
          "key": "detached_days",
          "value": "19"
        },
        {
          "key": "age_basis",
          "value": "never_attached"
        },
        {
          "key": "unit_price_gib_month",
          "value": "$0.0400"
        },
        {
          "key": "price_source",
          "value": "embedded_fallback sku=pd-standard region=us-central1"
        }
      ],
      "remediation": "gcloud compute disks snapshot NAME --zone ZONE --snapshot-names NAME-pre-delete \u0026\u0026 gcloud compute disks delete NAME --zone ZONE --quiet"
    }
  ],
  "total_monthly_waste_usd": 8,
  "finding_count": 1,
  "resources_scanned": 1,
  "rules_evaluated": 1,
  "projects_analyzed": 1,
  "resources_skipped": 0,
  "duration": 696427
}
```

```bash
./tellury scan --gcp-project my-project --fixture internal/cli/testdata/readme-assets.json \
  --rules detached_disk --format csv --at 2024-01-20T00:00:00Z
```

```
resource,rule,monthly_waste_usd,severity,confidence,kind,project,location,resource_id,evidence
disk/pd-standard-01,detached_disk,8.00,medium,1.00,disk,my-project,us-central1-a,//compute.googleapis.com/projects/my-project/zones/us-central1-a/disks/pd-standard-01,size_gb=200; disk_type=pd-standard; disk_sku=pd-standard; status=READY; attached_instances=0; detached_days=19; age_basis=never_attached; unit_price_gib_month=$0.0400; price_source=embedded_fallback sku=pd-standard region=us-central1
```

The `duration` field is the scan's wall clock in integer nanoseconds and varies run to
run; everything else is deterministic for the same input and `--at`. The table form
shows the ten largest findings and, when a scan has more, says so and points at the HTML
report the scan wrote — the `TOTAL` row still sums every finding, not just the ten shown:

```bash
./tellury scan --gcp-project my-project --fixture internal/cli/testdata/readme-many-findings.json \
  --rules detached_disk --at 2024-01-20T00:00:00Z
```

```
RESOURCE   RULE         MONTHLY WASTE
disk/pd-01 detached_disk        $8.00
disk/pd-02 detached_disk        $8.00
disk/pd-03 detached_disk        $8.00
disk/pd-04 detached_disk        $8.00
disk/pd-05 detached_disk        $8.00
disk/pd-06 detached_disk        $8.00
disk/pd-07 detached_disk        $8.00
disk/pd-08 detached_disk        $8.00
disk/pd-09 detached_disk        $8.00
disk/pd-10 detached_disk        $8.00
-------------------------------------
TOTAL      12 findings         $96.00
2 of 12 findings omitted; full report: file:///home/averbuks/repos/typeone_engine/tellury/tellury-out/projects-my-project-20260810T091503.962418143Z/report-projects-my-project.html
Summary: 1 project analyzed, 12 resources scanned, 1 rule evaluated, 12 findings, 0 resources skipped, 1ms
```

(the `file://` path reflects the machine and timestamp the example was run on; yours will
differ, and the report the scan wrote always carries the full list).

### Scan summary

Every scan ends with a one-line summary — after the findings table, or after the
no-findings line when there is no table. It is the "what did the scan actually look at"
context: the denominators that tell an operator whether an empty findings table means
**nothing wasteful** (`projects analyzed > 0`) or **nothing scanned** (a broken scope with
zero projects):

```
Summary: 1 project analyzed, 1 resource scanned, 1 rule evaluated, 1 finding, 0 resources skipped, 1ms
```

Each figure is carried by the report, never re-measured at render time:

- **projects analyzed** — the graph's project container nodes, independent of the findings,
  so a zero-findings scan still reports the projects it looked at;
- **resources scanned** — the real resources (containers never count toward this);
- **rules evaluated** — the selected rule count;
- **findings** — the count after `--min-waste`/`--min-confidence`;
- **resources skipped** — the exact total `--explain-skips` breaks down per (rule, code);
- **duration** — the scan's own wall clock, threaded through the report; a replayed or
  fixture scan reports its real duration, and `--at` never changes it.

The same denominators appear in the JSON report (`projects_analyzed`, `resources_skipped`,
`duration` — the last as integer nanoseconds) and in the HTML report's header meta line.

### Progress reporting

A scan over an organization takes a while — ingestion, then metric enrichment across every
project, then pricing, then rule evaluation — and previously printed nothing until it
finished, so an operator could not tell a slow scan from a hung one. `--progress` fixes
that with one line per phase on **stderr**. Forced on (the default `auto` stays silent when
stderr is a pipe, file, or CI log), the README fixture's fast scan reports its two real
phases:

```bash
./tellury scan --gcp-project my-project --fixture internal/cli/testdata/readme-assets.json \
  --rules detached_disk --progress on --at 2024-01-20T00:00:00Z
```

```
tellury: asset discovery: started
tellury: asset discovery: done (391µs, 1 resource)
tellury: rule evaluation: started
tellury: rule evaluation: 1/1 rules (118µs)
tellury: rule evaluation: done 1/1 rules (247µs)
```

`detached_disk` needs no metrics and the offline fixture prices from the embedded table, so
only asset discovery and rule evaluation appear. A live organization scan reports all four
phases, in order:

- **asset discovery** — no known total (Cloud Asset Inventory is a paginated stream with no
  announced page count), so it shows `started`, then `done (12.3s, 1,204 resources)` using
  the ingested resource count;
- **metric enrichment** — the slow, deliberately-parallel per-project phase. Every
  completed `(metric key, project)` fetch from the bounded worker pool counts against the
  `metrics × projects` denominator (`137/300 fetches`);
- **pricing catalogue** — the live Cloud Billing catalogue loads lazily on the first price
  lookup (inside rule evaluation), so it surfaces as its own phase with a per-service count
  (`done 2/2 services`);
- **rule evaluation** — each completed rule from the bounded engine pool counts against the
  selected rule total (`2/5 rules`).

Progress is a status channel, not a log:

- **STDERR only.** stdout is the report and is piped into other tools, so `tellury scan
  --format json | jq` parses the same JSON with progress on or off — a progress line never
  touches the machine-readable stream.
- **No ANSI, ever.** When stderr is a terminal the lines print more often; when it is a
  pipe, file or CI log they degrade to plain periodic lines (throttled harder) or stay
  silent. There is no spinner and no carriage-return animation to garble a log file.
- **`auto` is the default** (also `TELLURY_PROGRESS=auto`): progress appears only on an
  interactive terminal, so a CI job sees nothing by default. `--progress on` forces it
  (degrading to plain lines off a terminal — useful when you *want* `2>err.log` to show
  phase lines); `--progress off` silences it.
- **It never slows the scan down.** Metric enrichment and rule evaluation are deliberately
  concurrent with bounded parallelism; the progress counter is lock-free (atomic
  increments and a throttled print), so reporting progress cannot serialize the work it
  reports on.

In machine-readable output the currency is always named: the JSON report carries a
top-level `currency` field (plus `currency_source`, `currency_requested`, and
`currency_mixed` when relevant), the CSV renames its money column to `monthly_waste_<code>`
for non-USD scans and appends `currency`, `currency_source`, `currency_requested`,
`currency_mixed` columns, and amounts keep the code next to the number in the table and
HTML forms.

#### Exit codes

`0` — clean, no findings. `3` — ran clean but findings are present (disable with
`--fail-on-findings=false`). Non-zero otherwise on error. `2` is a usage error.

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
