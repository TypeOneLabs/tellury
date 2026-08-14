// Package orphaned_ami_snapshot implements the "orphaned_ami_snapshot" FinOps
// rule: an EBS snapshot that AWS created on behalf of an AMI, whose AMI has
// since been deregistered.
//
// This is the other half of unused_ami, and the half that actually costs money
// after an operator acts. Deregistering an AMI does NOT delete the snapshots
// behind it — AWS keeps them, they no longer appear under AMIs in the console,
// and they bill per GiB-month forever. Someone who dutifully deregisters every
// unused AMI therefore still pays the whole storage bill, with nothing in the
// UI pointing at why.
//
// An AMI-created snapshot is identified by the description AWS generates for
// it, which begins "Created by CreateImage(". That is AWS's own convention, not
// a heuristic of ours: a snapshot taken by hand or by a backup tool does not
// carry it, and is out of scope here because deleting it is a different
// decision with different risk.
//
// Cost is volume_size_gb * the snapshot storage rate. AWS exposes the SOURCE
// VOLUME size, never the compressed bytes actually stored, so the figure is a
// conservative upper bound and confidence is 0.6 — the same basis and the same
// confidence as unused_ami.
package orphaned_ami_snapshot

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
const ID = "orphaned_ami_snapshot"

// SnapshotStorageSKU is the pricing catalogue SKU token for EBS snapshot
// storage — the same token unused_ami prices through, and the one
// pkg/pricing/aws indexes the "Storage Snapshot" family under.
const SnapshotStorageSKU = "standard"

// MinAgeDays is the retention window from the rule design: only orphans at or
// beyond 90 days are reported, so a snapshot left behind by an AMI rebuild
// that is about to be re-registered is not flagged.
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
		Title:    "EBS snapshot was created for an AMI that no longer exists",
		Description: "Deregistering an AMI does not delete the EBS snapshots behind it. They " +
			"stop appearing under AMIs in the console and keep billing per GiB-month. This rule " +
			"reports snapshots AWS created for an AMI when no current AMI references them.",
		Severity:           rules.SeverityMedium,
		RequiredAssetTypes: []string{awsrules.TypeSnapshot},
		RequiredMetrics:    nil, // pure attribute + reference-count check — zero metric cost
		Remediation:        "aws ec2 delete-snapshot --snapshot-id SNAPSHOT_ID",
		Origin:             "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindSnapshot }

// Guards are evaluated in order; the first to return false decides the
// SkipCode recorded for the node.
func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		{Name: "asset_type", SkipCode: rules.SkipNotTargetAssetType,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				return n.AssetType == awsrules.TypeSnapshot
			}},

		{Name: "snapshot_id_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(awsrules.AttrSnapshotID)
				return ok && v != ""
			}},

		{Name: "volume_size_gb_positive", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Num(awsrules.AttrVolumeSizeGB)
				if !ok || v <= 0 {
					return false
				}
				nc.Set("size_gb", v)
				return true
			}},

		{Name: "creation_time_parseable", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				ts, ok := n.Str(awsrules.AttrCreationTimestamp)
				if !ok {
					return false
				}
				created, err := time.Parse(time.RFC3339, ts)
				if err != nil {
					return false
				}
				nc.Set("created_at", created)
				return true
			}},

		// A snapshot still being created, or in error, is not a stable target:
		// its size is not final and deleting it is not the right advice.
		{Name: "state_completed", SkipCode: rules.SkipNonBillingStatus,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				s, _ := n.Str(awsrules.AttrState)
				return s == awsrules.SnapshotStateCompleted
			}},

		// AWS's own description convention. A snapshot without it was made by
		// hand or by a backup tool: deleting that is a different decision with
		// different risk, and this rule declines to make it.
		{Name: "ami_created", SkipCode: rules.SkipNotAMISnapshot,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, _ := n.Bool(awsrules.AttrAMICreated)
				return v
			}},

		// If DescribeImages could not be read, the AMI inventory is unknown and
		// a zero reference count means nothing. Skip rather than conclude the
		// snapshot is orphaned — deleting a snapshot still backing a live AMI
		// destroys the AMI.
		{Name: "ami_reference_complete", SkipCode: rules.SkipReferencesUnknown,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Bool(awsrules.AttrAMIReferenceComplete)
				return ok && v
			}},

		{Name: "not_referenced_by_ami", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				// ABSENCE IS NOT ZERO: the normalizer writes this
				// unconditionally, so a missing value means the payload was not
				// parsed, not that nothing references the snapshot.
				count, ok := n.Num(awsrules.AttrReferencedByAMICount)
				return ok && count == 0
			}},

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

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	sizeAny, _ := nc.Get("size_gb")
	sizeGB := sizeAny.(float64)

	region := pricing.RegionOf(n.Location)
	unit, resolvedRegion, err := p.Price.UnitPrice(pricing.KindSnapshotStorage, "aws", SnapshotStorageSKU, region)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never a $0 assumption
	}

	// PriceEvidence (singular) is deliberate: there is exactly one priced
	// component here. PriceEvidenceFor returns a []Evidence, and stashing a
	// slice behind a value-typed read in ExtraEvidence silently drops the
	// provenance rather than failing.
	nc.Set("price_source", rules.PriceEvidence("price_source", p.Price,
		pricing.KindSnapshotStorage, SnapshotStorageSKU, resolvedRegion))
	nc.Set("currency", rules.CurrencyOf(p))
	nc.Set("unit_price", unit)

	return []rules.CostBranch{{
		Label:      "delete",
		Waste:      pricing.Round2(sizeGB * unit),
		Confidence: 0.6,
	}}, nil
}

// EvidenceKeys auto-collects the three node attrs that carry the rule's whole
// case: which snapshot, how big, and the reference count the rule checked
// rather than assumed (the not_referenced_by_ami guard makes it zero here).
// Everything else is computed or formatted in ExtraEvidence — a key must appear
// in exactly one of the two, or the engine emits it twice.
func (rule) EvidenceKeys() []string {
	return []string{awsrules.AttrSnapshotID, awsrules.AttrVolumeSizeGB, awsrules.AttrReferencedByAMICount}
}

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, _ rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	unit, _ := nc.Get("unit_price")

	out := []rules.Evidence{}
	if v, ok := nc.Get("age_days"); ok {
		out = append(out, rules.Evidence{Key: "age_days", Value: fmt.Sprintf("%.0f", v.(float64))})
	}
	// Same basis and caveat as unused_ami: AWS exposes the source volume size,
	// never the compressed bytes actually stored.
	out = append(out, rules.Evidence{Key: "size_basis", Value: "source_volume_size"})
	if u, ok := unit.(float64); ok {
		out = append(out, rules.EvMoneyIn("unit_price_gib_month", curStr, u, 4))
	}
	if v, ok := nc.Get("price_source"); ok {
		out = append(out, v.(rules.Evidence))
	}
	return out
}
