package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/TypeOneLabs/tellury/internal/config"
	"github.com/TypeOneLabs/tellury/pkg/cloud/gcp"
	"github.com/TypeOneLabs/tellury/pkg/graph"
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

func newGraphExportCmd(g *globalFlags) *cobra.Command {
	var cfg config.Scan
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Ingest a scope and write the graph as a JSON snapshot",
		Args:  cobra.NoArgs,
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
			assetTypes, _ := rules.Plan(selected)

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
	f.StringSliceVar(&cfg.Fixture, "fixture", nil, "read assets from CAI JSON fixtures instead of the API")
	f.StringVar(&cfg.CacheFile, "out", "", "output file (default: stdout)")
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
