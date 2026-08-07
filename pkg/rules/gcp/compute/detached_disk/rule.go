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

func init() { rules.Register(rule{}) }

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

func (rule) Eval(ctx context.Context, p *rules.Pass) ([]rules.Finding, error) {
	var out []rules.Finding

	p.Graph.ByKind(graph.KindDisk, func(n *graph.Node) bool {
		if ctx.Err() != nil {
			return false
		}

		// P0: exemption label.
		if n.Labels["tellury-exempt"] == "true" {
			p.SkipNode(ID, n.ID, rules.SkipExemptLabel)
			return true
		}

		// P1: shape valid.
		sizeGB, ok := n.Num("size_gb")
		if !ok || sizeGB <= 0 {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}
		diskType, ok := n.Str("disk_type")
		if !ok || diskType == "" {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}
		status, ok := n.Str("status")
		if !ok || status == "" {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}
		creationTS, ok := n.Str("creation_timestamp")
		if !ok {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}
		createdAt, err := time.Parse(time.RFC3339, creationTS)
		if err != nil {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}

		// P2: no graph attachment.
		if p.Graph.InDegree(n.ID, graph.EdgeAttachedTo) > 0 {
			p.SkipNode(ID, n.ID, rules.SkipAttached)
			return true
		}

		// P3: no CAI users.
		userCount, _ := n.Num("user_count") // absent => treat as 0
		if userCount > 0 {
			p.SkipNode(ID, n.ID, rules.SkipAttached)
			return true
		}

		// P4: billing status.
		if !billingStatuses[status] {
			p.SkipNode(ID, n.ID, rules.SkipNonBillingStatus)
			return true
		}

		// P5: detached long enough.
		detachedDays, ageBasis := detachedDays(p.Now, n, createdAt)
		if detachedDays < MinDetachedDays {
			p.SkipNode(ID, n.ID, rules.SkipRecentlyDetached)
			return true
		}

		// Cost formula (§6.2): capacity + IOPS + throughput.
		replicaZones, _ := n.Num("replica_zone_count")
		sku := diskSKU(diskType, replicaZones)
		region := pricing.RegionOf(n.Location)

		capPrice, capRegion, err := p.Price.UnitPrice(pricing.KindDiskCapacity, "gcp", sku, region)
		if err != nil {
			p.SkipNode(ID, n.ID, rules.SkipNoPrice)
			return true
		}
		iopsPrice, iopsRegion, _ := p.Price.UnitPrice(pricing.KindDiskIOPS, "gcp", sku, region)
		thrPrice, thrRegion, _ := p.Price.UnitPrice(pricing.KindDiskThroughput, "gcp", sku, region)

		iops, _ := n.Num("provisioned_iops")
		mbps, _ := n.Num("provisioned_throughput_mbps")

		monthlyWaste := sizeGB*capPrice + iops*iopsPrice + mbps*thrPrice

		// P7: material.
		if monthlyWaste < MinMonthlyWasteUSD {
			p.SkipNode(ID, n.ID, rules.SkipBelowMinWaste)
			return true
		}

		confidence := confidenceFor(ageBasis, status)

		// One price-source evidence entry per component that contributed a
		// nonzero amount. A live answer for capacity and an embedded answer
		// for IOPS/throughput must both stay visible on the Finding instead
		// of collapsing onto the dominant capacity source.
		pricedComps := diskPricedComponents(sku, region, capRegion, iopsPrice, iopsRegion, thrPrice, thrRegion, sizeGB, iops, mbps)

		ev := []rules.Evidence{
			{Key: "size_gb", Value: fmt.Sprintf("%.0f", sizeGB)},
			{Key: "disk_type", Value: diskType},
			{Key: "disk_sku", Value: sku},
			{Key: "status", Value: status},
			{Key: "attached_instances", Value: "0"},
			{Key: "detached_days", Value: fmt.Sprintf("%.0f", detachedDays)},
			{Key: "age_basis", Value: ageBasis},
			{Key: "unit_price_gib_month", Value: fmt.Sprintf("$%.4f", capPrice)},
		}
		ev = append(ev, rules.PriceEvidenceFor("price_source", p.Price, pricedComps...)...)
		if iops > 0 {
			ev = append(ev, rules.Evidence{Key: "provisioned_iops", Value: fmt.Sprintf("%.0f", iops)})
		}
		if mbps > 0 {
			ev = append(ev, rules.Evidence{Key: "provisioned_throughput_mbps", Value: fmt.Sprintf("%.0f", mbps)})
		}

		out = append(out, rules.Finding{
			RuleID:          ID,
			ResourceID:      n.ID,
			Resource:        n.Display(),
			Kind:            n.Kind,
			Project:         n.Project,
			Location:        n.Location,
			MonthlyWasteUSD: monthlyWaste,
			Confidence:      round2(confidence),
			Evidence:        ev,
		})
		return true
	})
	return out, nil
}

// diskPricedComponents returns the components of a disk's monthly cost that
// contributed a nonzero dollar amount. Capacity always contributes (a
// captured error was already fatal in Eval); IOPS and throughput contribute
// only if they have both a resolved price and a provisioned quantity.
// sku/region are the lookup keys; capRegion/iopsRegion/thrRegion are the
// regions each lookup actually resolved to.
func diskPricedComponents(sku, region, capRegion string, iopsPrice float64, iopsRegion string, thrPrice float64, thrRegion string, sizeGB, iops, mbps float64) []rules.PricedComponent {
	comps := make([]rules.PricedComponent, 0, 3)

	// Capacity: sizeGB is guaranteed > 0 by P1, and capPrice is guaranteed
	// resolvable by Eval's early return, so this leg always contributes.
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
