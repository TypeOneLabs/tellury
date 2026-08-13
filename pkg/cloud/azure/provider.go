package azure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/managementgroups/armmanagementgroups"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/monitor/armmonitor"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resourcegraph/armresourcegraph"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	metricsazure "github.com/TypeOneLabs/tellury/pkg/metrics/azure"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	pricingazure "github.com/TypeOneLabs/tellury/pkg/pricing/azure"

	// Ensure the Azure compute metric specs are registered so that
	// Supports()/SpecOf() returns true for cpu_utilization_p95 (and the keyed
	// memory metric) during EnrichMetrics.
	_ "github.com/TypeOneLabs/tellury/pkg/metrics/azure/compute"
)

// managementGroupsAPI is the subset of the management-groups API this provider
// calls. It exists so the offline fixture can stand in for the live SDK client
// and replay the same recursive Get calls with no credentials.
type managementGroupsAPI interface {
	Get(ctx context.Context, groupID string, options *armmanagementgroups.ClientGetOptions) (armmanagementgroups.ClientGetResponse, error)
}

// metricsAPIFactory constructs an Azure Monitor metrics client for one
// subscription. The live provider constructs an armmonitor.MetricsClient; a
// test can inject a fake to exercise EnrichMetrics without credentials or
// network.
type metricsAPIFactory func(ctx context.Context, subscriptionID string) (metricsazure.MetricsAPI, error)

// SubscriptionStatus records the outcome of attempting to scan one Azure
// subscription. It is the Azure analog of aws.AccountStatus: a
// management-group or tenant scan reports the subscriptions it found and which
// of them were actually scanned, so a total is never silently accepted when
// some subscriptions were unreachable.
type SubscriptionStatus struct {
	ID     string `json:"subscription_id"`
	Status string `json:"status"` // "scanned", "unreachable", "no_resources"
	Reason string `json:"reason,omitempty"`
}

// Provider is the Azure implementation of cloud.Provider. It ingests a single
// subscription (--azure-subscription, optionally filtered by
// --azure-resource-group) or walks a management-group/tenant hierarchy and
// queries Azure Resource Graph once per subscription found. A subscription
// whose ARG query fails with authorization is reported, not swallowed.
type Provider struct {
	log *slog.Logger

	offline bool
	fixture *Fixture

	credential azcore.TokenCredential
	argClient  resourceGraphAPI
	mgClient   managementGroupsAPI

	// resourceSKUs answers "what VM sizes exist and what are their shapes",
	// which is what lets a rule recommend a smaller size rather than only
	// stop/delete. Populated during Ingest from the Resource SKUs API.
	resourceSKUs resourceSKUsAPI

	// metricsFactory constructs Azure Monitor clients per subscription during
	// EnrichMetrics. It is nil on the offline path (and when a test has not
	// injected one), which EnrichMetrics treats as "metric enrichment
	// unavailable" rather than a fatal error.
	metricsFactory metricsAPIFactory

	pricer pricing.Pricer

	// sizer answers "what else exists in this VM's family". It is populated
	// during Ingest from the Resource SKUs API, and returned by Sizer.
	sizer *Sizer

	// subscriptionStatuses records the outcome for every subscription found in
	// the most recent tenant/management-group walk (scanned, unreachable,
	// no_resources). A single-subscription scan leaves it nil.
	subscriptionStatuses []SubscriptionStatus
}

var _ cloud.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithLogger sets the provider logger.
func WithLogger(l *slog.Logger) Option { return func(p *Provider) { p.log = l } }

// WithOffline builds a provider that never constructs an Azure SDK client and
// never resolves credentials. It is for scans whose data comes from local
// fixtures, so an offline Azure scan runs on a host with no Azure credentials.
func WithOffline() Option { return func(p *Provider) { p.offline = true } }

// WithFixture supplies the offline data source (LoadFixture's result).
func WithFixture(f *Fixture) Option { return func(p *Provider) { p.fixture = f } }

// WithPricer overrides the cost model (used by tests).
func WithPricer(pr pricing.Pricer) Option { return func(p *Provider) { p.pricer = pr } }

// WithMetricsAPIFactory overrides the Azure Monitor client factory used by
// EnrichMetrics. It is used by tests to inject a fake per-subscription
// MetricsAPI.
func WithMetricsAPIFactory(f metricsAPIFactory) Option {
	return func(p *Provider) { p.metricsFactory = f }
}

// New builds an Azure provider. Unless WithOffline is set, it constructs
// azidentity.DefaultAzureCredential and the Resource Graph, management-groups
// and Resource SKUs SDK clients from it. There is deliberately no custom
// credential resolution and no credentials flag: tellury reads no key files,
// and the Azure credential chain (az login, managed identity, workload
// identity/federation, service-principal environment variables) is the Azure
// counterpart of GCP Application Default Credentials and the AWS default
// chain.
//
// Pricing uses the public, unauthenticated Retail Prices API (cached lazily
// for the scan's lifetime) as the ONLY source. There is no embedded fallback
// table; an unresolvable price makes the rule skip. When TELLURY_PRICE_FIXTURE
// is set, the offline path loads the recorded Retail Prices response file.
//
// WithOffline constructs zero SDK clients and resolves zero credentials; the
// fixture's fakes stand in for every Resource Graph, management-groups and
// Resource SKUs call.
func New(ctx context.Context, opts ...Option) (*Provider, error) {
	p := &Provider{log: slog.Default()}
	for _, opt := range opts {
		opt(p)
	}

	if p.offline {
		if p.fixture != nil {
			p.argClient = &fakeResourceGraph{f: p.fixture}
			p.mgClient = &fakeManagementGroups{f: p.fixture}
			p.resourceSKUs = &fakeResourceSKUs{f: p.fixture}
		}
	} else {
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("azure: default credential: %w", err)
		}
		p.credential = cred

		argClient, err := armresourcegraph.NewClient(cred, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: resource graph client: %w", err)
		}
		p.argClient = argClient

		mgClient, err := armmanagementgroups.NewClient(cred, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: management groups client: %w", err)
		}
		p.mgClient = mgClient

		p.resourceSKUs = &resourceSKUsClient{credential: cred}
		p.metricsFactory = func(ctx context.Context, subscriptionID string) (metricsazure.MetricsAPI, error) {
			return armmonitor.NewMetricsClient(subscriptionID, p.credential, nil)
		}
	}

	p.sizer = NewSizer(p.resourceSKUs)

	if p.pricer == nil {
		if p.offline {
			p.pricer = offlinePricer(p.log)
		} else {
			cat, err := pricingazure.NewCatalogPricer(ctx, p.log)
			if err != nil {
				return nil, err
			}
			p.pricer = cat
		}
	}

	return p, nil
}

// offlinePricer builds a pricer for an offline Azure scan. When
// TELLURY_PRICE_FIXTURE is set, it loads the recorded Retail Prices API
// response file; otherwise it returns a NoPricePricer — every resource
// requiring a price skips rather than guess.
func offlinePricer(log *slog.Logger) pricing.Pricer {
	if path := os.Getenv("TELLURY_PRICE_FIXTURE"); path != "" {
		static, err := pricingazure.NewStaticPricerFromFile(path)
		if err != nil {
			log.Warn("azure: TELLURY_PRICE_FIXTURE set but could not load; resources requiring prices will skip",
				"path", path, "err", err)
			return pricing.NoPricePricer{}
		}
		log.Debug("azure: offline pricer loaded from TELLURY_PRICE_FIXTURE", "path", path)
		return static
	}
	log.Debug("azure: no price source available; resources requiring prices will skip")
	return pricing.NoPricePricer{}
}

// Name implements cloud.Provider.
func (p *Provider) Name() string { return ProviderName }

// Pricer implements cloud.Provider.
func (p *Provider) Pricer() pricing.Pricer { return p.pricer }

// Sizer implements cloud.Provider. It returns the Azure Resource SKUs sizer
// that rules use to price shape alternatives in a VM's own region.
func (p *Provider) Sizer() pricing.Sizer {
	if p.sizer == nil {
		return nil
	}
	return p.sizer
}

// Close implements cloud.Provider. Azure SDK clients hold no long-lived
// resources that need explicit release.
func (p *Provider) Close() error { return nil }

// SubscriptionStatuses returns the per-subscription outcomes from the most
// recent tenant/management-group ingestion. A single-subscription scan returns
// nil, mirroring the AWS provider's single-account AccountStatuses behavior.
func (p *Provider) SubscriptionStatuses() []SubscriptionStatus {
	return append([]SubscriptionStatus(nil), p.subscriptionStatuses...)
}

// Ingest performs ingestion for the given Azure scope.
//
// For --azure-subscription (optionally with --azure-resource-group) it queries
// Resource Graph once for that subscription. For --azure-tenant or
// --azure-management-group it walks the management-groups API, builds the
// container hierarchy, and queries Resource Graph once per subscription found.
//
// Graph layout:
//
//	resource -> region -> subscription -> management group -> tenant
//
// Every containment edge is contained -> container (resource points at its
// region, region at its subscription, etc.). The resource group is NOT a graph
// tier; it is the `resource_group` attribute on each resource node.
func (p *Provider) Ingest(ctx context.Context, sc cloud.Scope, assetTypeHints []string) (*graph.Graph, error) {
	if sc.Azure == nil {
		return nil, fmt.Errorf("azure: scope requires an Azure scope block")
	}

	p.subscriptionStatuses = nil

	g := graph.New()
	edges := make(map[graph.Edge]struct{}, 1024)

	addNode := func(n *graph.Node) error { return g.AddNode(n) }

	scope := *sc.Azure

	switch {
	case scope.Subscription != "":
		// Single-subscription path. No management-group walk; a denied ARG
		// query is fatal because a single-subscription scan cannot be partial.
		if err := addNode(subscriptionNode(scope.Subscription, "")); err != nil {
			return nil, err
		}
		rows, err := p.querySubscription(ctx, scope.Subscription, scope.ResourceGroup)
		if err != nil {
			return nil, fmt.Errorf("azure: subscription %s: %w", scope.Subscription, err)
		}
		if err := p.addResourceRows(ctx, g, edges, scope.Subscription, rows, assetTypeHints); err != nil {
			return nil, err
		}

	case scope.Tenant != "":
		if err := addNode(organizationNode(scope.Tenant)); err != nil {
			return nil, err
		}
		subs := map[string]bool{}
		visited := map[string]bool{}
		// The tenant root management group ID is the tenant ID.
		if err := p.walkManagementGroup(ctx, g, edges, scope.Tenant, organizationRef(scope.Tenant), subs, visited); err != nil {
			return nil, err
		}
		if err := p.ingestSubscriptions(ctx, g, edges, subs, assetTypeHints); err != nil {
			return nil, err
		}

	case scope.ManagementGroup != "":
		subs := map[string]bool{}
		visited := map[string]bool{}
		if err := p.walkManagementGroup(ctx, g, edges, scope.ManagementGroup, "", subs, visited); err != nil {
			return nil, err
		}
		if err := p.ingestSubscriptions(ctx, g, edges, subs, assetTypeHints); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("azure: scope requires a tenant, management group, or subscription")
	}

	for e := range edges {
		if err := g.AddEdge(e); err != nil {
			return nil, err
		}
	}
	g.Freeze()

	p.log.Info("azure ingest complete",
		"subscriptions", g.SubscriptionContainerCount(),
		"resource_nodes", g.ResourceNodeCount(),
		"container_nodes", g.NodeCount()-g.ResourceNodeCount(),
		"edges", g.EdgeCount(),
		"dangling_edges", g.DanglingEdges())

	return g, nil
}

// ingestSubscriptions queries ARG once per discovered subscription, merges the
// returned resources, and records every subscription outcome. An authorization
// failure on one subscription is recorded as unreachable and the scan
// continues; other query failures are also recorded as unreachable because a
// tenant/management-group total is only acceptable when every omitted
// subscription is named.
func (p *Provider) ingestSubscriptions(ctx context.Context, g *graph.Graph, edges map[graph.Edge]struct{}, subs map[string]bool, assetTypeHints []string) error {
	subscriptionIDs := make([]string, 0, len(subs))
	for subID := range subs {
		subscriptionIDs = append(subscriptionIDs, subID)
	}
	sort.Strings(subscriptionIDs)

	p.subscriptionStatuses = make([]SubscriptionStatus, 0, len(subscriptionIDs))
	for _, subID := range subscriptionIDs {
		rows, err := p.querySubscription(ctx, subID, "")
		if err != nil {
			reason := fmt.Sprintf("Resource Graph query failed: %v", err)
			if isAuthorizationError(err) {
				reason = fmt.Sprintf("not authorized: %v", err)
			}
			p.log.Warn("azure: subscription unreachable, skipping",
				"subscription", subID, "err", err)
			p.subscriptionStatuses = append(p.subscriptionStatuses, SubscriptionStatus{
				ID:     subID,
				Status: "unreachable",
				Reason: reason,
			})
			continue
		}

		if err := p.addResourceRows(ctx, g, edges, subID, rows, assetTypeHints); err != nil {
			return err
		}

		status := "scanned"
		if len(rows) == 0 {
			status = "no_resources"
		}
		p.subscriptionStatuses = append(p.subscriptionStatuses, SubscriptionStatus{
			ID:     subID,
			Status: status,
		})
	}
	return nil
}

// wantsAssetType reports whether assetTypeHints contains assetType. The hints
// come from rules.Plan: a rule set that does not require VMs must not trigger
// a Resource SKUs sweep just because the ARG query also returned VMs.
func wantsAssetType(assetTypeHints []string, assetType string) bool {
	for _, hint := range assetTypeHints {
		if hint == assetType {
			return true
		}
	}
	return false
}

// addResourceRows normalizes ARG rows into graph nodes and wires the
// resource -> region -> subscription containment chain. Rows of an unmodelled
// type are ignored (the query asks only for modelled types, but a fixture may
// be edited to include more).
//
// When VM asset types are wanted, the subscription's Resource SKUs are loaded
// once here (cached in the provider's Sizer) and vcpu_count, memory_gib and
// machine_family are hydrated onto VM nodes. A failed SKU load is logged, not
// fatal: a shape-dependent rule will skip the VM rather than guess.
// vmRegions returns the distinct, canonical regions the VM rows live in,
// sorted. It is the input to the Resource SKUs region filter, and it is a
// named function rather than an inline loop so a test can prove it reads the
// ARG type token ("microsoft.compute/virtualmachines") rather than tellury's
// own asset-type token ("azure.compute.vm"). Comparing against the wrong one
// yields an empty list, which silently falls back to an unfiltered API call
// that returns every SKU in every region — a 52-second discovery instead of a
// 7-second one, with nothing failing.
func vmRegions(rows []map[string]any) []string {
	seen := map[string]bool{}
	var regions []string
	for _, row := range rows {
		if t, _ := row["type"].(string); !strings.EqualFold(t, argTypeVM) {
			continue
		}
		loc, _ := row["location"].(string)
		if loc = locationRegion(loc); loc != "" && !seen[loc] {
			seen[loc] = true
			regions = append(regions, loc)
		}
	}
	sort.Strings(regions)
	return regions
}

func (p *Provider) addResourceRows(ctx context.Context, g *graph.Graph, edges map[graph.Edge]struct{}, subscriptionID string, rows []map[string]any, assetTypeHints []string) error {
	wantVM := wantsAssetType(assetTypeHints, TypeVM)
	if wantVM && p.sizer != nil {
		// No VM rows means no shapes to resolve. Skipping is not just an
		// optimisation: with an empty region list the SKUs call falls back to
		// its unfiltered form, so a scope holding no VMs paid 42 seconds to
		// fetch every SKU in every region and use none of them.
		if regions := vmRegions(rows); len(regions) > 0 {
			if err := p.sizer.LoadSubscription(ctx, subscriptionID, regions); err != nil {
				p.log.Warn("azure: could not resolve VM size shapes; shape-dependent rules will skip VMs",
					"subscription", subscriptionID, "err", err)
			}
		}
	}

	for _, row := range rows {
		n := NormalizeResource(row)
		if n == nil {
			continue
		}

		if n.Kind == graph.KindInstance && wantVM && p.sizer != nil {
			if size, ok := n.Str(AttrVMSize); ok && size != "" {
				if spec, found := p.sizer.Spec(size); found {
					n.SetAttr(AttrVCpuCount, spec.VCPU)
					n.SetAttr(AttrMemoryGiB, spec.MemoryGiB)
					n.SetAttr(AttrMachineFamily, spec.Family)
				}
			}
		}

		if err := g.AddNode(n); err != nil {
			return err
		}
		if n.Location == "" {
			// A resource without a location cannot be placed in the region
			// tier; it is still present as a node, but it has no region
			// containment edge. Resource Graph always returns location for the
			// modelled ARM types, so this is defensive only.
			continue
		}

		rn := regionNode(subscriptionID, n.Location)
		if err := g.AddNode(rn); err != nil {
			return err
		}
		edges[graph.Edge{From: n.ID, To: rn.ID, Kind: graph.EdgeContains}] = struct{}{}
		edges[graph.Edge{From: rn.ID, To: subscriptionRef(subscriptionID), Kind: graph.EdgeContains}] = struct{}{}
	}
	return nil
}

// isAuthorizationError reports whether err is an Azure SDK response error with
// HTTP 401 or 403. It is the single place an unreadable subscription is
// distinguished from a transient or malformed-request failure.
func isAuthorizationError(err error) bool {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == 401 || respErr.StatusCode == 403
	}
	return false
}

// EnrichMetrics implements cloud.Provider. It walks the graph for Azure VM
// nodes, groups them by subscription, constructs an Azure Monitor Metrics
// client per subscription, and delegates to the Azure metrics client for
// per-VM fan-out.
//
// Azure Monitor metrics are queried one resource at a time: each VM is a
// separate MetricsClient.List call. The Azure metrics client bounds the
// concurrency of those calls and reports progress through req.Progress.
//
// Enrichment failure is non-fatal at the caller level (the scan logs and
// continues), and per-job failures inside Fill are also isolated: one failing
// VM request does not cancel healthy siblings.
func (p *Provider) EnrichMetrics(ctx context.Context, g *graph.Graph, _ cloud.Scope, req metrics.Request) error {
	if len(req.Keys) == 0 {
		return nil
	}

	// Filter to keys the Azure backend actually has specs for. Unsupported
	// keys are dropped rather than sent to Azure Monitor, so a declaring rule
	// skips with a reason instead of silently receiving nothing.
	supported := make([]string, 0, len(req.Keys))
	for _, key := range req.Keys {
		if _, ok := metricsazure.SpecOf(key); ok {
			supported = append(supported, key)
			continue
		}
		p.log.Debug("azure: metric key unsupported by provider, skipping", "key", key)
	}
	if len(supported) == 0 {
		return nil
	}

	instances := make(map[string][]metricsazure.InstanceRef)
	g.Nodes(func(n *graph.Node) bool {
		if n.Kind != graph.KindInstance || n.Provider != "azure" || n.AssetType != TypeVM {
			return true
		}
		resourceID, ok := n.Str(AttrResourceID)
		if !ok || resourceID == "" {
			return true
		}
		subscriptionID := n.Project
		if subscriptionID == "" {
			subscriptionID = subscriptionFromID(resourceID)
		}
		if subscriptionID == "" {
			return true
		}
		instances[subscriptionID] = append(instances[subscriptionID], metricsazure.InstanceRef{
			Ref:        n.ID,
			ResourceID: resourceID,
		})
		return true
	})

	if len(instances) == 0 {
		p.log.Debug("azure: no Azure VM nodes found; skipping metric enrichment")
		return nil
	}

	if p.metricsFactory == nil {
		p.log.Debug("azure: Azure Monitor metrics client unavailable; metric-dependent rules will skip",
			"instances", len(instances))
		return nil
	}

	clients := make(map[string]metricsazure.MetricsAPI, len(instances))
	subscriptions := make([]string, 0, len(instances))
	for sub := range instances {
		subscriptions = append(subscriptions, sub)
	}
	sort.Strings(subscriptions)

	for _, sub := range subscriptions {
		api, err := p.metricsFactory(ctx, sub)
		if err != nil {
			p.log.Warn("azure: could not construct Azure Monitor client for subscription; its VMs will skip metric enrichment",
				"subscription", sub, "err", err)
			continue
		}
		clients[sub] = api
	}

	if len(clients) == 0 {
		return fmt.Errorf("azure: no Azure Monitor metrics clients could be constructed for metric enrichment")
	}

	mc := metricsazure.NewClient(p.log, instances, clients)
	subReq := metrics.Request{
		Keys:       supported,
		WindowDays: req.WindowDays,
		Progress:   req.Progress,
	}
	return mc.Fill(ctx, subReq, func(id graph.Ref, key string, v graph.MetricValue) {
		g.SetMetric(id, key, v)
	})
}
