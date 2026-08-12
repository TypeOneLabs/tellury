// Package metricsgcpcompute defines the GCP compute metric specs: the Query
// declarations for the compute.googleapis.com instance metric types, aligned
// hourly and reduced with REDUCE_NONE, with the client-side p95 percentile
// computed over the window (see metricsgcpreduce / metrics.Percentile).
package compute

import (
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	metricsgcp "github.com/TypeOneLabs/tellury/pkg/metrics/gcp"
)

// GCP metric types for the compute (gce_instance monitored-resource) specs.
const (
	MetricCPUUtilization       = "compute.googleapis.com/instance/cpu/utilization"
	MetricMemoryPercentUsed    = "agent.googleapis.com/memory/percent_used"
	MetricMemoryBalloonRAMUsed = "compute.googleapis.com/instance/memory/balloon/ram_used"
)

func init() {
	// Compute instance specs. The first metric type is the preferred one; a
	// later type is a fallback that only wins when the earlier type produced
	// no data for a given resource. REDUCE_NONE keeps every aligned value
	// (no cross-series reduction), and the p95 is computed client-side with
	// metrics.Percentile over the hourly-aligned window.
	metricsgcp.Register(metricsgcp.Spec{
		Key:                   metrics.KeyCPUUtilizationP95,
		MetricTypes:           []string{MetricCPUUtilization},
		MonitoredResourceType: metricsgcp.ResourceGCEInstance,
		JoinLabel:             "instance_id",
		JoinAttr:              "instance_id",
		Unit:                  "ratio",
		AlignmentSec:          3600,
		Aligner:               "ALIGN_MEAN",
		Reducer:               "REDUCE_NONE",
		TimeStat:              "p95",
		Quantile:              0.95,
		WindowDays:            0,
		MinSamples:            168,
		MinCoverage:           0.50,
		ClampLo:               0,
		ClampHi:               1,
	})

	// mem_utilization_p95 is deliberately NOT registered here.
	//
	// The previous spec declared both the agent-based percent_used metric
	// (which returns 0–100) and the hypervisor balloon/ram_used metric
	// (which returns bytes) as "Unit: ratio, ClampHi: 1". Every real value
	// from either metric clamped to exactly 1.0 — every instance reported
	// 100% memory used. The percent_used metric also carries a `state` label
	// (used/free/cached/buffered) for which the spec had no label-filter
	// field, causing multiple series per instance to merge into one.
	//
	// No rule currently declares KeyMemUtilizationP95 in RequiredMetrics.
	// When a rule does, the spec must be re-added with correct units:
	// percent_used is "Unit: percent, ClampHi: 100", with the state label
	// filtered, or a CloudWatch-agent-based memory spec is added on the AWS
	// side. Until then, an absent spec makes the GCP metrics provider's
	// Supports() return false for this key, and the engine blocks any rule
	// that declares it — exactly the right behavior for a metric that cannot
	// be delivered correctly.
	//
	// The MetricMemoryPercentUsed and MetricMemoryBalloonRAMUsed constants
	// remain as documentation of the known metric types.
}
