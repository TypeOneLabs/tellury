package azure

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/monitor/armmonitor"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	metricsazure "github.com/TypeOneLabs/tellury/pkg/metrics/azure"
)

type fakeAzureMonitor struct {
	mu       sync.Mutex
	response armmonitor.MetricsClientListResponse
	calls    int
}

func (f *fakeAzureMonitor) List(_ context.Context, _ string, _ *armmonitor.MetricsClientListOptions) (armmonitor.MetricsClientListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.response, nil
}

func (f *fakeAzureMonitor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestEnrichMetrics_WiresAzureMonitorPerSubscription(t *testing.T) {
	p, err := New(context.Background(), WithOffline(), WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	fake := &fakeAzureMonitor{
		response: armmonitor.MetricsClientListResponse{
			Response: armmonitor.Response{
				Value: []*armmonitor.Metric{
					{
						Name: &armmonitor.LocalizableString{Value: to.Ptr("Percentage CPU")},
						Timeseries: []*armmonitor.TimeSeriesElement{
							{Data: []*armmonitor.MetricValue{{TimeStamp: &now, Average: to.Ptr(40.0)}}},
						},
					},
				},
			},
		},
	}
	p.metricsFactory = func(_ context.Context, subscriptionID string) (metricsazure.MetricsAPI, error) {
		if subscriptionID != "sub-vm" {
			t.Fatalf("metrics factory called with subscription %q, want sub-vm", subscriptionID)
		}
		return fake, nil
	}

	g := graph.New()
	vmID := graph.Ref("/subscriptions/sub-vm/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-a")
	vm := &graph.Node{
		ID:        vmID,
		Kind:      graph.KindInstance,
		Provider:  "azure",
		Service:   serviceCompute,
		AssetType: TypeVM,
		Project:   "sub-vm",
		Location:  "westeurope",
		Attrs: map[string]any{
			AttrResourceID: string(vmID),
		},
	}
	if err := g.AddNode(vm); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	req := metrics.Request{Keys: []string{metrics.KeyCPUUtilizationP95}, WindowDays: 7}
	if err := p.EnrichMetrics(context.Background(), g, cloud.Scope{}, req); err != nil {
		t.Fatalf("EnrichMetrics: %v", err)
	}

	if fake.callCount() != 1 {
		t.Fatalf("Azure Monitor List called %d times, want 1", fake.callCount())
	}

	mv, ok := vm.Metric(metrics.KeyCPUUtilizationP95)
	if !ok {
		t.Fatal("cpu_utilization_p95 was not written to the VM node")
	}
	// Raw 40% -> ScaleDown /100 -> 0.40.
	if mv.Value != 0.40 {
		t.Fatalf("cpu_utilization_p95 = %v, want 0.40", mv.Value)
	}
}
