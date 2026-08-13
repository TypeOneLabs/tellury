package azure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/managementgroups/armmanagementgroups"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resourcegraph/armresourcegraph"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	pricingazure "github.com/TypeOneLabs/tellury/pkg/pricing/azure"
)

// managementGroupsAPI is the subset of the management-groups API this provider
// calls. It exists so the offline fixture can stand in for the live SDK client
// and replay the same recursive Get calls with no credentials.
type managementGroupsAPI interface {
	Get(ctx context.Context, groupID string, options *armmanagementgroups.ClientGetOptions) (armmanagementgroups.ClientGetResponse, error)
}

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

	pricer pricing.Pricer

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

// New builds an Azure provider. Unless WithOffline is set, it constructs
// azidentity.DefaultAzureCredential and the Resource Graph and
// management-groups SDK clients from it. There is deliberately no custom
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
// fixture's fakes stand in for every Resource Graph and management-groups call.
func New(ctx context.Context, opts ...Option) (*Provider, error) {
	p := &Provider{log: slog.Default()}
	for _, opt := range opts {
		opt(p)
	}

	if p.offline {
		if p.fixture != nil {
			p.argClient = &fakeResourceGraph{f: p.fixture}
			p.mgClient = &fakeManagementGroups{f: p.fixture}
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
	}

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

// Sizer implements cloud.Provider. Azure has no rightsizing catalog yet.
func (p *Provider) Sizer() pricing.Sizer { return nil }

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
		if err := p.addResourceRows(g, edges, scope.Subscription, rows); err != nil {
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
		if err := p.ingestSubscriptions(ctx, g, edges, subs); err != nil {
			return nil, err
		}

	case scope.ManagementGroup != "":
		subs := map[string]bool{}
		visited := map[string]bool{}
		if err := p.walkManagementGroup(ctx, g, edges, scope.ManagementGroup, "", subs, visited); err != nil {
			return nil, err
		}
		if err := p.ingestSubscriptions(ctx, g, edges, subs); err != nil {
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
func (p *Provider) ingestSubscriptions(ctx context.Context, g *graph.Graph, edges map[graph.Edge]struct{}, subs map[string]bool) error {
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

		if err := p.addResourceRows(g, edges, subID, rows); err != nil {
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

// addResourceRows normalizes ARG rows into graph nodes and wires the
// resource -> region -> subscription containment chain. Rows of an unmodelled
// type are ignored (the query asks only for modelled types, but a fixture may
// be edited to include more).
func (p *Provider) addResourceRows(g *graph.Graph, edges map[graph.Edge]struct{}, subscriptionID string, rows []map[string]any) error {
	for _, row := range rows {
		n := NormalizeResource(row)
		if n == nil {
			continue
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

// EnrichMetrics implements cloud.Provider. Azure has no metric client in this
// batch: an empty request is a no-op, and a request for metric keys returns an
// explicit error so the scan's "could not check" summary is truthful instead of
// silently producing metric-less skips.
func (p *Provider) EnrichMetrics(_ context.Context, _ *graph.Graph, _ cloud.Scope, req metrics.Request) error {
	if len(req.Keys) == 0 {
		return nil
	}
	return fmt.Errorf("azure: metrics not implemented")
}
