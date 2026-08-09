// Package detached_disk implements the "detached_disk" FinOps rule: a
// persistent disk with no attached instance still bills at the full
// provisioned-capacity rate. See docs/rule-specs for the exact thresholds
// and cost formula this file implements verbatim.
package detached_disk

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// ID is the stable rule identifier, and the RULE column value in CLI output.
const ID = "detached_disk"

// Thresholds and constants from the rule spec (verbatim, §6).
const (
	// MinDetachedDays suppresses noise from disks detached moments ago
	// during legitimate maintenance.
	MinDetachedDays = 7.0
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
		Title:    "Persistent disk is not attached to any instance",
		Description: "A zonal or regional persistent disk with no attached " +
			"instance still bills at the full provisioned-capacity rate. " +
			"Snapshot and delete, or attach it.",
		Severity: rules.SeverityMedium,
		RequiredAssetTypes: []string{
			"compute.googleapis.com/Disk",
			// MANDATORY: without Instance assets no attached_to edges exist
			// and every disk in scope would be misreported as detached.
			"compute.googleapis.com/Instance",
		},
		RequiredMetrics: nil, // pure graph topology - zero metric cost
		Remediation: "gcloud compute disks snapshot NAME --zone ZONE " +
			"--snapshot-names NAME-pre-delete && " +
			"gcloud compute disks delete NAME --zone ZONE --quiet",
		Origin: "native",
	}
}

// billingStatuses are the disk statuses that still incur capacity charges.
// CREATING/DELETING are excluded deliberately (spec P4): a disk mid-creation
// has no stable billable capacity, and a disk mid-deletion will be gone
// before remediation could ever run.
var billingStatuses = map[string]bool{
	"READY":       true,
	"FAILED":      true,
	"RESTORING":   true,
	"UNAVAILABLE": true,
}

func (rule) Kind() graph.ResourceKind { return graph.KindDisk }

func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		// P1: shape valid. Four checks share the missing-attribute code,
		// each independently named so a future --explain-skips can report
		// exactly which shape field failed.
		{Name: "size_gb_positive", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Num("size_gb")
				return ok && v > 0
			}},
		{Name: "disk_type_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str("disk_type")
				return ok && v != ""
			}},
		{Name: "status_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str("status")
				return ok && v != ""
			}},
		{Name: "creation_timestamp_parseable", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				ts, ok := n.Str("creation_timestamp")
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

		// P2: no graph attachment.
		{Name: "not_attached", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				return p.Graph.InDegree(n.ID, graph.EdgeAttachedTo) == 0
			}},

		// P3: no CAI users.
		{Name: "no_cai_users", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				// ABSENCE MEANS ZERO: the normalizer writes user_count
				// unconditionally (users[] is always counted, even when
				// empty), so a missing key means "no users", never an
				// unknown to skip on. Skipping on absence would silently
				// lose every capacity-only finding for a disk whose users[]
				// was not parsed.
				uc, _ := n.Num("user_count")
				return uc == 0
			}},

		// P4: billing status.
		{Name: "billing_status", SkipCode: rules.SkipNonBillingStatus,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				s, _ := n.Str("status")
				return billingStatuses[s]
			}},

		// P5: detached long enough. This guard also computes the two values
		// Cost (confidence) and ExtraEvidence (detached_days, age_basis)
		// consume, so the three-branch detachedDays logic lives here exactly
		// once and every downstream reader sees the same answer.
		{Name: "detached_long_enough", SkipCode: rules.SkipRecentlyDetached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				createdAt, _ := nc.Get("created_at")
				days, basis := detachedDays(p.Now, n, createdAt.(time.Time))
				if days < MinDetachedDays {
					return false
				}
				nc.Set("detached_days", days)
				nc.Set("age_basis", basis)
				return true
			}},
	}
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	// size_gb, disk_type and status are guaranteed present and valid by the
	// shape guards; the defaulting reads keep the accessor style uniform.
	sizeGB, _ := n.Num("size_gb")
	diskType, _ := n.Str("disk_type")
	status, _ := n.Str("status")

	// ABSENCE MEANS ZERO (defaulting accessor): replica_zone_count,
	// provisioned_iops and provisioned_throughput_mbps are written
	// unconditionally by the normalizer, so a missing key is "0", never an
	// unknown to skip on. Skipping on absence would silently lose the
	// capacity-only findings for any disk whose IOPS/throughput payload was
	// not parsed — and every existing fixture carries these fields, so no
	// test would catch the regression.
	replicaZones, _ := n.Num("replica_zone_count")
	sku := diskSKU(diskType, replicaZones)
	region := pricing.RegionOf(n.Location)

	capPrice, capRegion, err := p.Price.UnitPrice(pricing.KindDiskCapacity, "gcp", sku, region)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never a $0 assumption (Invariant I4)
	}
	iopsPrice, iopsRegion, _ := p.Price.UnitPrice(pricing.KindDiskIOPS, "gcp", sku, region)
	thrPrice, thrRegion, _ := p.Price.UnitPrice(pricing.KindDiskThroughput, "gcp", sku, region)

	iops, _ := n.Num("provisioned_iops")
	mbps, _ := n.Num("provisioned_throughput_mbps")

	monthlyWaste := sizeGB*capPrice + iops*iopsPrice + mbps*thrPrice

	ageBasis, _ := nc.Get("age_basis")
	confidence := round2(confidenceFor(ageBasis.(string), status))

	// Stash the values ExtraEvidence needs. The price-source entries are
	// rendered here because ExtraEvidence has no Pass to reach the pricer.
	comps := diskPricedComponents(sku, region, capRegion, iopsPrice, iopsRegion, thrPrice, thrRegion, sizeGB, iops, mbps)
	nc.Set("disk_sku", sku)
	nc.Set("cap_price", capPrice)
	nc.Set("price_source_evidence", rules.PriceEvidenceFor("price_source", p.Price, comps...))
	nc.Set("provisioned_iops", iops)
	nc.Set("provisioned_throughput_mbps", mbps)

	return []rules.CostBranch{{
		Waste:      monthlyWaste,
		Confidence: confidence,
		Label:      "detached",
	}}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

// EvidenceKeys returns nil: the first evidence entry (size_gb) is rendered
// with %.0f, which %v would not reproduce for fractional sizes, and the rest
// are computed (detached_days, age_basis), formatted ($%.4f) or conditional
// (provisioned_iops / provisioned_throughput_mbps). ExtraEvidence renders the
// whole list so the pre-refactor keys, values and order stay byte-for-byte.
func (rule) EvidenceKeys() []string { return nil }

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	sizeGB, _ := n.Num("size_gb")
	diskType, _ := n.Str("disk_type")
	status, _ := n.Str("status")

	sku, _ := nc.Get("disk_sku")
	capPrice, _ := nc.Get("cap_price")
	detachedDays, _ := nc.Get("detached_days")
	ageBasis, _ := nc.Get("age_basis")

	ev := []rules.Evidence{
		{Key: "size_gb", Value: fmt.Sprintf("%.0f", sizeGB)},
		{Key: "disk_type", Value: diskType},
		{Key: "disk_sku", Value: sku.(string)},
		{Key: "status", Value: status},
		{Key: "attached_instances", Value: "0"},
		{Key: "detached_days", Value: fmt.Sprintf("%.0f", detachedDays.(float64))},
		{Key: "age_basis", Value: ageBasis.(string)},
		{Key: "unit_price_gib_month", Value: fmt.Sprintf("$%.4f", capPrice.(float64))},
	}
	if v, ok := nc.Get("price_source_evidence"); ok {
		ev = append(ev, v.([]rules.Evidence)...)
	}
	if iops, _ := nc.Get("provisioned_iops"); iops.(float64) > 0 {
		ev = append(ev, rules.Evidence{Key: "provisioned_iops", Value: fmt.Sprintf("%.0f", iops.(float64))})
	}
	if mbps, _ := nc.Get("provisioned_throughput_mbps"); mbps.(float64) > 0 {
		ev = append(ev, rules.Evidence{Key: "provisioned_throughput_mbps", Value: fmt.Sprintf("%.0f", mbps.(float64))})
	}
	return ev
}

// diskPricedComponents returns the components of a disk's monthly cost that
// contributed a nonzero dollar amount. Capacity always contributes (a
// captured error was already fatal in Cost); IOPS and throughput contribute
// only if they have both a resolved price and a provisioned quantity.
// sku/region are the lookup keys; capRegion/iopsRegion/thrRegion are the
// regions each lookup actually resolved to.
func diskPricedComponents(sku, region, capRegion string, iopsPrice float64, iopsRegion string, thrPrice float64, thrRegion string, sizeGB, iops, mbps float64) []rules.PricedComponent {
	comps := make([]rules.PricedComponent, 0, 3)

	// Capacity: sizeGB is guaranteed > 0 by P1, and capPrice is guaranteed
	// resolvable by Cost's early return, so this leg always contributes.
	if sizeGB != 0 {
		comps = append(comps, rules.PricedComponent{
			Kind:   pricing.KindDiskCapacity,
			SKU:    sku,
			Region: capRegion,
			Key:    "capacity",
		})
	}
	if iops > 0 && iopsPrice != 0 {
		comps = append(comps, rules.PricedComponent{
			Kind:   pricing.KindDiskIOPS,
			SKU:    sku,
			Region: iopsRegion,
			Key:    "iops",
		})
	}
	if mbps > 0 && thrPrice != 0 {
		comps = append(comps, rules.PricedComponent{
			Kind:   pricing.KindDiskThroughput,
			SKU:    sku,
			Region: thrRegion,
			Key:    "throughput",
		})
	}
	if len(comps) == 0 {
		// Defensive: capacity must have contributed by construction; never
		// return an empty provenance list.
		comps = append(comps, rules.PricedComponent{
			Kind:   pricing.KindDiskCapacity,
			SKU:    sku,
			Region: capRegion,
			Key:    "capacity",
		})
	}
	return comps
}

// detachedDays is the total function from spec §6.1, three branches:
//  1. last_detach_time present -> age_basis = "last_detach"
//  2. never attached (no last_attach_time) -> age_basis = "never_attached"
//  3. CAI inconsistency fallback -> age_basis = "creation_fallback"
func detachedDays(now time.Time, n *graph.Node, createdAt time.Time) (float64, string) {
	if ts, ok := n.Str("last_detach_time"); ok {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return now.Sub(t).Hours() / 24, "last_detach"
		}
	}
	if _, hasAttach := n.Str("last_attach_time"); !hasAttach {
		return now.Sub(createdAt).Hours() / 24, "never_attached"
	}
	return now.Sub(createdAt).Hours() / 24, "creation_fallback"
}

// diskSKU derives the pricing SKU from disk_type + replica_zone_count (§5.3).
func diskSKU(diskType string, replicaZones float64) string {
	if replicaZones >= 2 {
		return diskType + "-regional"
	}
	return diskType
}

// confidenceFor implements §6.4 exactly.
func confidenceFor(ageBasis, status string) float64 {
	c := 1.0
	if ageBasis == "creation_fallback" {
		c = 0.9
	}
	if status == "FAILED" || status == "UNAVAILABLE" {
		c *= 0.9
	}
	return c
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
