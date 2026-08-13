# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and `tellury`
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the major
version is `0`, the CLI surface and the rule interface may change between minor releases.

## [Unreleased]

## [0.5.0] — 2026-08-13

Azure is tellury's third cloud. Inventory, hierarchy and live pricing, with two rules.

**Upgrading:** nothing to do. GCP and AWS scans are byte-identical to 0.4.0 — this release is
additive. If you scan Azure, read [docs/azure-setup.md](docs/azure-setup.md) first, in
particular the difference between the Reader role and a least-privilege custom role.

### Added

- **Azure provider.** Inventory comes from Azure Resource Graph, one KQL query per
  subscription, projecting only the columns the rules read. Resource Graph returns resource
  properties directly, so unlike the AWS path there is no per-resource hydration call.
- **Four scopes.** `--azure-subscription`, `--azure-resource-group`,
  `--azure-management-group` and `--azure-tenant`, each with a `TELLURY_AZURE_*` environment
  variable. `--azure-resource-group` narrows a subscription scan and is applied as a filter
  on the Resource Graph query, so it costs no extra API calls; passing it without
  `--azure-subscription` is a usage error naming what to add. It is the first scope flag in
  the tool that depends on another.
- **Hierarchy.** Tenant, management group and subscription become container nodes, so waste
  rolls up through them exactly as it does through GCP's folders and AWS's OUs. A management
  group or tenant scan fans out one query per subscription rather than handing the whole
  scope to Resource Graph, so each subscription's outcome is reported separately and one the
  identity cannot read is named rather than silently missing from the total.
- **Live pricing from the Azure Retail Prices API**, which is public and unauthenticated —
  Azure pricing needs no credentials, no role and no API enablement, unlike both other
  clouds. Rates are filtered to Consumption, excluding the Reservation, DevTest, spot,
  low-priority and Windows rows the same query returns; a lookup that matches two distinct
  billable rows refuses to pick rather than taking the first.
- **`unattached_managed_disk`** — a managed disk attached to no virtual machine, priced by
  its provisioned tier, with a seven-day grace period so a disk freed moments ago is not
  reported as waste.
- **`unassociated_public_ip`** — a Standard public IP address associated with nothing.
- **[docs/azure-setup.md](docs/azure-setup.md)**, recording a permission set verified by
  assigning it to a service principal with no other rights and running a real scan.

### Changed

- The provider conflict check is now three-way and names which two providers collided.
- The owner column reads `SUBSCRIPTION` on Azure, alongside `ACCOUNT` on AWS and `PROJECT`
  on GCP.

### Known issue

On Azure, an identity that lacks read access to a resource **type** produces an empty scan
rather than an error: Resource Graph returns no rows instead of denying the query, so a
permissions gap is indistinguishable from a clean bill of health. This is a property of
Resource Graph, not of tellury, and it is why the setup guide recommends the built-in Reader
role unless someone owns keeping a custom role current.

### Notes on what is not here

Azure metrics. There is no Azure Monitor integration and therefore no Azure rightsizing rule.
AWS shipped inventory and configuration-only rules one release before CloudWatch arrived, and
that sequencing kept the work small enough to finish; Azure follows it.

## [0.4.0] — 2026-08-12

tellury reads metrics on AWS. The first metric-dependent AWS rule ships with it.

**Upgrading — action required.** The scanner needs three IAM permissions it did not need
before: `ec2:DescribeInstances`, `ec2:DescribeInstanceTypes` and `cloudwatch:GetMetricData`.
Without them `underutilized_ec2` cannot run; the scan still succeeds and says what it could
not evaluate, but it will find nothing. The full policy is in
[docs/aws-setup.md](docs/aws-setup.md).

Two other things to expect. `cloudwatch:GetMetricData` sits outside the CloudWatch free tier
at $0.01 per 1,000 metrics requested — a scan needs tens of thousands of instances before
that rounds to a cent, but it is not free. And a new rule means new findings, so an account
that exited `0` may now exit `3`; `--fail-on-findings=false` restores the old behaviour.

### Added

- **CloudWatch metric enrichment.** `pkg/metrics/aws` implements the same provider-agnostic
  seam GCP uses, so the cross-cloud metric vocabulary means one thing on both clouds.
  `cpu_utilization_p95` comes from `AWS/EC2 CPUUtilization` via `GetMetricData`, batched up
  to 500 queries per call, fanned out per account and region with bounded concurrency.
  CloudWatch reports CPU as a percentage and the graph stores a fraction, so the conversion
  happens once on the AWS side — the existing thresholds are written against fractions, and
  a missing conversion would have produced findings that looked entirely reasonable.
- **`underutilized_ec2`.** A running, non-spot EC2 instance whose p95 CPU leaves more than
  40% headroom. Recommends a smaller size in the same family, or stop/delete when the family
  has no smaller member. Auto Scaling group members are skipped with their own code: a
  group owns its members' size, so a per-member recommendation is advice nobody can take.
- **EC2 instance discovery.** `DescribeInstances` with shapes resolved live through
  `DescribeInstanceTypes`, including the full size ladder of each family present so a
  rightsizing candidate can be found. No embedded instance-type table — the same reasoning
  that removed the price tables in 0.3.0.
- **Live On-Demand instance pricing.** A targeted `GetProducts` lookup per
  (region, instance type, OS), cached for the scan. Instance prices are deliberately not
  preloaded like other product families: `Compute Instance` is the largest family in the
  price list, and fetching it whole was measured at over a minute without finishing.
  One filter table now feeds both the live request and the fixture matcher, so a wrong
  constant — `capacitystatus`, `tenancy`, `licenseModel` — fails a test instead of silently
  returning a real price for the wrong product.

### Changed

- The owner column (`ACCOUNT` on AWS, `PROJECT` on GCP) now appears whenever the scope can
  hold more than one owner, rather than when the findings happen to span several. An
  organization scan whose findings all landed in one account previously printed no column at
  all — exactly the case where the reader cannot infer the owner from the scope.
- The AWS rule for overprovisioned instances is `underutilized_ec2`. It briefly carried a
  different ID during development; no released version used it.

### Fixed

- `Provider.Sizer()` returned nil, making the rightsizing branch unreachable: every
  overprovisioned instance would have been reported as stop/delete with its full cost as
  waste, even where a smaller size existed.
- EC2 tags never reached node labels, so the Auto Scaling guard read an always-absent label
  and every ASG member would have received a recommendation its operator cannot apply.
- Metric enrichment treated an unknown caller identity as "every account is my own", the
  opposite of what ingestion does. A transient `sts:GetCallerIdentity` failure would have
  had an organization scan query member accounts with the caller's own credentials and skip
  every metric rule without saying why.
- The price fixture loader wrote two maps outside the lock that price lookups read under it.
- A missing `state` attribute was reported as "not running", telling the operator an
  instance was stopped when the truth was that its state had not parsed.
- Removed the GCP `mem_utilization_p95` spec. No rule declared it, so it was never fetched;
  had one declared it, `percent_used` (0–100) and `balloon/ram_used` (bytes) were both
  registered as a ratio clamped to 1, so every instance would have reported 100% memory
  used. Neither cloud publishes guest memory without an agent in the VM, and the correct
  units are recorded in a comment for whoever re-adds it.

### Documentation

- The quick start covers AWS and GCP evenly instead of leading with GCP, and organization
  scanning is shown for both.
- `docs/cli.md` listed only the `--gcp-*` scope flags and still claimed GCP was the only
  provider implemented, three releases after AWS shipped.
- `docs/aws-setup.md` documents the three new permissions.
- `docs/offline.md` states plainly that EC2 instances are not priced offline.

## [0.3.0] — 2026-08-12

AWS grows from single-account scans to whole organizations, and pricing becomes live-only.

**Upgrading:** `--price-file` is gone and there is no embedded price table. A scan without
access to the pricing API now reports resources as skipped and unpriced where it previously
printed figures from a hand-maintained table. That table is why four pricing defects went
unnoticed — each time a live lookup failed, a plausible number appeared in its place.

### Added

- **AWS organization and organizational-unit scopes.** `--aws-organization` and
  `--aws-organizational-unit` walk the Organizations tree — `ListRoots`, then recursing
  `ListOrganizationalUnitsForParent` and `ListAccountsForParent` — and every OU and account
  becomes a container node, so a finding rolls up through account and OU exactly as a GCP
  finding rolls up through project and folder. Member accounts are reached by assuming a
  role, configurable rather than hard-coded to the `OrganizationAccountAccessRole`
  convention. Accounts that cannot be reached are reported by name and count in both the
  summary and the JSON: a scan that quietly skips part of an organization and prints a total
  is worse than one that fails, because the number means something other than it appears to.
- **Resource Explorer discovery**, replacing the blind region sweep. A scan asks which
  regions of an account actually hold the resource types the selected rules need, then
  hydrates only those. Where Resource Explorer is unavailable the full sweep still runs, per
  account rather than for the whole scan, and the scan reports which path each account used.
- **Live AWS pricing** through the Price List API, resolving EBS capacity, IOPS and
  throughput and the hourly public-IPv4 charge.

### Changed

- **Pricing is live-only.** The embedded price tables and `--price-file` are gone. A price
  that cannot be resolved skips the resource with `SkipNoPrice` rather than being guessed
  at, and the scan reports what it could not price.

  This is a behaviour change worth understanding before upgrading: a scan without pricing
  API access now reports resources found and skipped as unpriced, where it previously
  printed figures from a hand-maintained table. That table was the reason four pricing
  defects went unnoticed — each time a live lookup silently failed, a plausible number
  appeared in its place. `--fixture` still replays inventory offline, but pricing needs the
  API.
- The AWS price fetch is filtered by product family instead of paging the entire EC2
  catalogue, which took over 1m39s and had not finished; the same scan now takes under three
  seconds. The filter is verified rather than trusted — a load that indexes nothing is
  treated as a broken filter and falls back to the unfiltered fetch, because a filter on a
  wrong attribute name returns empty with no error.

### Fixed

- **Three of four AWS price kinds never resolved live** and silently used the embedded
  table. IOPS is published under product family `System Operation` and throughput under
  `Provisioned Throughput`, neither of which was fetched; the public-IPv4 charge is not an
  EC2 product at all but an `AmazonVPC` one with no product family, keyed on a
  region-prefixed usagetype. Throughput also needed converting: the catalogue quotes
  $40.96 per GiBps-month against the $0.04 per MiBps-month a rule works in, so indexing it
  directly would have overstated throughput a thousandfold.
- Prices resolved by display name through a hand-maintained table that held
  `Europe (Ireland)` where the API says `EU (Ireland)`, so every Irish rate was discarded and
  the scan reported plausible figures that were not the live ones. The region now comes from
  the `regionCode` the API already supplies.
- The recorded price fixture disagreed with the API it stood in for — it placed gp3 IOPS and
  throughput under `Storage` and invented an `Elastic IP` family, neither of which
  GetProducts returns — so the whole pricing suite validated fiction. It is now recorded from
  the live API.
- An AWS scan with no credentials waited for the EC2 instance metadata service to time out
  before failing. Credentials resolve up front and the error names what is missing.
- `unattached_ebs_volume` did not round, so live prices rendered as `17.599999999999998`.
- `docs/aws-setup.md` asked operators to grant `resource-explorer-2:ListIndexes`, which the
  code never calls.
- `--aws-organization` was never checked against the organization the credentials belong to.
  `DescribeOrganization` takes no ID — it answers for the caller — so a mistyped value
  traversed a different organization while the report carried the requested name. A scan
  labelled `organizations/o-notreal` was scanning `o-44tzls6k3v`. The mismatch is now an
  error naming both.

## [0.2.0] — 2026-08-11

AWS is a second provider, and the resource graph gained a region tier that both providers
use. A scan still runs one provider at a time.

### Added

- **AWS**, scoped with `--aws-account` (and `TELLURY_AWS_ACCOUNT`). Credentials come from the
  standard AWS chain — environment, shared credentials file, `AWS_PROFILE`, instance role —
  with no credentials flag and no key file of tellury's own. Three read-only permissions are
  the entire surface it calls: `ec2:DescribeRegions`, `ec2:DescribeVolumes`,
  `ec2:DescribeAddresses`. See [docs/aws-setup.md](docs/aws-setup.md).
- Two AWS rules, neither needing CloudWatch: `unattached_ebs_volume` (a volume in state
  `available`, priced by capacity plus any IOPS and throughput above the type's included
  allowance) and `unassociated_eip` (an Elastic IP that bills hourly for doing nothing).
- `--aws-regions` narrows the region sweep. Without it a scan covers every region the
  account has enabled, discovered through `DescribeRegions` rather than a fixed list, so
  regions the account never opted into are never queried. An availability-zone spelling like
  `eu-west-1a` is accepted and flattened to its region. Every scan reports the regions it
  actually covered.
- Scope flags are mutually exclusive across providers: passing a `--gcp-*` and an `--aws-*`
  flag together fails before any work with a usage error naming both. The provider is
  inferred from the scope flag, so `--provider` is not needed.
- A **region tier** in the resource graph, between a resource and its project or account:
  `resource → region → project → folder → organization`. Region nodes are per-project or
  per-account, so waste rolls up by place as well as by owner, and the HTML report gained a
  waste-by-region breakdown — suppressed when everything is in one region, where a single
  full-width bar carries no information.

### Changed

- Location strings are canonicalised once, at ingestion, through a single function shared
  with pricing. Cloud Storage reports `EU` and `EUROPE-WEST4` where Compute reports `eu` and
  `us-central1`; treating those as different places split a region's rollup across two nodes
  that both looked plausible.
- Graph snapshots are version 3. A version 2 snapshot replays with its region tier rebuilt
  from each node's location, so existing `--cache-file` snapshots keep working.
- The README was rewritten and reference material moved into [docs/](docs/): a CLI
  reference, per-provider setup, and offline scanning. The rule-writing guide moved out of a
  vendor-specific directory to [docs/writing-a-rule.md](docs/writing-a-rule.md), with
  repository conventions in `AGENTS.md`.
- A findings table names its scope on the summary line. The `PROJECT` column only appears
  when findings span several projects, so a single-project scan previously named the project
  nowhere at all, and a table pasted into a ticket could not be attributed.

### Fixed

- **EBS volumes were priced about $20/month too high, each.** gp3 includes 3000 IOPS and
  125 MiB/s at no charge and every gp3 volume reports those figures, so billing the raw
  provisioning charged the free allowance: a real 1 GiB volume was reported at $20.08 when
  it costs $0.08. io1 and io2 have no allowance and are unaffected. The AWS price list does
  not encode the allowance — its dimension reads "per provisioned IOPS-month" — so this
  could not be derived from the API, and two tests asserted the overcharge rather than
  catching it.
- The region canonicaliser mangled AWS regions. It treated any single-character trailing
  segment as a zone suffix, which is right for the `a` in `us-central1-a` and wrong for the
  `1` in `us-east-1`, turning the most-used AWS region into one that has never existed. The
  three shapes are now distinguished by what the trailing segment is rather than how long it
  is.
- An AWS scan with no credentials waited for the EC2 instance metadata service to time out
  and then reported a failure about IMDS. Credentials are resolved up front and the error
  names what is missing. This also stopped `go test ./...` reaching the network.
- A rule with no resources to evaluate is no longer reported as blocked for lack of metric
  data. An organization with no compute instances was told `underutilized_instance` could
  not be evaluated, which sent operators looking for a Monitoring permission they did not
  need.

## [0.1.4] — 2026-08-10

A scan now says what it looked at and shows its progress while it runs, and the HTML report
has been rebuilt around how a cost report is actually read.

### Added

- Every scan prints a summary: projects analyzed, resources scanned, rules evaluated,
  findings, resources skipped, and wall-clock duration — in the table and in JSON. A waste
  total means little without the denominator, and an empty result reads very differently
  once you can see whether the scope resolved 4 resources or 4,000.
- `--progress auto|on|off` reports each phase of a scan on stderr while it runs: asset
  discovery, metric enrichment, pricing catalogue, rule evaluation. Stdout stays clean, so
  `--format json` still pipes into a parser; no ANSI or carriage returns are written when
  stderr is not a terminal, so redirected logs stay readable. Default `auto` means
  interactive terminals only.
- The HTML report's findings table carries severity, confidence, evidence and remediation
  inline, with text search, severity filters and sorting. Every finding is in the table —
  the row limit is a display convenience that printing and `<noscript>` both override, so a
  printed report never silently omits findings.

### Changed

- The HTML report is rebuilt. It leads with the total, then where the waste is concentrated
  (waste by project, waste by rule), then every finding with the reasoning behind it. The
  collapsible hierarchy tree is gone: it answered the same question as a ranked list while
  taking longer to read, and the summaries are derived from the findings alone rather than a
  graph traversal — which also removes the rendering path responsible for both
  total-mismatch defects fixed in 0.1.1.
- README examples are regenerated from real runs rather than retyped.

### Fixed

- A rule with no resources to evaluate was reported as blocked for lack of metric data. An
  organization with no compute instances was told `underutilized_instance` could not be
  evaluated, which is false — no metric would have changed the answer, because there was
  nothing to measure — and it sends an operator hunting for a Monitoring permission they do
  not need. A rule is now reported as blocked only when it had candidate resources and the
  data they needed was absent.

## [0.1.3] — 2026-08-10

Output you can read and figures you can reconcile. Terminal output no longer truncates the
identifiers you need to act on, and prices are reported in your billing account's own
currency at full precision.

### Added

- `--currency` (and `TELLURY_CURRENCY`) prices a scan in an ISO 4217 currency: the Cloud
  Billing Catalog API converts the whole catalogue server-side (`ListSkusRequest.
  CurrencyCode`), so every figure — findings, totals, rollups — is expressed in that
  currency rather than converted afterwards. Without a flag, tellury best-effort detects a
  billing account's currency (fixed at creation) from any project in scope and falls back
  to USD. Every output format names the currency actually in use and how it was decided;
  the embedded USD fallback table answering a non-USD request is disclosed loudly as a
  WARNING. A malformed code is rejected before the scan starts; a well-formed but
  unsupported code fails at the API naming the currency. Default behaviour (no flag) is
  unchanged: USD, byte-identical output.
- The table prints the ten largest findings, then names how many were omitted and gives a
  `file://` link to the HTML report. The total still sums every finding, not the ten shown.
  `json` and `csv` output remain complete, since tools consume them.

### Changed

- Table columns size to their widest value instead of fixed widths, so project and resource
  identifiers appear in full. A truncated project id is not merely ugly — `alpha-da…` cannot
  be pasted into a `gcloud` command, and during this project a truncated name was misread as
  a project that does not exist.
- Documented every IAM role tellury uses and what each one buys, including that live pricing
  needs no billing role at all, and that impersonation additionally requires
  `roles/iam.serviceAccountTokenCreator` on the caller.

### Fixed

- Live prices were truncated to whole cents. Cloud Billing expresses a price as whole units
  plus nanos, and the catalogue parser discarded everything below a cent — so coldline
  storage ($0.004/GiB-month) and custom RAM ($0.004446/GiB-hour) both truncated to ZERO and
  were priced free, and a vCPU-hour lost about 10% of its value. It went unnoticed because
  the USD SKUs anyone happened to verify land on round cents; every non-USD scan was wrong
  by construction, since a converted rate almost never does. A real EUR snapshot rate of
  0.043890 became 0.04, understating the bill by 9%.
- Evidence hardcoded a `$` into every money value, so a scan priced in EUR rendered its
  table correctly as `1.25 EUR` while its own evidence read `$0.0439` for the same figure.
  Money in evidence now follows the currency the prices are actually in.
- The TOTAL row squeezed its finding count into the PROJECT column and truncated it to
  `9 findin…`. The test covering it had been widened to accept the truncation rather than
  the defect being fixed; it now asserts the exact cell contents.

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

[Unreleased]: https://github.com/TypeOneLabs/tellury/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/TypeOneLabs/tellury/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/TypeOneLabs/tellury/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/TypeOneLabs/tellury/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/TypeOneLabs/tellury/compare/v0.1.4...v0.2.0
[0.1.4]: https://github.com/TypeOneLabs/tellury/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/TypeOneLabs/tellury/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/TypeOneLabs/tellury/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/TypeOneLabs/tellury/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/TypeOneLabs/tellury/releases/tag/v0.1.0
