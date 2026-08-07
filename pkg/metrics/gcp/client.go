package gcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
)

// RefLookup resolves a monitored-resource label value (instance_id,
// bucket_name) back to the graph ref that owns it. Built during ingestion in
// pkg/cloud/gcp and handed to the enrichment client here.
type RefLookup func(resourceType, joinValue string) (graph.Ref, bool)

// maxConcurrentFetches bounds the number of simultaneous ListTimeSeries RPCs
// Fill issues. Concurrency here is across (metric key, project) pairs, not
// per-resource: aggregation is done server-side per call, so one call already
// covers every in-scope resource of that type in that project. A small,
// fixed limit is enough to get the parallel win the architecture calls for
// without tripping Cloud Monitoring's per-project rate limits.
const maxConcurrentFetches = 8

// Client is the Cloud Monitoring implementation of metrics.Provider, built on
// the official SDK (cloud.google.com/go/monitoring/apiv3/v2).
//
// The aggregation contract it honours is fixed and non-negotiable (spec §4.2,
// which corrects the architecture doc):
//
//	ONE ListTimeSeries call per metric type, with
//	  filter             = metric.type="<mt>" AND resource.type="<mrt>"
//	  perSeriesAligner   = ALIGN_MEAN
//	  alignmentPeriod    = Spec.AlignmentSec
//	  crossSeriesReducer = REDUCE_NONE (REDUCE_SUM + groupByFields for GCS)
//	  view               = FULL
//
// The percentile is then computed CLIENT-SIDE, per series, using the nearest-
// rank definition in metrics.Percentile. REDUCE_PERCENTILE_95 reduces across
// series within one alignment period, not across time, so it cannot produce a
// p95 over the window — using it would silently report the wrong number.
//
// Cloud Monitoring is always project-scoped: a Request must carry the
// distinct project IDs to query in Projects (a folder/organization ingest
// resolves this from the graph before calling Fill).
type Client struct {
	log    *slog.Logger
	lookup RefLookup
	client *monitoring.MetricClient
}

var _ metrics.Provider = (*Client)(nil)

// NewClient builds the enrichment client, authenticating via Application
// Default Credentials — the same posture as the ingestion clients in
// pkg/cloud/gcp. There is no key-file flag and none is accepted here either.
func NewClient(ctx context.Context, log *slog.Logger, lookup RefLookup) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}
	c, err := monitoring.NewMetricClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp metrics: create Cloud Monitoring client (check Application Default Credentials): %w", err)
	}
	return &Client{log: log, lookup: lookup, client: c}, nil
}

// Close releases the underlying client's connection pool.
func (c *Client) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// Supports implements metrics.Provider: every registered GCP metric spec key
// is considered supported.
func (c *Client) Supports(key string) bool {
	_, ok := SpecOf(key)
	return ok
}

// Fill implements metrics.Provider. It fans the requested (key, project)
// pairs out across bounded goroutines — safe because graph.Graph.SetMetric
// (called indirectly through set) is guarded by its own mutex and never
// touches the topology Freeze() seals. A resource with no returned points is
// never written: invariant I5 requires rules to skip missing data rather
// than have it silently become a zero.
//
// Per-job failures (e.g. exactly one project missing roles/monitoring.viewer
// in a 50-project org scan) are deliberately ISOLATED: a failing job costs
// only its own (key, project) pair, and the healthy siblings keep running and
// writing their metrics. The caller can then still use whatever the healthy
// projects produced. This is intentional: an errgroup.WithContext would
// cancel every sibling the moment one job errors, dropping metric enrichment
// for all projects just because one is unreadable. Instead a bounded
// WaitGroup keeps the same concurrency ceiling, and errors are collected and
// returned (joined when more than one) so the caller can log once.
func (c *Client) Fill(ctx context.Context, req metrics.Request, set metrics.Setter) error {
	if len(req.Keys) == 0 {
		return nil
	}
	if len(req.Projects) == 0 {
		return fmt.Errorf("gcp metrics: metrics.Request.Projects is required (Cloud Monitoring is project-scoped, unlike Cloud Asset Inventory)")
	}
	for _, key := range req.Keys {
		if !metrics.Known(key) {
			return fmt.Errorf("gcp metrics: unknown metric key %q", key)
		}
	}

	var jobs []fetchJob
	for _, key := range req.Keys {
		for _, project := range req.Projects {
			jobs = append(jobs, fetchJob{key: key, project: project})
		}
	}

	return runFetchJobs(ctx, jobs, func(ctx context.Context, j fetchJob) error {
		return c.fillOne(ctx, j.project, j.key, req, set)
	})
}

// fetchJob is one (metric key, project) pair Fill hands to its worker pool.
type fetchJob struct {
	key     string
	project string
}

// runFetchJobs runs worker for every job with the same bounded-pool
// semantics as Fill, isolating the outcome of each job from its siblings. It
// is extracted as its own (package-internal) function purely so the isolation
// guarantee can be unit-tested with a fake worker and no cloud client. The
// []error is the per-job failures; it is non-empty iff at least one job
// failed, and nil otherwise.
func runFetchJobs(ctx context.Context, jobs []fetchJob, worker func(ctx context.Context, j fetchJob) error) error {
	sem := make(chan struct{}, maxConcurrentFetches)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, j := range jobs {
		j := j
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := worker(ctx, j); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		// Join the independent failures into one diagnostic while keeping the
		// healthy jobs' data on the graph. errors.Join keeps EVERY failure
		// individually reachable through errors.Is/errors.As — the previous
		// %w;%v chaining flattened all but the first to text.
		all := make([]error, 0, len(errs)+1)
		all = append(all, fmt.Errorf("gcp metrics: metric enrichment failed for %d of %d (key, project) pairs; the rest succeeded", len(errs), len(jobs)))
		all = append(all, errs...)
		return errors.Join(all...)
	}
}

// fillOne fetches every metric type declared for key in one project, applies
// the aggregation above, and writes one MetricValue per resource that ended
// up with at least one point.
func (c *Client) fillOne(ctx context.Context, project, key string, req metrics.Request, set metrics.Setter) error {
	spec, ok := SpecOf(key)
	if !ok {
		return fmt.Errorf("gcp metrics: no spec for metric key %q", key)
	}

	aligner, err := alignerFromName(spec.Aligner)
	if err != nil {
		return fmt.Errorf("gcp metrics: metric %q: %w", key, err)
	}
	reducer, err := reducerFromName(spec.Reducer)
	if err != nil {
		return fmt.Errorf("gcp metrics: metric %q: %w", key, err)
	}

	windowDays := Window(req, spec)
	end := time.Now().UTC()
	start := end.Add(-time.Duration(windowDays) * 24 * time.Hour)

	perRef := map[graph.Ref]metrics.Series{}
	sourceOf := map[graph.Ref]string{}

	// spec.MetricTypes is priority-ordered: the first type that produces
	// data for a resource wins. A later type is never allowed to mix its
	// (differently-united) samples into a resource another type already
	// populated - e.g. agent.googleapis.com/memory/percent_used and
	// compute.googleapis.com/instance/memory/balloon/ram_used are not the
	// same unit, so blending them would silently corrupt the value.
	for _, mt := range spec.MetricTypes {
		if err := ctx.Err(); err != nil {
			return err
		}

		agg := &monitoringpb.Aggregation{
			AlignmentPeriod:  durationpb.New(time.Duration(spec.AlignmentSec) * time.Second),
			PerSeriesAligner: aligner,
		}
		if reducer != monitoringpb.Aggregation_REDUCE_NONE {
			// GCS metrics carry a storage_class metric label; summing across
			// it per bucket (grouped by the join label) is a same-time-period
			// reduction across a dimension, not across time, so it does not
			// touch the percentile invariant above.
			agg.CrossSeriesReducer = reducer
			agg.GroupByFields = []string{"resource.label." + spec.JoinLabel}
		}

		it := c.client.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{
			Name:   "projects/" + project,
			Filter: fmt.Sprintf(`metric.type="%s" AND resource.type="%s"`, mt, spec.MonitoredResourceType),
			Interval: &monitoringpb.TimeInterval{
				StartTime: timestamppb.New(start),
				EndTime:   timestamppb.New(end),
			},
			Aggregation: agg,
			View:        monitoringpb.ListTimeSeriesRequest_FULL,
		})

		for {
			ts, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return mapMonitoringError(project, mt, err)
			}

			joinValue := ts.GetResource().GetLabels()[spec.JoinLabel]
			if joinValue == "" {
				continue
			}
			ref, ok := c.lookup(spec.MonitoredResourceType, joinValue)
			if !ok {
				continue // resource not in this scan's scope
			}
			if _, already := sourceOf[ref]; already {
				continue // an earlier, preferred metric type already won this resource
			}

			var series metrics.Series
			for _, pt := range ts.GetPoints() {
				v, ok := typedValueToFloat(pt.GetValue())
				if !ok {
					continue
				}
				series = append(series, metrics.Sample{
					Timestamp: pt.GetInterval().GetEndTime().AsTime(),
					Value:     v,
				})
			}
			if len(series) == 0 {
				continue
			}
			perRef[ref] = append(perRef[ref], series...)
			sourceOf[ref] = mt
		}
	}

	windowSecs := windowDays * 86400
	expected := 0
	if spec.AlignmentSec > 0 {
		expected = windowSecs / spec.AlignmentSec
	}

	for ref, pts := range perRef {
		if len(pts) == 0 {
			// Missing data is not zero (invariant I5): leave this resource's
			// metric absent so the rule's MetricOK gate skips it with a
			// clear reason, rather than reporting a fabricated value.
			continue
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].Timestamp.Before(pts[j].Timestamp) })

		value, err := reduceTimeStat(spec, pts)
		if err != nil {
			return fmt.Errorf("gcp metrics: metric %q: %w", key, err)
		}
		if value < spec.ClampLo {
			value = spec.ClampLo
		}
		if value > spec.ClampHi {
			value = spec.ClampHi
		}

		coverage := 0.0
		if expected > 0 {
			coverage = float64(len(pts)) / float64(expected)
			if coverage > 1 {
				coverage = 1
			}
		}

		set(ref, key, graph.MetricValue{
			Value:           value,
			Unit:            spec.Unit,
			Stat:            spec.TimeStat,
			WindowDays:      windowDays,
			Samples:         len(pts),
			ExpectedSamples: expected,
			Coverage:        coverage,
			Source:          sourceOf[ref],
			Aligner:         spec.Aligner,
			Reducer:         spec.Reducer,
			WindowStart:     start,
			WindowEnd:       end,
		})
	}
	return nil
}

// typedValueToFloat extracts the numeric payload of a Cloud Monitoring point.
// Our metrics are always DOUBLE (ratios) or INT64 (byte/object counts) after
// ALIGN_MEAN; anything else is a metric type we do not model.
func typedValueToFloat(v *monitoringpb.TypedValue) (float64, bool) {
	if v == nil {
		return 0, false
	}
	switch tv := v.GetValue().(type) {
	case *monitoringpb.TypedValue_DoubleValue:
		return tv.DoubleValue, true
	case *monitoringpb.TypedValue_Int64Value:
		return float64(tv.Int64Value), true
	default:
		return 0, false
	}
}

// alignerFromName resolves a Spec.Aligner string (e.g. "ALIGN_MEAN") to its
// proto enum. Specs are static and reviewed, so an unknown name is a build-
// time programming error surfaced as a plain error rather than a panic.
func alignerFromName(name string) (monitoringpb.Aggregation_Aligner, error) {
	v, ok := monitoringpb.Aggregation_Aligner_value[name]
	if !ok {
		return 0, fmt.Errorf("gcp metrics: unknown aligner %q", name)
	}
	return monitoringpb.Aggregation_Aligner(v), nil
}

// reducerFromName resolves a Spec.Reducer string (e.g. "REDUCE_NONE") to its
// proto enum.
func reducerFromName(name string) (monitoringpb.Aggregation_Reducer, error) {
	v, ok := monitoringpb.Aggregation_Reducer_value[name]
	if !ok {
		return 0, fmt.Errorf("gcp metrics: unknown reducer %q", name)
	}
	return monitoringpb.Aggregation_Reducer(v), nil
}

// mapMonitoringError turns a raw gRPC status from ListTimeSeries into a
// message an operator can act on, mirroring mapListAssetsError's approach for
// Cloud Asset Inventory.
func mapMonitoringError(project, metricType string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("gcp metrics: list time series for projects/%s (%s): %w", project, metricType, err)
	}
	switch st.Code() {
	case codes.PermissionDenied:
		return fmt.Errorf(
			"gcp metrics: permission denied listing time series for projects/%s: grant roles/monitoring.viewer "+
				"to the identity behind your Application Default Credentials: %s", project, st.Message())
	case codes.NotFound:
		return fmt.Errorf("gcp metrics: project %s not found or has no Cloud Monitoring data: %s", project, st.Message())
	case codes.InvalidArgument:
		return fmt.Errorf("gcp metrics: invalid ListTimeSeries request for projects/%s (%s): %s", project, metricType, st.Message())
	case codes.DeadlineExceeded, codes.Canceled:
		return fmt.Errorf("gcp metrics: list time series for projects/%s (%s): %w", project, metricType, context.DeadlineExceeded)
	case codes.ResourceExhausted:
		return fmt.Errorf("gcp metrics: Cloud Monitoring quota exceeded for projects/%s; retry later or request a quota increase: %s", project, st.Message())
	case codes.Unauthenticated:
		return fmt.Errorf(
			"gcp metrics: unauthenticated listing time series for projects/%s: no valid Application Default Credentials found "+
				"(run `gcloud auth application-default login` or set GOOGLE_APPLICATION_CREDENTIALS): %s",
			project, st.Message())
	default:
		return fmt.Errorf("gcp metrics: list time series for projects/%s (%s): %s: %s", project, metricType, st.Code(), st.Message())
	}
}

// StaticProvider replays pre-aggregated metric values, keyed by graph ref then
// metric key. It is the offline counterpart of Client and is what makes
// metric-dependent rules testable (and --fixture/--cache-file scans runnable)
// with no cloud access.
type StaticProvider struct {
	Values map[graph.Ref]map[string]graph.MetricValue
}

var _ metrics.Provider = (*StaticProvider)(nil)

// Supports implements metrics.Provider.
func (s *StaticProvider) Supports(key string) bool { return metrics.Known(key) }

// Fill implements metrics.Provider by replaying the static table. Absent
// values stay absent: a missing series must never be materialized as zero.
func (s *StaticProvider) Fill(ctx context.Context, req metrics.Request, set metrics.Setter) error {
	for ref, byKey := range s.Values {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, key := range req.Keys {
			v, ok := byKey[key]
			if !ok {
				continue
			}
			set(ref, key, v)
		}
	}
	return nil
}
