package rules

import (
	"sort"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// MetricsBlocked reports which of the given rules could not evaluate for lack
// of metric data. A rule is "blocked" when it REQUIRES at least one metric
// (Meta.RequiredMetrics is non-empty) AND the graph carries no sample-positive
// series for any of those keys.
//
// It is the offline summary's plumbing: a raw Cloud Asset Inventory fixture
// carries no metric series at all, so every metric-dependent rule is blocked
// — but that must be STATED, not left to --explain-skips to reveal as a wall
// of "no_metric" skips. A cached-snapshot scan, by contrast, replays the
// serialized Metrics map with full fidelity, so metric-dependent rules are
// typically NOT blocked.
//
// The result is sorted by rule ID for deterministic rendering.
func MetricsBlocked(selected []Rule, g *graph.Graph) []string {
	missing := map[string]bool{}
	for _, r := range selected {
		reqs := r.Meta().RequiredMetrics
		if len(reqs) == 0 {
			continue
		}
		present := false
		for _, k := range reqs {
			if g.HasMetric(k) {
				present = true
				break
			}
		}
		if !present {
			missing[r.Meta().ID] = true
		}
	}
	out := make([]string, 0, len(missing))
	for id := range missing {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
