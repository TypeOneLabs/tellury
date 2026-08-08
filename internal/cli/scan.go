package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/TypeOneLabs/tellury/internal/config"
	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/cloud/gcp"
	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/output"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

func newScanCmd(g *globalFlags) *cobra.Command {
	var (
		cfg config.Scan
		at  string
	)
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Scan a cloud scope for waste",
		Args:  cobra.NoArgs,
		Example: "  tellury scan --gcp-project my-gcp-project\n" +
			"  tellury scan --gcp-project p --rules detached_disk --format json\n" +
			"  tellury scan --cache-file snap.json   # offline replay (full fidelity)\n" +
			"  tellury scan --fixture cai-assets.json   # offline replay (topology only)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			now := time.Now().UTC()
			if at != "" {
				t, err := time.Parse(time.RFC3339, at)
				if err != nil {
					return newUsageError(fmt.Errorf("invalid --at %q: %w", at, err))
				}
				now = t.UTC()
			}
			return runScan(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), g, cfg, now)
		},
	}

	f := cmd.Flags()
	// Scope flags are registered from the provider registry, exactly like the
	// environment variables: cloud.ScopesFor(gcp) yields each scope dimension
	// with its provider-owned --gcp-<scope> flag name. No literal GCP flag
	// set is hardcoded here.
	addScopeFlags(f, gcp.ProviderName, &cfg)
	f.StringVar(&cfg.Provider, "provider", "gcp", "cloud provider")
	f.StringSliceVar(&cfg.Rules, "rules", nil, "rule IDs to run (default: all)")
	f.StringSliceVar(&cfg.SkipRules, "skip-rules", nil, "rule IDs to exclude")
	f.StringVar(&cfg.Format, "format", "table", "table|json|csv")
	f.StringVar(&cfg.Sort, "sort", "waste", "waste|resource|rule")
	f.IntVar(&cfg.WindowDays, "window", config.DefaultWindowDays, "metric lookback window in days (7-30)")
	f.Float64Var(&cfg.MinWaste, "min-waste", 0, "hide findings below $/month")
	f.Float64Var(&cfg.MinConfidence, "min-confidence", 0, "hide findings below confidence")
	f.StringVar(&cfg.CacheFile, "cache-file", "", "read graph from file if it exists, else write it")
	f.StringSliceVar(&cfg.Fixture, "fixture", nil, "read assets from CAI JSON fixtures instead of the API")
	f.StringVar(&cfg.OutDir, "out-dir", config.DefaultOutDir, "directory to write scan artifacts into (created if absent)")
	f.StringVar(&cfg.PriceFile, "price-file", "", "path to a JSON price override file. Price "+
		"precedence, highest first: --price-file override > live Cloud Billing Catalog API "+
		"(cached for this scan) > embedded fallback table used when the API is unreachable or "+
		"billing access is missing")
	f.BoolVar(&cfg.FailOnFindings, "fail-on-findings", true, "exit 3 when findings exist")
	f.BoolVar(&cfg.ExplainSkips, "explain-skips", false, "print the per-rule skip tally to stderr")
	f.StringVar(&at, "at", "", "evaluation instant (RFC3339); default now. Makes age predicates reproducible")

	return cmd
}

// runScan is the whole pipeline, in order, with no hidden magic.
func runScan(
	ctx context.Context,
	out io.Writer,
	errOut io.Writer,
	g *globalFlags,
	cfg config.Scan,
	now time.Time,
) error {
	// 1. Configuration.
	if err := cfg.Validate(); err != nil {
		return newUsageError(err)
	}
	log := newLogger(g, errOut)

	ctx, cancel := withTimeout(ctx, g)
	defer cancel()

	// 2. Rule selection.
	selected, err := rules.Select(cfg.Provider, cfg.Rules, cfg.SkipRules)
	if err != nil {
		return newUsageError(err)
	}
	if len(selected) == 0 {
		return newUsageError(fmt.Errorf("no rules selected for provider %q", cfg.Provider))
	}

	// 3. Data plan — computed before any I/O so we fetch exactly what is needed.
	assetTypes, metricKeys := rules.Plan(selected)
	log.Info("scan plan",
		"rules", len(selected),
		"asset_types", len(assetTypes),
		"metric_keys", len(metricKeys),
		"window_days", cfg.WindowDays)

	// 4. Artifact directory. Create it up-front (before any I/O) so a scan
	// cannot fail partway through for want of a directory it promised to
	// write into. Errors here are fatal: if we cannot write our artifacts we
	// must not pretend the scan produced a directory.
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return newUsageError(fmt.Errorf("create --out-dir %s: %w", cfg.OutDir, err))
	}

	// 4b. Offline decision — before any provider/client is built. A scan is
	// offline when it either replays a fixture (assets come straight from
	// local JSON) or the --cache-file is present (the graph is replayed
	// verbatim). An offline scan constructs no cloud SDK client at all, so it
	// runs on a host with no Application Default Credentials.
	//
	// The cache check now happens BEFORE the provider is constructed: a cache
	// hit must never cost even one SDK client, let alone an ADC resolution.
	cachedGraph, cacheSnap, cacheErr := cacheIfPresent(cfg.CacheFile)
	if cacheErr != nil && !errors.Is(cacheErr, os.ErrNotExist) {
		return cacheErr
	}
	cacheHit := cacheErr == nil
	offline := len(cfg.Fixture) > 0 || cacheHit

	// 5. Provider. Offline providers build no cloud clients.
	provider, err := newProvider(ctx, cfg, log, offline, cacheHit)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := provider.Close(); cerr != nil {
			log.Debug("provider close", "err", cerr)
		}
	}()

	// --price-file always wins over whatever pricer the provider built
	// (live catalog API, itself falling back to the embedded table): any
	// pricer that supports overrides implements pricing.OverlayLoader.
	pricer := provider.Pricer()
	if cfg.PriceFile != "" {
		ol, ok := pricer.(pricing.OverlayLoader)
		if !ok {
			return fmt.Errorf("--price-file is not supported by the %s pricer", cfg.Provider)
		}
		if err := ol.OverlayFile(cfg.PriceFile); err != nil {
			return err
		}
	}

	scope := cfg.Scope()

	// 6-8. Graph: use the cache already loaded, otherwise ingest and enrich.
	gr, err := buildGraph(ctx, cfg, provider, scope, assetTypes, metricKeys, log, cachedGraph, cacheSnap, cacheHit)
	if err != nil {
		return err
	}
	gr.Freeze()

	// 9. Rule evaluation.
	pass := &rules.Pass{
		Graph:  gr,
		Price:  pricer,
		Scope:  scope.String(),
		Window: cfg.WindowDays,
		Log:    log,
		Now:    now,
		Sizer:  provider.Sizer(),
	}
	res, err := rules.Engine{}.Run(ctx, pass, selected)
	if err != nil {
		return err
	}
	for id, rerr := range res.Errors {
		log.Warn("rule failed", "rule", id, "err", rerr)
	}

	// 10. Filters and ordering, then the report.
	res.Findings = filterFindings(res.Findings, cfg)
	rules.SortFindings(res.Findings, cfg.SortOrder())

	// The report's ResourcesScanned is the number of real resources, so
	// container nodes (project/folder/organization hierarchy scaffolding)
	// never inflate the operator-facing "N resources" figure.
	report := output.NewReport(res, output.Meta{
		Scope:            scope.String(),
		Provider:         cfg.Provider,
		GeneratedAt:      now,
		WindowDays:       cfg.WindowDays,
		ResourcesScanned: gr.ResourceNodeCount(),
		RulesEvaluated:   len(selected),
		MultiProject:     gr.ProjectCount() > 1,
	})

	// Offline honesty: when the scan's data carried no metric series (a raw
	// CAI fixture), the metric-dependent rules could not evaluate. State which
	// ones explicitly on the report so an empty table is not mistaken for "no
	// waste" when it actually means "could not check". A cached-snapshot replay
	// usually carries the serialized Metrics map with full fidelity, so it is
	// rarely blocked here.
	report.MetricsBlocked = rules.MetricsBlocked(selected, gr)

	// 11. Artifacts: write the enriched graph snapshot, the findings JSON and
	// the self-contained HTML report into --out-dir BEFORE rendering stdout,
	// so a scan that crashes after this point has still left all three behind.
	// A failure here aborts: the operator asked for a directory of artifacts,
	// and a scan that cannot produce them must say so rather than pretend it
	// succeeded with only terminal output.
	artifactDir := artifactDirName(cfg.OutDir, scope.String())
	if err := writeArtifacts(artifactDir, cfg, gr, scope.String(), report); err != nil {
		return err
	}

	// 12. Render. Terminal output is byte-for-byte unchanged by the artifact
	// writing above: artifact names are only logged to stderr, stdout gets the
	// same table/JSON/CSV it always did.
	renderer, err := output.For(cfg.Format)
	if err != nil {
		return newUsageError(err)
	}
	if err := renderer.Render(out, report); err != nil {
		return err
	}
	if cfg.ExplainSkips {
		printSkips(errOut, res)
	}

	if cfg.FailOnFindings && report.FindingCount > 0 {
		return errFindings
	}
	return nil
}

// artifactDirName renders the per-scan artifact subdirectory: the --out-dir
// base joined with <scope>-<timestamp>. The scope and a sub-second-resolution
// timestamp are baked into the name so consecutive scans never silently
// overwrite each other's artifacts. Wall-clock UTC with nanosecond precision
// guarantees that two scans run back-to-back (even within the same second,
// as a test or CI does) still land in distinct subdirectories.
func artifactDirName(outDir, scope string) string {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	return filepath.Join(outDir, sanitizeSegment(scope)+"-"+stamp)
}

// sanitizeSegment makes a scope token safe to embed in a directory name. A
// scope is "projects/my-project" or "folders/123" or "organizations/456";
// the slash becomes a dash. Any characters that would be invalid in a path
// segment on any supported filesystem are replaced with an underscore.
func sanitizeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\':
			b.WriteByte('-')
		case r == ':' || r == ' ' || r < 0x20 || r == 0x7f:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// writeArtifacts emits the three required files into dir:
//
//   - graph-<scope>.json — the enriched graph snapshot, serialized with
//     WriteSnapshot exactly as --cache-file produces it, so it is replayable
//     through `tellury scan --cache-file` with full fidelity.
//   - findings-<scope>.json — the scan report (findings, totals, scope,
//     metrics_blocked) as JSON.
//   - report-<scope>.html — the self-contained HTML report: collapsible
//     rollup hierarchy plus the top-findings table. No CDN, no external
//     stylesheet, no runtime network fetch — an operator can email it, attach
//     it to a ticket, or open it on an air-gapped machine.
//
// All three names carry the scope so an artifact directory containing many
// scans stays navigable.
func writeArtifacts(dir string, cfg config.Scan, gr *graph.Graph, scope string, report output.Report) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// 1. Graph snapshot (full fidelity, replayable).
	graphPath := filepath.Join(dir, "graph-"+sanitizeSegment(scope)+".json")
	if err := writeGraphSnapshot(graphPath, gr, cfg.Provider, scope); err != nil {
		return err
	}

	// 2. Findings JSON.
	findingsPath := filepath.Join(dir, "findings-"+sanitizeSegment(scope)+".json")
	if err := writeFindingsJSON(findingsPath, report); err != nil {
		return err
	}

	// 3. Self-contained HTML report.
	reportPath := filepath.Join(dir, "report-"+sanitizeSegment(scope)+".html")
	if err := writeHTMLReport(reportPath, gr, report, scope); err != nil {
		return err
	}
	return nil
}

// writeHTMLReport renders the self-contained HTML report into path. The
// rollup hierarchy is rebuilt from the graph's containment edges and the
// report's findings, so the same scan materializes the same tree.
func writeHTMLReport(path string, gr *graph.Graph, report output.Report, scope string) error {
	root := output.BuildHierarchy(gr, report.Findings, scope)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return output.RenderHTML(f, report, root)
}

// writeGraphSnapshot writes one graph.Snapshot to path. It is the exact same
// serialization `scan --cache-file` writes, so the artifact directory can be
// replayed directly.
func writeGraphSnapshot(path string, gr *graph.Graph, provider, scope string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return gr.WriteSnapshot(f, provider, scope)
}

// writeFindingsJSON writes the report as indented JSON — the same shape
// `scan --format json` prints, saved into the artifact directory.
func writeFindingsJSON(path string, report output.Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// cacheIfPresent attempts to load a --cache-file graph. It returns the graph
// and snapshot on a hit, or (nil, nil, os.ErrNotExist) when the file does not
// exist. Any other error is a real failure. This runs BEFORE the provider is
// built so that a cache hit never constructs a cloud client.
func cacheIfPresent(path string) (*graph.Graph, *graph.Snapshot, error) {
	if path == "" {
		return nil, nil, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return graph.LoadSnapshot(f)
}

// newProvider builds the cloud provider. It accepts two offline signals it
// has no way to derive on its own:
//
//   - offline=true  => this is a --fixture or a cache-hit scan: no cloud SDK
//     client is constructed at all (see gcp.WithOffline). The embedded static
//     table prices the replay; a fixture lister, when present, is wired in.
//   - cacheHit=true => the graph already came from the cache file, so there
//     is no asset source to build; this is only used for the log line.
func newProvider(ctx context.Context, cfg config.Scan, log *slog.Logger, offline, cacheHit bool) (*gcp.Provider, error) {
	opts := []gcp.Option{gcp.WithLogger(log)}
	if len(cfg.Fixture) > 0 {
		lister, err := gcp.LoadFakeLister(cfg.Fixture...)
		if err != nil {
			return nil, err
		}
		opts = append(opts, gcp.WithLister(lister))
		log.Info("using fixture assets", "files", len(cfg.Fixture), "assets", len(lister.Assets))
	}
	if offline {
		opts = append(opts, gcp.WithOffline())
	}
	return gcp.New(ctx, opts...)
}

// buildGraph returns a graph either replayed from a pre-loaded --cache-file
// cache (cachedGraph/cacheSnap) or ingested and enriched from the provider. A
// cache hit performs zero API calls.
func buildGraph(
	ctx context.Context,
	cfg config.Scan,
	provider cloud.Provider,
	scope cloud.Scope,
	assetTypes, metricKeys []string,
	log *slog.Logger,
	cachedGraph *graph.Graph,
	cacheSnap *graph.Snapshot,
	cacheHit bool,
) (*graph.Graph, error) {
	if cacheHit {
		log.Info("replayed graph from cache",
			"file", cfg.CacheFile, "captured_at", cacheSnap.CapturedAt,
			"nodes", cachedGraph.NodeCount(), "edges", cachedGraph.EdgeCount())
		return cachedGraph, nil
	}

	if cfg.CacheFile != "" {
		// We already verified earlier that the file does not exist (a real
		// error would have stopped runScan). Log the miss and ingest live.
		log.Info("cache miss, ingesting", "file", cfg.CacheFile)
	}

	gr, err := provider.Ingest(ctx, scope, assetTypes)
	if err != nil {
		return nil, err
	}

	if len(metricKeys) > 0 {
		req := metrics.Request{Keys: metricKeys, WindowDays: cfg.WindowDays}
		if err := provider.EnrichMetrics(ctx, gr, scope, req); err != nil {
			// Enrichment failure is not fatal: rules that need a metric skip
			// their nodes (invariant I5) instead of guessing, so the report
			// stays correct — just narrower.
			log.Warn("metric enrichment incomplete; metric-dependent rules will skip",
				"keys", len(metricKeys), "err", err)
		}
	}

	if cfg.CacheFile != "" {
		if err := writeCache(cfg.CacheFile, gr, provider.Name(), scope.String()); err != nil {
			log.Warn("could not write cache file", "file", cfg.CacheFile, "err", err)
		}
	}
	return gr, nil
}

func writeCache(path string, gr *graph.Graph, provider, scope string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return gr.WriteSnapshot(f, provider, scope)
}

// filterFindings applies --min-waste and --min-confidence.
func filterFindings(fs []rules.Finding, cfg config.Scan) []rules.Finding {
	if cfg.MinWaste <= 0 && cfg.MinConfidence <= 0 {
		return fs
	}
	kept := fs[:0]
	for _, f := range fs {
		if f.MonthlyWasteUSD < cfg.MinWaste {
			continue
		}
		if f.Confidence < cfg.MinConfidence {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

func printSkips(w io.Writer, res rules.Result) {
	tallies := res.SkipTotals()
	if len(tallies) == 0 {
		return
	}
	fmt.Fprintln(w, "\nskipped resources:")
	for _, t := range tallies {
		fmt.Fprintf(w, "  %-28s %-32s %d\n", t.RuleID, t.Code, t.Count)
	}
}
