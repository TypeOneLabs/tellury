package aws

import (
	"fmt"
	"sort"

	"github.com/TypeOneLabs/tellury/pkg/metrics"
)

// reduceTimeStat computes spec.TimeStat over a Series that is already ordered
// ascending by timestamp. "p95" is the client-side percentile; the rest are
// plain reductions over the aligned window.
//
// It returns a sorted copy of the series' values for the percentile path and
// delegates the rank lookup to the provider-agnostic metrics.Percentile, so
// the percentile definition never drifts from the core.
func reduceTimeStat(spec Spec, series metrics.Series) (float64, error) {
	pts := series
	switch spec.TimeStat {
	case "p95":
		vals := make([]float64, len(pts))
		for i, s := range pts {
			vals[i] = s.Value
		}
		sort.Float64s(vals)
		return metrics.Percentile(vals, spec.Quantile), nil
	case "mean":
		var sum float64
		for _, s := range pts {
			sum += s.Value
		}
		return sum / float64(len(pts)), nil
	case "first":
		return pts[0].Value, nil
	case "last":
		return pts[len(pts)-1].Value, nil
	default:
		return 0, fmt.Errorf("aws metrics: unsupported time_stat %q", spec.TimeStat)
	}
}
