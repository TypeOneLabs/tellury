// Package metricsgcp is the GCP side of metric enrichment. It owns the
// Cloud Monitoring client that turns a metrics.Request into per-node graph
// metrics, the immutable Spec type that declares how a metric key is queried,
// the monitored-resource tokens, and the Spec registry that the compute and
// storage subpackages assemble via init().
//
// The core metrics package (pkg/metrics) stays cloud-free; everything in this
// package is GCP-specific by design.
package gcp

import (
	"sort"
	"sync"
)

// Monitored-resource type tokens, shared by the metric specs (registered by
// the compute/storage subpackages) and the ingestion join index in
// pkg/cloud/gcp, so both sides agree on the exact string without importing
// cloud code.
const (
	ResourceGCEInstance = "gce_instance"
	ResourceGCSBucket   = "gcs_bucket"
)

// Spec is the immutable declaration of how a Key is produced by the GCP
// Cloud Monitoring backend: which metric types to query, on which monitored
// resource, with which alignment / alignment period / reduction, and the
// client-side stat (and its quantile) computed over the aligned window.
type Spec struct {
	Key                   string
	MetricTypes           []string
	MonitoredResourceType string
	JoinLabel             string
	JoinAttr              string

	Unit         string
	AlignmentSec int
	Aligner      string
	Reducer      string
	TimeStat     string
	Quantile     float64

	// WindowDays: 0 => use the caller's Request.WindowDays. >0 => fixed window.
	WindowDays  int
	MinSamples  int
	MinCoverage float64

	ClampLo, ClampHi float64
}

var (
	mu    sync.RWMutex
	specs = map[string]Spec{}
)

// Register makes a Spec known to the GCP metric backend. It is called from
// each service subpackage's init() (pkg/metrics/gcp/compute and
// pkg/metrics/gcp/storage). Panics on a duplicate key: those are build-time
// programming errors.
func Register(s Spec) {
	mu.Lock()
	defer mu.Unlock()
	if s.Key == "" {
		panic("gcp metrics: Register with empty Key")
	}
	if _, dup := specs[s.Key]; dup {
		panic("gcp metrics: duplicate metric key " + s.Key)
	}
	specs[s.Key] = s
}

// SpecOf looks up a metric's declaration.
func SpecOf(key string) (Spec, bool) {
	mu.RLock()
	defer mu.RUnlock()
	s, ok := specs[key]
	return s, ok
}

// Specs returns every registered spec sorted by key.
func Specs() []Spec {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Spec, 0, len(specs))
	for _, s := range specs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
