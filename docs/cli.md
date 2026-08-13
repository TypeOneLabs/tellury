# CLI reference

Every command, flag and exit code. For a short introduction see the
[README](../README.md).

## Commands

| Command | Purpose |
|---|---|
| `tellury scan` | Scan a scope for waste |
| `tellury rules list` | List registered rules |
| `tellury rules explain <id>` | Print a rule's full declaration |
| `tellury graph export` | Write a resource-graph snapshot |
| `tellury version` | Print version, commit and build time |

## Global flags

| Flag | Default | Meaning |
|---|---|---|
| `--log-level` | `warn` | `error`, `warn`, `info`, `debug`. Diagnostics on stderr. |
| `--no-color` | off | Disable ANSI colour. |
| `--timeout` | `5m` | Overall deadline for the run, including every API call. |

## `tellury scan`

### Scope

Exactly one scope is required. Flags take precedence over environment variables.

| Flag | Environment | Scope |
|---|---|---|
| `--gcp-project` | `TELLURY_GCP_PROJECT` | One project |
| `--gcp-folder` | `TELLURY_GCP_FOLDER` | A folder and everything under it |
| `--gcp-organization` | `TELLURY_GCP_ORGANIZATION` | An organization and everything under it |
| `--aws-account` | `TELLURY_AWS_ACCOUNT` | One account |
| `--aws-organizational-unit` | `TELLURY_AWS_ORGANIZATIONAL_UNIT` | An OU and every account under it |
| `--aws-organization` | `TELLURY_AWS_ORGANIZATION` | An organization and every account in it |
| `--azure-subscription` | `TELLURY_AZURE_SUBSCRIPTION` | One subscription |
| `--azure-resource-group` | `TELLURY_AZURE_RESOURCE_GROUP` | One resource group (requires `--azure-subscription`) |
| `--azure-management-group` | `TELLURY_AZURE_MANAGEMENT_GROUP` | A management group and every subscription under it |
| `--azure-tenant` | `TELLURY_AZURE_TENANT` | A tenant and every subscription in it |

Mixing two providers' scope flags is a usage error: `--gcp-project` with `--aws-account`
exits `2` naming both providers and asking for one at a time.

`--azure-resource-group` narrows a subscription scope rather than standing alone. Passing it
without `--azure-subscription` exits `2`; it is the only scope flag that depends on another.

`--provider` (`gcp`, `aws`, `azure`) is inferred from the scope flags and rarely needs
setting; with no scope flags at all it defaults to `gcp`.

**GCP.** The scope is passed to Cloud Asset Inventory's `SearchAllResources` as its parent.
Scanning a folder or organization builds the whole hierarchy from that one result — no
separate Resource Manager calls.

**AWS.** An organization or OU scope is expanded through the Organizations API into the
accounts beneath it, and each is scanned in turn. The caller's own account is scanned
directly; every other account is reached by assuming a role in it (see `--aws-role-name`).
An account that cannot be reached is reported in the account outcomes rather than failing
the scan.

**Azure.** Inventory comes from Azure Resource Graph, one KQL query per subscription. A
management-group or tenant scope is expanded through the management-groups API and each
subscription is queried separately, so a subscription the identity cannot read is reported as
unreachable rather than silently omitted. A resource-group scope adds a filter to the same
query and costs no extra calls. See [Azure setup](azure-setup.md) for the permissions —
notably that a missing resource-type permission yields an empty result rather than an error.

### AWS-specific flags

| Flag | Default | Meaning |
|---|---|---|
| `--aws-regions` | every region enabled for the account | Regions to sweep. An availability-zone form like `us-east-1a` is accepted and flattened to its region. |
| `--aws-role-name` | `OrganizationAccountAccessRole` | IAM role to assume in member accounts during an organization or OU scan. |

Narrowing `--aws-regions` is the single biggest lever on scan time: without it every enabled
region is swept for every account in scope.

### Selecting rules

| Flag | Meaning |
|---|---|
| `--rules` | Run only these rule IDs (comma-separated). Default: all. |
| `--skip-rules` | Exclude these rule IDs. |

Rule selection also narrows what is fetched: only the asset types and metric keys the
selected rules declare are requested, so `--rules detached_disk` does not pay for
Cloud Monitoring calls.

### Filtering output

| Flag | Default | Meaning |
|---|---|---|
| `--min-waste` | none | Hide findings below this monthly amount. |
| `--min-confidence` | none | Hide findings below this confidence (0–1). |
| `--sort` | `waste` | `waste`, `resource` or `rule`. Applies to terminal, JSON and CSV; the HTML report is always waste-descending. |

These filter what is **reported**, not what is evaluated. A rule's own noise floor
(`MinWasteUSD`) is applied earlier and independently.

### Time and metrics

| Flag | Default | Meaning |
|---|---|---|
| `--window` | `14` | Metric lookback in days, 7–30. |
| `--at` | now | Evaluation instant (RFC3339). Pins age-based predicates so a scan is reproducible. |

### Pricing

| Flag | Environment | Meaning |
|---|---|---|
| `--currency` | `TELLURY_CURRENCY` | ISO 4217 code to price in, e.g. `EUR`. Overrides auto-detection. |

Pricing comes from the live catalog API: Cloud Billing Catalog for GCP, Price List API
(pricing:GetProducts) for AWS, Retail Prices for Azure — the last of which is public, so
Azure pricing needs no credentials. There is no embedded fallback table. A price that cannot be
resolved makes the rule skip the resource with `SkipNoPrice` — it never guesses a dollar
figure.

A scan that cannot reach the pricing API (no credentials, no billing access, or offline
without `TELLURY_PRICE_FIXTURE`) reports all resources as skipped (unpriced). Every output
format carries the skip reason so the operator knows why no findings priced.

`TELLURY_PRICE_FIXTURE` is a test-only environment variable pointing at a recorded price
response file. It is not a user-facing flag and is not documented as a way to price a real
scan.

### Output

| Flag | Default | Meaning |
|---|---|---|
| `--format` | `table` | `table`, `json` or `csv`, on stdout. |
| `--out-dir` | `tellury-out` | Directory for scan artifacts. |
| `--explain-skips` | off | Print the per-rule skip tally to stderr. |
| `--progress` | `auto` | `auto`, `on`, `off`. See below. |
| `--fail-on-findings` | `true` | Exit `3` when findings exist. |

`table` shows the ten largest findings and links to the HTML report for the rest. `json`
and `csv` always contain every finding — they are consumed by other tools.

The table carries an owner column — `ACCOUNT` on AWS, `PROJECT` on GCP — whenever the scope
can hold more than one owner. A single `--aws-account` or `--gcp-project` scan omits it,
since every finding belongs to the scope named on the command line.

### Offline input

| Flag | Meaning |
|---|---|
| `--fixture` | Read assets from Cloud Asset Inventory JSON instead of the API. |
| `--cache-file` | Read the graph from this file if it exists, otherwise write it. |

The two are not equivalent. See [Offline mode](offline.md).

## `tellury graph export`

Writes a resource-graph snapshot that `scan --cache-file` replays.

| Flag | Default | Meaning |
|---|---|---|
| `--out` | stdout | Output file. |
| `--no-enrich-metrics` | off | Topology only, no Cloud Monitoring series. Metric-dependent rules will skip on replay. |

It also takes the scope, `--rules`, `--skip-rules`, `--window` and `--fixture` flags, which
have the same meaning as on `scan`.

## Progress

`--progress` (or `TELLURY_PROGRESS`) reports each scan phase — asset discovery, metric
enrichment, pricing catalogue, rule evaluation — on **stderr**:

```
tellury: asset discovery: done (689ms, 17 resources)
tellury: metric enrichment: done 6/6 fetches (1.263s, 3 projects)
tellury: pricing catalogue: 1/2 services (7.977s)
```

- `auto` (default) reports only when stderr is an interactive terminal.
- `on` always reports; off a terminal it degrades to plain periodic lines and never emits
  ANSI escapes or carriage returns, so redirected logs stay readable.
- `off` silences it.

Progress never touches stdout, so `--format json` still pipes into a parser with progress
enabled. It is a status channel independent of `--log-level`.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Ran clean, no findings. |
| `2` | Usage error — a bad flag or an invalid value. |
| `3` | Ran clean, findings present. Disable with `--fail-on-findings=false`. |
| other | Error. |

## Environment variables

| Variable | Equivalent flag |
|---|---|
| `TELLURY_GCP_PROJECT` | `--gcp-project` |
| `TELLURY_GCP_FOLDER` | `--gcp-folder` |
| `TELLURY_GCP_ORGANIZATION` | `--gcp-organization` |
| `TELLURY_AWS_ACCOUNT` | `--aws-account` |
| `TELLURY_AWS_ORGANIZATIONAL_UNIT` | `--aws-organizational-unit` |
| `TELLURY_AWS_ORGANIZATION` | `--aws-organization` |
| `TELLURY_AZURE_SUBSCRIPTION` | `--azure-subscription` |
| `TELLURY_AZURE_RESOURCE_GROUP` | `--azure-resource-group` |
| `TELLURY_AZURE_MANAGEMENT_GROUP` | `--azure-management-group` |
| `TELLURY_AZURE_TENANT` | `--azure-tenant` |
| `TELLURY_CURRENCY` | `--currency` |
| `TELLURY_PROGRESS` | `--progress` |

A flag always overrides its environment variable.
