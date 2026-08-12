package aws

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
)

// maxConcurrentFetches bounds the number of simultaneous GetMetricData calls
// Fill issues. Concurrency here is across (metric key, account, region) jobs,
// not per-instance: one GetMetricData call can carry up to 500 queries, so
// each job packs many instances into few calls.
const maxConcurrentFetches = 8

// maxMetricsPerCall is the CloudWatch GetMetricData limit: up to 500
// MetricDataQuery entries per call.
const maxMetricsPerCall = 500

// CloudWatchAPI is the subset of the CloudWatch API this client calls. It is
// what lets a fake stand in for the live SDK client, so tests need no
// credentials or network.
type CloudWatchAPI interface {
	GetMetricData(ctx context.Context, params *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// AccountRegion is a composite key for the per-account, per-region CloudWatch
// client map.
type AccountRegion struct {
	Account string
	Region  string
}

// InstanceRef is one instance to query metrics for.
type InstanceRef struct {
	Ref        graph.Ref
	InstanceID string
}

// Client is the CloudWatch implementation of metrics.Provider. It fans out
// (key, account, region) jobs across a bounded pool, packing per-instance
// queries into GetMetricData calls of up to 500 MetricDataQuery entries each.
//
// CloudWatch has no server-side percentile over a multi-period window, so the
// p95 is computed client-side over the aligned points — exactly the same
// contract as the GCP client.
type Client struct {
	log       *slog.Logger
	instances map[AccountRegion][]InstanceRef
	clients   map[AccountRegion]CloudWatchAPI
}

var _ metrics.Provider = (*Client)(nil)

// NewClient builds the enrichment client with pre-built CloudWatch clients
// and per-(account, region) instance lists. The caller
// (pkg/cloud/aws.Provider.EnrichMetrics) owns credential acquisition and
// client construction; this constructor receives the ready-to-use state.
func NewClient(log *slog.Logger, instances map[AccountRegion][]InstanceRef, clients map[AccountRegion]CloudWatchAPI) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{log: log, instances: instances, clients: clients}
}

// Supports implements metrics.Provider: every registered AWS metric spec key
// is considered supported.
func (c *Client) Supports(key string) bool {
	_, ok := SpecOf(key)
	return ok
}

// Fill implements metrics.Provider. It fans the requested (key, account,
// region) tuples out across bounded goroutines — safe because
// graph.Graph.SetMetric (called indirectly through set) is guarded by its own
// mutex and never touches the topology Freeze() seals. A resource with no
// returned points is never written: invariant I5 requires rules to skip
// missing data rather than have it silently become a zero.
//
// Per-job failures are isolated: one failing (key, account, region) job does
// not cancel its siblings. Errors are collected and joined.
//
// When req.Progress is non-nil, it is invoked once per completed job with the
// cumulative (completed, total) counts.
func (c *Client) Fill(ctx context.Context, req metrics.Request, set metrics.Setter) error {
	if len(req.Keys) == 0 {
		return nil
	}
	for _, key := range req.Keys {
		if !metrics.Known(key) {
			return fmt.Errorf("aws metrics: unknown metric key %q", key)
		}
	}

	if len(c.clients) == 0 {
		return fmt.Errorf("aws metrics: no CloudWatch clients configured")
	}

	// De-duplicate account/region pairs from the clients map.
	type ar struct{ account, region string }
	seen := make(map[ar]bool)
	var ars []ar
	for k := range c.clients {
		p := ar{k.Account, k.Region}
		if !seen[p] {
			seen[p] = true
			ars = append(ars, p)
		}
	}

	// Build jobs: every (key, account, region) that has at least one instance
	// to query. A (key, account, region) with no instances skips the job
	// entirely — no CloudWatch call is made for an empty instance list.
	var jobs []fetchJob
	for _, key := range req.Keys {
		for _, p := range ars {
			arKey := AccountRegion{p.account, p.region}
			if _, hasCW := c.clients[arKey]; !hasCW {
				continue
			}
			insts := c.instances[arKey]
			jobs = append(jobs, fetchJob{
				key:       key,
				account:   p.account,
				region:    p.region,
				instances: insts,
			})
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

// fetchJob is one (metric key, account, region) job Fill hands to its worker
// pool, together with the instances to query in that (account, region).
type fetchJob struct {
	key       string
	account   string
	region    string
	instances []InstanceRef
}

// runFetchJobs runs worker for every job with the same bounded-pool semantics
// as the GCP client, isolating the outcome of each job from its siblings.
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
	all = append(all, fmt.Errorf("aws metrics: metric enrichment failed for %d of %d (key, account, region) jobs; the rest succeeded", len(errs), len(jobs)))
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

// fillOne fetches the metric data for one (key, account, region) job. It
// packs the per-instance queries into GetMetricData calls of up to 500
// MetricDataQuery entries each, applies the ScaleDown conversion, computes
// the client-side stat, and writes one MetricValue per instance that ends up
// with at least one point.
func (c *Client) fillOne(ctx context.Context, j fetchJob, req metrics.Request, set metrics.Setter) error {
	spec, ok := SpecOf(j.key)
	if !ok {
		return fmt.Errorf("aws metrics: no spec for metric key %q", j.key)
	}

	cw, ok := c.clients[AccountRegion{j.account, j.region}]
	if !ok {
		return fmt.Errorf("aws metrics: no CloudWatch client for account %s region %s", j.account, j.region)
	}

	if len(j.instances) == 0 {
		return nil
	}

	windowDays := Window(req, spec)
	end := time.Now().UTC()
	start := end.Add(-time.Duration(windowDays) * 24 * time.Hour)

	// Pack queries into batches of up to maxMetricsPerCall.
	for batchStart := 0; batchStart < len(j.instances); batchStart += maxMetricsPerCall {
		batchEnd := batchStart + maxMetricsPerCall
		if batchEnd > len(j.instances) {
			batchEnd = len(j.instances)
		}
		batch := j.instances[batchStart:batchEnd]

		queries := make([]types.MetricDataQuery, 0, len(batch))
		queryToRef := make(map[string]graph.Ref, len(batch))

		for i, ir := range batch {
			qID := fmt.Sprintf("q%d", i)
			queries = append(queries, types.MetricDataQuery{
				Id:         aws.String(qID),
				ReturnData: aws.Bool(true),
				MetricStat: &types.MetricStat{
					Metric: &types.Metric{
						Namespace:  aws.String(spec.Namespace),
						MetricName: aws.String(spec.MetricName),
						Dimensions: []types.Dimension{
							{
								Name:  aws.String(spec.DimensionKey),
								Value: aws.String(ir.InstanceID),
							},
						},
					},
					Period: aws.Int32(int32(spec.PeriodSec)),
					Stat:   aws.String(spec.Stat),
				},
			})
			queryToRef[qID] = ir.Ref
		}

		// Make the GetMetricData call, looping for truncated results.
		var nextToken *string
		perRef := make(map[graph.Ref]metrics.Series, len(batch))
		for {
			out, err := cw.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
				MetricDataQueries: queries,
				StartTime:         aws.Time(start),
				EndTime:           aws.Time(end),
				NextToken:         nextToken,
			})
			if err != nil {
				return fmt.Errorf("aws metrics: GetMetricData for account %s region %s key %q: %w", j.account, j.region, j.key, err)
			}

			for _, res := range out.MetricDataResults {
				qID := aws.ToString(res.Id)
				ref, ok := queryToRef[qID]
				if !ok {
					continue
				}
				// CloudWatch returns Timestamps and Values as parallel slices.
				// Convert them to our Series type.
				for idx, ts := range res.Timestamps {
					if idx >= len(res.Values) {
						break
					}
					v := res.Values[idx]
					// Apply ScaleDown if set (e.g. CPUUtilization percentage → fraction).
					if spec.ScaleDown != nil {
						v = spec.ScaleDown(v)
					}
					perRef[ref] = append(perRef[ref], metrics.Sample{
						Timestamp: ts,
						Value:     v,
					})
				}
			}

			nextToken = out.NextToken
			if nextToken == nil || *nextToken == "" {
				break
			}
		}

		// Compute client-side reduction and write results.
		windowSecs := windowDays * 86400
		expected := 0
		if spec.PeriodSec > 0 {
			expected = windowSecs / spec.PeriodSec
		}

		for ref, pts := range perRef {
			if len(pts) == 0 {
				continue
			}
			sort.Slice(pts, func(i, j int) bool { return pts[i].Timestamp.Before(pts[j].Timestamp) })

			value, err := reduceTimeStat(spec, pts)
			if err != nil {
				return fmt.Errorf("aws metrics: metric %q: %w", j.key, err)
			}
			value = clamp(value, spec.ClampLo, spec.ClampHi)

			coverage := 0.0
			if expected > 0 {
				coverage = float64(len(pts)) / float64(expected)
				if coverage > 1 {
					coverage = 1
				}
			}

			set(ref, j.key, graph.MetricValue{
				Value:           value,
				Unit:            spec.Unit,
				Stat:            spec.TimeStat,
				WindowDays:      windowDays,
				Samples:         len(pts),
				ExpectedSamples: expected,
				Coverage:        coverage,
				Source:          fmt.Sprintf("cloudwatch:%s:%s", spec.Namespace, spec.MetricName),
				Aligner:         spec.Stat,
				Reducer:         "REDUCE_NONE",
				WindowStart:     start,
				WindowEnd:       end,
			})
		}
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

// StaticProvider replays pre-aggregated metric values, keyed by graph ref
// then metric key. It is the offline counterpart of Client and is what makes
// metric-dependent rules testable (and --fixture scans runnable) with no
// cloud access.
type StaticProvider struct {
	Values map[graph.Ref]map[string]graph.MetricValue
}

var _ metrics.Provider = (*StaticProvider)(nil)

// Supports implements metrics.Provider: every registered AWS metric spec key
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
