# Offline scanning

Two offline paths, neither of which makes an API call or needs credentials. They have
**different fidelity**, and a scan says which rules it could not evaluate rather than
letting an empty table read as "no waste".

**Pricing requires the live API.** An offline scan replays inventory from local JSON,
but pricing needs the Cloud Billing Catalog (GCP) or Price List API (AWS). Without a
live pricing connection, resources are found and reported as skipped (unpriced) rather
than carrying a dollar total. The `TELLURY_PRICE_FIXTURE` environment variable provides a
test-only pricing fixture for golden tests; it is not a supported way to price a real
scan.

## `--fixture` — raw Cloud Asset Inventory, topology only

Replays a captured Cloud Asset Inventory export: assets with their resource payloads, but
**no metric series** and **no pricing data**. It runs the full normalization, edge
extraction and hierarchy pass, so topology rules evaluate exactly as they would live.
Metric-dependent rules cannot — a raw export carries no Cloud Monitoring data. Pricing
rules skip with `no_price`.

```bash
tellury scan --gcp-project my-project \
  --fixture internal/cli/testdata/readme-assets.json --at 2024-01-20T00:00:00Z
```

```
No waste found.
Summary: projects/my-project — 1 project analyzed, 1 resource scanned, 5 rules evaluated, 0 findings, 1 resource skipped, 2ms

5 rule(s) could not be evaluated for lack of metric data: …
```

**EC2 instances are never priced offline.** `TELLURY_PRICE_FIXTURE` carries flat
per-SKU rates, which covers disks, snapshots and addresses. An instance price is
resolved by a targeted lookup keyed on (region, instance type, operating system)
and does not come from that table, so an offline scan reports every instance as
skipped (unpriced) and `underutilized_ec2` produces no findings. This is a
deliberate limit rather than an oversight: pricing needs the API, and building a
second offline pricing path would mean maintaining two answers to the same
question — which is what the embedded price tables did before they were removed.

With a price fixture (test-only):

```bash
TELLURY_PRICE_FIXTURE=pkg/pricing/gcp/testdata/price-fixture.json \
  tellury scan --gcp-project my-project \
  --fixture internal/cli/testdata/readme-assets.json --at 2024-01-20T00:00:00Z
```

```
RESOURCE            RULE         MONTHLY WASTE
disk/pd-standard-01 detached_disk        $8.00
----------------------------------------------
TOTAL               1 findings           $8.00
Summary: projects/my-project — 1 project analyzed, 1 resource scanned, 5 rules evaluated, 1 finding, 0 resources skipped, 2ms

2 rule(s) could not be evaluated for lack of metric data: no_lifecycle_policy, underutilized_instance
(use --cache-file from a live `scan` or an enriched `graph export` to evaluate them)
```

That last line is the point. It appears in the terminal, in the findings JSON as
`metrics_blocked`, and in the HTML report.

### Capturing your own fixture

`gcloud` calls the same `SearchAllResources` RPC tellury's live path uses, so its output
feeds `--fixture` unedited:

```bash
gcloud asset search-all-resources \
  --scope=projects/MY_PROJECT \
  --asset-types=compute.googleapis.com/Instance,compute.googleapis.com/Disk,compute.googleapis.com/Snapshot,compute.googleapis.com/Address,compute.googleapis.com/Network,storage.googleapis.com/Bucket \
  --read-mask='*' \
  --format=json \
  > cai-assets.json
```

`--read-mask='*'` is **mandatory**. Without it the payload omits the versioned resource
fields the normalizers read, and every asset arrives as an empty shell that no rule can
evaluate.

Replace `--scope` with `folders/123` or `organizations/456` for a wider capture.

## `--cache-file` — full-fidelity graph snapshot

Reads or writes the same in-memory graph a live scan builds, serialized **after**
enrichment, with every node's metric values, sample counts, coverage and window bounds.
A replay therefore drives **every** rule, metric-dependent ones included. Pricing still
requires the live API (or `TELLURY_PRICE_FIXTURE` for tests).

```bash
# First run: live scan, graph written back.
tellury scan --gcp-project my-project --cache-file snapshot.json

# Later runs: replayed verbatim — no API calls, same rules, same data.
tellury scan --cache-file snapshot.json
```

A replay needs no scope flag: the snapshot carries its own. It writes a fresh artifact
directory under the new run's timestamp, so a replay is auditable like any other scan.

Without a live pricing API connection, a replay will find resources but report them as
skipped (unpriced).

## `graph export`

Writes a snapshot without evaluating anything, which is useful for capturing a scope once
and iterating on rules against it:

```bash
tellury graph export --gcp-project my-project --out snapshot.json
tellury scan --cache-file snapshot.json
```

`--no-enrich-metrics` writes a topology-only snapshot: faster and free of Monitoring calls,
but metric-dependent rules will skip on replay, exactly as with a fixture.

## Which to use

| | `--fixture` | `--cache-file` |
|---|---|---|
| Source | Raw CAI JSON | tellury's own graph |
| Metrics | No | Yes, if enriched |
| Rules that can evaluate | Topology only | All |
| Portable to another machine | Yes | Yes |
| Produced by | `gcloud asset search-all-resources` | `tellury scan` or `graph export` |
| Pricing | Requires API or `TELLURY_PRICE_FIXTURE` | Requires API or `TELLURY_PRICE_FIXTURE` |

## Scan artifacts

Every scan — live, fixture or replay — writes a timestamped directory under `--out-dir`
(default `tellury-out/`), so a run nobody watched still records what it saw:

```
tellury-out/
  projects-my-project-20260807T123456.789012345Z/
    graph-projects-my-project.json      replayable snapshot
    findings-projects-my-project.json   the findings, as --format json prints them
    report-projects-my-project.html     self-contained HTML report
```

Consecutive runs never overwrite each other. The HTML report inlines its CSS and
JavaScript and fetches nothing at runtime, so it opens on a machine with no network and
survives being emailed.
