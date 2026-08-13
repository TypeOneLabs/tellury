package azure

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/monitor/armmonitor"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
)

// maxConcurrentFetches bounds the number of simultaneous Azure Monitor List
// calls Fill issues. Unlike CloudWatch, Azure Monitor has no batch endpoint:
// exactly one resource URI is queried per List call. Concurrency is therefore
// per-VM rather than per-(key, account, region), and a small fixed limit keeps
// fan-out from tripping ARM throttling.
const maxConcurrentFetches = 8

// MetricsAPI is the subset of the Azure Monitor API this client calls. It is
// what lets a fake stand in for the live SDK client, so tests need no
// credentials or network.
type MetricsAPI interface {
	List(ctx context.Context, resourceURI string, options *armmonitor.MetricsClientListOptions) (armmonitor.MetricsClientListResponse, error)
}

// InstanceRef is one Azure VM to query metrics for. ResourceID is the VM's
// ARM resource ID and is passed verbatim to Azure Monitor as the resourceURI.
type InstanceRef struct {
	Ref        graph.Ref
	ResourceID string
}

// Client is the Azure Monitor implementation of metrics.Provider. It fans out
// one List request per VM per query shape, grouping requested metric names
// that share a namespace, aggregation and interval into a single call so
// adding memory later does not multiply VM requests.
type Client struct {
	log       *slog.Logger
	instances map[string][]InstanceRef // subscription ID -> VMs
	clients   map[string]MetricsAPI    // subscription ID -> Azure Monitor client
}

var _ metrics.Provider = (*Client)(nil)

// NewClient builds the enrichment client with pre-built Azure Monitor clients
// and per-subscription VM lists. The caller (pkg/cloud/azure.Provider.
// EnrichMetrics) owns credential acquisition and client construction; this
// constructor receives the ready-to-use state.
func NewClient(log *slog.Logger, instances map[string][]InstanceRef, clients map[string]MetricsAPI) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{log: log, instances: instances, clients: clients}
}

// Supports implements metrics.Provider: every registered Azure metric spec
// key is considered supported.
func (c *Client) Supports(key string) bool {
	_, ok := SpecOf(key)
	return ok
}

// requestShape is the Azure Monitor query shape shared by a set of metric
// names. Metric names may be grouped into one List call only when all four
// fields match, because Azure Monitor applies one namespace, aggregation,
// interval and timespan to the whole call.
type requestShape struct {
	namespace   string
	aggregation string
	interval    string
	windowDays  int
}

// metricGroup is one Azure Monitor List shape and the specs that use it.
type metricGroup struct {
	shape requestShape
	specs map[string]Spec
}

// fetchJob is one Azure Monitor List call Fill hands to its worker pool.
type fetchJob struct {
	subscription string
	ref          graph.Ref
	resourceID   string
	windowDays   int
	shape        requestShape
	specs        map[string]Spec
}

// Fill implements metrics.Provider. It builds one job per VM per Azure
// Monitor query shape and runs them across a bounded pool — safe because
// graph.Graph.SetMetric (called indirectly through set) is guarded by its own
// mutex and never touches the topology Freeze() seals. A resource with no
// returned points is never written: invariant I5 requires rules to skip
// missing data rather than have it silently become a zero.
//
// Per-job failures are isolated: one failing VM List call does not cancel its
// siblings. Errors are collected and joined.
//
// When req.Progress is non-nil, it is invoked once per completed job with the
// cumulative (completed, total) counts.
func (c *Client) Fill(ctx context.Context, req metrics.Request, set metrics.Setter) error {
	if len(req.Keys) == 0 {
		return nil
	}
	for _, key := range req.Keys {
		if !metrics.Known(key) {
			return fmt.Errorf("azure metrics: unknown metric key %q", key)
		}
	}

	if len(c.clients) == 0 {
		return fmt.Errorf("azure metrics: no Azure Monitor clients configured")
	}

	// Group the requested keys by the Azure Monitor query shape that will be
	// sent. Two keys share a request only when namespace, aggregation and
	// interval match.
	groups := make(map[requestShape]*metricGroup)
	for _, key := range req.Keys {
		spec, ok := SpecOf(key)
		if !ok {
			continue
		}
		shape := requestShape{
			namespace:   spec.Namespace,
			aggregation: spec.Aggregation,
			interval:    spec.Interval,
			windowDays:  Window(req, spec),
		}
		g := groups[shape]
		if g == nil {
			g = &metricGroup{shape: shape, specs: map[string]Spec{}}
			groups[shape] = g
		}
		g.specs[key] = spec
	}

	if len(groups) == 0 {
		return nil
	}

	shapes := make([]requestShape, 0, len(groups))
	for shape := range groups {
		shapes = append(shapes, shape)
	}
	sort.Slice(shapes, func(i, j int) bool {
		if shapes[i].namespace != shapes[j].namespace {
			return shapes[i].namespace < shapes[j].namespace
		}
		if shapes[i].aggregation != shapes[j].aggregation {
			return shapes[i].aggregation < shapes[j].aggregation
		}
		return shapes[i].interval < shapes[j].interval
	})

	subscriptions := make([]string, 0, len(c.clients))
	for sub := range c.clients {
		subscriptions = append(subscriptions, sub)
	}
	sort.Strings(subscriptions)

	var jobs []fetchJob
	for _, sub := range subscriptions {
		if _, ok := c.clients[sub]; !ok {
			continue
		}
		for _, ir := range c.instances[sub] {
			for _, shape := range shapes {
				group := groups[shape]
				jobs = append(jobs, fetchJob{
					subscription: sub,
					ref:          ir.Ref,
					resourceID:   ir.ResourceID,
					windowDays:   shape.windowDays,
					shape:        shape,
					specs:        group.specs,
				})
			}
		}
	}

	total := len(jobs)
	return runFetchJobs(ctx, jobs, func(ctx context.Context, j fetchJob) error {
		return c.fillOne(ctx, j, req, set)
	}, func(done int) {
		if req.Progress != nil {
			req.Progress(done, total)
		}
	})
}

// runFetchJobs runs worker for every job with the same bounded-pool semantics
// as the AWS and GCP clients, isolating the outcome of each job from its
// siblings. onDone is invoked once per completed job with the cumulative
// completed count.
func runFetchJobs(ctx context.Context, jobs []fetchJob, worker func(ctx context.Context, j fetchJob) error, onDone ...func(done int)) error {
	sem := make(chan struct{}, maxConcurrentFetches)
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		errs      []error
		completed atomic.Int64
	)
	report := func(int) {}
	if len(onDone) > 0 && onDone[0] != nil {
		report = onDone[0]
	}
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
			report(int(completed.Add(1)))
		}()
	}
	wg.Wait()

	if len(errs) == 0 {
		return nil
	}
	if len(errs) == 1 {
		return errs[0]
	}
	all := make([]error, 0, len(errs)+1)
	all = append(all, fmt.Errorf("azure metrics: metric enrichment failed for %d of %d VM requests; the rest succeeded", len(errs), len(jobs)))
	all = append(all, errs...)
	return &multiError{errs: all}
}

// multiError is a simple error that joins multiple errors.
type multiError struct{ errs []error }

func (e *multiError) Error() string {
	if len(e.errs) == 0 {
		return ""
	}
	s := e.errs[0].Error()
	for _, err := range e.errs[1:] {
		s += "\n" + err.Error()
	}
	return s
}

// fillOne fetches one Azure Monitor List response for one VM and one query
// shape, applies each spec's ScaleDown conversion, computes the client-side
// stat, and writes one MetricValue per metric that ends up with at least one
// point.
func (c *Client) fillOne(ctx context.Context, j fetchJob, req metrics.Request, set metrics.Setter) error {
	api, ok := c.clients[j.subscription]
	if !ok {
		return fmt.Errorf("azure metrics: no Azure Monitor client for subscription %s", j.subscription)
	}

	names := make([]string, 0, len(j.specs))
	for _, spec := range j.specs {
		names = append(names, spec.MetricName)
	}
	sort.Strings(names)

	end := time.Now().UTC()
	start := end.Add(-time.Duration(j.windowDays) * 24 * time.Hour)

	metricNames := strings.Join(names, ",")
	resp, err := api.List(ctx, j.resourceID, &armmonitor.MetricsClientListOptions{
		Metricnames:         to.Ptr(metricNames),
		Metricnamespace:     to.Ptr(j.shape.namespace),
		Aggregation:         to.Ptr(j.shape.aggregation),
		Interval:            to.Ptr(j.shape.interval),
		Timespan:            to.Ptr(start.Format(time.RFC3339) + "/" + end.Format(time.RFC3339)),
		ResultType:          to.Ptr(armmonitor.ResultTypeData),
		AutoAdjustTimegrain: to.Ptr(false),
	})
	if err != nil {
		return fmt.Errorf("azure metrics: List for resource %s key(s) %s: %w", j.resourceID, metricNames, err)
	}

	expected := 0
	if secs := intervalSeconds(j.shape.interval); secs > 0 {
		expected = j.windowDays * 86400 / secs
	}

	// Group the returned metrics by Azure Monitor metric name. A fake or a
	// future multi-metric call may return the metrics in any order, so match
	// by name rather than by position.
	byMetricName := make(map[string][]*armmonitor.Metric, len(resp.Value))
	for _, m := range resp.Value {
		if m == nil || m.Name == nil || m.Name.Value == nil {
			continue
		}
		byMetricName[*m.Name.Value] = append(byMetricName[*m.Name.Value], m)
	}

	for key, spec := range j.specs {
		matches, ok := byMetricName[spec.MetricName]
		if !ok {
			continue // missing metric stays absent, never zero
		}

		var series metrics.Series
		for _, m := range matches {
			for _, ts := range m.Timeseries {
				if ts == nil {
					continue
				}
				for _, point := range ts.Data {
					if point == nil || point.TimeStamp == nil || point.Average == nil {
						continue
					}
					v := *point.Average
					if spec.ScaleDown != nil {
						v = spec.ScaleDown(v)
					}
					series = append(series, metrics.Sample{
						Timestamp: *point.TimeStamp,
						Value:     v,
					})
				}
			}
		}
		if len(series) == 0 {
			continue // invariant I5: missing data is not zero
		}

		sort.Slice(series, func(i, j int) bool { return series[i].Timestamp.Before(series[j].Timestamp) })

		value, err := reduceTimeStat(spec, series)
		if err != nil {
			return fmt.Errorf("azure metrics: metric %q: %w", key, err)
		}
		value = clamp(value, spec.ClampLo, spec.ClampHi)

		coverage := 0.0
		if expected > 0 {
			coverage = float64(len(series)) / float64(expected)
			if coverage > 1 {
				coverage = 1
			}
		}

		set(j.ref, key, graph.MetricValue{
			Value:           value,
			Unit:            spec.Unit,
			Stat:            spec.TimeStat,
			WindowDays:      j.windowDays,
			Samples:         len(series),
			ExpectedSamples: expected,
			Coverage:        coverage,
			Source:          fmt.Sprintf("azuremonitor:%s:%s", spec.Namespace, spec.MetricName),
			Aligner:         spec.Aggregation,
			Reducer:         "REDUCE_NONE",
			WindowStart:     start,
			WindowEnd:       end,
		})
	}

	return nil
}

// clamp bounds v to [lo, hi].
func clamp(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// intervalSeconds converts an Azure Monitor ISO 8601 interval string (e.g.
// "PT1H") to whole seconds. It handles the duration forms used by the
// registered specs: PnD and PTnH/nM/nS. An unknown form returns 0, which the
// caller treats as "expected sample count unavailable".
func intervalSeconds(iso string) int {
	if iso == "" || iso == "FULL" || !strings.HasPrefix(iso, "P") {
		return 0
	}
	total := 0
	num := 0
	for _, r := range iso[1:] {
		switch {
		case r == 'T':
			// Marks the start of the time component. For our specs this is
			// always present before H/M/S, but a date-only PnD works too.
		case r >= '0' && r <= '9':
			num = num*10 + int(r-'0')
		case r == 'D':
			total += num * 86400
			num = 0
		case r == 'H':
			total += num * 3600
			num = 0
		case r == 'M':
			total += num * 60
			num = 0
		case r == 'S':
			total += num
			num = 0
		}
	}
	return total
}

// StaticProvider replays pre-aggregated metric values, keyed by graph ref
// then metric key. It is the offline counterpart of Client and is what makes
// metric-dependent rules testable with no cloud access.
type StaticProvider struct {
	Values map[graph.Ref]map[string]graph.MetricValue
}

var _ metrics.Provider = (*StaticProvider)(nil)

// Supports implements metrics.Provider: every registered Azure metric spec key
// is considered supported.
func (s *StaticProvider) Supports(key string) bool {
	_, ok := SpecOf(key)
	return ok
}

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
