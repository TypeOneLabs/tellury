// Package no_lifecycle_policy implements the "no_lifecycle_policy" FinOps
// rule: a STANDARD-class bucket with zero lifecycle rules is paying full
// price to store data that a class-transition policy would move to cheaper
// storage.
package no_lifecycle_policy

import (
	"context"
	"fmt"
	"math"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// ID is the stable rule identifier.
const ID = "no_lifecycle_policy"

// Constants verbatim from the rule spec (architecture §6.9 cost model).
const (
	// MinBytes: below this the finding is noise (10 GiB).
	MinBytes = 10 << 30
	// ColdFraction is the conservative modelled fraction of bucket bytes
	// that are cold (untouched >30d) and thus eligible for a class
	// transition to NEARLINE.
	ColdFraction = 0.60
	// FromClass / ToClass for the class-transition delta.
	FromClass = "STANDARD"
	ToClass   = "NEARLINE"
	// Confidence is fixed: bucket size is measured, cold fraction is modelled.
	Confidence = 0.6
	// MinSamplesReq / MinCoverageReq are the metric gate (Invariant I5):
	// the byte mean is only trusted with at least 7 aligned samples covering
	// 20% of the window.
	MinSamplesReq  = 7
	MinCoverageReq = 0.20
)

func init() { rules.RegisterNode(rule{}) }

type rule struct{}

func (rule) Meta() rules.Meta {
	return rules.Meta{
		ID:       ID,
		Provider: "gcp",
		Service:  "gcs",
		Title:    "Bucket has no lifecycle policy",
		Description: "A STANDARD-class bucket with zero lifecycle rules keeps " +
			"all objects at full price forever. A class-transition rule to " +
			"NEARLINE reclaims the delta on the cold fraction of stored bytes.",
		Severity:           rules.SeverityLow,
		RequiredAssetTypes: []string{"storage.googleapis.com/Bucket"},
		RequiredMetrics:    []string{metrics.KeyBucketTotalBytesMean},
		Remediation:        "gcloud storage buckets update gs://NAME --lifecycle-file=lifecycle.json",
		Origin:             "native",
	}
}

func (rule) Kind() graph.ResourceKind { return graph.KindBucket }

func (rule) Guards() []rules.Guard {
	return []rules.Guard{
		{Name: "bucket_name_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str("bucket_name")
				return ok && v != ""
			}},
		{Name: "lifecycle_count_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				_, ok := n.Num("lifecycle_rule_count")
				return ok
			}},
		{Name: "storage_class_present", SkipCode: rules.SkipMissingAttr,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				v, ok := n.Str("storage_class")
				return ok && v != ""
			}},
		{Name: "no_lifecycle_rules", SkipCode: rules.SkipHasLifecycle,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				cnt, _ := n.Num("lifecycle_rule_count")
				return cnt == 0
			}},
		{Name: "standard_class", SkipCode: rules.SkipNotStandardClass,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				class, _ := n.Str("storage_class")
				return normalizeStorageClass(class) == "STANDARD"
			}},
		{Name: "no_autoclass", SkipCode: rules.SkipAutoclass,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				autoclass, _ := n.Bool("autoclass_enabled")
				return !autoclass
			}},
		{Name: "not_retention_locked", SkipCode: rules.SkipRetentionLocked,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				locked, _ := n.Bool("retention_locked")
				return !locked
			}},
		{Name: "metric_sufficient", SkipCode: rules.SkipNoMetric,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				m, ok := n.MetricOK(metrics.KeyBucketTotalBytesMean, MinSamplesReq, MinCoverageReq)
				if !ok {
					return false
				}
				nc.Set("total_bytes", m.Value)
				return true
			}},
		{Name: "min_bytes_reached", SkipCode: rules.SkipBelowMinBytes,
			Check: func(n *graph.Node, nc *rules.NodeContext, p *rules.Pass) bool {
				totalBytes, _ := nc.Get("total_bytes")
				return totalBytes.(float64) >= MinBytes
			}},
	}
}

func (rule) Cost(ctx context.Context, n *graph.Node, nc *rules.NodeContext, p *rules.Pass) ([]rules.CostBranch, error) {
	totalBytes, _ := nc.Get("total_bytes")
	region := pricing.RegionOf(n.Location)

	fromPrice, fromRegion, err := p.Price.UnitPrice(pricing.KindGCSStorage, "gcp", FromClass, region)
	if err != nil {
		return nil, err // engine records SkipNoPrice; never a $0 assumption (Invariant I4)
	}
	toPrice, toRegion, err := p.Price.UnitPrice(pricing.KindGCSStorage, "gcp", ToClass, region)
	if err != nil {
		return nil, err
	}
	deltaPerGiBMonth := fromPrice - toPrice

	totalGiB := totalBytes.(float64) / (1 << 30)
	monthlyWaste := totalGiB * ColdFraction * deltaPerGiBMonth

	// Stash evidence inputs; the price-source entries are rendered here
	// because ExtraEvidence has no Pass to reach the pricer.
	nc.Set("delta_per_gb_month", deltaPerGiBMonth)
	nc.Set("from_price_source", rules.PriceEvidence("from_price_source", p.Price, pricing.KindGCSStorage, FromClass, fromRegion))
	nc.Set("to_price_source", rules.PriceEvidence("to_price_source", p.Price, pricing.KindGCSStorage, ToClass, toRegion))

	return []rules.CostBranch{{
		Waste:      monthlyWaste,
		Confidence: round2(Confidence),
		Label:      "class_transition",
	}}, nil
}

// MinWasteUSD is the dollar noise floor below which a branch is dropped. This
// rule has no dollar floor — its real noise floor is MinBytes, enforced as a
// guard — so 0.0 keeps every non-negative branch. The floor still does real
// work here: a negative class delta (fromPrice < toPrice) yields a negative
// branch, which the engine drops and reports as SkipBelowMinWaste, exactly
// matching the pre-refactor `if monthlyWaste < 0 { SkipBelowMinWaste }` skip.
func (rule) MinWasteUSD() float64 { return 0.0 }

// EvidenceKeys returns nil: every evidence entry is either formatted with a
// non-%v layout (total_bytes as %.0f, cold_fraction as %.2f, delta as $%.4f)
// or produced from a price lookup, so the whole list is rendered by
// ExtraEvidence to keep the pre-refactor keys, values and order byte-for-byte.
func (rule) EvidenceKeys() []string { return nil }

func (rule) ExtraEvidence(n *graph.Node, nc *rules.NodeContext, branch rules.CostBranch) []rules.Evidence {
	totalBytes, _ := nc.Get("total_bytes")
	storageClass, _ := n.Str("storage_class")
	lifecycleCount, _ := n.Num("lifecycle_rule_count")
	delta, _ := nc.Get("delta_per_gb_month")

	ev := []rules.Evidence{
		{Key: "total_bytes", Value: fmt.Sprintf("%.0f", totalBytes.(float64))},
		{Key: "storage_class", Value: storageClass},
		{Key: "lifecycle_rules", Value: fmt.Sprintf("%.0f", lifecycleCount)},
		{Key: "cold_fraction", Value: fmt.Sprintf("%.2f", ColdFraction)},
		{Key: "delta_per_gb_month", Value: fmt.Sprintf("$%.4f", delta.(float64))},
	}
	if v, ok := nc.Get("from_price_source"); ok {
		ev = append(ev, v.(rules.Evidence))
	}
	if v, ok := nc.Get("to_price_source"); ok {
		ev = append(ev, v.(rules.Evidence))
	}
	return ev
}

// normalizeStorageClass collapses legacy class names onto their modern
// equivalent (§5.3 GCSSKU rule).
func normalizeStorageClass(class string) string {
	switch class {
	case "MULTI_REGIONAL", "REGIONAL", "DURABLE_REDUCED_AVAILABILITY":
		return "STANDARD"
	default:
		return class
	}
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
