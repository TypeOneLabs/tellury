// Package metricsawscompute defines the AWS compute metric specs: the
// declarations for the AWS/EC2 CloudWatch namespace, with 1-hour period and
// Average statistic, with the client-side p95 percentile computed over the
// window (see metricsawsreduce / metrics.Percentile).
package compute

import (
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	metricsaws "github.com/TypeOneLabs/tellury/pkg/metrics/aws"
)

// CloudWatch metric names for the AWS/EC2 namespace.
const (
	MetricCPUUtilization = "CPUUtilization"
)

func init() {
	metricsaws.Register(metricsaws.Spec{
		Key:          metrics.KeyCPUUtilizationP95,
		Namespace:    "AWS/EC2",
		MetricName:   MetricCPUUtilization,
		DimensionKey: "InstanceId",
		JoinAttr:     "instance_id",
		Stat:         "Average",
		PeriodSec:    3600,
		Unit:         "ratio",
		TimeStat:     "p95",
		Quantile:     0.95,
		WindowDays:   0,
		MinSamples:   168,
		MinCoverage:  0.50,
		ClampLo:      0,
		ClampHi:      1,
		// CloudWatch CPUUtilization is a percentage (0-100). The graph stores
		// fractions (0-1), matching the GCP convention. Divide by 100 on every
		// sample so the rule's thresholds (MinOverprovisionRatio=0.40,
		// TargetCPUUtil=0.60) work unchanged.
		ScaleDown: func(v float64) float64 { return v / 100 },
	})
}
