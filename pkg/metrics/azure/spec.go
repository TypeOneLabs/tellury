// Package azure is the Azure Monitor side of metric enrichment. It owns the
// Azure Monitor client that turns a metrics.Request into per-node graph
// metrics, the immutable Spec type that declares how a metric key is produced
// by Azure Monitor, and the Spec registry that the compute subpackage
// assembles via init().
//
// The core metrics package (pkg/metrics) stays cloud-free; everything in this
// package is Azure-specific by design.
package azure

import (
	"sort"
	"sync"
)

// Spec is the immutable declaration of how a metrics.Key is produced by
// Azure Monitor: namespace, metric name, aggregation and time grain. It is
// deliberately different from the AWS Spec — Azure Monitor has no dimension
// join to a graph attribute because each List call is already scoped to a
// single ARM resource URI, and its period is an ISO 8601 interval string
// (e.g. "PT1H") rather than a period in seconds.
type Spec struct {
	Key         string
	MetricName  string // e.g. "Percentage CPU"
	Namespace   string // e.g. "Microsoft.Compute/virtualMachines"
	Aggregation string // e.g. "Average" — Azure Monitor aggregation
	Interval    string // e.g. "PT1H" — Azure Monitor time grain

	Unit     string // e.g. "ratio"
	TimeStat string // "p95" | "mean" — client-side stat
	Quantile float64

	// WindowDays: 0 => use the caller's Request.WindowDays. >0 => fixed window.
	WindowDays  int
	MinSamples  int
	MinCoverage float64

	ClampLo, ClampHi float64

	// ScaleDown, when non-nil, is applied to each raw Azure Monitor sample
	// before the series is stored. Percentage CPU (0-100) is divided by 100
	// so the graph stores a fraction; Available Memory Percentage is inverted
	// into used memory before being stored as a fraction.
	ScaleDown func(float64) float64
}

var (
	mu    sync.RWMutex
	specs = map[string]Spec{}
)

// Register makes a Spec known to the Azure metric backend. It is called from
// each service subpackage's init() (pkg/metrics/azure/compute). Panics on a
// duplicate key: those are build-time programming errors.
func Register(s Spec) {
	mu.Lock()
	defer mu.Unlock()
	if s.Key == "" {
		panic("azure metrics: Register with empty Key")
	}
	if _, dup := specs[s.Key]; dup {
		panic("azure metrics: duplicate metric key " + s.Key)
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
