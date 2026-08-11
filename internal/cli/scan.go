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
			"  tellury scan --currency EUR --gcp-project my-gcp-project   # price in EUR\n" +
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
	f.StringVar(&cfg.Currency, "currency", "", "ISO 4217 currency code to price the scan in, e.g. EUR. "+
		"Overrides auto-detection from the billing account; the default (also "+
		config.CurrencyEnvVar+") is USD. A well-formed but unsupported code fails at the "+
		"Cloud Billing API with the currency named, never silently falling back to USD")
	f.BoolVar(&cfg.FailOnFindings, "fail-on-findings", true, "exit 3 when findings exist")
	f.BoolVar(&cfg.ExplainSkips, "explain-skips", false, "print the per-rule skip tally to stderr")
	f.StringVar(&cfg.Progress, "progress", "", "auto|on|off: report scan phase progress to stderr. "+
		"auto (default, also "+config.ProgressEnvVar+") reports only on an interactive terminal; "+
		"on always reports (off a terminal it degrades to plain periodic lines, never ANSI); "+
		"off silences it. Progress is a stderr status channel, independent of --log-level, and "+
		"never touches stdout")
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
	// The scan's own wall clock. The duration the report carries is measured
	// from this instant, up to report construction, and is NEVER re-measured
	// inside a renderer — so a replayed or fixture-driven scan reports its
	// real duration, and the value is testable with a fixed clock rather than
	// being whatever the machine felt like at render time.
	start := time.Now()

	// 1. Configuration.
	if err := cfg.Validate(); err != nil {
		return newUsageError(err)
	}
	log := newLogger(g, errOut)

	// Progress reporting is a status channel on stderr — never stdout, which
	// is the report and is piped into other tools (`--format json | jq` must
	// stay pure). See progress.go for the auto/on/off resolution, the non-TTY
	// degradation and the no-ANSI guarantee.
	prog := newProgress(errOut, cfg.Progress)

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

	// 5. Provider. Offline providers build no cloud clients. The explicit
	// --currency flag is threaded in at construction so the pricer's ListSkus
	// requests carry it; best-effort detection (which needs the ingested
	// graph) is applied after buildGraph below.
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

	// The live Cloud Billing catalogue loads lazily on the first UnitPrice
	// call — i.e. inside rule evaluation — so its load is surfaced as its own
	// progress phase (with the number of billing services as the denominator)
	// rather than pre-warmed: pre-warming would spend a billing API call even
	// on a scan whose rules price nothing. A pricer with no lazy catalogue
	// (the offline static table) simply implements no hook and no phase runs.
	if hook, ok := pricer.(catalogueProgressSetter); ok {
		var pricePhase *Phase
		hook.SetCatalogueProgress(func(done, total int, final bool) {
			if pricePhase == nil {
				pricePhase = prog.Begin("pricing catalogue", "services")
			}
			if pricePhase == nil {
				return
			}
			pricePhase.Set(done, total)
			if final {
				if total > 0 {
					pricePhase.End("catalogue loaded")
				} else {
					pricePhase.End("catalogue unavailable; using embedded price table")
				}
			}
		})
	}

	scope := cfg.Scope()

	// 6-8. Graph: use the cache already loaded, otherwise ingest and enrich.
	gr, err := buildGraph(ctx, cfg, provider, scope, assetTypes, metricKeys, log, cachedGraph, cacheSnap, cacheHit, prog)
	if err != nil {
		return err
	}
	gr.Freeze()

	// 8b. Currency. Precedence, highest first: the explicit
	// --currency/TELLURY_CURRENCY flag, then best-effort detection of a
	// billing account's currency, then USD. Detection needs the ingested
	// graph for a folder/organization scope (no single project to ask), so it
	// runs here, after ingestion and before rule evaluation — the pricer's
	// catalogue loads lazily on the first UnitPrice call, so the currency set
	// here still reaches every ListSkus request.
	currencyState := resolveScanCurrency(ctx, cfg, pricer, gr, log, offline)

	// 9. Rule evaluation.
	rulePhase := prog.Begin("rule evaluation", "rules")
	var onRuleProgress func(completed, total int)
	if rulePhase != nil {
		onRuleProgress = rulePhase.Set
	}
	pass := &rules.Pass{
		Graph:  gr,
		Price:  pricer,
		Scope:  scope.String(),
		Window: cfg.WindowDays,
		Log:    log,
		Now:    now,
		Sizer:  provider.Sizer(),
	}
	res, err := rules.Engine{OnProgress: onRuleProgress}.Run(ctx, pass, selected)
	if rulePhase != nil {
		rulePhase.End("")
	}
	if err != nil {
		return err
	}
	for id, rerr := range res.Errors {
		log.Warn("rule failed", "rule", id, "err", rerr)
	}

	// A well-formed but unsupported currency makes the Cloud Billing
	// catalogue reject every ListSkus call with InvalidArgument. That is an
	// operator error to surface — naming the currency — never a degradation
	// to absorb by silently pricing the scan from the USD embedded table.
	if ce, ok := pricer.(pricing.CatalogueErrorer); ok {
		if cerr := ce.CatalogueError(); cerr != nil {
			return cerr
		}
	}

	// 10. Filters and ordering, then the report.
	res.Findings = filterFindings(res.Findings, cfg)
	rules.SortFindings(res.Findings, cfg.SortOrder())

	// What the figures are ACTUALLY in: the requested currency when the live
	// catalogue answered, otherwise USD (the embedded table's currency). A
	// scan that mixed USD fallback prices into a non-USD request must say so
	// loudly in every output format.
	effectiveCurrency, currencyMixed := reportCurrency(pricer, currencyState)

	// The report's ResourcesScanned is the number of real resources, so
	// container nodes (project/folder/organization/region hierarchy
	// scaffolding) never inflate the operator-facing "N resources" figure.
	// ProjectsAnalyzed counts the graph's project container nodes (never the
	// findings), and Duration is the wall clock elapsed since start — both
	// measured by this scan, not by the renderer.
	report := output.NewReport(res, output.Meta{
		Scope:             scope.String(),
		Provider:          cfg.Provider,
		GeneratedAt:       now,
		WindowDays:        cfg.WindowDays,
		ResourcesScanned:  gr.ResourceNodeCount(),
		RulesEvaluated:    len(selected),
		ProjectsAnalyzed:  gr.ProjectContainerCount(),
		Duration:          time.Since(start),
		MultiProject:      gr.ProjectCount() > 1,
		Currency:          effectiveCurrency,
		CurrencySource:    currencyState.source,
		CurrencyRequested: requestedCurrency(currencyState),
		CurrencyMixed:     currencyMixed,
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
	reportPath, err := writeArtifacts(artifactDir, cfg, gr, scope.String(), report)
	if err != nil {
		return err
	}
	// The table renderer's "N of M findings omitted; full report: file://..."
	// footer needs a path a terminal can make clickable from any working
	// directory, so hand it the absolute HTML report path. The field itself is
	// excluded from the JSON serialization, so the machine-readable findings
	// are unaffected.
	if abs, aerr := filepath.Abs(reportPath); aerr == nil {
		reportPath = abs
	}
	report.ReportPath = reportPath

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

// requestedCurrency renders the currency the scan was asked/detected to price
// in, defaulting to USD so a report field is never empty when the source says
// flag or detected (the two non-default cases always carry a requested code).
func requestedCurrency(state currencyResolution) string {
	if state.requested != "" {
		return state.requested
	}
	return "USD"
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

// writeArtifacts emits the three required files into dir and returns the path
// of the HTML report (for the table renderer's file:// footer):
//
//   - graph-<scope>.json — the enriched graph snapshot, serialized with
//     WriteSnapshot exactly as --cache-file produces it, so it is replayable
//     through `tellury scan --cache-file` with full fidelity.
//   - findings-<scope>.json — the scan report (findings, totals, scope,
//     metrics_blocked) as JSON.
//   - report-<scope>.html — the self-contained HTML report: hero number,
//     waste-by-project / waste-by-region / waste-by-rule summaries, the full
//     findings table with client-side filter/sort, and collapsed scan
//     details. No CDN, no external stylesheet, no runtime network fetch — an
//     operator can email it, attach it to a ticket, or open it on an
//     air-gapped machine.
//
// All three names carry the scope so an artifact directory containing many
// scans stays navigable.
func writeArtifacts(dir string, cfg config.Scan, gr *graph.Graph, scope string, report output.Report) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	// 1. Graph snapshot (full fidelity, replayable).
	graphPath := filepath.Join(dir, "graph-"+sanitizeSegment(scope)+".json")
	if err := writeGraphSnapshot(graphPath, gr, cfg.Provider, scope); err != nil {
		return "", err
	}

	// 2. Findings JSON.
	findingsPath := filepath.Join(dir, "findings-"+sanitizeSegment(scope)+".json")
	if err := writeFindingsJSON(findingsPath, report); err != nil {
		return "", err
	}

	// 3. Self-contained HTML report.
	reportPath := filepath.Join(dir, "report-"+sanitizeSegment(scope)+".html")
	if err := writeHTMLReport(reportPath, report); err != nil {
		return "", err
	}
	return reportPath, nil
}

// writeHTMLReport renders the self-contained HTML report into path.
func writeHTMLReport(path string, report output.Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return output.RenderHTML(f, report)
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
	opts := []gcp.Option{gcp.WithLogger(log), gcp.WithCurrency(cfg.Currency)}
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

// snapshotMigrator is the optional provider capability that upgrades a graph
// deserialized from an older snapshot version to the current topology. The
// only implementor today is gcp.Provider.MigrateV2ToV3, which reconstructs the
// region container tier (KindRegion nodes and their containment edges) that v2
// snapshots predate, so an operator's cached scan replays instead of being
// rejected. Providers that have nothing to migrate simply do not implement it,
// and a replay is used as loaded.
type snapshotMigrator interface {
	MigrateV2ToV3(g *graph.Graph) error
}

// buildGraph returns a graph either replayed from a pre-loaded --cache-file
// cache (cachedGraph/cacheSnap) or ingested and enriched from the provider. A
// cache hit performs zero API calls. The two long phases run inside here and
// report through prog: asset discovery (ingestion, no known total) and metric
// enrichment (per-project, the slowest phase, with a hard denominator).
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
	prog *Progress,
) (*graph.Graph, error) {
	if cacheHit {
		log.Info("replayed graph from cache",
			"file", cfg.CacheFile, "captured_at", cacheSnap.CapturedAt,
			"nodes", cachedGraph.NodeCount(), "edges", cachedGraph.EdgeCount())
		// A snapshot older than the current version is still valid data: the
		// version is advisory and the graph deserialized. Reconstruct whatever
		// topology the newer version added from the node fields already in the
		// snapshot, rather than rejecting the cache or silently running a scan
		// missing its region tier. The provider's migration is idempotent, so
		// a current snapshot replays untouched.
		if cacheSnap.Version < graph.SnapshotVersion {
			if mig, ok := provider.(snapshotMigrator); ok {
				if err := mig.MigrateV2ToV3(cachedGraph); err != nil {
					return nil, fmt.Errorf("migrate cache snapshot v%d to v%d: %w",
						cacheSnap.Version, graph.SnapshotVersion, err)
				}
				log.Info("migrated cache from v2 to v3 (regions reconstructed)",
					"from", cacheSnap.Version, "to", graph.SnapshotVersion,
					"nodes", cachedGraph.NodeCount(), "edges", cachedGraph.EdgeCount())
			}
		}
		return cachedGraph, nil
	}

	if cfg.CacheFile != "" {
		// We already verified earlier that the file does not exist (a real
		// error would have stopped runScan). Log the miss and ingest live.
		log.Info("cache miss, ingesting", "file", cfg.CacheFile)
	}

	// Asset discovery: a paginated inventory stream with no known total, so
	// the phase reports its start and its end (with the resource count the
	// ingest landed on) rather than a running denominator.
	ingestPhase := prog.Begin("asset discovery", "")
	gr, err := provider.Ingest(ctx, scope, assetTypes)
	if err != nil {
		return nil, err
	}
	if ingestPhase != nil {
		ingestPhase.End(progressCount(gr.ResourceNodeCount(), "resource"))
	}

	if len(metricKeys) > 0 {
		// Metric enrichment is the slow phase and the only one with a hard
		// denominator from the start: len(metricKeys) x the distinct projects
		// in the graph. The fetch pool reports each completed (key, project)
		// job through the phase's lock-free Set, so the progress lines never
		// serialize the bounded-concurrency enrichment they report on.
		enrichPhase := prog.Begin("metric enrichment", "fetches")
		req := metrics.Request{Keys: metricKeys, WindowDays: cfg.WindowDays}
		if enrichPhase != nil {
			req.Progress = enrichPhase.Set
		}
		if err := provider.EnrichMetrics(ctx, gr, scope, req); err != nil {
			// Enrichment failure is not fatal: rules that need a metric skip
			// their nodes (invariant I5) instead of guessing, so the report
			// stays correct — just narrower.
			log.Warn("metric enrichment incomplete; metric-dependent rules will skip",
				"keys", len(metricKeys), "err", err)
		}
		if enrichPhase != nil {
			enrichPhase.End(progressCount(gr.ProjectCount(), "project"))
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
