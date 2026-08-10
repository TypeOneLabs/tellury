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
		// A rule with no candidate resources was not blocked — there was
		// simply nothing for it to look at, and reporting it as blocked
		// invites the operator to go hunting for a permission or an API they
		// do not need. An organization with no compute instances at all was
		// told "underutilized_instance could not be evaluated for lack of
		// metric data", which is false: no metric would have changed the
		// answer, because there was nothing to measure.
		//
		// TargetKind is empty for cross-node rules implementing Rule directly.
		// Those keep the metric-only test, since the engine cannot know what
		// they iterate.
		if kind := r.Meta().TargetKind; kind != "" && g.CountByKind(kind) == 0 {
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
