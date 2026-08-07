package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	metricsgcp "github.com/TypeOneLabs/tellury/pkg/metrics/gcp"
	// The service subpackages register their metric specs from init(). Without these blank
	// imports the registry is empty, every metric key is reported unsupported, and every
	// metric-dependent rule silently skips — with a green build, vet and test.
	_ "github.com/TypeOneLabs/tellury/pkg/metrics/gcp/compute"
	_ "github.com/TypeOneLabs/tellury/pkg/metrics/gcp/storage"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	pricinggcp "github.com/TypeOneLabs/tellury/pkg/pricing/gcp"
)

// Provider is the GCP implementation of cloud.Provider.
type Provider struct {
	lister       AssetLister
	metrics      metrics.Provider
	metricsClose func() error
	pricer       pricing.Pricer
	pricerClose  func() error
	sizer        pricing.Sizer
	log          *slog.Logger

	// offline marks a provider built for a scan that never needs cloud access
	// (a --fixture replay or a --cache-file hit). New skips constructing every
	// cloud SDK client when this is set, so an offline scan runs on a host
	// with no Application Default Credentials at all.
	offline bool

	// joinIndex maps a Monitoring resource-label value (instance_id or
	// bucket_name) to the graph ref that owns it. Built during Ingest, which is
	// the only pass that sees both.
	mu        sync.RWMutex
	joinIndex map[string]map[string]graph.Ref
}

var _ cloud.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithLister overrides the asset source (used by tests and --fixture).
func WithLister(l AssetLister) Option { return func(p *Provider) { p.lister = l } }

// WithMetricsProvider overrides the enrichment source.
func WithMetricsProvider(m metrics.Provider) Option { return func(p *Provider) { p.metrics = m } }

// WithPricer overrides the cost model (used by tests). Bypasses the default
// live-catalog-with-embedded-fallback pricer entirely.
func WithPricer(pr pricing.Pricer) Option { return func(p *Provider) { p.pricer = pr } }

// WithLogger sets the provider logger.
func WithLogger(l *slog.Logger) Option { return func(p *Provider) { p.log = l } }

// WithOffline builds a provider that never constructs any cloud SDK client:
// no Cloud Asset Inventory, no Cloud Monitoring, no Cloud Billing. It is for
// scans whose graph comes from local data (a --fixture or a --cache-file
// replay): the lister — if any — must be an offline source (FixtureLister),
// enrichment is a no-op (p.metrics stays nil, which EnrichMetrics already
// short-circuits on), and pricing uses the embedded static table only. This
// is what lets an offline scan run on a host with no credentials.
func WithOffline() Option { return func(p *Provider) { p.offline = true } }

// New builds a GCP provider. Defaults: the live CAI client, the live Cloud
// Monitoring client (pkg/metrics/gcp), the live Cloud Billing Catalog pricer
// (itself backed by the embedded price table as a fallback when the API is
// unreachable or the caller lacks billing permission), and the embedded
// machine catalog.
//
// With WithOffline (see runScan: a --fixture/--cache-file offline scan) none
// of the cloud clients are constructed at all, so New never touches ADC.
// Without it, constructing the Monitoring client is non-fatal: an operand
// that cannot build the Monitoring client (e.g. this is a billing-only or
// fixture-flavoured host) gets a warning and a nil p.metrics — EnrichMetrics
// pays that back as "no metric enrichment", and metric-dependent rules skip
// (invariant I5). Building the CAI client, however, remains fatal: a scan
// that must ingest the live graph has no way to proceed without it.
func New(ctx context.Context, opts ...Option) (*Provider, error) {
	sizer, err := pricinggcp.NewMachineCatalog()
	if err != nil {
		return nil, err
	}
	p := &Provider{
		sizer:     sizer,
		log:       slog.Default(),
		joinIndex: map[string]map[string]graph.Ref{},
	}
	for _, opt := range opts {
		opt(p)
	}
	if !p.offline && p.lister == nil {
		c, err := NewCAIClient(ctx, p.log)
		if err != nil {
			return nil, err
		}
		p.lister = c
	}
	if !p.offline && p.metrics == nil {
		c, err := metricsgcp.NewClient(ctx, p.log, p.lookupRef)
		if err != nil {
			// Non-fatal, mirroring NewCatalogPricer's tolerance: an operator
			// legitimately may have no Monitoring permission, or no ADC at all
			// on an offline/partial host. Leave p.metrics nil — EnrichMetrics
			// short-circuits and metric-dependent rules skip rather than guess.
			p.log.Warn("gcp: Cloud Monitoring client unavailable; metric-dependent rules will skip", "err", err)
		} else {
			p.metrics = c
			p.metricsClose = c.Close
		}
	}
	if p.pricer == nil {
		if p.offline {
			// No Cloud Billing client either: the embedded table is enough for
			// an offline replay.
			static, err := pricinggcp.NewStaticPricer()
			if err != nil {
				return nil, err
			}
			p.pricer = static
		} else {
			c, err := pricinggcp.NewCatalogPricer(ctx, p.log)
			if err != nil {
				return nil, err
			}
			p.pricer = c
			p.pricerClose = c.Close
		}
	}
	return p, nil
}

// Name implements cloud.Provider.
func (p *Provider) Name() string { return "gcp" }

// Pricer implements cloud.Provider.
func (p *Provider) Pricer() pricing.Pricer { return p.pricer }

// Sizer exposes the machine catalog so the CLI can put it on the rules Pass.
func (p *Provider) Sizer() pricing.Sizer { return p.sizer }

// Close releases the underlying clients.
func (p *Provider) Close() error {
	var errs []error
	if p.lister != nil {
		if err := p.lister.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if p.metricsClose != nil {
		if err := p.metricsClose(); err != nil {
			errs = append(errs, err)
		}
	}
	if p.pricerClose != nil {
		if err := p.pricerClose(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("gcp: close: %v", errs)
}

// Ingest performs one pass over Cloud Asset Inventory and returns a frozen
// graph. Edges are de-duplicated across both emission directions, so a disk
// attached to an out-of-scope instance is still linked.
//
// The graph leaves hold exactly the resources normalization recognizes; the
// SearchAllResources result's hierarchy fields (project / folders /
// organization) are turned into container nodes (KindProject / KindFolder /
// KindOrganization) plus containment edges, so a finding can be attributed to
// a folder or rolled up beyond a single project. Containers are not waste and
// never reach rule evaluation (they are structurally excluded by
// graph.ByKind) and never inflate the scan's "N resources" count
// (graph.ResourceNodeCount).
//
// The scope is validated (exactly one of project/folder/organization) ONLY on
// the live path. An offline provider (built with WithOffline for a --fixture
// or --cache-file replay) is exempt: its scope comes from the local input, so
// the CLI's offline exemption in config.Validate must not be re-contradicted
// here. The live path keeps the strict contract — an ingestion that must
// address Cloud Asset Inventory has no way to do so without a parent scope.
func (p *Provider) Ingest(ctx context.Context, sc cloud.Scope, assetTypeHints []string) (*graph.Graph, error) {
	if !p.offline {
		if err := sc.Validate(); err != nil {
			return nil, err
		}
	}
	if p.lister == nil {
		return nil, fmt.Errorf("gcp: no asset lister; this offline provider cannot ingest live assets")
	}
	types := assetTypeHints
	if len(types) == 0 {
		types = SupportedAssetTypes
	}

	g := graph.New()
	edges := make(map[graph.Edge]struct{}, 1024)
	joins := map[string]map[string]graph.Ref{
		metricsgcp.ResourceGCEInstance: {},
		metricsgcp.ResourceGCSBucket:   {},
	}
	seen, kept := 0, 0

	// Container nodes are one per distinct project/folder/organization token,
	// not one per leaf resource. addHierarchyNode dedupes via the graph's own
	// AddNode (last write wins), so a token seen on many resources resolves to
	// a single node.
	addHierarchyNode := func(n *graph.Node) {
		if err := g.AddNode(n); err != nil {
			// AddNode only fails before Freeze with an empty/nil ID, which can
			// never happen for a hierarchy node (its token is non-empty by
			// construction) or for a nil node. Treat any such error as fatal:
			// a malformed hierarchy node would silently corrupt containment.
			p.log.Error("gcp: add hierarchy node failed", "id", n.ID, "err", err)
		}
	}
	emitHierarchyEdge := func(e graph.Edge) { edges[e] = struct{}{} }

	err := p.lister.ListAssets(ctx, ListRequest{
		Parent:     sc.Parent(),
		AssetTypes: types,
		PageSize:   1000,
	}, func(a *RawAsset) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		seen++

		n, err := Normalize(a, p.sizer)
		if err != nil {
			return fmt.Errorf("gcp: normalize %s: %w", a.Name, err)
		}
		if n != nil {
			if err := g.AddNode(n); err != nil {
				return err
			}
			kept++
			indexJoinKeys(joins, n)
		}
		// Build the hierarchy container nodes and edges for this asset.
		// buildHierarchy internally skips the whole pass when Normalize
		// returned nil (an unmapped asset type), so a project whose only
		// assets are unmodelled types never gets container nodes with no
		// resources under them.
		buildHierarchy(a, n, addHierarchyNode, emitHierarchyEdge)
		return Link(a, func(e graph.Edge) { edges[e] = struct{}{} })
	})
	if err != nil {
		return nil, err
	}

	for e := range edges {
		if err := g.AddEdge(e); err != nil {
			return nil, err
		}
	}
	g.Freeze()

	p.mu.Lock()
	p.joinIndex = joins
	p.mu.Unlock()

	p.log.Info("ingest complete",
		"scope", sc.Parent(),
		"assets_seen", seen,
		"nodes", kept,
		"resource_nodes", g.ResourceNodeCount(),
		"container_nodes", g.NodeCount()-g.ResourceNodeCount(),
		"edges", g.EdgeCount(),
		"dangling_edges", g.DanglingEdges())
	return g, nil
}

// indexJoinKeys records the Monitoring join keys a node exposes.
func indexJoinKeys(joins map[string]map[string]graph.Ref, n *graph.Node) {
	switch n.Kind {
	case graph.KindInstance:
		if id, ok := n.Str(AttrInstanceID); ok && id != "" {
			joins[metricsgcp.ResourceGCEInstance][id] = n.ID
		}
	case graph.KindBucket:
		if name, ok := n.Str(AttrBucketName); ok && name != "" {
			joins[metricsgcp.ResourceGCSBucket][name] = n.ID
		}
	}
}

// lookupRef resolves a monitored-resource label value back to a graph ref.
// Returns "" for out-of-scope resources, which the enrichment pass skips.
func (p *Provider) lookupRef(resourceType, joinValue string) (graph.Ref, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ref, ok := p.joinIndex[resourceType][joinValue]
	return ref, ok
}

// EnrichMetrics batch-fills node metrics for the requested keys. Keys the
// provider does not support are silently dropped, per the cloud.Provider
// contract.
//
// Cloud Monitoring's ListTimeSeries is always project-scoped — unlike Cloud
// Asset Inventory it has no native folder/organization aggregation — so this
// resolves the distinct project IDs actually present in the ingested graph
// (rather than trusting the scan scope, which may itself be a folder or
// organization) and hands that set to the provider.
func (p *Provider) EnrichMetrics(ctx context.Context, g *graph.Graph, sc cloud.Scope, req metrics.Request) error {
	if len(req.Keys) == 0 || p.metrics == nil {
		return nil
	}
	supported := make([]string, 0, len(req.Keys))
	for _, k := range req.Keys {
		if p.metrics.Supports(k) {
			supported = append(supported, k)
			continue
		}
		p.log.Debug("metric key unsupported by provider, skipping", "key", k)
	}
	if len(supported) == 0 {
		return nil
	}

	projects := distinctProjects(g)
	if len(projects) == 0 {
		p.log.Debug("no projects found in graph; skipping metric enrichment")
		return nil
	}

	sub := metrics.Request{Keys: supported, WindowDays: req.WindowDays, Projects: projects}
	return p.metrics.Fill(ctx, sub, func(id graph.Ref, key string, v graph.MetricValue) {
		g.SetMetric(id, key, v)
	})
}

// distinctProjects returns the sorted, de-duplicated set of project IDs
// present on resource nodes in g. Container project nodes are excluded: they
// are computed scaffolding (each project appears once even when no resource
// normalized) and would otherwise list projects with no billable resources.
func distinctProjects(g *graph.Graph) []string {
	seen := map[string]bool{}
	var out []string
	g.Nodes(func(n *graph.Node) bool {
		if n.Container() {
			return true
		}
		if n.Project != "" && !seen[n.Project] {
			seen[n.Project] = true
			out = append(out, n.Project)
		}
		return true
	})
	return out
}
