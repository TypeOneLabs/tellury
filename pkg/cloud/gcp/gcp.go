package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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

	// currency is the ISO 4217 code the live pricing catalogue is fetched in
	// ("" = USD). It comes from --currency/TELLURY_CURRENCY (the explicit
	// flag) and is threaded into NewCatalogPricer at construction so every
	// ListSkus request carries it. Best-effort detection of a billing
	// account's currency happens later, in the CLI, after the graph is
	// ingested, and is applied to the pricer via pricing.CurrencySetter.
	currency string

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
// live-catalog pricer entirely.
func WithPricer(pr pricing.Pricer) Option { return func(p *Provider) { p.pricer = pr } }

// WithLogger sets the provider logger.
func WithLogger(l *slog.Logger) Option { return func(p *Provider) { p.log = l } }

// WithCurrency sets the ISO 4217 currency code the live pricing catalogue is
// fetched in. "" (the default) prices the catalogue in USD. Detection of a
// billing account's currency happens later, in the CLI, and is applied to the
// pricer via pricing.CurrencySetter after the graph is ingested; the explicit
// flag value is threaded here at construction so NewCatalogPricer's ListSkus
// requests carry it even before detection runs.
func WithCurrency(code string) Option { return func(p *Provider) { p.currency = code } }

// WithOffline builds a provider that never constructs any cloud SDK client:
// no Cloud Asset Inventory, no Cloud Monitoring, no Cloud Billing. It is for
// scans whose graph comes from local data (a --fixture or a --cache-file
// replay): the lister — if any — must be an offline source (FixtureLister),
// enrichment is a no-op (p.metrics stays nil, which EnrichMetrics already
// short-circuits on), and pricing uses the TELLURY_PRICE_FIXTURE file if set,
// or a NoPricePricer (all resources skip) otherwise. This is what lets an
// offline scan run on a host with no credentials.
func WithOffline() Option { return func(p *Provider) { p.offline = true } }

// New builds a GCP provider. Defaults: the live CAI client, the live Cloud
// Monitoring client (pkg/metrics/gcp), the live Cloud Billing Catalog pricer
// (with no embedded fallback — a missing price makes the rule skip), and the
// embedded machine catalog.
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
			// Non-fatal: an operator legitimately may have no Monitoring
			// permission, or no ADC at all on an offline/partial host. Leave
			// p.metrics nil — EnrichMetrics short-circuits and
			// metric-dependent rules skip rather than guess.
			p.log.Warn("gcp: Cloud Monitoring client unavailable; metric-dependent rules will skip", "err", err)
		} else {
			p.metrics = c
			p.metricsClose = c.Close
		}
	}
	if p.pricer == nil {
		if p.offline {
			// Offline: use TELLURY_PRICE_FIXTURE if set, otherwise no prices.
			p.pricer = offlinePricer(p.log)
		} else {
			c, err := pricinggcp.NewCatalogPricer(ctx, p.log, p.currency)
			if err != nil {
				return nil, err
			}
			p.pricer = c
			p.pricerClose = c.Close
		}
	}
	return p, nil
}

// offlinePricer builds a pricer for an offline scan. When TELLURY_PRICE_FIXTURE
// is set, it loads from that file; otherwise it returns a NoPricePricer —
// every resource requiring a price will skip rather than guess.
func offlinePricer(log *slog.Logger) pricing.Pricer {
	if path := os.Getenv("TELLURY_PRICE_FIXTURE"); path != "" {
		static, err := pricinggcp.NewStaticPricerFromFile(path)
		if err != nil {
			log.Warn("gcp: TELLURY_PRICE_FIXTURE set but could not load; resources requiring prices will skip",
				"path", path, "err", err)
			return pricing.NoPricePricer{}
		}
		log.Debug("gcp: offline pricer loaded from TELLURY_PRICE_FIXTURE", "path", path)
		return static
	}
	log.Debug("gcp: no price source available; resources requiring prices will skip")
	return pricing.NoPricePricer{}
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
// KindOrganization) plus containment edges, and every leaf's canonical
// location is turned into a per-project region container node between the
// leaf and its project (KindRegion). Containers are not waste and never reach
// rule evaluation (they are structurally excluded by graph.ByKind) and never
// inflate the scan's "N resources" count (graph.ResourceNodeCount).
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
// organization) and hands that set to the provider. The caller's
// req.Progress callback (if any) is carried into the provider's Fill so the
// scan can report how far the (key, project) fan-out has come.
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

	sub := metrics.Request{Keys: supported, WindowDays: req.WindowDays, Projects: projects, Progress: req.Progress}
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

// MigrateV2ToV3 adds the region container tier to a graph deserialized from a
// v2 snapshot. v2 snapshots predate KindRegion and carry no region nodes or
// region edges; every leaf's Location field (canonicalised at normalization
// time through the single pricing.CanonicalRegion wrapper) carries enough data
// to reconstruct the tier exactly, so a cached v2 scan is replayed rather than
// rejected. The report that comes out of the migrated graph is identical to a
// fresh v3 scan of the same data — same region nodes, same region edges, same
// rollup.
//
// The migration is additive: the resource -> project containment edge a v2
// snapshot already carries stays, and only the two missing edges (resource ->
// region, region -> project) are added. The result therefore has one more
// contains path (resource -> project directly) than a fresh v3 ingestion,
// which changes no reachability and no operator-facing number — walking out
// from a resource still reaches its region, then its project, then its
// folder(s), then its org.
//
// It is idempotent: a graph that already carries any KindRegion node (a v3
// snapshot loaded directly) is returned untouched.
//
// The graph must be freshly deserialized (graph.LoadSnapshot) and not yet
// shared with concurrent readers: the method unfreezes it to add nodes and
// edges and re-freezes it before returning.
func (p *Provider) MigrateV2ToV3(g *graph.Graph) error {
	if g == nil {
		return fmt.Errorf("gcp: migrate v2->v3: nil graph")
	}

	// Idempotency gate: region nodes already present means a v3 snapshot.
	hasRegion := false
	g.Nodes(func(n *graph.Node) bool {
		if n.Kind == graph.KindRegion {
			hasRegion = true
			return false
		}
		return true
	})
	if hasRegion {
		return nil
	}

	// Collect the leaves first: the iteration must not be disturbed by the
	// nodes we are about to add.
	var leaves []*graph.Node
	g.Nodes(func(n *graph.Node) bool {
		if !n.Container() {
			leaves = append(leaves, n)
		}
		return true
	})
	if len(leaves) == 0 {
		return nil
	}

	g.Unfreeze()
	// Edge de-duplication: several leaves of one project can share a location
	// (two disks in us-central1-a), and each would independently emit the same
	// region -> project edge. The live ingest path de-dupes its edges through
	// a set before AddEdge (see Ingest); the migration must do the same, or a
	// replayed v2 scan carries a duplicated containment edge that the fresh v3
	// scan does not.
	addedEdges := map[graph.Edge]struct{}{}
	emit := func(e graph.Edge) error {
		if _, ok := addedEdges[e]; ok {
			return nil
		}
		addedEdges[e] = struct{}{}
		return g.AddEdge(e)
	}
	for _, leaf := range leaves {
		loc := locationRegion(leaf.Location)
		if loc == "" {
			continue
		}
		// Rewrite the leaf's own Location too, not just the region node built
		// from it. A v2 snapshot stores whatever spelling the service returned
		// — Cloud Storage says "EUROPE-WEST4" and "US" where Compute says
		// "europe-west4" and "us" — and a finding copies Location verbatim.
		// Migrating the region node but leaving the leaf raw would put a
		// replayed snapshot and a fresh scan of the same bucket in two
		// different rows of the waste-by-region chart, for one real region.
		// Money is unaffected either way (pricing re-canonicalises), which is
		// exactly why this would go unnoticed.
		leaf.Location = loc

		// Resolve the owning project CONTAINER token from the v2 leaf ->
		// project containment edge — the same token fresh ingestion derives
		// from RawAsset.Project. For a GCS bucket the leaf.Project field is
		// the parent project NUMBER while the container node is
		// "projects/<id>"; only the containment edge knows the real token, so
		// the region node must hang off that edge's endpoint or the chain
		// would never reach the project container. The "projects/<id>" form
		// is the defensive fallback for a leaf whose edge was pruned.
		projectToken, projectID := "", ""
		for _, e := range g.Out(leaf.ID) {
			if e.Kind != graph.EdgeContains {
				continue
			}
			if pn, ok := g.Node(e.To); ok && pn.Kind == graph.KindProject {
				projectToken = string(e.To)
				projectID = pn.Project
				break
			}
		}
		if projectToken == "" && leaf.Project != "" {
			tok := graph.Ref("projects/" + leaf.Project)
			if pn, ok := g.Node(tok); ok && pn.Kind == graph.KindProject {
				projectToken = string(tok)
				projectID = pn.Project
			}
		}
		if projectToken == "" {
			continue
		}
		if projectID == "" {
			projectID = leaf.Project
		}

		rn := regionNode(projectToken, projectID, loc)
		if err := g.AddNode(rn); err != nil {
			return fmt.Errorf("gcp: migrate v2->v3: add region node: %w", err)
		}
		if err := emit(graph.Edge{From: leaf.ID, To: rn.ID, Kind: graph.EdgeContains}); err != nil {
			return fmt.Errorf("gcp: migrate v2->v3: add leaf->region edge: %w", err)
		}
		if err := emit(graph.Edge{From: rn.ID, To: graph.Ref(projectToken), Kind: graph.EdgeContains}); err != nil {
			return fmt.Errorf("gcp: migrate v2->v3: add region->project edge: %w", err)
		}
	}
	g.Freeze()

	p.log.Info("reconstructed region containers from v2 snapshot",
		"region_nodes", g.CountByKind(graph.KindRegion))
	return nil
}
