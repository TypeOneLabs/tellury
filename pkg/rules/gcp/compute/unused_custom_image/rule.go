// Package unused_custom_image implements the "unused_custom_image" FinOps
// rule: a GCP custom image whose archive no instance template references is
// idle storage. The image bills per GiB-month on archiveSizeBytes — the
// actual stored bytes — not on diskSizeGb (the source disk size). An image
// referenced only by an instance template is IN USE even when no current
// instance runs from it, because the template can launch an instance later.
package unused_custom_image

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	gcprules "github.com/TypeOneLabs/tellury/pkg/rules/gcp"
)

// ID is the stable rule identifier. It MUST equal the package basename:
// pkg/rules/all/all_external_test.go enforces the 1:1 mapping.
const ID = "unused_custom_image"

// ImageStorageSKU is the pricing catalogue SKU token for custom image
// storage. The live Cloud Billing catalogue and the fixture both index
// pricing.KindImageStorage under this exact token.
const ImageStorageSKU = "standard"

const (
	// MaxAgeDays is the retention window. Only unreferenced custom images at
	// or beyond 90 days old are reported.
	MaxAgeDays = 90

	// MinMonthlyWasteUSD hides findings below $0.10/month (noise floor).
	MinMonthlyWasteUSD = 0.10
)

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "gcp",
		Service:  "compute",
		Title:    "Custom image is not referenced by any instance template",
		Description: "A GCP custom image bills per GiB-month for its stored archive " +
			"whether or not anything runs from it. An image referenced by an instance " +
			"template is in use even with no current instances; only an image with no " +
			"template reference at or beyond the 90-day retention window is reclaimable.",
		Severity:           rules.SeverityMedium,
		RequiredAssetTypes: []string{gcprules.TypeImage},
		RequiredMetrics:    nil, // pure attribute + template-reference pass — zero metric cost
		Remediation:        "gcloud compute images delete NAME",
		Origin:             "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindImage }

// Guards are evaluated in order; the first to return false decides the
// SkipCode recorded for the node. The engine's built-in exempt-label check
// runs before any of these.
func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		// G1: the specific image asset type this rule targets. Several image
		// rules share graph.KindImage; without this the rule would price
		// machine images or foreign image types as if they were custom images.
		{Name: "asset_type", SkipCode: rules.SkipNotTargetAssetType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				return n.AssetType == gcprules.TypeImage
			}},
		// G2: billable stored bytes present. Absent means the payload was not
		// parsed; present-but-zero is a real free image and falls out below
		// the minimum-waste floor rather than skipping as missing data.
		{Name: "storage_bytes_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				_, ok := n.Num(gcprules.AttrStorageBytes)
				return ok
			}},
		// G3: creation time parseable; stash the parsed instant once so the
		// age guard, Cost and ExtraEvidence all read the SAME instant.
		{Name: "creation_time_parseable", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				ts, ok := n.Str(gcprules.AttrCreationTime)
				if !ok {
					return false
				}
				createdAt, err := time.Parse(time.RFC3339, ts)
				if err != nil {
					return false
				}
				nc.Set("created_at", createdAt)
				return true
			}},
		// G4: billing status. Only READY is a stable, billable image state.
		{Name: "status_ready", SkipCode: rules.SkipNonBillingStatus,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				status, _ := n.Str(gcprules.AttrStatus)
				return status == gcprules.StatusReady
			}},
		// G5: a single canonical storage location. Multiple distinct
		// storageLocations[] entries would make a one-region price an
		// understatement, so the rule refuses to guess.
		{Name: "storage_location_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				loc, ok := n.Str(gcprules.AttrStorageLocation)
				return ok && loc != ""
			}},
		// G6: reference enumeration complete. If instanceTemplates.list failed
		// for this project, the rule refuses to assume the image is
		// unreferenced.
		{Name: "references_complete", SkipCode: rules.SkipReferencesUnknown,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				complete, ok := n.Bool(gcprules.AttrReferencesComplete)
				return ok && complete
			}},
		// G7: not referenced. The normalizer writes reference_count
		// unconditionally, so absence means zero.
		{Name: "not_referenced", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				cnt, _ := n.Num(gcprules.AttrReferenceCount)
				return cnt == 0
			}},
		// G8: old enough. The design's 90-day retention window suppresses
		// pipeline noise from freshly built images.
		{Name: "old_enough", SkipCode: rules.SkipTooYoung,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				createdAt, _ := nc.Get("created_at")
				created := createdAt.(time.Time)
				days := math.Floor(p.Now.Sub(created).Hours() / 24)
				if days < MaxAgeDays {
					return false
				}
				nc.Set("age_days", days)
				return true
			}},
	}
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	// The billable quantity is archiveSizeBytes. diskSizeGb (source disk
	// size) is deliberately NOT priced: it is a nominal/source size that
	// over-reports stored bytes.
	storageBytes, _ := n.Num(gcprules.AttrStorageBytes)
	sizeGB := storageBytes / (1 << 30)
	region := pricing.RegionOf(n.Location)
	unit, resolvedRegion, err := p.Price.UnitPrice(pricing.KindImageStorage, "gcp", ImageStorageSKU, region)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never a $0 assumption
	}
	monthlyWaste := sizeGB * unit

	nc.Set("currency", rules.CurrencyOf(p))
	nc.Set("storage_gib", sizeGB)
	nc.Set("unit_price_gib_month", unit)
	nc.Set("price_source", rules.PriceEvidence("price_source", p.Price, pricing.KindImageStorage, ImageStorageSKU, resolvedRegion))

	return []rules.CostBranch{{
		Waste:      round2(monthlyWaste),
		Confidence: 0.7, // deterministic reference+age check, but "delete old image" is a judgment
		Label:      "delete_image",
	}}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

// EvidenceKeys auto-collects the raw CAI-derived fields first; the computed
// GiB figure and price evidence are rendered in ExtraEvidence.
func (rule) EvidenceKeys() []string {
	return []string{gcprules.AttrStorageBytes, gcprules.AttrSourceDiskSizeGB, gcprules.AttrCreationTime}
}

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	storageGiB, _ := nc.Get("storage_gib")
	unit, _ := nc.Get("unit_price_gib_month")
	ageDays, _ := nc.Get("age_days")

	ev := []rules.Evidence{
		{Key: "storage_gib", Value: fmt.Sprintf("%.2f", storageGiB.(float64))},
		{Key: "age_days", Value: fmt.Sprintf("%.0f", ageDays.(float64))},
		{Key: "size_basis", Value: "archive_size_bytes"},
		rules.EvMoneyIn("unit_price_gib_month", curStr, unit.(float64), 4),
	}
	if loc, ok := n.Str(gcprules.AttrStorageLocation); ok && loc != "" {
		ev = append(ev, rules.Evidence{Key: "storage_location", Value: loc})
	}
	if count, ok := n.Num(gcprules.AttrReferenceCount); ok {
		ev = append(ev, rules.Evidence{Key: "reference_count", Value: fmt.Sprintf("%.0f", count)})
	}
	if v, ok := nc.Get("price_source"); ok {
		ev = append(ev, v.(rules.Evidence))
	}
	return ev
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
