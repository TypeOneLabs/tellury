package azure

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resourcegraph/armresourcegraph"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func loadTestFixture(t *testing.T) *Fixture {
	t.Helper()
	f, err := LoadFixture(filepath.Join("testdata", "azure-fixture.json"))
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	return f
}

type fakeARG struct {
	rows map[string][]map[string]any
	errs map[string]error
}

func (f *fakeARG) Resources(_ context.Context, query armresourcegraph.QueryRequest, _ *armresourcegraph.ClientResourcesOptions) (armresourcegraph.ClientResourcesResponse, error) {
	resp := armresourcegraph.ClientResourcesResponse{
		QueryResponse: armresourcegraph.QueryResponse{
			Data:            []map[string]any{},
			Count:           ptrInt64(0),
			TotalRecords:    ptrInt64(0),
			ResultTruncated: ptrResultTruncated(armresourcegraph.ResultTruncatedFalse),
		},
	}
	subID := ""
	if len(query.Subscriptions) > 0 && query.Subscriptions[0] != nil {
		subID = *query.Subscriptions[0]
	}
	if err := f.errs[subID]; err != nil {
		return resp, err
	}
	rows := f.rows[subID]
	count := int64(len(rows))
	resp.Count = ptrInt64(count)
	resp.TotalRecords = ptrInt64(count)
	resp.Data = rows
	return resp, nil
}

func ptrInt64(v int64) *int64 { return &v }

func ptrResultTruncated(v armresourcegraph.ResultTruncated) *armresourcegraph.ResultTruncated {
	return &v
}

func hasEdge(edges []graph.Edge, want graph.Edge) bool {
	for _, e := range edges {
		if e == want {
			return true
		}
	}
	return false
}

func TestNew_OfflineConstructsZeroSDKClients(t *testing.T) {
	p, err := New(context.Background(), WithOffline(), WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("offline azure.New must succeed with no credentials: %v", err)
	}
	defer p.Close()
	if p.credential != nil {
		t.Error("offline provider must not construct a credential")
	}
	if p.argClient != nil {
		t.Error("offline provider with no fixture must not construct a resource graph client")
	}
	if p.mgClient != nil {
		t.Error("offline provider with no fixture must not construct a management groups client")
	}
}

func TestIngest_TenantFixtureBuildsHierarchy(t *testing.T) {
	p, err := New(context.Background(),
		WithOffline(),
		WithFixture(loadTestFixture(t)),
		WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	sc := cloud.Scope{Provider: "azure", Azure: &cloud.AzureScope{Tenant: "tenant-aaaa-1111"}}
	gr, err := p.Ingest(context.Background(), sc, nil)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if got := gr.CountByKind(graph.KindOrganization); got != 1 {
		t.Errorf("organization nodes = %d, want 1", got)
	}
	if got := gr.CountByKind(graph.KindFolder); got != 2 {
		t.Errorf("management group nodes = %d, want 2", got)
	}
	if got := gr.CountByKind(graph.KindSubscription); got != 2 {
		t.Errorf("subscription nodes = %d, want 2", got)
	}
	if got := gr.CountByKind(graph.KindRegion); got != 3 {
		t.Errorf("region nodes = %d, want 3 (sub-1/westeurope, sub-1/eastus, sub-2/westeurope)", got)
	}
	if got := gr.CountByKind(graph.KindDisk); got != 2 {
		t.Errorf("disk nodes = %d, want 2", got)
	}
	if got := gr.CountByKind(graph.KindAddress); got != 1 {
		t.Errorf("address nodes = %d, want 1", got)
	}
	if got := gr.ResourceNodeCount(); got != 3 {
		t.Errorf("ResourceNodeCount = %d, want 3", got)
	}
	if got := gr.SubscriptionContainerCount(); got != 2 {
		t.Errorf("SubscriptionContainerCount = %d, want 2", got)
	}

	// Example node IDs at each tier.
	for _, id := range []graph.Ref{
		"tenants/tenant-aaaa-1111",
		"management-groups/tenant-aaaa-1111",
		"management-groups/mg-child",
		"subscriptions/sub-1",
		"subscriptions/sub-2",
		"subscriptions/sub-1/regions/westeurope",
		"subscriptions/sub-1/regions/eastus",
		"subscriptions/sub-2/regions/westeurope",
	} {
		if _, ok := gr.Node(id); !ok {
			t.Errorf("expected node %s is missing", id)
		}
	}

	if n, ok := gr.Node("subscriptions/sub-1/regions/westeurope"); !ok || n.Name != "westeurope" {
		t.Errorf("sub-1 westeurope region node = %#v, want Name westeurope", n)
	}

	// Resource group is an attribute, not a tier.
	diskID := graph.Ref("/subscriptions/sub-1/resourceGroups/rg-a/providers/Microsoft.Compute/disks/disk-a")
	n, ok := gr.Node(diskID)
	if !ok {
		t.Fatalf("disk node %s missing", diskID)
	}
	if got, _ := n.Str(AttrResourceGroup); got != "rg-a" {
		t.Errorf("disk resource_group = %q, want rg-a", got)
	}

	// Containment edges: contained -> container.
	if got := gr.EdgeCount(); got != 10 {
		t.Errorf("EdgeCount = %d, want 10", got)
	}
	if !hasEdge(gr.Out("management-groups/tenant-aaaa-1111"), graph.Edge{From: "management-groups/tenant-aaaa-1111", To: "tenants/tenant-aaaa-1111", Kind: graph.EdgeContains}) {
		t.Error("root management group is not contained by the tenant")
	}
	if !hasEdge(gr.Out("subscriptions/sub-1"), graph.Edge{From: "subscriptions/sub-1", To: "management-groups/mg-child", Kind: graph.EdgeContains}) {
		t.Error("subscription sub-1 is not contained by its management group")
	}

	statuses := p.SubscriptionStatuses()
	if len(statuses) != 2 {
		t.Fatalf("SubscriptionStatuses = %d, want 2", len(statuses))
	}
	if statuses[0].ID != "sub-1" || statuses[0].Status != "scanned" {
		t.Errorf("statuses[0] = %#v, want sub-1 scanned", statuses[0])
	}
	if statuses[1].ID != "sub-2" || statuses[1].Status != "scanned" {
		t.Errorf("statuses[1] = %#v, want sub-2 scanned", statuses[1])
	}
}

func TestIngest_SubscriptionResourceGroupFilter(t *testing.T) {
	p, err := New(context.Background(),
		WithOffline(),
		WithFixture(loadTestFixture(t)),
		WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	sc := cloud.Scope{Provider: "azure", Azure: &cloud.AzureScope{
		Subscription:  "sub-1",
		ResourceGroup: "rg-b",
	}}
	gr, err := p.Ingest(context.Background(), sc, nil)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if got := gr.CountByKind(graph.KindDisk); got != 0 {
		t.Errorf("disk nodes = %d, want 0 (rg-b has only a public IP)", got)
	}
	if got := gr.CountByKind(graph.KindAddress); got != 1 {
		t.Errorf("address nodes = %d, want 1 (rg-b public IP)", got)
	}
	if got := gr.ResourceNodeCount(); got != 1 {
		t.Errorf("ResourceNodeCount = %d, want 1", got)
	}
	if n, ok := gr.Node("/subscriptions/sub-1/resourceGroups/rg-b/providers/Microsoft.Network/publicIPAddresses/pip-a"); !ok {
		t.Error("rg-b public IP node missing")
	} else if got, _ := n.Str(AttrResourceGroup); got != "rg-b" {
		t.Errorf("resource_group = %q, want rg-b", got)
	}
}

func TestIngest_TenantReportsUnreachableSubscription(t *testing.T) {
	fx := loadTestFixture(t)
	p, err := New(context.Background(),
		WithOffline(),
		WithFixture(fx),
		WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	p.argClient = &fakeARG{
		rows: map[string][]map[string]any{
			"sub-1": fx.Subscriptions["sub-1"].Resources,
		},
		errs: map[string]error{
			"sub-2": &azcore.ResponseError{StatusCode: 403, ErrorCode: "AuthorizationFailed"},
		},
	}

	sc := cloud.Scope{Provider: "azure", Azure: &cloud.AzureScope{Tenant: "tenant-aaaa-1111"}}
	gr, err := p.Ingest(context.Background(), sc, nil)
	if err != nil {
		t.Fatalf("Ingest must continue after an unreachable subscription: %v", err)
	}

	if got := gr.CountByKind(graph.KindSubscription); got != 2 {
		t.Errorf("subscription nodes = %d, want 2 (hierarchy includes unreachable sub)", got)
	}
	if got := gr.ResourceNodeCount(); got != 2 {
		t.Errorf("ResourceNodeCount = %d, want 2 (only sub-1 resources)", got)
	}

	statuses := p.SubscriptionStatuses()
	if len(statuses) != 2 {
		t.Fatalf("SubscriptionStatuses = %d, want 2", len(statuses))
	}
	if statuses[0].ID != "sub-1" || statuses[0].Status != "scanned" {
		t.Errorf("statuses[0] = %#v, want sub-1 scanned", statuses[0])
	}
	if statuses[1].ID != "sub-2" || statuses[1].Status != "unreachable" {
		t.Errorf("statuses[1] = %#v, want sub-2 unreachable", statuses[1])
	}
	if statuses[1].Reason == "" {
		t.Error("unreachable subscription must carry a reason")
	}
}
