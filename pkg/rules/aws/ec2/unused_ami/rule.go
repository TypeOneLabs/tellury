// Package unused_ami implements the "unused_ami" FinOps rule: a customer-owned
// AMI has no cost itself, but the EBS snapshots named by its block-device
// mappings bill every month. When no instance, launch template, launch
// configuration, EC2 Fleet or Spot Fleet request references the AMI, those
// backing snapshots are reclaimable by deregistering the AMI and deleting the
// snapshots it exclusively owns.
//
// Cost is deliberately derived from backing_exclusive_size_gb, not from the
// AMI. A snapshot shared with another AMI is excluded from this AMI's waste
// because deleting this AMI does not reclaim it. AWS exposes only the source
// volume size (VolumeSize), not the actual stored bytes, so the price is a
// conservative upper bound and confidence is 0.6.
package unused_ami

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	awsrules "github.com/TypeOneLabs/tellury/pkg/rules/aws"
)

// ID is the stable rule identifier, and the RULE column value in CLI output.
const ID = "unused_ami"

// SnapshotStorageSKU is the pricing catalogue SKU token for EBS snapshot
// storage. It is the design's standard snapshot storage tier, not an
// invented spelling: pkg/pricing/aws indexes the "Storage Snapshot" product
// family under this exact token.
const SnapshotStorageSKU = "standard"

// MinAgeDays is the retention window from the rule design. Only unreferenced
// AMIs at or beyond 90 days old are reported.
const MinAgeDays = 90.0

// MinMonthlyWasteUSD hides findings below $0.10/month (noise floor).
const MinMonthlyWasteUSD = 0.10

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "aws",
		Service:  "ec2",
		Title:    "AMI is not referenced by any instance, launch template, launch configuration, or fleet",
		Description: "A customer-owned AMI costs nothing on its own, but the EBS snapshots " +
			"named by its block-device mappings bill every month. When nothing references the AMI, " +
			"deregister it and delete the snapshots it exclusively owns to reclaim that storage spend.",
		Severity:           rules.SeverityMedium,
		RequiredAssetTypes: []string{awsrules.TypeImage},
		RequiredMetrics:    nil, // pure attribute + reference-count check — zero metric cost
		Remediation: "aws ec2 deregister-image --image-id IMAGE_ID && " +
			"aws ec2 delete-snapshot --snapshot-id SNAPSHOT_ID",
		Origin: "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindImage }

// Guards are evaluated in order; the first to return false decides the
// SkipCode recorded for the node. The engine's built-in exempt-label check
// runs before any of these — a rule must never declare it.
func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		// G1: the specific image asset type this rule targets. Several image
		// rules share graph.KindImage; without this the rule would price GCP
		// custom images or Azure gallery image versions as if they were AMIs.
		{Name: "asset_type", SkipCode: rules.SkipNotTargetAssetType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				return n.AssetType == awsrules.TypeImage
			}},
		// G2: shape valid — an image ID must be present.
		{Name: "image_id_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(awsrules.AttrImageID)
				return ok && v != ""
			}},
		// G3: creation time parseable; stashes the parsed instant once so the
		// age guard, Cost and ExtraEvidence all read the SAME instant.
		{Name: "creation_time_parseable", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				ts, ok := n.Str(awsrules.AttrCreationTimestamp)
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
		// G4: billing state. Only "available" is a stable, billable image
		// state; pending/failed accrue no stable storage charge worth reporting.
		{Name: "state_available", SkipCode: rules.SkipNonBillingStatus,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				s, _ := n.Str(awsrules.AttrState)
				return s == awsrules.ImageStateAvailable
			}},
		// G5: backing snapshot inventory complete. If a block-device mapping
		// named a snapshot that DescribeSnapshots did not return, the rule
		// cannot price the AMI conservatively and must skip.
		{Name: "backing_complete", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				complete, ok := n.Bool(awsrules.AttrBackingComplete)
				return ok && complete
			}},
		// G6: reference enumeration complete. If any reference API failed, the
		// rule refuses to assume the AMI is unreferenced.
		{Name: "references_complete", SkipCode: rules.SkipReferencesUnknown,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				complete, ok := n.Bool(awsrules.AttrReferencesComplete)
				return ok && complete
			}},
		// G7: not referenced. The normalizer writes reference_count
		// unconditionally, so absence means zero — exactly like the other
		// normalizers' countable attributes.
		{Name: "not_referenced", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				cnt, _ := n.Num(awsrules.AttrReferenceCount)
				return cnt == 0
			}},
		// G8: old enough. The design's 90-day retention window suppresses
		// pipeline noise from freshly built AMIs.
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
	// Only snapshots referenced exclusively by this AMI count: deleting the
	// AMI does not reclaim a snapshot another AMI still references.
	sizeGB, _ := n.Num(awsrules.AttrBackingExclusiveSizeGB)
	region := pricing.RegionOf(n.Location)
	unit, resolvedRegion, err := p.Price.UnitPrice(pricing.KindSnapshotStorage, "aws", SnapshotStorageSKU, region)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never a $0 assumption (Invariant I4)
	}
	monthlyWaste := sizeGB * unit

	// Stash the values ExtraEvidence needs. ExtraEvidence has no Pass, so the
	// price-source entry is rendered here — the only place the pricer is
	// reachable — and carried through nc.
	nc.Set("currency", rules.CurrencyOf(p))
	nc.Set("exclusive_size_gb", sizeGB)
	nc.Set("unit_price", unit)
	nc.Set("price_source", rules.PriceEvidence("price_source", p.Price, pricing.KindSnapshotStorage, SnapshotStorageSKU, resolvedRegion))

	return []rules.CostBranch{{
		Waste:      pricing.Round2(monthlyWaste),
		Confidence: 0.6, // source_volume_size basis: AWS exposes no stored-bytes figure
		Label:      "backing_snapshots",
	}}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

// EvidenceKeys auto-collects image_id and image_name as the leading evidence.
// Everything else is computed or formatted in ExtraEvidence.
func (rule) EvidenceKeys() []string {
	return []string{awsrules.AttrImageID, awsrules.AttrImageName}
}

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	sizeGB, _ := nc.Get("exclusive_size_gb")
	unit, _ := nc.Get("unit_price")
	ageDays, _ := nc.Get("age_days")

	ev := []rules.Evidence{
		{Key: "backing_exclusive_size_gb", Value: fmt.Sprintf("%.0f", sizeGB.(float64))},
		{Key: "age_days", Value: fmt.Sprintf("%.0f", ageDays.(float64))},
		{Key: "size_basis", Value: "source_volume_size"},
		rules.EvMoneyIn("unit_price_gib_month", curStr, unit.(float64), 4),
	}
	if v, ok := nc.Get("price_source"); ok {
		ev = append(ev, v.(rules.Evidence))
	}
	if count, ok := n.Num(awsrules.AttrBackingSnapshotCount); ok {
		ev = append(ev, rules.Evidence{Key: "backing_snapshot_count", Value: fmt.Sprintf("%.0f", count)})
	}
	if refs, ok := n.Num(awsrules.AttrReferenceCount); ok {
		ev = append(ev, rules.Evidence{Key: "reference_count", Value: fmt.Sprintf("%.0f", refs)})
	}
	return ev
}
