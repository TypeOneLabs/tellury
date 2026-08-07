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
)

func init() { rules.Register(rule{}) }

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

func (rule) Eval(ctx context.Context, p *rules.Pass) ([]rules.Finding, error) {
	var out []rules.Finding

	p.Graph.ByKind(graph.KindBucket, func(n *graph.Node) bool {
		if ctx.Err() != nil {
			return false
		}

		// P0: exemption label.
		if n.Labels["tellury-exempt"] == "true" {
			p.SkipNode(ID, n.ID, rules.SkipExemptLabel)
			return true
		}

		// Shape: bucket_name, lifecycle_rule_count, storage_class required.
		bucketName, ok := n.Str("bucket_name")
		if !ok || bucketName == "" {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}
		lifecycleCount, ok := n.Num("lifecycle_rule_count")
		if !ok {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}
		storageClass, ok := n.Str("storage_class")
		if !ok || storageClass == "" {
			p.SkipNode(ID, n.ID, rules.SkipMissingAttr)
			return true
		}

		// Detection: lifecycle_rule_count == 0 AND storage_class == STANDARD.
		if lifecycleCount != 0 {
			p.SkipNode(ID, n.ID, rules.SkipHasLifecycle)
			return true
		}
		normalizedClass := normalizeStorageClass(storageClass)
		if normalizedClass != "STANDARD" {
			p.SkipNode(ID, n.ID, rules.SkipNotStandardClass)
			return true
		}

		// Autoclass supersedes manual lifecycle management.
		autoclass, _ := n.Bool("autoclass_enabled")
		if autoclass {
			p.SkipNode(ID, n.ID, rules.SkipAutoclass)
			return true
		}

		// Retention lock prevents any class transition.
		retentionLocked, _ := n.Bool("retention_locked")
		if retentionLocked {
			p.SkipNode(ID, n.ID, rules.SkipRetentionLocked)
			return true
		}

		bytesMetric, ok := n.MetricOK(metrics.KeyBucketTotalBytesMean, 7, 0.20)
		if !ok {
			p.SkipNode(ID, n.ID, rules.SkipNoMetric)
			return true
		}
		totalBytes := bytesMetric.Value

		if totalBytes < MinBytes {
			p.SkipNode(ID, n.ID, rules.SkipBelowMinBytes)
			return true
		}

		region := pricing.RegionOf(n.Location)
		fromPrice, fromRegion, err := p.Price.UnitPrice(pricing.KindGCSStorage, "gcp", FromClass, region)
		if err != nil {
			p.SkipNode(ID, n.ID, rules.SkipNoPrice)
			return true
		}
		toPrice, toRegion, err := p.Price.UnitPrice(pricing.KindGCSStorage, "gcp", ToClass, region)
		if err != nil {
			p.SkipNode(ID, n.ID, rules.SkipNoPrice)
			return true
		}
		deltaPerGiBMonth := fromPrice - toPrice

		totalGiB := totalBytes / (1 << 30)
		monthlyWaste := totalGiB * ColdFraction * deltaPerGiBMonth

		if monthlyWaste < 0 {
			p.SkipNode(ID, n.ID, rules.SkipBelowMinWaste)
			return true
		}

		ev := []rules.Evidence{
			{Key: "total_bytes", Value: fmt.Sprintf("%.0f", totalBytes)},
			{Key: "storage_class", Value: storageClass},
			{Key: "lifecycle_rules", Value: fmt.Sprintf("%.0f", lifecycleCount)},
			{Key: "cold_fraction", Value: fmt.Sprintf("%.2f", ColdFraction)},
			{Key: "delta_per_gb_month", Value: fmt.Sprintf("$%.4f", deltaPerGiBMonth)},
			rules.PriceEvidence("from_price_source", p.Price, pricing.KindGCSStorage, FromClass, fromRegion),
			rules.PriceEvidence("to_price_source", p.Price, pricing.KindGCSStorage, ToClass, toRegion),
		}

		out = append(out, rules.Finding{
			RuleID:          ID,
			ResourceID:      n.ID,
			Resource:        n.Display(),
			Kind:            n.Kind,
			Project:         n.Project,
			Location:        n.Location,
			MonthlyWasteUSD: monthlyWaste,
			Confidence:      round2(Confidence),
			Evidence:        ev,
		})
		return true
	})
	return out, nil
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
