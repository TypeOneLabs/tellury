// Package unattached_ebs_volume implements the "unattached_ebs_volume"
// FinOps rule: an EBS volume in state "available" is attached to no instance
// and still bills at its full provisioned rate — capacity, plus provisioned
// IOPS/throughput for the types that bill them — while doing nothing. It is
// the AWS mirror of the GCP detached_disk rule: the same guard structure with
// typed skip codes, the same $0.10/month noise floor, and evidence carrying
// the size, type and unit price with its provenance.
package unattached_ebs_volume

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
const ID = "unattached_ebs_volume"

// Thresholds and constants from the AWS rule spec.
const (
	// MinDetachedDays suppresses noise from volumes freed moments ago during
	// legitimate maintenance. AWS exposes no detach history in DescribeVolumes
	// (no last-detach timestamp), so the age is measured from create_time.
	MinDetachedDays = 7.0
	// MinMonthlyWasteUSD hides findings below $0.10/month (noise floor).
	MinMonthlyWasteUSD = 0.10
)

// VolumeSKU derives the pricing SKU for a volume: the volume-type token
// exactly as DescribeVolumes returns it (ec2types.VolumeType, e.g. "gp3") —
// the same string the embedded table's disk_capacity/disk_iops/
// disk_throughput entries and the live catalogue's volumeApiName attribute
// index EBS prices under. It is identity today; the function exists so
// pkg/pricing/aws/catalog_test.go can pin the token this rule queries against
// the token the live catalogue produces, and a drift fails the test instead
// of silently falling back to the embedded table.
func VolumeSKU(volumeType string) string { return volumeType }

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "aws",
		Service:  "ec2",
		Title:    "EBS volume is not attached to any instance",
		Description: "An EBS volume in state \"available\" is attached to no " +
			"instance and still bills at its full provisioned rate — capacity, " +
			"plus provisioned IOPS/throughput for gp3/io1/io2 — while doing " +
			"nothing. Snapshot and delete it, or attach it to the instance " +
			"that needs it.",
		Severity:           rules.SeverityMedium,
		RequiredAssetTypes: []string{awsrules.TypeVolume},
		RequiredMetrics:    nil, // pure attribute + topology check - zero metric cost
		Remediation: "aws ec2 create-snapshot --volume-id ID --description 'NAME pre-delete' && " +
			"aws ec2 delete-volume --volume-id ID",
		Origin: "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindDisk }

// Guards are evaluated in order; the first to return false decides the
// SkipCode recorded for the node. The engine's built-in exempt-label check
// runs before any of these — a rule must never declare it.
func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		// G1: shape valid — a billable size.
		{Name: "size_gb_positive", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Num(awsrules.AttrSizeGB)
				return ok && v > 0
			}},
		// G2: volume type present — it is the pricing SKU; without it the
		// rule would have to guess a rate.
		{Name: "volume_type_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(awsrules.AttrVolumeType)
				return ok && v != ""
			}},
		// G3: state present.
		{Name: "state_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str(awsrules.AttrState)
				return ok && v != ""
			}},
		// G4: creation time parseable; stashes the parsed instant once so the
		// age guard, Cost and ExtraEvidence all read the SAME instant.
		{Name: "creation_time_parseable", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				ts, ok := n.Str(awsrules.AttrCreateTime)
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
		// G5: not attached — both the node's own attachment_count (always
		// written by the normalizer, 0 when none) AND the graph's attached_to
		// edges must be empty. The provider creates instance -> volume
		// EdgeAttachedTo edges from the same DescribeVolumes.Attachments
		// slice, so the two signals are the dual cross-check detached_disk
		// uses (no CAI users AND no graph edge).
		//
		// ABSENCE MEANS ZERO: the normalizer writes attachment_count
		// unconditionally, so a missing key means "no attachments", never an
		// unknown to skip on.
		{Name: "not_attached", SkipCode: rules.SkipAttached,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				cnt, _ := n.Num(awsrules.AttrAttachmentCount)
				return cnt == 0 && p.Graph.InDegree(n.ID, graph.EdgeAttachedTo) == 0
			}},
		// G6: billing state. Only "available" is a stable, billing,
		// unattached state: an in-use volume is attached, and creating /
		// deleting / deleted / error accrue no stable charge worth reporting.
		{Name: "available_state", SkipCode: rules.SkipNonBillingStatus,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				s, _ := n.Str(awsrules.AttrState)
				return s == awsrules.StateAvailable
			}},
		// G7: detached long enough. AWS exposes no detach history in the
		// DescribeVolumes response (no last-detach timestamp), so create_time
		// is the only age basis — the "creation_fallback" basis with reduced
		// confidence, exactly as the GCP detached_disk rule handles the same
		// situation. The guard computes detached_days once; Cost (confidence)
		// and ExtraEvidence (detached_days, age_basis) read the same value.
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
	// size_gb, volume_type and state are guaranteed present and valid by the
	// shape guards; iops/throughput are written unconditionally by the
	// normalizer only when the volume has them (0 when absent), so a missing
	// key means 0 provisioned, never an unknown.
	sizeGB, _ := n.Num(awsrules.AttrSizeGB)
	volumeType, _ := n.Str(awsrules.AttrVolumeType)
	sku := VolumeSKU(volumeType)
	region := pricing.RegionOf(n.Location)

	// Capacity is the mandatory leg: a price lookup failure is an error, never
	// a $0 assumption (Invariant I4); the engine records SkipNoPrice.
	capPrice, capRegion, err := p.Price.UnitPrice(pricing.KindDiskCapacity, "aws", sku, region)
	if err != nil {
		return nil, err
	}
	// IOPS/throughput are auxiliary legs: a type without a provisioned-IOPS
	// charge (gp2, st1, sc1, standard) has no such price in the catalogue, and
	// a miss there means "no charge", not an error — exactly how detached_disk
	// treats the same two legs.
	iopsPrice, iopsRegion, _ := p.Price.UnitPrice(pricing.KindDiskIOPS, "aws", sku, region)
	thrPrice, thrRegion, _ := p.Price.UnitPrice(pricing.KindDiskThroughput, "aws", sku, region)

	iops, _ := n.Num(awsrules.AttrIops)
	mbps, _ := n.Num(awsrules.AttrThroughput)

	// Only provisioning ABOVE the type's included baseline is billable. gp3
	// ships 3000 IOPS and 125 MiB/s free and EVERY gp3 volume reports them,
	// so charging the raw figures added $20/month of cost that does not exist
	// to every gp3 volume in an account — a 1 GiB volume priced at $20.08
	// against a real $0.08. The offer file does not encode this: its price
	// dimension reads "per provisioned IOPS-month" with no mention of the
	// allowance, which is why deriving the tokens from it was not enough.
	billableIOPS := billableAbove(iops, includedIOPS[sku])
	billableMBps := billableAbove(mbps, includedThroughputMBps[sku])

	monthlyWaste := sizeGB*capPrice + billableIOPS*iopsPrice + billableMBps*thrPrice

	// Stash the values ExtraEvidence needs. The price-source entries are
	// rendered here because ExtraEvidence has no Pass to reach the pricer.
	nc.Set("billable_iops", billableIOPS)
	nc.Set("billable_throughput", billableMBps)
	nc.Set("currency", rules.CurrencyOf(p))
	nc.Set("disk_sku", sku)
	nc.Set("cap_price", capPrice)
	nc.Set("price_source_evidence", rules.PriceEvidenceFor("price_source", p.Price,
		diskPricedComponents(sku, region, capRegion, iopsPrice, iopsRegion, thrPrice, thrRegion, sizeGB, iops, mbps)...))
	nc.Set("provisioned_iops", iops)
	nc.Set("provisioned_throughput", mbps)

	return []rules.CostBranch{{
		Waste:      monthlyWaste,
		Confidence: 0.85, // creation_fallback age basis: AWS exposes no detach history
		Label:      "detached",
	}}, nil
}

func (rule) MinWasteUSD() float64 { return MinMonthlyWasteUSD }

// EvidenceKeys returns nil: the leading evidence entries (size_gb,
// volume_type, state) are rendered by ExtraEvidence so their formatting
// (%.0f for the size, the raw SKU token) is explicit, and everything else is
// computed (detached_days, age_basis), formatted ($%.4f) or conditional
// (iops / throughput, price_source). This mirrors detached_disk's evidence
// assembly.
func (rule) EvidenceKeys() []string { return nil }

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	cur, _ := nc.Get("currency")
	curStr, _ := cur.(string)
	sizeGB, _ := n.Num(awsrules.AttrSizeGB)
	volumeType, _ := n.Str(awsrules.AttrVolumeType)
	state, _ := n.Str(awsrules.AttrState)

	sku, _ := nc.Get("disk_sku")
	capPrice, _ := nc.Get("cap_price")
	detachedDays, _ := nc.Get("detached_days")
	ageBasis, _ := nc.Get("age_basis")

	ev := []rules.Evidence{
		{Key: "size_gb", Value: fmt.Sprintf("%.0f", sizeGB)},
		{Key: "volume_type", Value: volumeType},
		{Key: "disk_sku", Value: sku.(string)},
		{Key: "state", Value: state},
		{Key: "attached_instances", Value: "0"},
		{Key: "detached_days", Value: fmt.Sprintf("%.0f", detachedDays.(float64))},
		{Key: "age_basis", Value: ageBasis.(string)},
		rules.EvMoneyIn("unit_price_gib_month", curStr, capPrice.(float64), 4),
	}
	if v, ok := nc.Get("price_source_evidence"); ok {
		ev = append(ev, v.([]rules.Evidence)...)
	}
	if iops, _ := nc.Get("provisioned_iops"); iops.(float64) > 0 {
		ev = append(ev, rules.Evidence{Key: "iops", Value: fmt.Sprintf("%.0f", iops.(float64))})
	}
	if mbps, _ := nc.Get("provisioned_throughput"); mbps.(float64) > 0 {
		ev = append(ev, rules.Evidence{Key: "throughput", Value: fmt.Sprintf("%.0f", mbps.(float64))})
	}
	return ev
}

// diskPricedComponents returns the components of a volume's monthly cost that
// contributed a nonzero dollar amount. Capacity always contributes (a
// captured error was already fatal in Cost); IOPS and throughput contribute
// only if they have both a resolved price and a provisioned quantity.
func diskPricedComponents(sku, region, capRegion string, iopsPrice float64, iopsRegion string, thrPrice float64, thrRegion string, sizeGB, iops, mbps float64) []rules.PricedComponent {
	comps := make([]rules.PricedComponent, 0, 3)

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
		comps = append(comps, rules.PricedComponent{
			Kind:   pricing.KindDiskCapacity,
			SKU:    sku,
			Region: capRegion,
			Key:    "capacity",
		})
	}
	return comps
}


// includedIOPS and includedThroughputMBps are the provisioning each volume type
// includes at no charge. AWS bills only what is provisioned ABOVE these, and a
// volume reports its TOTAL provisioning, so charging the reported figure bills
// the free allowance too.
//
// gp3's allowance is 3000 IOPS and 125 MiB/s, which every gp3 volume has by
// default — so the error was not an edge case but a flat overcharge on the most
// common volume type. io1/io2 have no allowance: every provisioned IOPS is
// billable. Types absent here (gp2, st1, sc1, standard) have no separate IOPS
// or throughput charge at all and never reach this multiplication, because the
// catalogue has no price for them.
var includedIOPS = map[string]float64{
	"gp3": 3000,
	"io1": 0,
	"io2": 0,
}

var includedThroughputMBps = map[string]float64{
	"gp3": 125,
}

// billableAbove returns the provisioning that exceeds an included allowance,
// never a negative number.
func billableAbove(provisioned, included float64) float64 {
	if provisioned <= included {
		return 0
	}
	return provisioned - included
}
