// Package unused_gallery_image_version implements the
// "unused_gallery_image_version" FinOps rule: an Azure Compute Gallery image
// version is replicated to a configured number of target regions and bills per
// replica, even when no VM or VM scale set references it. The rule refuses to
// report a version whose reference inventory could not be completed, and then
// prices the version's total replicated storage:
//
//	sum over targetRegions(size_gib * regional_replica_count * unit_price_gib_month)
//
// Pricing on size alone is the replication trap this rule exists to catch: a
// version with three replicas costs three times a version with one.
package unused_gallery_image_version

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	azurerules "github.com/TypeOneLabs/tellury/pkg/rules/azure"
)

// ID is the stable rule identifier, and the RULE column value in CLI output.
const ID = "unused_gallery_image_version"

const (
	// MinAgeDays is the retention window from the rule design. Only
	// unreferenced gallery image versions at or beyond 90 days old are
	// reported.
	MinAgeDays = 90.0
	// MinMonthlyWasteUSD hides findings below $0.10/month (noise floor).
	MinMonthlyWasteUSD = 0.10

	bytesPerGiB = 1 << 30
)

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "azure",
		Service:  "compute",
		Title:    "Compute Gallery image version is not referenced by any VM or VM scale set",
		Description: "A Compute Gallery image version bills storage once per replica in each " +
			"configured target region. When no VM or VM scale set references the version, its " +
			"replicated storage is reclaimable by deleting the image version.",
		Severity:           rules.SeverityMedium,
		RequiredAssetTypes: []string{azurerules.TypeGalleryImageVersion},
		RequiredMetrics:    nil, // pure attribute + reference-count check — zero metric cost
		Remediation:        "az sig image-version delete --gallery-image-version NAME --gallery-name GALLERY --gallery-image-definition IMAGE --resource-group RG",
		Origin:             "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindImage }

// Guards are evaluated in order; the first to return false decides the
// SkipCode recorded for the node. The engine's built-in exempt-label check
// runs before any of these — a rule must never declare it.
func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		// G1: the specific image asset type this rule targets. Several image
		// rules share graph.KindImage, so the asset type is the discriminator.
		{Name: "asset_type", SkipCode: rules.SkipNotTargetAssetType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				return n.AssetType == azurerules.TypeGalleryImageVersion
			}},
		// G2: shape valid — a billable stored size.
		{Name: "size_bytes_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Num(azurerules.AttrGallerySizeBytes)
				return ok && v > 0
			}},
		// G3: creation time parseable; stashes the parsed instant once so the
		// age guard, Cost and ExtraEvidence all read the SAME instant.
		{Name: "creation_time_parseable", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				ts, ok := n.Str(azurerules.AttrCreationTimestamp)
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
		// G4: billing state. Only Succeeded is a stable, billable image
		// version state.
		{Name: "provisioning_state_succeeded", SkipCode: rules.SkipNonBillingStatus,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				s, _ := n.Str(azurerules.AttrProvisioningState)
				return s == "Succeeded"
			}},
		// G5: replica list present. Without targetRegions the rule cannot know
		// the replica multiple, and pricing size alone would be the trap.
		{Name: "replica_regions_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				regions, ok := galleryReplicaRegions(n)
				return ok && len(regions) > 0
			}},
		// G6: reference enumeration complete. If the VM/VMSS reference rows
		// could not be parsed, the rule refuses to assume unreferenced.
		{Name: "references_complete", SkipCode: rules.SkipReferencesUnknown,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				complete, ok := n.Bool(azurerules.AttrReferencesComplete)
				return ok && complete
			}},
		// G7: not referenced. The normalizer writes reference_count
		// unconditionally, so absence means zero.
		{Name: "not_referenced", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				cnt, _ := n.Num(azurerules.AttrReferenceCount)
				return cnt == 0
			}},
		// G8: old enough. The design's 90-day retention window suppresses
		// pipeline noise from freshly built image versions.
		{Name: "old_enough", SkipCode: rules.SkipTooYoung,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				createdAt, _ := nc.Get("created_at")
				created := createdAt.(time.Time)
				days := math.Floor(p.Now.Sub(created).Hours() / 24)
				if days < MinAgeDays {
					return false
				}
				nc.Set("age_days", days)
				return true
			}},
	}
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	regions, _ := galleryReplicaRegions(n)
	sizeBytes, _ := n.Num(azurerules.AttrGallerySizeBytes)
	sizeGiB := sizeBytes / bytesPerGiB

	var (
		totalWaste    float64
		priceEvidence []rules.Evidence
		moneyEvidence []rules.Evidence
		components    []rules.PricedComponent
	)
	currency := rules.CurrencyOf(p)
	nc.Set("currency", currency)

	for _, region := range regions {
		regionName, _ := region["region"].(string)
		sku, _ := region["storage_account_type"].(string)
		replicaCount, _ := region["replica_count"].(float64)
		if regionName == "" || sku == "" || replicaCount <= 0 {
			return nil, pricing.ErrNoPrice
		}

		unit, resolvedRegion, err := p.Price.UnitPrice(pricing.KindGalleryImageStorage, "azure", sku, regionName)
		if err != nil {
			return nil, err // engine records SkipNoPrice; never a $0 assumption (Invariant I4)
		}
		totalWaste += sizeGiB * replicaCount * unit
		components = append(components, rules.PricedComponent{
			Kind:   pricing.KindGalleryImageStorage,
			SKU:    sku,
			Region: resolvedRegion,
			Key:    resolvedRegion,
		})
		moneyEvidence = append(moneyEvidence,
			rules.EvMoneyIn("unit_price_gib_month_"+resolvedRegion, currency, unit, 4))
	}

	// PriceEvidenceFor renders one entry for one region, and one entry per
	// region for a multi-region replica list. ExtraEvidence appends them below.
	priceEvidence = rules.PriceEvidenceFor("price_source", p.Price, components...)
	nc.Set("size_gib", sizeGiB)
	nc.Set("total_waste", totalWaste)
	nc.Set("price_evidence", priceEvidence)
	nc.Set("money_evidence", moneyEvidence)

	return []rules.CostBranch{{
		Waste:      pricing.Round2(totalWaste),
		Confidence: 0.7,
		Label:      "replicated_storage",
	}}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

// EvidenceKeys returns nil: the leading evidence entries are rendered by
// ExtraEvidence so formatting is explicit, and the rest are computed or
// money-formatted.
func (rule) EvidenceKeys() []string { return nil }

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	sizeGiB, _ := nc.Get("size_gib")
	ageDays, _ := nc.Get("age_days")

	resourceID, _ := n.Str(azurerules.AttrResourceID)
	galleryImageID, _ := n.Str(azurerules.AttrGalleryImageID)
	sizeBytes, _ := n.Num(azurerules.AttrGallerySizeBytes)
	replicaCount, _ := n.Num(azurerules.AttrGalleryReplicaCount)
	refCount, _ := n.Num(azurerules.AttrReferenceCount)

	ev := []rules.Evidence{
		{Key: "resource_id", Value: resourceID},
		{Key: "gallery_image_id", Value: galleryImageID},
		{Key: "size_bytes", Value: fmt.Sprintf("%.0f", sizeBytes)},
		{Key: "size_gib", Value: fmt.Sprintf("%.0f", sizeGiB.(float64))},
		{Key: "replica_count", Value: fmt.Sprintf("%.0f", replicaCount)},
		{Key: "reference_count", Value: fmt.Sprintf("%.0f", refCount)},
		{Key: "age_days", Value: fmt.Sprintf("%.0f", ageDays.(float64))},
		rules.EvMoneyIn("monthly_waste", curStr, branch.Waste, 2),
	}
	if v, ok := nc.Get("money_evidence"); ok {
		ev = append(ev, v.([]rules.Evidence)...)
	}
	if v, ok := nc.Get("price_evidence"); ok {
		ev = append(ev, v.([]rules.Evidence)...)
	}
	return ev
}

// galleryReplicaRegions reads the normalizer's replica_regions attribute in
// both shapes it can have after a graph round-trip: []map[string]any (the
// in-memory normalizer shape) and []any of map[string]any (a JSON-decoded
// snapshot). A malformed shape is treated as absent, so the guard skips with
// SkipMissingAttr.
func galleryReplicaRegions(n *graph.Node) ([]map[string]any, bool) {
	raw, ok := n.Attrs[azurerules.AttrGalleryReplicaRegions]
	if !ok {
		return nil, false
	}
	switch t := raw.(type) {
	case []map[string]any:
		return t, true
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, v := range t {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, false
			}
			out = append(out, m)
		}
		return out, true
	default:
		return nil, false
	}
}
