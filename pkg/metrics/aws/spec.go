// Package metricsaws is the AWS side of metric enrichment. It owns the
// CloudWatch client that turns a metrics.Request into per-node graph metrics,
// the immutable Spec type that declares how a metric key is produced by
// CloudWatch, and the Spec registry that the compute subpackage assembles via
// init().
//
// The core metrics package (pkg/metrics) stays cloud-free; everything in this
// package is AWS-specific by design.
package aws

import (
	"sort"
	"sync"
)

// ResourceEC2Instance is the monitored-resource token for EC2 instances. The
// ingestion join index in pkg/cloud/aws maps InstanceId to graph.Ref using
// this token, and the metric specs register against it, so both sides agree
// on the exact string without importing cloud code.
const ResourceEC2Instance = "AWS::EC2::Instance"

// Spec is the immutable declaration of how a metrics.Key is produced by
// CloudWatch: namespace, metric name, dimension, statistic, and period. It
// is deliberately different from the GCP Spec — CloudWatch has no aligner,
// reducer, or monitored-resource type, and the client-side reduction is
// simpler (no multi-metric-type fallback chain).
type Spec struct {
	Key          string
	Namespace    string // e.g. "AWS/EC2"
	MetricName   string // e.g. "CPUUtilization"
	DimensionKey string // e.g. "InstanceId" — the dimension that joins to a graph node
	JoinAttr     string // e.g. "instance_id" — the graph node attribute containing the dimension value
	Stat         string // e.g. "Average" — the CloudWatch statistic
	PeriodSec    int    // e.g. 3600

	Unit     string // e.g. "ratio"
	TimeStat string // "p95" | "mean" — client-side stat
	Quantile float64

	// WindowDays: 0 => use the caller's Request.WindowDays. >0 => fixed window.
	WindowDays  int
	MinSamples  int
	MinCoverage float64

	ClampLo, ClampHi float64

	// ScaleDown, when non-nil, is applied to each raw CloudWatch sample
	// before the series is stored. For CPUUtilization (percentage → fraction),
	// it divides by 100.
	ScaleDown func(float64) float64
}

var (
	mu    sync.RWMutex
	specs = map[string]Spec{}
)

// Register makes a Spec known to the AWS metric backend. It is called from
// each service subpackage's init() (pkg/metrics/aws/compute). Panics on a
// duplicate key: those are build-time programming errors.
func Register(s Spec) {
	mu.Lock()
	defer mu.Unlock()
	if s.Key == "" {
		panic("aws metrics: Register with empty Key")
	}
	if _, dup := specs[s.Key]; dup {
		panic("aws metrics: duplicate metric key " + s.Key)
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
