// Package metricsgcpstorage defines the GCP storage (GCS bucket) metric
// specs: the storage.googleapis.com metric types, aligned daily with
// REDUCE_SUM grouped by the bucket join label, over a fixed 30-day window.
package storage

import (
	"math"

	metricsgcp "github.com/TypeOneLabs/tellury/pkg/metrics/gcp"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
)

// GCP metric types for the storage (gcs_bucket monitored-resource) specs.
const (
	MetricTotalBytes  = "storage.googleapis.com/storage/total_bytes"
	MetricObjectCount = "storage.googleapis.com/storage/object_count"

	// storageWindowDays is the fixed lookback for GCS metrics. Unlike the
	// compute metrics (which follow --window), GCS total_bytes is a cumulative
	// daily-sampled series that needs a 30-day silhouette to produce a stable
	// first/last/mean picture, regardless of the CLI's --window.
	storageWindowDays = 30
)

func init() {
	// All GCS specs query total_bytes on the gcs_bucket monitored resource,
	// summed across the storage_class dimension (REDUCE_SUM grouped by the
	// bucket join label). Only the client-side TimeStat differs.
	registerBucket := func(key string, stat string) {
		metricsgcp.Register(metricsgcp.Spec{
			Key:                   key,
			MetricTypes:           []string{MetricTotalBytes},
			MonitoredResourceType: metricsgcp.ResourceGCSBucket,
			JoinLabel:             "bucket_name",
			JoinAttr:              "bucket_name",
			Unit:                  "bytes",
			AlignmentSec:          86400,
			Aligner:               "ALIGN_MEAN",
			Reducer:               "REDUCE_SUM",
			TimeStat:              stat,
			WindowDays:            storageWindowDays,
			MinSamples:            7,
			MinCoverage:           0.20,
			ClampLo:               0,
			ClampHi:               math.MaxFloat64,
		})
	}

	registerBucket(metrics.KeyBucketTotalBytesMean, "mean")
	registerBucket(metrics.KeyBucketTotalBytesFirst, "first")
	registerBucket(metrics.KeyBucketTotalBytesLast, "last")

	metricsgcp.Register(metricsgcp.Spec{
		Key:                   metrics.KeyBucketObjectCountLast,
		MetricTypes:           []string{MetricObjectCount},
		MonitoredResourceType: metricsgcp.ResourceGCSBucket,
		JoinLabel:             "bucket_name",
		JoinAttr:              "bucket_name",
		Unit:                  "count",
		AlignmentSec:          86400,
		Aligner:               "ALIGN_MEAN",
		Reducer:               "REDUCE_SUM",
		TimeStat:              "last",
		WindowDays:            storageWindowDays,
		MinSamples:            1,
		MinCoverage:           0,
		ClampLo:               0,
		ClampHi:               math.MaxFloat64,
	})
}
