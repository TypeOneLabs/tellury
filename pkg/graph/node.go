package graph

import (
	"encoding/json"
	"time"
)

// ResourceKind is the normalized, provider-agnostic resource class.
// Kept as a short lowercase token because it is the first column of CLI
// output: disk/pd-standard-01  ->  Kind="disk", Name="pd-standard-01"
type ResourceKind string

const (
	KindInstance    ResourceKind = "instance"
	KindDisk        ResourceKind = "disk"
	KindSnapshot    ResourceKind = "snapshot"
	KindImage       ResourceKind = "image"
	KindBucket      ResourceKind = "bucket"
	KindAddress     ResourceKind = "address"
	KindForwardRule ResourceKind = "forwarding_rule"
	KindNetwork     ResourceKind = "network"
	KindSubnetwork  ResourceKind = "subnetwork"
	// Container kinds. These are resource-hierarchy scaffolding: they let a
	// finding be attributed to a folder or organization and let a rollup walk
	// beyond a single project. They are populated during ingestion from the
	// SearchAllResources hierarchy fields and are NEVER rule evaluation
	// targets and never appear as findings (see Node.Container and
	// Graph.ResourceNodeCount).
	KindProject      ResourceKind = "project"
	KindFolder       ResourceKind = "folder"
	KindOrganization ResourceKind = "organization"
	KindUnknown      ResourceKind = "unknown"
)

// Ref is a stable, globally unique node identity. For GCP we use the Cloud
// Asset Inventory asset name verbatim.
type Ref string

// MetricValue is a pre-aggregated statistic over the scan window.
//
// All fields are written by the enrichment pass; rules never mutate them.
type MetricValue struct {
	Value      float64 `json:"value"`
	Unit       string  `json:"unit"` // "ratio" | "bytes" | "count" | "iops" | "mibps"
	Stat       string  `json:"stat"` // "p95" | "mean" | "max" | "min" | "first" | "last"
	WindowDays int     `json:"window_days"`
	Samples    int     `json:"samples"` // 0 => rules MUST skip

	// ExpectedSamples is the number of aligned points the window should have
	// produced, capped by resource age. Coverage = Samples/ExpectedSamples.
	ExpectedSamples int     `json:"expected_samples,omitempty"`
	Coverage        float64 `json:"coverage,omitempty"`

	// Source is the fully-qualified Cloud Monitoring metric type that
	// produced this value. Empty for values loaded from a v1 snapshot.
	Source string `json:"source,omitempty"`

	Aligner string `json:"aligner,omitempty"`
	Reducer string `json:"reducer,omitempty"`

	WindowStart time.Time `json:"window_start,omitempty"`
	WindowEnd   time.Time `json:"window_end,omitempty"`
}

// Node is a single cloud resource vertex.
type Node struct {
	ID        Ref               `json:"id"`
	Kind      ResourceKind      `json:"kind"`
	Name      string            `json:"name"`
	Provider  string            `json:"provider"`
	Service   string            `json:"service"`
	AssetType string            `json:"asset_type"`
	Project   string            `json:"project"`
	Location  string            `json:"location"`
	Labels    map[string]string `json:"labels,omitempty"`

	// Attrs holds normalized, rule-facing scalars extracted at ingestion.
	Attrs map[string]any `json:"attrs,omitempty"`

	// Metrics holds enrichment output keyed by metrics.Key.
	Metrics map[string]MetricValue `json:"metrics,omitempty"`

	// Raw is the untouched provider payload.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// Container reports whether the node is resource-hierarchy scaffolding rather
// than a billable leaf resource. Organization, folder and project nodes are
// containers. They are added during ingestion so a finding can be attributed
// to a folder or rolled up across projects, but they must never be evaluated
// by a rule and never appear as a finding.
//
// This exclusion is structural, not per-rule: every rule enters the graph
// through Graph.ByKind with a leaf ResourceKind, and a container node's Kind
// is always one of the container kinds, so a rule can never be handed a
// container node regardless of which leaf kinds it iterates. The scan
// report's "N resources" count uses Graph.ResourceNodeCount, which excludes
// containers, so containers never inflate that figure either.
func (n *Node) Container() bool {
	switch n.Kind {
	case KindOrganization, KindFolder, KindProject:
		return true
	}
	return false
}

// Display returns the CLI RESOURCE column value: "disk/pd-standard-01".
func (n *Node) Display() string { return string(n.Kind) + "/" + n.Name }

// SetAttr writes a normalized, rule-facing scalar. It is the only way
// normalize.go (or any other producer) should populate Attrs: it lazily
// allocates the map so callers never need a nil check before their first
// write.
func (n *Node) SetAttr(key string, value any) {
	if n.Attrs == nil {
		n.Attrs = make(map[string]any, 8)
	}
	n.Attrs[key] = value
}

// Str / Num / Bool are the only accessors rules should use on Attrs.
func (n *Node) Str(k string) (string, bool) {
	v, ok := n.Attrs[k].(string)
	return v, ok
}

func (n *Node) Num(k string) (float64, bool) {
	v, ok := n.Attrs[k].(float64)
	return v, ok
}

func (n *Node) Bool(k string) (bool, bool) {
	v, ok := n.Attrs[k].(bool)
	return v, ok
}

// Time parses an Attrs value as an RFC3339 timestamp. Used by predicates such
// as "older_than_days".
func (n *Node) Time(k string) (time.Time, bool) {
	s, ok := n.Str(k)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Label reads a resource label. ok=false when the label (or the map) is absent.
func (n *Node) Label(k string) (string, bool) {
	if n.Labels == nil {
		return "", false
	}
	v, ok := n.Labels[k]
	return v, ok
}

// Exempt implements Invariant I7: a node labeled tellury-exempt=true is
// skipped by every rule before any other predicate is evaluated.
func (n *Node) Exempt() bool {
	return n.Labels["tellury-exempt"] == "true"
}

// Metric returns a metric value; ok=false when absent or sample-less.
func (n *Node) Metric(key string) (MetricValue, bool) {
	m, ok := n.Metrics[key]
	if !ok || m.Samples == 0 {
		return MetricValue{}, false
	}
	return m, true
}

// MetricOK is the single gate every rule uses to read a metric: it enforces
// both a minimum sample count and a minimum coverage ratio.
func (n *Node) MetricOK(key string, minSamples int, minCoverage float64) (MetricValue, bool) {
	m, ok := n.Metrics[key]
	if !ok || m.Samples < minSamples {
		return MetricValue{}, false
	}
	if minCoverage > 0 && m.Coverage < minCoverage {
		return MetricValue{}, false
	}
	return m, true
}
