// Package unattached_managed_disk implements the "unattached_managed_disk"
// FinOps rule: an Azure managed disk in the Unattached state has no VM using
// it and still bills its full per-disk-month tier rate while doing nothing.
// It is the Azure mirror of the GCP detached_disk rule and the AWS
// unattached_ebs_volume rule: the same guard structure with typed skip codes,
// the same $0.10/month noise floor, and evidence carrying the ARM SKU, the
// billable tier token, the detached age and the unit price with its
// provenance.
package unattached_managed_disk

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	pricingazure "github.com/TypeOneLabs/tellury/pkg/pricing/azure"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	azurerules "github.com/TypeOneLabs/tellury/pkg/rules/azure"
)

// ID is the stable rule identifier, and the RULE column value in CLI output.
const ID = "unattached_managed_disk"

// Thresholds and constants from the Azure rule spec.
const (
	// MinDetachedDays suppresses noise from disks freed moments ago during
	// legitimate maintenance. Azure exposes no detach-history timestamp in
	// Resource Graph, so the age is measured from time_created.
	MinDetachedDays = 7.0
	// MinMonthlyWasteUSD hides findings below $0.10/month (noise floor).
	MinMonthlyWasteUSD = 0.10
)

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "azure",
		Service:  "compute",
		Title:    "Managed disk is not attached to any virtual machine",
		Description: "An Azure managed disk in the Unattached state is attached " +
			"to no virtual machine and still bills its full per-disk-month tier " +
			"rate while doing nothing. Snapshot and delete it, or attach it to " +
			"the VM that needs it.",
		Severity:           rules.SeverityMedium,
		RequiredAssetTypes: []string{azurerules.TypeDisk},
		RequiredMetrics:    nil, // pure attribute + ARG field check — zero metric cost
		Remediation:        "az disk delete --ids ID --yes",
		Origin:             "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindDisk }

// Guards are evaluated in order; the first to return false decides the
// SkipCode recorded for the node. The engine's built-in exempt-label check
// runs before any of these — a rule must never declare it.
func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		// G1: shape valid — a billable size.
		{Name: "disk_size_gb_positive", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Num(azurerules.AttrDiskSizeGB)
				return ok && v > 0
			}},
		// G2: ARM SKU present — it is the input to the disk-tier mapping;
		// without it the rule would have to guess a rate.
		{Name: "sku_name_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(azurerules.AttrSKUName)
				return ok && v != ""
			}},
		// G3: disk state present. A missing state is an unparsed payload, not
		// "not attached" and not "non-billing".
		{Name: "disk_state_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(azurerules.AttrDiskState)
				return ok && v != ""
			}},
		// G4: creation time parseable; stashes the parsed instant once so the
		// age guard, Cost and ExtraEvidence all read the SAME instant.
		{Name: "time_created_parseable", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				createdAt, ok := n.Time(azurerules.AttrTimeCreated)
				if !ok {
					return false
				}
				nc.Set("created_at", createdAt)
				return true
			}},
		// G5: not attached. Both ARM signals must say unattached: diskState
		// is not "Attached" and managedBy is empty. The normalizer writes
		// managed_by unconditionally ("" means unattached), so the empty
		// string is a business fact, never a missing attribute.
		{Name: "not_attached", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				state, _ := n.Str(azurerules.AttrDiskState)
				if state == "Attached" {
					return false
				}
				managedBy, _ := n.Str(azurerules.AttrManagedBy)
				return managedBy == ""
			}},
		// G6: billing state. Only "Unattached" is a stable, billing,
		// unattached state. Other non-attached states (e.g. a disk mid-
		// deletion) accrue no stable charge worth reporting.
		{Name: "billing_state", SkipCode: rules.SkipNonBillingStatus,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				state, _ := n.Str(azurerules.AttrDiskState)
				return state == "Unattached"
			}},
		// G7: detached long enough. Azure exposes no detach history in the
		// ARG row (no last-detach timestamp), so time_created is the only age
		// basis — the "creation_fallback" basis with reduced confidence,
		// exactly as the GCP and AWS disk rules handle the same situation.
		// The guard computes detached_days once; Cost (confidence) and
		// ExtraEvidence (detached_days, age_basis) read the same value.
		{Name: "detached_long_enough", SkipCode: rules.SkipRecentlyDetached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				createdAt, _ := nc.Get("created_at")
				created := createdAt.(time.Time)
				days := math.Floor(p.Now.Sub(created).Hours() / 24)
				if days < MinDetachedDays {
					return false
				}
				nc.Set("detached_days", days)
				nc.Set("age_basis", "creation_fallback")
				return true
			}},
	}
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	// disk_size_gb and sku_name are guaranteed present and valid by the shape
	// guards. The ARM SKU + size maps to a fixed Retail Prices API tier token
	// (e.g. Premium_LRS + 128 GiB -> "P10 LRS"). An unknown size or SKU is a
	// price-resolution failure: the rule skips rather than guessing at the
	// next tier up.
	armSKUName, _ := n.Str(azurerules.AttrSKUName)
	sizeGB, _ := n.Num(azurerules.AttrDiskSizeGB)

	diskSKU, ok := pricingazure.ManagedDiskTierSKU(armSKUName, sizeGB)
	if !ok {
		return nil, pricing.ErrNoPrice // engine records SkipNoPrice; never a guessed tier
	}

	region := pricing.RegionOf(n.Location)
	unit, resolvedRegion, err := p.Price.UnitPrice(pricing.KindManagedDisk, "azure", diskSKU, region)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never a $0 assumption (Invariant I4)
	}

	// Stash the values ExtraEvidence needs. ExtraEvidence has no Pass, so the
	// price-source entry is rendered here — the only place the pricer is
	// reachable — and carried through nc.
	nc.Set("currency", rules.CurrencyOf(p))
	nc.Set("disk_sku", diskSKU)
	nc.Set("unit_price_disk_month", unit)
	nc.Set("price_source", rules.PriceEvidence("price_source", p.Price, pricing.KindManagedDisk, diskSKU, resolvedRegion))

	return []rules.CostBranch{{
		// A flat per-disk-month charge: the whole monthly cost is waste, with
		// no capacity or IOPS partial component to subtract.
		Waste:      pricing.Round2(unit),
		Confidence: 0.85, // creation_fallback age basis: Azure exposes no detach history
		Label:      "unattached",
	}}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

// EvidenceKeys returns nil: the leading evidence entries (size_gb, sku_name,
// disk_state, time_created) are rendered by ExtraEvidence so their formatting
// is explicit, and the rest are computed (disk_sku, detached_days, age_basis)
// or money-formatted (unit_price_disk_month, price_source).
func (rule) EvidenceKeys() []string { return nil }

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	sizeGB, _ := n.Num(azurerules.AttrDiskSizeGB)
	armSKU, _ := n.Str(azurerules.AttrSKUName)
	state, _ := n.Str(azurerules.AttrDiskState)
	managedBy, _ := n.Str(azurerules.AttrManagedBy)
	created, _ := n.Str(azurerules.AttrTimeCreated)

	diskSKU, _ := nc.Get("disk_sku")
	unit, _ := nc.Get("unit_price_disk_month")
	detachedDays, _ := nc.Get("detached_days")
	ageBasis, _ := nc.Get("age_basis")

	ev := []rules.Evidence{
		{Key: "size_gb", Value: fmt.Sprintf("%.0f", sizeGB)},
		{Key: "sku_name", Value: armSKU},
		{Key: "disk_sku", Value: diskSKU.(string)},
		{Key: "disk_state", Value: state},
		{Key: "managed_by", Value: managedBy},
		{Key: "time_created", Value: created},
		{Key: "detached_days", Value: fmt.Sprintf("%.0f", detachedDays.(float64))},
		{Key: "age_basis", Value: ageBasis.(string)},
		rules.EvMoneyIn("unit_price_disk_month", curStr, unit.(float64), 4),
	}
	if v, ok := nc.Get("price_source"); ok {
		ev = append(ev, v.(rules.Evidence))
	}
	return ev
}
