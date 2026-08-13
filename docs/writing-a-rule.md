# Write a tellury rule (NodeRule)

How to write one tellury cost-waste rule from a cold start: implement the `NodeRule`
interface, wire it into the registry so it actually runs, prove with a mutation check that
its test tests the condition, and ship it.

This document is written to be followed by a person or by a coding agent — the steps are
the same, and every command in it was run to produce the output shown. The
worked example — `old_snapshot`, a rule that flags persistent-disk snapshots
older than the retention window and prices their flat per-GiB-month storage cost
on the billable, deduplicated bytes (`storage_bytes`) — is **real code in this repo**:
`pkg/rules/gcp/compute/old_snapshot/`. You can read it, run its tests, and copy its shape.

## 0. Read these first

Do not guess at APIs. The files below are the whole surface a rule touches; read
them before writing anything.

- `pkg/rules/noderule.go` — the `NodeRule` interface and its supporting types (`Guard`, `NodeContext`, `CostBranch`).
- `pkg/rules/adapt.go` — the evaluation skeleton every `NodeRule` gets for free (`AdaptNodeRule` / `evalNodeRule`). This is what you are NOT re-implementing.
- `pkg/rules/skip.go` — every shared `SkipCode`. You will pick from this list.
- `pkg/rules/priceevidence.go` — `PriceEvidence` / `PriceEvidenceFor`, the one way to attach price provenance to a finding.
- `pkg/graph/node.go` — the `Node` you receive: `Str`/`Num`/`Bool`/`Time`/`Metric`/`MetricOK`/`Exempt`, `Attrs`, `Metrics`, `Labels`.
- `pkg/graph/edge.go` — edge kinds, if your guard needs "is this attached?" (`EdgeAttachedTo`, `EdgeUses`, ...).
- `pkg/pricing/pricer.go` — `Pricer` (`UnitPrice`, `MonthlyCost`), `Kind`, `HoursPerMonth`, `ErrNoPrice`.
- One shipped rule as a model: `pkg/rules/gcp/compute/unused_reserved_ip/rule.go` (smallest) and `pkg/rules/gcp/compute/detached_disk/rule.go` (richest).
- One shipped end-to-end example as a model: `internal/cli/old_snapshot_fixture_test.go`.

Every command below was run to produce the output shown. Do not trust a
command's output until you have run it yourself.

## 1. The interface, method by method

A `NodeRule` is a rule the engine evaluates **one node at a time**. The engine
owns the whole evaluation skeleton; you supply the decisions. Here is each method,
what it returns, and why.

```go
type NodeRule interface {
    Meta() Meta

    Kind() graph.ResourceKind

    Guards() []Guard

    Cost(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error)

    MinWasteUSD() float64

    EvidenceKeys() []string

    ExtraEvidence(n *graph.Node, nc *NodeContext, branch CostBranch) []Evidence
}
```

### `Meta() Meta`
The rule's static declaration: `ID`, `Provider`, `Service`, `Title`,
`Description`, `Severity`, `RequiredAssetTypes`, `RequiredMetrics`,
`Remediation`, `Origin`. Returns the struct, which the engine reads for the
registry, the scan plan, and the report.

- `ID` **must equal the package basename** (`old_snapshot` package → ID `old_snapshot`). `pkg/rules/all/all_external_test.go` enforces this 1:1 mapping; breaking it fails the registration test.
- `RequiredAssetTypes` drives the server-side scan filter — declare exactly the CAI type(s) your rule iterates, e.g. `[]string{"compute.googleapis.com/Snapshot"}`.
- `RequiredMetrics` lists `metrics.Key` values. If your rule is pure attribute/age (no metrics), return `nil` — the scan then skips metric enrichment entirely for your rule, which is faster and offline-friendly.
- `Remediation` is the actionable `gcloud` command, shown on the finding.

### `Kind() graph.ResourceKind`
The single `graph.ResourceKind` the engine iterates: `KindInstance`, `KindDisk`,
`KindSnapshot`, `KindBucket`, `KindAddress`, ... Returns the kind so the engine
can call `Graph.ByKind` for exactly your resource class. The engine hands you one
node of this kind at a time.

### `Guards() []Guard`
Ordered predicates. Returns a slice of `Guard{SkipCode, Name, Check}`; the engine
evaluates them in slice order and **the first `Check` that returns `false`
terminates the chain**, recording that guard's `SkipCode` for the node. This is
how a scan stays auditable: every skipped node carries a typed reason.

- Each guard must have a **stable, distinct `Name`** (`"old_enough"`,
  `"billable_bytes_present"`, ...) so a future `--explain-skips` enhancement can say
  exactly which check failed.
- Each guard must return a **non-empty typed `SkipCode`** — never `""`.
- A guard may write computed values into `nc` (`nc.Set("age_days", days)`) for
  `Cost` and `ExtraEvidence` to read. The engine allocates a fresh `NodeContext`
  per node, so values never leak between nodes. **Compute a value once, in the
  guard, and read it downstream** — that is the entire data-flow contract.
- Guards are pure functions of `(n, nc, p)`: no network, no disk, no randomness.

### `Cost(ctx, n, nc, p) ([]CostBranch, error)`
The pricing step. Returns one or more `CostBranch{Waste, Confidence, Label}` —
alternative ways to price the node's waste. The engine drops branches whose
`Waste < MinWasteUSD()`, picks the highest-waste survivor, and turns it into a
`Finding`. If no branch survives — including `Cost` returning `nil, nil` — the
node is skipped as `SkipBelowMinWaste`.

- **A price lookup failure MUST be returned as an error, never assumed to be
  $0** (Invariant I4). The engine catches the error and records `SkipNoPrice`.
- Read raw attributes from `n` directly; read guard-computed values from `nc`.
- Give every branch a `Label` (e.g. `"delete_snapshot"`, `"rightsize"`,
  `"stop_delete"`) — it shows up in diagnostics.
- An "idle/unattached resource with a flat cost" (the shape of this guide's
  example) has exactly one branch: `billable quantity × flat monthly unit rate`,
  confidence documented, no partial component.

### `MinWasteUSD() float64`
The per-rule noise floor. Returns a constant (the shipped rules use `$0.10`).
Branches below it are dropped *before* a finding is constructed. This is the
engine's job — never implement the floor check yourself in `Cost`.

### `EvidenceKeys() []string`
Node attribute and/or metric keys the engine auto-collects as evidence. Each key
is looked up first in `n.Attrs` (rendered `%v`), then in `n.Metrics` (rendered
`%g`, sample-positive only); keys not found are silently omitted. Returns the
list. These become the leading evidence entries on the finding and are stable —
they become CLI columns.

### `ExtraEvidence(n, nc, branch) []Evidence`
Evidence the engine cannot derive from `EvidenceKeys` alone: guard-computed
values (`age_days`), the unit price, and **price-source entries**. Returns the
slice. Use `rules.PriceEvidence("price_source", p, kind, sku, region)` for every
price — but note `ExtraEvidence` has no `Pass`, so the price-source entry is
rendered in `Cost` (the only place the pricer is reachable) and carried through
`nc` via `nc.Set("price_source", rules.PriceEvidence(...))`.

### The supporting types you receive

```go
type Guard struct {
    SkipCode SkipCode                                  // recorded when Check returns false — never ""
    Name     string                                    // stable, distinct, human-readable
    Check    func(n *graph.Node, nc *NodeContext, p *Pass) bool
}

type NodeContext struct {
    Values map[string]any                              // guards write, Cost/ExtraEvidence read
}

type CostBranch struct {
    Waste      float64                                 // monthly waste in USD
    Confidence float64                                 // in [0,1], documented
    Label      string                                  // "rightsize", "stop_delete", "delete_snapshot", ...
}
```

## 2. The skeleton the engine already runs — and what you must NOT do

`pkg/rules/adapt.go`'s `evalNodeRule` is the one place the evaluation skeleton
lives. For each node of your `Kind()` it already does, in order:

1. context cancellation → stop;
2. `node.Exempt()` (the `tellury-exempt=true` label) → skip `SkipExemptLabel`;
3. your `Guards()` in order, first failure records that guard's `SkipCode`;
4. your `Cost`; on a returned error → skip `SkipNoPrice`;
5. the `MinWasteUSD()` floor, then highest-waste branch selection;
6. `Finding` construction (rule/resource/project/location/waste/confidence);
7. evidence assembly: `autoCollect(EvidenceKeys)` + your `ExtraEvidence`.

Consequently, in your rule you must **never**:

- check `tellury-exempt` yourself — the engine does it first;
- check `MinWasteUSD` in `Cost` — the engine does it;
- call `p.SkipNode` — the engine calls it;
- construct a `rules.Finding` — the engine constructs it;
- iterate nodes — the engine iterates them.

If your detection genuinely cannot be expressed per-node (ranking, cross-node
aggregation), stop and implement the plain `Rule` interface (`Meta` + `Eval`)
instead; both styles coexist in the registry.

## 3. Skip codes — the vocabulary and how to choose one

`pkg/rules/skip.go` defines the shared vocabulary. Reuse a shared code whenever
your reason matches its meaning; define a package-local code only when no shared
code says what you mean.

| SkipCode | Meaning | Choose it when |
|---|---|---|
| `SkipExemptLabel` | `tellury-exempt=true` label | **Never** — the engine records it before your guards run |
| `SkipMissingAttr` | a field is absent or unparsable | your guard cannot find `creation_timestamp`, `storage_bytes`, ... and refuses to guess |
| `SkipBadAttrType` | attr present but wrong type | a number where a string discriminator is required |
| `SkipAttached` | the resource is in use | something is attached/using it — not waste |
| `SkipNonBillingStatus` | status bills nothing stable | e.g. a disk mid-deletion |
| `SkipRecentlyDetached` | freed but below the age threshold | recently-released resource |
| `SkipTooYoung` | younger than the rule's minimum age | your age gate: too young to be confidently waste (used by `old_snapshot`) |
| `SkipNoPrice` | no price for SKU/region | your `Cost` returns a price error |
| `SkipBelowMinWaste` | waste below the noise floor | `Cost` produced nothing above `MinWasteUSD()` (engine records it) |
| `SkipNoMetric` / `SkipLowCoverage` | metric series absent / thin | your rule is metric-dependent and the series is missing or under-sampled |
| `SkipBelowOverprovision` | utilization below the overprovision threshold | the resource is measured but not overprovisioned |
| `SkipUnknownMachineType`, `SkipNotRunning`, `SkipSpot`, `SkipAccelerator`, `SkipNoSmallerSize`, `SkipManagedByMIG` | instance-shape reasons | instance rightsizing specifics |
| `SkipHasLifecycle`, `SkipNotStandardClass`, `SkipAutoclass`, `SkipRetentionLocked`, `SkipBelowMinBytes` | bucket-policy reasons | bucket lifecycle specifics |
| `SkipInternalAddress` | address is INTERNAL | internal addresses are free |

Choosing rules of thumb:

- A missing/unparsable field → `SkipMissingAttr`. A present-but-wrong type → `SkipBadAttrType`.
- "In use / attached" → `SkipAttached`. A status that bills nothing → `SkipNonBillingStatus`.
- "Not old enough yet" → `SkipTooYoung`. "No price resolves" → `SkipNoPrice`.
- If you genuinely need a new reason (e.g. `"snapshot_not_old_enough"`), declare a
  package-local constant `const skipNotOldEnough rules.SkipCode = "snapshot_not_old_enough"`
  and use it — but first check that the shared list does not already say it.
- Never return `""`. A guard with an empty `SkipCode` would make `--explain-skips`
  render a blank reason and defeat the audit trail.
- `old_snapshot` reuses only shared codes: `SkipMissingAttr` (no `creation_timestamp`
  or no `storage_bytes`), `SkipTooYoung` (under 90 days), `SkipNoPrice` (price error),
  `SkipBelowMinWaste` (under the $0.10 floor).

## 4. The complete worked example: `old_snapshot`

This section is the rule you will find implemented in the repo. Follow it step by
step; each step below is the real code that compiled, vetted, and passed tests.

### 4.1 What the rule detects and why it is useful

Persistent-disk snapshots bill a **flat per-GiB-month storage rate** on the
incremental, deduplicated bytes they actually store (`storage_bytes`), not on the
size of the disk they were taken from. A snapshot at or beyond the 90-day
retention window is idle storage: its point-in-time recovery value has decayed
and the whole monthly cost is reclaimable by deleting it. This is the classic
"snapshot sprawl" finding — an idle/unattached resource with a flat cost, the
same shape as an unattached reserved IP.

The worked example's fixture is deliberately adversarial: the first snapshot came
from a 250 GiB source disk but stores only 30 GiB of billable bytes. At
`$0.050/GiB-month`, the correct figure is **30 GiB × $0.050/GiB-month = $1.50/month**.
Pricing the source disk instead would produce $12.50/month. Measured against a
real organization, that source-disk mistake overstated snapshot waste by ~9x, and
the per-snapshot ratio varied from 15% down to 0% — so no rate adjustment can
stand in for reading the billable field.

### 4.2 The plumbing the rule needs (do this check first)

Before the rule itself, confirm the pipeline can even produce the node and the
price your rule needs:

- **Normalizer**: `pkg/cloud/gcp/normalize.go`'s `normalizeSnapshot` writes
  `storage_bytes` **only when the CAI payload carried the snapshot's
  `storageBytes` field**. That distinction is load-bearing:

  - absent `storage_bytes` means the payload was not parsed — the rule skips with
    `SkipMissingAttr` and refuses to guess;
  - present-but-zero `storage_bytes` is a real, fully deduplicated snapshot that
    costs nothing, and it falls out below the `MinWasteUSD` floor rather than
    being reported as missing data.

  The normalizer also surfaces the console's source-disk size as evidence so a
  reader can see why tellury's figure differs from the UI. That source-disk
  evidence is **never** the basis for a price.

- **Price**: snapshot storage is `pricing.KindSnapshotStorage`
  (`"snapshot_storage"`). The rule queries token `SnapshotStorageSKU = "standard"`.
  The fixture price table has:

  ```json
  "snapshot_storage": {
    "standard": {"default": 0.05},
    "archive": {"default": 0.024}
  }
  ```

  The live Cloud Billing Catalog matcher (`pkg/pricing/gcp/catalog.go`,
  `matchSKU`) maps the `PDSnapshot` resource group to this kind, and separates
  the standard storage rate from the archive tier and from one-off early-deletion
  charges. The worked example therefore uses **$0.050/GiB-month**.

### 4.3 The rule — `pkg/rules/gcp/compute/old_snapshot/rule.go`

```go
// Package old_snapshot implements the "old_snapshot" FinOps rule: a
// persistent disk snapshot at or beyond the retention window bills its full
// per-GiB-month storage cost for doing nothing. Snapshot storage is a flat,
// idle cost — a snapshot is never "attached" to anything — so there is no
// partial waste to compute: the entire monthly charge is reclaimable by
// deleting the snapshot, exactly like an unattached reserved IP.
package old_snapshot

import (
    "context"
    "fmt"
    "math"
    "time"

    "github.com/TypeOneLabs/tellury/pkg/graph"
    "github.com/TypeOneLabs/tellury/pkg/pricing"
    "github.com/TypeOneLabs/tellury/pkg/rules"
)

// ID is the stable rule identifier. It MUST equal the package basename.
const ID = "old_snapshot"

// SnapshotStorageSKU is the pricing catalogue SKU token for standard
// snapshot storage, priced per GiB-month (pricing.KindSnapshotStorage).
const SnapshotStorageSKU = "standard"

// Rule constants.
const (
    // MaxAgeDays is the retention window. A snapshot younger than this is a
    // live recovery point and is not reported; at or beyond it the
    // point-in-time recovery value has decayed and the flat monthly storage
    // bill is treated as reclaimable.
    MaxAgeDays = 90

    // MinMonthlyWasteUSD hides findings below $0.10/month (noise floor).
    // A billable snapshot needs ~2 GiB at $0.050/GiB-month to clear it.
    MinMonthlyWasteUSD = 0.10
)

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
    return rules.Meta{
        ID:       ID,
        Provider: "gcp",
        Service:  "compute",
        Title:    "Persistent disk snapshot is older than the retention window",
        Description: "A persistent disk snapshot bills a flat per-GiB-month " +
            "storage rate for as long as it exists, whether or not anything " +
            "ever restores from it. A snapshot at or beyond the 90-day " +
            "retention window is idle storage: its point-in-time recovery " +
            "value has decayed, and the entire monthly cost is reclaimable by " +
            "deleting it. There is no partial component to compute — the whole " +
            "cost is waste.",
        Severity:           rules.SeverityLow,
        RequiredAssetTypes: []string{"compute.googleapis.com/Snapshot"},
        RequiredMetrics:    nil, // pure attribute + age check on the CAI payload — zero metric cost
        Remediation:        "gcloud compute snapshots delete NAME",
        Origin:             "native",
    }
}

func (rule) Kind() graph.ResourceKind { return graph.KindSnapshot }

// Guards are evaluated in order; the first to return false decides the
// SkipCode recorded for the node. The engine's built-in exempt-label check
// runs before any of these — a rule must never declare it.
func (rule) Guards() []rules.Guard {
    return []rules.Guard{
        {Name: "creation_timestamp_parseable", SkipCode: rules.SkipMissingAttr,
            Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
                createdAt, ok := n.Time("creation_timestamp")
                if !ok {
                    return false
                }
                // Stash once here; the age guard, Cost and ExtraEvidence all
                // read the SAME parsed instant so confidence and evidence can
                // never disagree about what "old" means.
                nc.Set("created_at", createdAt)
                return true
            }},
        {Name: "billable_bytes_present", SkipCode: rules.SkipMissingAttr,
            Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
                // Absent means the payload was not parsed. Present-but-zero is
                // a real and common state — a snapshot fully deduplicated
                // against the rest of its chain occupies no billable bytes —
                // and it is not an error, it is simply worth nothing. It falls
                // out below the minimum-waste floor rather than being skipped
                // here, so `--explain-skips` reports it as immaterial rather
                // than as missing data.
                _, ok := n.Num("storage_bytes")
                return ok
            }},
        {Name: "old_enough", SkipCode: rules.SkipTooYoung,
            Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
                createdAt, _ := nc.Get("created_at")
                age := p.Now.Sub(createdAt.(time.Time))
                days := math.Floor(age.Hours() / 24)
                nc.Set("age_days", days)
                return days >= MaxAgeDays
            }},
    }
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
    // Snapshot storage bills a flat per-GiB-month rate on storage_bytes: the
    // incremental, compressed bytes the snapshot occupies after deduplication
    // against the rest of its chain. It is NOT the source disk's size. The
    // whole cost is waste; there is no partial component to subtract.
    sizeGB, _ := n.Num("storage_bytes")
    sizeGB /= 1 << 30
    region := pricing.RegionOf(n.Location)
    unit, resolvedRegion, err := p.Price.UnitPrice(pricing.KindSnapshotStorage, "gcp", SnapshotStorageSKU, region)
    if err != nil {
        return nil, err // engine records SkipNoPrice; never a $0 assumption (Invariant I4)
    }
    monthlyWaste := unit * sizeGB

    // Stash the values ExtraEvidence needs. ExtraEvidence has no Pass, so the
    // price-source entry is rendered here — the only place the pricer is
    // reachable — and carried through nc.
    nc.Set("currency", rules.CurrencyOf(p))
    nc.Set("unit_price_gib_month", unit)
    nc.Set("price_source", rules.PriceEvidence("price_source", p.Price, pricing.KindSnapshotStorage, SnapshotStorageSKU, resolvedRegion))

    return []rules.CostBranch{{
        Waste:      round2(monthlyWaste),
        Confidence: 0.7,
        // 0.7, not 1.0: the age gate is deterministic, but the judgment that
        // "older than 90 days ⇒ reclaimable" is not. Some snapshots are kept
        // past the window deliberately (compliance, DR, legal hold), so the
        // rule states its reading of the evidence at 70% rather than asserting
        // the delete is always right.
        Label: "delete_snapshot",
    }}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
    cur, _ := nc.Get("currency")
    curStr, _ := cur.(string)
    ageDays, _ := nc.Get("age_days")
    unit, _ := nc.Get("unit_price_gib_month")
    ev := []rules.Evidence{
        {Key: "age_days", Value: fmt.Sprintf("%.0f", ageDays.(float64))},
        rules.EvMoneyIn("unit_price_gib_month", curStr, unit.(float64), 4),
    }
    if v, ok := nc.Get("price_source"); ok {
        ev = append(ev, v.(rules.Evidence))
    }
    return ev
}

func round2(v float64) float64 {
    return math.Round(v*100) / 100
}
```

`EvidenceKeys` auto-collects the billable size and the creation instant:
`storage_bytes` and `creation_timestamp`. The rule source also surfaces the
console's source-disk size as a third evidence key for side-by-side comparison,
but that key is evidence only — it is never read by `Cost` and never priced.

Notice the data flow: the `creation_timestamp_parseable` guard parses the instant
**once** and stashes `created_at`; the `old_enough` guard computes `age_days`
**once**; `Cost` reads `storage_bytes` from the node, converts bytes to GiB with
`1 << 30`, and multiplies by the pricer's `$0.050/GiB-month` unit rate;
`ExtraEvidence` reads the stashed values. Nothing is recomputed, so confidence
and evidence cannot drift.

### 4.4 The fixture — `pkg/rules/gcp/compute/old_snapshot/testdata/old-snapshot.json`

A CAI fixture with three snapshots: one past the window (fires), one inside it
(skipped `too_young`), one with no `storageBytes` (skipped `missing_attribute`).
The first asset is shown here with the field that matters:

```json
{
  "name": "//compute.googleapis.com/projects/my-project/global/snapshots/backup-2023-01-01",
  "assetType": "compute.googleapis.com/Snapshot",
  "project": "projects/my-project",
  "resource": {
    "version": "v1",
    "parent": "//cloudresourcemanager.googleapis.com/projects/34968801978",
    "location": "us-central1",
    "data": {
      "name": "backup-2023-01-01",
      "status": "READY",
      "sourceDisk": "//compute.googleapis.com/projects/my-project/zones/us-central1-a/disks/web-db-01",
      "creationTimestamp": "2023-01-01T00:00:00Z",
      "storageBytes": "32212254720"
    }
  }
}
```

`32212254720` bytes is exactly **30 GiB**. The full fixture also carries the
console's source-disk size field for this asset — **250 GiB** — deliberately much
larger than the billable `storageBytes` figure, so a rule that prices the wrong
field cannot accidentally produce a plausible-looking number. The other two
assets are:

- `backup-2024-01-10`, a young snapshot with its own `storageBytes`, skipped as
  `too_young` at the documented `--at` instant;
- `legacy-no-size`, an old snapshot whose payload omits `storageBytes`, skipped
  as `missing_attribute`.

### 4.5 The test — `internal/cli/old_snapshot_fixture_test.go`

The rule package has its own unit tests for guard boundaries and the mutation
check. The worked-example **price arithmetic** is pinned end-to-end by
`internal/cli/old_snapshot_fixture_test.go`, which runs the exact CLI command
shape through the real scan pipeline. This is the test that keeps the defect
fixed:

```go
package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/internal/config"
)

// oldSnapshotFixture is the fixture the `old_snapshot` rule ships with, one
// level down from the rule package it lives beside. The scan below runs the
// exact command shape docs/writing-a-rule.md documents against it, through the
// real runScan pipeline (rule selection -> offline provider -> ingest ->
// rules -> table render + --explain-skips), so the guide's worked example is
// not help text: it is this test.
const oldSnapshotFixture = "../../pkg/rules/gcp/compute/old_snapshot/testdata/old-snapshot.json"

// TestSkillWorkedExample_OldSnapshotScan pins the worked example the
// rule-writing guide ships: at a fixed evaluation instant (--at) the old
// snapshot fires at its exact flat cost, the young snapshot is skipped as
// too_young, and the size-less snapshot is skipped as missing_attribute. It
// also logs the real stdout/stderr the guide's commands produce.
func TestSkillWorkedExample_OldSnapshotScan(t *testing.T) {
	gcpPriceFixtureEnv(t)

	cfg := config.Scan{
		Provider:       "gcp",
		Project:        "my-project",
		Fixture:        []string{oldSnapshotFixture},
		Format:         "table",
		Rules:          []string{"old_snapshot"},
		FailOnFindings: false,
		ExplainSkips:   true,
		OutDir:         filepath.Join(t.TempDir(), "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g := &globalFlags{LogLevel: "warn"}
	var out, errOut bytes.Buffer
	// The exact evaluation instant the guide documents:
	// 2024-01-20T00:00:00Z, making every age deterministic.
	scanAt := readmeNow // 2024-01-20T00:00:00Z
	if err := runScan(context.Background(), &out, &errOut, g, cfg, scanAt); err != nil {
		t.Fatalf("runScan (old_snapshot fixture): %v", err)
	}

	t.Logf("=== `tellury scan --fixture ... --rules old_snapshot --at 2024-01-20T00:00:00Z --explain-skips` (stdout) ===\n%s", out.String())
	t.Logf("=== same command (stderr: --explain-skips) ===\n%s", errOut.String())

	got := out.String()
	if !strings.Contains(got, "snapshot/backup-2023-01-01") {
		t.Errorf("table output missing the old snapshot resource:\n%s", got)
	}
	if !strings.Contains(got, "old_snapshot") {
		t.Errorf("table output missing the old_snapshot rule column:\n%s", got)
	}
	// The snapshot bills on storageBytes (30 GiB incremental), NOT on the 250
	// GiB source disk: 30 x $0.050/GiB-month = $1.50/month. Pricing the source
	// disk instead gives $12.50 — the defect this asserts against, which
	// overstated a real organization's snapshot waste by ~9x.
	if !strings.Contains(got, "$1.50") {
		t.Errorf("table output missing the $1.50 monthly waste (30 GiB billable x $0.050/GiB-month):\n%s", got)
	}

	skips := errOut.String()
	if !strings.Contains(skips, "too_young") || !strings.Contains(skips, "1") {
		t.Errorf("--explain-skips missing the too_young tally for the young snapshot:\n%s", skips)
	}
	if !strings.Contains(skips, "missing_attribute") {
		t.Errorf("--explain-skips missing the missing_attribute tally for the size-less snapshot:\n%s", skips)
	}
}
```

### 4.6 Run the test — the firing case passes

```bash
go test ./pkg/rules/gcp/compute/old_snapshot/ -run TestEval_OldSnapshot_Fires -count=1 -v
```

```
=== RUN   TestEval_OldSnapshot_Fires
--- PASS: TestEval_OldSnapshot_Fires (0.00s)
PASS
ok  	github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/old_snapshot	0.006s
```

The end-to-end fixture test logs the exact stdout/stderr shown in section 7.

### 4.7 THE MUTATION CHECK — break it, watch it fail, restore it

See section 6 — it is mandatory, not optional. It is shown here with the exact
commands because it is part of shipping this rule.

```bash
# 1. Fire the firing-case test: passes (above).

# 2. BREAK the condition the rule detects: in rule.go change
#      MaxAgeDays = 90   →   MaxAgeDays = 200
#    so a 120-day-old snapshot no longer clears the gate.

# 3. The test MUST now fail:
go test ./pkg/rules/gcp/compute/old_snapshot/ -run TestEval_OldSnapshot_Fires -count=1 -v
# → FAIL: rule_test.go:98: want 1 finding, got 0 ([])
```

```bash
# 4. RESTORE MaxAgeDays = 90.

# 5. The test passes again:
go test ./pkg/rules/gcp/compute/old_snapshot/ -run TestEval_OldSnapshot_Fires -count=1
```

```
ok  	github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/old_snapshot	0.007s
```

Record the mutation (what you changed, what failure you observed) in a comment on
the test — see the comment block above `TestEval_OldSnapshot_Fires`.

## 5. Registration — and the silent-nothing trap

Your rule's `init()` calls `rules.RegisterNode(rule{})`. `RegisterNode` adapts your
`NodeRule` to the plain `Rule` the engine runs and puts it in the registry.

**The trap.** `init()` only runs when *something imports your package*. A rule in a
package nothing imports registers **silently as nothing at all** — `go build`,
`go vet` and `go test ./...` are all green, the CLI runs, and your rule is simply
absent from `tellury rules list` and every scan. Nothing reports it. There is no
linker error and no "unused package" warning for a blank import that was never
added.

**The fix — add the blank import here.** The exact file is
`pkg/rules/all/all.go`:

```go
// Package all registers every built-in rule via import side effects. This is the
// ONLY place rule packages are referenced, keeping pkg/rules free of any
// dependency on concrete rules.
package all

import (
	_ "github.com/TypeOneLabs/tellury/pkg/rules/aws/ec2/unassociated_eip"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/aws/ec2/unattached_ebs_volume"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/aws/ec2/underutilized_instance"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/azure/compute/unattached_managed_disk"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/azure/compute/underutilized_vm"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/azure/network/unassociated_public_ip"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/detached_disk"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/old_snapshot"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/underutilized_instance"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/unused_reserved_ip"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/gcs/no_lifecycle_policy"
)
```

The CLI (`internal/cli/root.go`) imports `pkg/rules/all`, so once the blank import
is there, your rule registers on every run.

There is a second file to touch, `pkg/rules/all/all_external_test.go`: it imports
every rule package **directly** (independent of `all`) and compares "what `all`
imports" against "what the registry holds", in both directions. That is what makes
a *dropped* blank import fail CI later (the rule stays registered in the test but
`all` no longer ships it). Add your package to its import block too.

Verify registration with:

```bash
tellury rules list
```

```
ID                       PROVIDER  SERVICE  SEVERITY  METRICS  TITLE
detached_disk            gcp       compute  medium    0        Persistent disk is not attached to any instance
no_lifecycle_policy      gcp       gcs      low       1        Bucket has no lifecycle policy
old_snapshot             gcp       compute  low       0        Persistent disk snapshot is older than the retention window
unassociated_eip         aws       ec2      medium    0        Elastic IP address is not associated with any instance
unassociated_public_ip   azure     network  medium    0        Public IP address is not associated with any resource
unattached_ebs_volume    aws       ec2      medium    0        EBS volume is not attached to any instance
unattached_managed_disk  azure     compute  medium    0        Managed disk is not attached to any virtual machine
underutilized_ec2        aws       ec2      high      1        Instance is significantly overprovisioned for its CPU load
underutilized_instance   gcp       compute  high      1        Instance is significantly overprovisioned for its CPU load
underutilized_vm         azure     compute  high      1        Virtual machine is significantly overprovisioned for its CPU load
unused_reserved_ip       gcp       compute  medium    0        Reserved external IP address is not attached to anything
```

If your rule is not in this list, the blank import is missing — see the trap above.
The registration test (`pkg/rules/all/all_external_test.go`) is the automated guard:
it fails when an imported rule never registered, when the registry is empty, or
when a rule is registered but `all` no longer imports it.

## 6. THE MUTATION CHECK — mandatory, not optional

This is a hard requirement. During this project, five agent-written tests
asserted nothing, and one was widened to accept broken output — both classes of
defect sail through `go build`/`go vet`/`go test` because the test function
*runs* but proves nothing. The mutation check is the only reliable defence: you
must prove, in the PR, that your test fails when the condition it asserts is
broken.

Workflow, concretely:

1. **Write the firing-case test and verify it passes.**

   ```bash
   go test ./pkg/rules/<provider>/<service>/<rule_id>/ -run TestEval_<YourRule>_Fires -count=1
   ```

2. **Break the condition your rule detects.** Change the threshold, flip a
   comparison, delete the guard — make the firing case stop firing. Do not touch
   the test.

3. **Run the test and confirm it FAILS.** The failure must be in the assertion
   (wrong finding count, wrong waste, wrong skip code) — not a compile error and
   not "test file not found". For `old_snapshot` this was:

   ```bash
   go test ./pkg/rules/gcp/compute/old_snapshot/ -run TestEval_OldSnapshot_Fires -count=1 -v
   # → FAIL: rule_test.go:98: want 1 finding, got 0 ([])
   ```

4. **Restore the correct condition.**

5. **Run the test and confirm it passes again.**

6. **Document the mutation in a comment on the test** — what you changed, what
   failure you observed. This is what makes the check auditable by a reviewer:

   ```go
   // MUTATION CHECK: setting MaxAgeDays to 200 in rule.go caused this test
   // to fail with "want 1 finding, got 0" for a 120-day-old snapshot — the
   // gate no longer fired. The original 90-day value was restored; the test
   // now passes, proving it detects the age condition.
   ```

A mutation that changes the *price* must produce a failure in the **waste**
assertion; a mutation that removes a *guard* must produce a failure in the
**skip-code** or **finding-count** assertion. If any mutation leaves the test
green, the test is not testing what you think it is — fix the test.

## 7. Checklist before submitting

Run these, in order, from the repo root. All must pass before the PR is opened.

1. **Build**
   ```bash
   go build ./...
   ```
2. **Vet**
   ```bash
   go vet ./...
   ```
3. **Test — the whole suite, uncached**
   ```bash
   go test -count=1 ./...
   ```
4. **Your rule's tests pass**
   ```bash
   go test -count=1 ./pkg/rules/<provider>/<service>/<rule_id>/
   ```
5. **The registration test passes** (proves the blank import and the 1:1 ID mapping)
   ```bash
   go test -count=1 ./pkg/rules/all/
   ```
6. **The mutation check** — performed and documented (section 6).
7. **The rule appears in the catalogue**
   ```bash
   tellury rules list
   # your rule ID must be in the table
   ```
8. **The rule fires on its fixture, with auditable skips**
   ```bash
   tellury scan --fixture pkg/rules/gcp/compute/old_snapshot/testdata/old-snapshot.json \
     --rules old_snapshot --at 2024-01-20T00:00:00Z --explain-skips --fail-on-findings=false
   ```

   The `old_snapshot` example produces exactly this (the `--at` instant makes the
   ages deterministic):

   ```
   FINDINGS
   --------------------------------------------------------------
   RESOURCE                   RULE         SEVERITY MONTHLY WASTE
   snapshot/backup-2023-01-01 old_snapshot LOW              $1.50
   --------------------------------------------------------------
   TOTAL                      1 finding                     $1.50
   ```

   and, on stderr from `--explain-skips`:

   ```
   SKIP TALLY
     RULE                           CODE                             COUNT
     old_snapshot                   missing_attribute                1
     old_snapshot                   too_young                        1
   ```

   Every skip reason in that tally is a typed `SkipCode` from `pkg/rules/skip.go`
   — that is what makes a scan auditable: an operator can see not just what was
   reported, but exactly why every other node was not.

## 8. Anti-patterns — never do these

- Do not check `tellury-exempt` in a guard — the engine does it first.
- Do not check `MinWasteUSD` in `Cost` — the engine does it.
- Do not call `p.SkipNode` anywhere — the engine calls it.
- Do not construct a `rules.Finding` — the engine constructs it.
- Do not iterate nodes — the engine iterates them.
- Do not skip on attribute *absence* when the normalizer writes the key
  unconditionally and absent genuinely means zero (e.g. `user_count`,
  `provisioned_iops`). For snapshots the opposite is true: `storage_bytes` is
  written **only when the payload carried it**, so absent `storage_bytes` means
  "unparsed payload" and must skip as `SkipMissingAttr`. A present zero is a
  fully deduplicated snapshot and falls out below the noise floor. Read
  `pkg/cloud/gcp/normalize.go` for your asset type and copy the convention it
  actually uses.
- Do not assume a price. If `UnitPrice` returns an error, return it from `Cost`;
  the engine records `SkipNoPrice`. Never return `$0`.
- Do not write a test that asserts `len(findings) >= 0` or any other tautology —
  and do not widen an assertion to accept broken output. That is the exact defect
  the mutation check exists to catch.
