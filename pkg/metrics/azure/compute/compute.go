// Package metricsazurecompute defines the Azure compute metric specs: the
// declarations for the Microsoft.Compute/virtualMachines metric namespace,
// with a one-hour time grain and Average aggregation, and the client-side p95
// percentile computed over the window (see metrics.Percentile).
package compute

import (
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	metricsazure "github.com/TypeOneLabs/tellury/pkg/metrics/azure"
)

// Azure Monitor platform metric names for Microsoft.Compute/virtualMachines.
const (
	MetricPercentageCPU          = "Percentage CPU"
	MetricAvailableMemoryPercent = "Available Memory Percentage"
)

func init() {
	metricsazure.Register(metricsazure.Spec{
		Key:         metrics.KeyCPUUtilizationP95,
		MetricName:  MetricPercentageCPU,
		Namespace:   "Microsoft.Compute/virtualMachines",
		Aggregation: "Average",
		Interval:    "PT1H",
		Unit:        "ratio",
		TimeStat:    "p95",
		Quantile:    0.95,
		WindowDays:  0,
		MinSamples:  168,
		MinCoverage: 0.50,
		ClampLo:     0,
		ClampHi:     1,
		// Azure Monitor's Percentage CPU has unit Percent (0-100). The graph
		// stores fractions (0-1), matching the AWS and GCP convention. Divide
		// by 100 on every sample so the rule's thresholds
		// (MinOverprovisionRatio=0.40, TargetCPUUtil=0.60) work unchanged.
		ScaleDown: func(v float64) float64 { return v / 100 },
	})

	metricsazure.Register(metricsazure.Spec{
		Key:         metrics.KeyMemUtilizationP95,
		MetricName:  MetricAvailableMemoryPercent,
		Namespace:   "Microsoft.Compute/virtualMachines",
		Aggregation: "Average",
		Interval:    "PT1H",
		Unit:        "ratio",
		TimeStat:    "p95",
		Quantile:    0.95,
		WindowDays:  0,
		MinSamples:  168,
		MinCoverage: 0.50,
		ClampLo:     0,
		ClampHi:     1,
		// Azure reports AVAILABLE memory; mem_utilization_p95 means USED.
		// Invert before the client-side p95 so an idle VM reporting 90%
		// available becomes 0.10 used, not 0.90.
		ScaleDown: func(v float64) float64 { return (100 - v) / 100 },
	})
}
