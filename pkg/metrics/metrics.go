// Package metrics is the provider-agnostic core of metric enrichment. It owns
// the shared value types (Sample, Series), the enrichment contracts (Request,
// Setter, Provider), the client-side percentile, and the cross-cloud set of
// metric key tokens that rules declare against.
//
// It is deliberately GCP-free. Anything that names a concrete GCP concept — a
// Cloud Monitoring metric type, an alignment period, a monitored-resource
// label, a project scope — lives under pkg/metrics/gcp (compute and storage
// define their own specs there), never here.
package metrics

import (
	"context"
	"math"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// Key constants. A rule may only declare keys present in Known.
//
// These are cross-cloud identifier tokens (e.g. "cpu_utilization_p95"), NOT
// cloud metric types; a cloud's metric subpackage maps each token to its own
// concrete query. Rules reference the tokens; the GCP subpackages define the
// specs.
const (
	KeyCPUUtilizationP95     = "cpu_utilization_p95"
	KeyCPUUtilizationMean    = "cpu_utilization_mean"
	KeyMemUtilizationP95     = "mem_utilization_p95"
	KeyBucketTotalBytesMean  = "bucket_total_bytes_mean"
	KeyBucketTotalBytesFirst = "bucket_total_bytes_first"
	KeyBucketTotalBytesLast  = "bucket_total_bytes_last"
	KeyBucketObjectCountLast = "bucket_object_count_last"
)

// knownKeys is the provider-agnostic set of metric-key tokens a rule may
// declare. It is the single source of truth for the cross-cloud metric
// vocabulary: a rule's RequiredMetrics may only name keys present here,
// independent of which cloud's specs are loaded.
var knownKeys = map[string]bool{
	KeyCPUUtilizationP95:     true,
	KeyCPUUtilizationMean:    true,
	KeyMemUtilizationP95:     true,
	KeyBucketTotalBytesMean:  true,
	KeyBucketTotalBytesFirst: true,
	KeyBucketTotalBytesLast:  true,
	KeyBucketObjectCountLast: true,
}

// Known reports whether key is a registered metric token.
func Known(key string) bool { return knownKeys[key] }

// Sample is one measurement in a Series: an aligned value together with the
// wall-clock timestamp the aligned bucket covers. A provider produces Series
// after applying its alignment; stats such as "first"/"last" are resolved
// across the window from the timestamps.
type Sample struct {
	Timestamp time.Time
	Value     float64
}

// Series is a time-ordered (ascending) collection of Samples for one metric
// and resource, after alignment. A provider hands a Series to the shared
// reduction logic so client-side stats (p95, mean, first, last) are computed
// deterministically.
type Series []Sample

// Request declares what enrichment the selected rules need.
type Request struct {
	Keys       []string
	WindowDays int

	// Projects lists the distinct project IDs enrichment should query. Some
	// providers are project-scoped with no native folder/organization
	// aggregation, so a caller that ingested a folder/organization scope must
	// resolve this to every distinct project actually present in the graph
	// before calling Fill. A project-scoped caller sets it to a single-element
	// slice. Providers that need no such scoping (e.g. a provider replaying a
	// fixed table) ignore it.
	Projects []string

	// Progress, when non-nil, is invoked by Provider.Fill as work completes:
	// (completed, total) counts of the discrete jobs the provider fans out
	// (for the GCP client, (key, project) fetch pairs). It is called from the
	// provider's worker goroutines and must therefore be safe for concurrent
	// use; it is the seam scan progress reporting threads through without
	// ever touching the provider's own concurrency bound. Optional.
	Progress func(done, total int)
}

// Setter writes one enrichment value onto the graph. Implementations of
// Provider.Fill call it once per (ref, key) pair they can produce.
type Setter func(id graph.Ref, key string, v graph.MetricValue)

// Provider is implemented per cloud (or per a cloud's metric subpackage). A
// provider that drives a cloud metrics API implements Supports against its
// own registry of known keys and Fill against a Request.
type Provider interface {
	Supports(key string) bool
	Fill(ctx context.Context, req Request, set Setter) error
}

// Percentile implements the nearest-rank definition (Invariant I8):
// for ascending sorted v[0..n-1]: P(q) = v[clamp(ceil(q*n)-1, 0, n-1)].
func Percentile(sortedAsc []float64, q float64) float64 {
	n := len(sortedAsc)
	if n == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx > n-1 {
		idx = n - 1
	}
	return sortedAsc[idx]
}
