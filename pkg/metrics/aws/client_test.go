package aws_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	metricsaws "github.com/TypeOneLabs/tellury/pkg/metrics/aws"
	_ "github.com/TypeOneLabs/tellury/pkg/metrics/aws/compute"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeCloudWatch is a fake CloudWatch client that replays pre-recorded
// GetMetricData responses keyed by a deterministic signature of the request.
type fakeCloudWatch struct {
	// responses maps a key derived from the request to a pre-canned response.
	responses map[string]*cloudwatch.GetMetricDataOutput
	// calls records every call for inspection.
	calls []*cloudwatch.GetMetricDataInput
}

func (f *fakeCloudWatch) GetMetricData(_ context.Context, params *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	f.calls = append(f.calls, params)
	// Build a key from the first query's namespace + metric + dimension.
	key := requestKey(params)
	if resp, ok := f.responses[key]; ok {
		return resp, nil
	}
	// Default: return empty results.
	return &cloudwatch.GetMetricDataOutput{}, nil
}

func requestKey(params *cloudwatch.GetMetricDataInput) string {
	if len(params.MetricDataQueries) == 0 {
		return ""
	}
	q := params.MetricDataQueries[0]
	if q.MetricStat == nil || q.MetricStat.Metric == nil {
		return ""
	}
	m := q.MetricStat.Metric
	return aws.ToString(m.Namespace) + "/" + aws.ToString(m.MetricName)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestClient_SupportsCPUUtilizationP95 verifies that the AWS metrics client
// supports the cpu_utilization_p95 key when the compute specs are registered.
func TestClient_SupportsCPUUtilizationP95(t *testing.T) {
	c := metricsaws.NewClient(newTestLogger(), nil, nil, nil)
	if !c.Supports(metrics.KeyCPUUtilizationP95) {
		t.Fatal("cpu_utilization_p95 must be supported — is pkg/metrics/aws/compute imported?")
	}
}

// TestClient_SupportsMemUtilizationP95_False verifies that the AWS metrics
// client returns false for mem_utilization_p95. EC2 publishes no memory
// metric without the CloudWatch agent, so a rule that declares it must be
// told the metric is unavailable — never silently treat missing data as low
// memory usage.
func TestClient_SupportsMemUtilizationP95_False(t *testing.T) {
	c := metricsaws.NewClient(newTestLogger(), nil, nil, nil)
	if c.Supports(metrics.KeyMemUtilizationP95) {
		t.Fatal("mem_utilization_p95 must NOT be supported on AWS — EC2 publishes no memory metric without the CloudWatch agent")
	}
}

// TestFill_PercentageToFractionConversion is the regression test for the
// percentage-to-fraction conversion. CloudWatch's CPUUtilization is a
// percentage (0-100), but the graph stores fractions (0-1) to match the GCP
// convention. A 100x error here produces findings that look sane — e.g. a
// 3% utilized instance would report as 300% utilized, which still looks
// plausible as a value between 0 and 1.0 after clamping.
func TestFill_PercentageToFractionConversion(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)

	// Recorded CloudWatch response: CPUUtilization at 60% average over 3
	// hours. Without the ScaleDown conversion, the stored value would be
	// 60.0, which after clamping to [0,1] becomes 1.0 — i.e. the instance
	// appears 100% utilized. With the conversion, it becomes 0.60.
	fake := &fakeCloudWatch{
		responses: map[string]*cloudwatch.GetMetricDataOutput{
			"AWS/EC2/CPUUtilization": {
				MetricDataResults: []types.MetricDataResult{
					{
						Id:         aws.String("q0"),
						Values:     []float64{60.0, 65.0, 55.0},
						Timestamps: []time.Time{now.Add(-2 * time.Hour), now.Add(-1 * time.Hour), now},
					},
				},
			},
		},
	}

	ar := metricsaws.AccountRegion{Account: "123456789012", Region: "us-east-1"}
	clients := map[metricsaws.AccountRegion]metricsaws.CloudWatchAPI{ar: fake}
	instances := map[metricsaws.AccountRegion][]metricsaws.InstanceRef{
		ar: {{Ref: "accounts/123456789012/regions/us-east-1/instances/i-test", InstanceID: "i-test"}},
	}

	c := metricsaws.NewClient(newTestLogger(), nil, instances, clients)

	var gotValue float64
	set := func(ref graph.Ref, key string, v graph.MetricValue) {
		if key == metrics.KeyCPUUtilizationP95 {
			gotValue = v.Value
		}
	}

	req := metrics.Request{
		Keys:       []string{metrics.KeyCPUUtilizationP95},
		WindowDays: 14,
	}

	if err := c.Fill(context.Background(), req, set); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	// With 3 hourly samples (60, 65, 55) after ScaleDown (/100):
	// values = [0.55, 0.60, 0.65] (sorted).
	// p95 at quantile 0.95 with n=3:
	//   idx = ceil(0.95 * 3) - 1 = ceil(2.85) - 1 = 3 - 1 = 2
	//   => sorted[2] = 0.65
	want := 0.65
	if gotValue != want {
		t.Fatalf("p95 CPU = %v, want %v — the percentage-to-fraction conversion (/100) must be applied. "+
			"Without it the stored value would be 1.0 (clamped from 65.0), making a 65%% utilized instance "+
			"appear 100%% utilized.", gotValue, want)
	}

	// Verify the conversion was applied per-sample.
	if gotValue > 1.0 {
		t.Fatalf("p95 CPU = %v exceeds 1.0 — clamping should have caught this, meaning the conversion is missing", gotValue)
	}
}

// TestFill_MissingInstanceGetsNoMetric verifies that an instance with no
// CloudWatch data is not written to the graph — invariant I5: missing data
// must not read as zero.
func TestFill_MissingInstanceGetsNoMetric(t *testing.T) {
	fake := &fakeCloudWatch{
		responses: map[string]*cloudwatch.GetMetricDataOutput{
			"AWS/EC2/CPUUtilization": {
				MetricDataResults: []types.MetricDataResult{
					{
						Id:         aws.String("q0"),
						Values:     []float64{},
						Timestamps: []time.Time{},
					},
				},
			},
		},
	}

	ar := metricsaws.AccountRegion{Account: "123456789012", Region: "us-east-1"}
	clients := map[metricsaws.AccountRegion]metricsaws.CloudWatchAPI{ar: fake}
	instances := map[metricsaws.AccountRegion][]metricsaws.InstanceRef{
		ar: {{Ref: "accounts/123456789012/regions/us-east-1/instances/i-missing", InstanceID: "i-missing"}},
	}

	c := metricsaws.NewClient(newTestLogger(), nil, instances, clients)

	setCalled := false
	set := func(ref graph.Ref, key string, v graph.MetricValue) {
		setCalled = true
	}

	req := metrics.Request{
		Keys:       []string{metrics.KeyCPUUtilizationP95},
		WindowDays: 14,
	}

	if err := c.Fill(context.Background(), req, set); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	if setCalled {
		t.Fatal("set was called for an instance with no CloudWatch data — invariant I5 violated: missing data must not be materialized as zero")
	}
}

// TestFill_MultipleInstancesBatched verifies that instances are packed into
// GetMetricData calls correctly and each instance receives its own metric.
func TestFill_MultipleInstancesBatched(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)

	fake := &fakeCloudWatch{
		responses: map[string]*cloudwatch.GetMetricDataOutput{
			"AWS/EC2/CPUUtilization": {
				MetricDataResults: []types.MetricDataResult{
					{
						Id:         aws.String("q0"),
						Values:     []float64{10.0, 12.0},
						Timestamps: []time.Time{now.Add(-1 * time.Hour), now},
					},
					{
						Id:         aws.String("q1"),
						Values:     []float64{80.0, 82.0},
						Timestamps: []time.Time{now.Add(-1 * time.Hour), now},
					},
				},
			},
		},
	}

	ar := metricsaws.AccountRegion{Account: "123456789012", Region: "us-east-1"}
	clients := map[metricsaws.AccountRegion]metricsaws.CloudWatchAPI{ar: fake}
	instances := map[metricsaws.AccountRegion][]metricsaws.InstanceRef{
		ar: {
			{Ref: "accounts/123456789012/regions/us-east-1/instances/i-low", InstanceID: "i-low"},
			{Ref: "accounts/123456789012/regions/us-east-1/instances/i-high", InstanceID: "i-high"},
		},
	}

	c := metricsaws.NewClient(newTestLogger(), nil, instances, clients)

	got := make(map[string]graph.MetricValue)
	set := func(ref graph.Ref, key string, v graph.MetricValue) {
		got[string(ref)] = v
	}

	req := metrics.Request{
		Keys:       []string{metrics.KeyCPUUtilizationP95},
		WindowDays: 14,
	}

	if err := c.Fill(context.Background(), req, set); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 instance metrics, got %d", len(got))
	}

	lowRef := "accounts/123456789012/regions/us-east-1/instances/i-low"
	highRef := "accounts/123456789012/regions/us-east-1/instances/i-high"

	lowVal, ok := got[lowRef]
	if !ok {
		t.Fatalf("no metric for %s", lowRef)
	}
	// Sorted: [0.10, 0.12], n=2, p95 idx = ceil(0.95*2)-1 = ceil(1.9)-1 = 2-1 = 1 → 0.12
	if lowVal.Value != 0.12 {
		t.Errorf("i-low p95 = %v, want 0.12 (from 10%%, 12%% after /100 → 0.10, 0.12)", lowVal.Value)
	}

	highVal, ok := got[highRef]
	if !ok {
		t.Fatalf("no metric for %s", highRef)
	}
	// Sorted: [0.80, 0.82], n=2, p95 idx = 1 → 0.82
	if highVal.Value != 0.82 {
		t.Errorf("i-high p95 = %v, want 0.82 (from 80%%, 82%% after /100 → 0.80, 0.82)", highVal.Value)
	}
}

// TestStaticProvider_ReplaysValues verifies that StaticProvider correctly
// replays pre-aggregated values.
func TestStaticProvider_ReplaysValues(t *testing.T) {
	ref := graph.Ref("accounts/123456789012/regions/us-east-1/instances/i-test")
	sp := &metricsaws.StaticProvider{
		Values: map[graph.Ref]map[string]graph.MetricValue{
			ref: {
				metrics.KeyCPUUtilizationP95: {
					Value: 0.65, Unit: "ratio", Stat: "p95",
					WindowDays: 14, Samples: 336, ExpectedSamples: 336,
					Coverage: 1.0, Source: "cloudwatch:AWS/EC2:CPUUtilization",
				},
			},
		},
	}

	var got graph.MetricValue
	set := func(r graph.Ref, k string, v graph.MetricValue) {
		if r == ref && k == metrics.KeyCPUUtilizationP95 {
			got = v
		}
	}

	req := metrics.Request{Keys: []string{metrics.KeyCPUUtilizationP95}}
	if err := sp.Fill(context.Background(), req, set); err != nil {
		t.Fatalf("StaticProvider.Fill: %v", err)
	}

	if got.Value != 0.65 {
		t.Errorf("value = %v, want 0.65", got.Value)
	}
}
