// Package metricsgcpcompute defines the GCP compute metric specs: the Query
// declarations for the compute.googleapis.com instance metric types, aligned
// hourly and reduced with REDUCE_NONE, with the client-side p95 percentile
// computed over the window (see metricsgcpreduce / metrics.Percentile).
package compute

import (
	metricsgcp "github.com/TypeOneLabs/tellury/pkg/metrics/gcp"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
)

// GCP metric types for the compute (gce_instance monitored-resource) specs.
const (
	MetricCPUUtilization        = "compute.googleapis.com/instance/cpu/utilization"
	MetricMemoryPercentUsed     = "agent.googleapis.com/memory/percent_used"
	MetricMemoryBalloonRAMUsed  = "compute.googleapis.com/instance/memory/balloon/ram_used"
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
	metricsgcp.Register(metricsgcp.Spec{
		Key:                   metrics.KeyMemUtilizationP95,
		MetricTypes:           []string{MetricMemoryPercentUsed, MetricMemoryBalloonRAMUsed},
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
}
