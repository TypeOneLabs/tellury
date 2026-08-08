package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/TypeOneLabs/tellury/internal/config"
	"github.com/TypeOneLabs/tellury/pkg/cloud/gcp"
	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// newGraphCmd exposes the ingested graph for debugging. It is the fastest way
// to answer "why did the rule not fire?" — the answer is almost always a
// missing edge.
func newGraphCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "graph",
		Short: "Inspect the ingested resource graph (debug)",
	}
	cmd.AddCommand(newGraphExportCmd(g))
	return cmd
}

// newGraphExportCmd ingests a scope, enriches it with metrics, and writes the
// graph as a JSON snapshot that `tellury scan --cache-file` replays verbatim.
//
// Enrichment is ON by default: the whole point of export is to produce a
// snapshot that preserves fidelity, and a cache written WITHOUT metrics would
// silently become "could not check" on replay for every metric-dependent rule.
// Drop --no-enrich-metrics to write a topology-only snapshot (all metric
// values are then absent on replay and metric-dependent rules skip).
//
// The emitted file is a graph.Snapshot (version 2), NOT a CAI fixture. To
// capture raw Cloud Asset Inventory JSON for `--fixture`, use the gcloud
// command documented in README.md ("Capturing your own fixture") instead.
func newGraphExportCmd(g *globalFlags) *cobra.Command {
	var (
		cfg            config.Scan
		noEnrichMetrics bool
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Ingest a scope, enrich it with metrics, and write the graph as a full-fidelity snapshot",
		Long: "Ingest the scope's asset inventory, enrich it with Cloud Monitoring " +
			"series (exactly as `tellury scan` does), and write the result as a JSON " +
			"graph snapshot. `tellury scan --cache-file THIS_FILE` then replays it " +
			"verbatim, so every rule — metric-dependent ones included — can fire. " +
			"Enrichment is on by default; pass --no-enrich-metrics to write a " +
			"topology-only snapshot whose replay leaves metric-dependent rules unable " +
			"to evaluate.",
		Example: "  tellury graph export --gcp-project p --out graph.json\n" +
			"  tellury scan --cache-file graph.json   # full-fidelity replay",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cfg.Validate(); err != nil {
				return newUsageError(err)
			}
			log := newLogger(g, cmd.ErrOrStderr())

			ctx, cancel := withTimeout(cmd.Context(), g)
			defer cancel()

			selected, err := rules.Select(cfg.Provider, cfg.Rules, cfg.SkipRules)
			if err != nil {
				return newUsageError(err)
			}
			assetTypes, metricKeys := rules.Plan(selected)

			// `graph export` always ingests fresh assets; it never replays a
			// cache. So it is an online scan: offline=false, cacheHit=false.
			// A --fixture here still works (that is an offline ingest), and
			// gcp.New's offline guard applies only when offline=true.
			offline := len(cfg.Fixture) > 0
			provider, err := newProvider(ctx, cfg, log, offline, false)
			if err != nil {
				return err
			}
			defer func() { _ = provider.Close() }()

			scope := cfg.Scope()
			gr, err := provider.Ingest(ctx, scope, assetTypes)
			if err != nil {
				return err
			}

			// Enrich by default so the snapshot can drive metric-dependent
			// rules on replay, exactly like a `scan` with a cache file. A
			// failed enrichment is not fatal: it just narrows the replay the
			// same way it narrows a scan.
			if len(metricKeys) > 0 && !noEnrichMetrics {
				req := metrics.Request{Keys: metricKeys, WindowDays: cfg.WindowDays}
				if err := provider.EnrichMetrics(ctx, gr, scope, req); err != nil {
					log.Warn("graph export: metric enrichment incomplete; replayed scans will skip metric-dependent rules",
						"keys", len(metricKeys), "err", err)
				}
			}
			gr.Freeze()
			return writeSnapshot(cmd, cfg.CacheFile, gr, provider.Name(), scope.String())
		},
	}
	f := cmd.Flags()
	// Scope flags are registered from the provider registry, exactly like the
	// environment variables: cloud.ScopesFor(gcp) yields each scope dimension
	// with its provider-owned --gcp-<scope> flag name. No literal GCP flag
	// set is hardcoded here.
	addScopeFlags(f, gcp.ProviderName, &cfg)
	f.StringVar(&cfg.Provider, "provider", "gcp", "cloud provider")
	f.StringSliceVar(&cfg.Rules, "rules", nil, "rule IDs to drive asset type and metric planning (default: all)")
	f.StringSliceVar(&cfg.SkipRules, "skip-rules", nil, "rule IDs to exclude from planning")
	f.StringSliceVar(&cfg.Fixture, "fixture", nil, "read assets from CAI JSON fixtures instead of the API")
	f.StringVar(&cfg.CacheFile, "out", "", "output file (default: stdout)")
	f.IntVar(&cfg.WindowDays, "window", config.DefaultWindowDays, "metric lookback window in days (7-30)")
	f.BoolVar(&noEnrichMetrics, "no-enrich-metrics", false,
		"write a topology-only snapshot (no Cloud Monitoring series); metric-dependent rules will skip on replay")
	return cmd
}

func writeSnapshot(cmd *cobra.Command, path string, gr *graph.Graph, provider, scope string) error {
	if path == "" {
		return gr.WriteSnapshot(cmd.OutOrStdout(), provider, scope)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := gr.WriteSnapshot(f, provider, scope); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d nodes, %d edges to %s\n",
		gr.NodeCount(), gr.EdgeCount(), path)
	return err
}
