package azure_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/monitor/armmonitor"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	metricsazure "github.com/TypeOneLabs/tellury/pkg/metrics/azure"
	_ "github.com/TypeOneLabs/tellury/pkg/metrics/azure/compute"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeMetricsAPI replays pre-recorded Azure Monitor List responses keyed by
// the comma-separated metricnames string.
type fakeMetricsAPI struct {
	mu        sync.Mutex
	responses map[string]armmonitor.MetricsClientListResponse
	errs      map[string]error
	calls     []*armmonitor.MetricsClientListOptions
	uris      []string
}

func (f *fakeMetricsAPI) List(_ context.Context, resourceURI string, options *armmonitor.MetricsClientListOptions) (armmonitor.MetricsClientListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, options)
	f.uris = append(f.uris, resourceURI)

	key := ""
	if options != nil && options.Metricnames != nil {
		key = *options.Metricnames
	}
	if err := f.errs[key]; err != nil {
		return armmonitor.MetricsClientListResponse{}, err
	}
	if resp, ok := f.responses[key]; ok {
		return resp, nil
	}
	return armmonitor.MetricsClientListResponse{}, nil
}

func (f *fakeMetricsAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeMetricsAPI) lastCall() *armmonitor.MetricsClientListOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func metricResponse(metricName string, values []float64, stamps []time.Time) armmonitor.MetricsClientListResponse {
	var points []*armmonitor.MetricValue
	for i, v := range values {
		ts := stamps[i]
		point := armmonitor.MetricValue{
			TimeStamp: &ts,
			Average:   to.Ptr(v),
		}
		points = append(points, &point)
	}
	return armmonitor.MetricsClientListResponse{
		Response: armmonitor.Response{
			Value: []*armmonitor.Metric{
				{
					Name: &armmonitor.LocalizableString{Value: to.Ptr(metricName)},
					Timeseries: []*armmonitor.TimeSeriesElement{
						{Data: points},
					},
				},
			},
		},
	}
}

func stamps(start time.Time, hours int) []time.Time {
	out := make([]time.Time, hours)
	for i := range out {
		out[i] = start.Add(time.Duration(i) * time.Hour)
	}
	return out
}

func newClient(fake metricsazure.MetricsAPI, subs []string, refs []string) *metricsazure.Client {
	instances := map[string][]metricsazure.InstanceRef{}
	clients := map[string]metricsazure.MetricsAPI{}
	for i, sub := range subs {
		clients[sub] = fake
		instances[sub] = []metricsazure.InstanceRef{
			{Ref: graph.Ref(refs[i]), ResourceID: "/subscriptions/" + sub + "/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-" + sub},
		}
	}
	return metricsazure.NewClient(newTestLogger(), instances, clients)
}

func TestClient_SupportsComputeMetrics(t *testing.T) {
	c := metricsazure.NewClient(newTestLogger(), nil, nil)
	if !c.Supports(metrics.KeyCPUUtilizationP95) {
		t.Fatal("cpu_utilization_p95 must be supported — is pkg/metrics/azure/compute imported?")
	}
	// Memory is keyed and specced in this release, but underutilized_vm does
	// not require it. Supports must still say true so a future rule that does
	// declare it can fetch it.
	if !c.Supports(metrics.KeyMemUtilizationP95) {
		t.Fatal("mem_utilization_p95 must be supported — Azure reports memory without an agent")
	}
}

// TestFill_PercentageToFractionConversion is the regression test for the CPU
// percent-to-fraction conversion. Azure Monitor's Percentage CPU has unit
// Percent (0-100), but the graph stores a fraction (0-1) and every threshold
// is written against a fraction. A 100x error here produces findings that look
// entirely reasonable.
func TestFill_PercentageToFractionConversion(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	fake := &fakeMetricsAPI{
		responses: map[string]armmonitor.MetricsClientListResponse{
			"Percentage CPU": metricResponse("Percentage CPU", []float64{60, 65, 55}, stamps(now.Add(-2*time.Hour), 3)),
		},
	}

	c := newClient(fake, []string{"sub-1"}, []string{"sub-1/vm-a"})

	var got graph.MetricValue
	set := func(ref graph.Ref, key string, v graph.MetricValue) {
		if key == metrics.KeyCPUUtilizationP95 {
			got = v
		}
	}

	req := metrics.Request{Keys: []string{metrics.KeyCPUUtilizationP95}, WindowDays: 14}
	if err := c.Fill(context.Background(), req, set); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	// Raw [55,60,65] -> after /100 [0.55,0.60,0.65]; p95 n=3 idx 2 -> 0.65.
	if got.Value != 0.65 {
		t.Fatalf("p95 CPU = %v, want 0.65 — the percentage-to-fraction conversion (/100) must be applied", got.Value)
	}
	if got.Value > 1.0 {
		t.Fatalf("p95 CPU = %v exceeds 1.0 — the percentage-to-fraction conversion is missing", got.Value)
	}

	call := fake.lastCall()
	if call == nil {
		t.Fatal("expected a Metrics List call")
	}
	if call.Interval == nil || *call.Interval != "PT1H" {
		t.Errorf("Interval = %v, want PT1H", call.Interval)
	}
	if call.Aggregation == nil || *call.Aggregation != "Average" {
		t.Errorf("Aggregation = %v, want Average", call.Aggregation)
	}
	if call.ResultType == nil || *call.ResultType != armmonitor.ResultTypeData {
		t.Errorf("ResultType = %v, want %v", call.ResultType, armmonitor.ResultTypeData)
	}
	if call.AutoAdjustTimegrain == nil || *call.AutoAdjustTimegrain {
		t.Errorf("AutoAdjustTimegrain = %v, want false", call.AutoAdjustTimegrain)
	}
}

// TestFill_MemoryAvailableToUsedInversion pins the Azure-specific inversion:
// Azure reports AVAILABLE memory, while mem_utilization_p95 means USED. An
// idle VM reporting 90% available must become 0.10 used, not 0.90.
func TestFill_MemoryAvailableToUsedInversion(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	fake := &fakeMetricsAPI{
		responses: map[string]armmonitor.MetricsClientListResponse{
			"Available Memory Percentage": metricResponse("Available Memory Percentage", []float64{90, 92, 88}, stamps(now.Add(-2*time.Hour), 3)),
		},
	}

	c := newClient(fake, []string{"sub-1"}, []string{"sub-1/vm-a"})

	var got graph.MetricValue
	set := func(ref graph.Ref, key string, v graph.MetricValue) {
		if key == metrics.KeyMemUtilizationP95 {
			got = v
		}
	}

	req := metrics.Request{Keys: []string{metrics.KeyMemUtilizationP95}, WindowDays: 14}
	if err := c.Fill(context.Background(), req, set); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	// Raw available [88,90,92] -> used [0.12,0.10,0.08] -> sorted [0.08,0.10,0.12] -> p95 0.12.
	if got.Value != 0.12 {
		t.Fatalf("p95 memory = %v, want 0.12 — Azure available-memory must be inverted into used memory", got.Value)
	}
	if got.Value > 0.5 {
		t.Fatalf("p95 memory = %v looks like available memory, not used memory — inversion was dropped", got.Value)
	}
}

// TestFill_MissingMetricStaysAbsent verifies invariant I5: a VM with no Azure
// Monitor points is never materialized as zero.
func TestFill_MissingMetricStaysAbsent(t *testing.T) {
	fake := &fakeMetricsAPI{responses: map[string]armmonitor.MetricsClientListResponse{
		"Percentage CPU": metricResponse("Percentage CPU", nil, nil),
	}}

	c := newClient(fake, []string{"sub-1"}, []string{"sub-1/vm-missing"})

	setCalled := false
	set := func(ref graph.Ref, key string, v graph.MetricValue) { setCalled = true }

	req := metrics.Request{Keys: []string{metrics.KeyCPUUtilizationP95}, WindowDays: 14}
	if err := c.Fill(context.Background(), req, set); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if setCalled {
		t.Fatal("set was called for a VM with no Azure Monitor data — invariant I5 violated")
	}
}

// TestFill_GroupsMetricNamesPerResource pins the Azure batching shape: two
// metric names sharing namespace/aggregation/interval must be requested in one
// List call per VM, not one call per key.
func TestFill_GroupsMetricNamesPerResource(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	cpu := metricResponse("Percentage CPU", []float64{50}, stamps(now, 1))
	mem := metricResponse("Available Memory Percentage", []float64{90}, stamps(now, 1))
	both := armmonitor.MetricsClientListResponse{
		Response: armmonitor.Response{Value: append(append([]*armmonitor.Metric{}, cpu.Value...), mem.Value...)},
	}

	fake := &fakeMetricsAPI{
		responses: map[string]armmonitor.MetricsClientListResponse{
			"Available Memory Percentage,Percentage CPU": both,
		},
	}

	c := newClient(fake, []string{"sub-1"}, []string{"sub-1/vm-a"})

	got := map[string]graph.MetricValue{}
	set := func(ref graph.Ref, key string, v graph.MetricValue) { got[key] = v }

	req := metrics.Request{
		Keys:       []string{metrics.KeyCPUUtilizationP95, metrics.KeyMemUtilizationP95},
		WindowDays: 14,
	}
	if err := c.Fill(context.Background(), req, set); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d metrics, want 2 (cpu and memory)", len(got))
	}
	if fake.callCount() != 1 {
		t.Fatalf("List called %d times, want 1 — metric names sharing a query shape must be grouped per resource", fake.callCount())
	}
}

// TestFill_ProgressReceivesCumulativeDone pins the Request.Progress seam for
// Azure's per-VM fan-out.
func TestFill_ProgressReceivesCumulativeDone(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	fake := &fakeMetricsAPI{
		responses: map[string]armmonitor.MetricsClientListResponse{
			"Percentage CPU": metricResponse("Percentage CPU", []float64{30}, stamps(now, 1)),
		},
	}

	subs := []string{"sub-1", "sub-2", "sub-3"}
	refs := []string{"sub-1/vm-a", "sub-2/vm-b", "sub-3/vm-c"}
	instances := map[string][]metricsazure.InstanceRef{}
	clients := map[string]metricsazure.MetricsAPI{}
	for i, sub := range subs {
		clients[sub] = fake
		instances[sub] = []metricsazure.InstanceRef{{Ref: graph.Ref(refs[i]), ResourceID: "/subscriptions/" + sub + "/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-" + sub}}
	}
	c := metricsazure.NewClient(newTestLogger(), instances, clients)

	var (
		mu  sync.Mutex
		got []int
	)
	req := metrics.Request{
		Keys:       []string{metrics.KeyCPUUtilizationP95},
		WindowDays: 14,
		Progress: func(done, total int) {
			mu.Lock()
			got = append(got, done)
			mu.Unlock()
		},
	}

	if err := c.Fill(context.Background(), req, func(graph.Ref, string, graph.MetricValue) {}); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	sort.Ints(got)
	if len(got) != len(subs) {
		t.Fatalf("progress callback invoked %d times, want %d", len(got), len(subs))
	}
	for i, d := range got {
		if d != i+1 {
			t.Fatalf("cumulative done counts out of order: got[%d]=%d, want %d", i, d, i+1)
		}
	}
}

// TestFill_IsolatesPerJobFailures verifies that one failing VM request does not
// cancel healthy siblings — enrichment failure stays non-fatal at the job
// level and the healthy VMs still receive their metrics.
func TestFill_IsolatesPerJobFailures(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	poison := &fakeMetricsAPI{errs: map[string]error{"Percentage CPU": fmt.Errorf("simulated throttling")}}
	healthy := &fakeMetricsAPI{
		responses: map[string]armmonitor.MetricsClientListResponse{
			"Percentage CPU": metricResponse("Percentage CPU", []float64{30}, stamps(now, 1)),
		},
	}

	instances := map[string][]metricsazure.InstanceRef{
		"sub-poison": {{Ref: graph.Ref("sub-poison/vm-a"), ResourceID: "/subscriptions/sub-poison/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-a"}},
		"sub-health": {{Ref: graph.Ref("sub-health/vm-b"), ResourceID: "/subscriptions/sub-health/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-b"}},
	}
	clients := map[string]metricsazure.MetricsAPI{
		"sub-poison": poison,
		"sub-health": healthy,
	}
	c := metricsazure.NewClient(newTestLogger(), instances, clients)

	written := 0
	set := func(graph.Ref, string, graph.MetricValue) { written++ }

	req := metrics.Request{Keys: []string{metrics.KeyCPUUtilizationP95}, WindowDays: 14}
	err := c.Fill(context.Background(), req, set)
	if err == nil {
		t.Fatal("Fill must report the poisoned job's failure")
	}
	if written != 1 {
		t.Fatalf("healthy VM metrics written = %d, want 1 (poisoned sibling must not cancel it)", written)
	}
}
