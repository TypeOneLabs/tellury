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

// ID is the stable rule identifier. It MUST equal the package basename:
// pkg/rules/all/all_external_test.go enforces the 1:1 mapping
// (package path → Meta.ID) as the registration guard.
const ID = "old_snapshot"

// SnapshotStorageSKU is the pricing catalogue SKU token for standard
// snapshot storage, priced per GiB-month (pricing.KindSnapshotStorage). Both
// the embedded price table and the live Cloud Billing Catalog lookup
// (pkg/pricing/gcp/catalog.go matchSKU's "storagesnapshot" resource group)
// index it under this token.
const SnapshotStorageSKU = "standard"

// Rule constants.
const (
	// MaxAgeDays is the retention window. A snapshot younger than this is a
	// live recovery point and is not reported; at or beyond it the
	// point-in-time recovery value has decayed and the flat monthly storage
	// bill is treated as reclaimable.
	MaxAgeDays = 90

	// MinMonthlyWasteUSD hides findings below $0.10/month (noise floor),
	// matching the convention every other native rule uses. In practice a
	// billable snapshot (≥ ~4 GiB at $0.026) clears this once it crosses the
	// age gate, but a tiny/cheap snapshot stays below it.
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
	// against the rest of its chain. NOT the source disk's size — measured
	// against a real organization those differed by ~9x, and the ratio ranged
	// from 15% to 0% per snapshot, so no rate adjustment could stand in for
	// it. The whole cost is waste; there is no partial component to subtract.
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

// EvidenceKeys auto-collects the two node attributes that describe the
// resource: the billable size and the creation instant. Everything derived —
// age_days, the unit price, the price source — is computed and rendered by
// ExtraEvidence.
func (rule) EvidenceKeys() []string {
	// source_disk_size_gb is surfaced alongside the billable bytes because it
	// is the number the console shows, so a reader comparing tellury's figure
	// against the UI can see immediately why they differ.
	return []string{"storage_bytes", "source_disk_size_gb", "creation_timestamp"}
}

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	ageDays, _ := nc.Get("age_days")
	unit, _ := nc.Get("unit_price_gib_month")
	ev := []rules.Evidence{
		{Key: "age_days", Value: fmt.Sprintf("%.0f", ageDays.(float64))},
		{Key: "unit_price_gib_month", Value: fmt.Sprintf("$%.4f", unit.(float64))},
	}
	if v, ok := nc.Get("price_source"); ok {
		ev = append(ev, v.(rules.Evidence))
	}
	return ev
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
