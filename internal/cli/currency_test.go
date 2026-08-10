package cli

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/TypeOneLabs/tellury/internal/config"
	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// stubPricer is the minimum pricing.Pricer the currency-resolution seams
// need. resolveScanCurrency only calls SetCurrency when best-effort detection
// succeeds, so the stub needs no currency state of its own.
type stubPricer struct{}

func (stubPricer) MonthlyCost(pricing.Item) (float64, error) { return 0, nil }
func (stubPricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	return 0, region, pricing.ErrNoPrice
}

// reporterPricer is a stub pricer that carries an explicit CurrencyInfo, so
// reportCurrency's delegation to the pricer can be driven without a Cloud
// Billing client.
type reporterPricer struct {
	stubPricer
	info pricing.CurrencyInfo
}

func (r reporterPricer) CurrencyInfo() pricing.CurrencyInfo { return r.info }

// TestResolveScanCurrency_FlagOverridesDetection: precedence is highest for
// the explicit flag. Even when detection would have projects to ask, the flag
// short-circuits and no currency setter is consulted — the flag value was
// already threaded into the pricer at construction.
func TestResolveScanCurrency_FlagOverridesDetection(t *testing.T) {
	cfg := config.Scan{Project: "my-project", Currency: "EUR"}
	gr := projectGraph(t, "my-project")
	state := resolveScanCurrency(context.Background(), cfg, stubPricer{}, gr,
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	if state.requested != "EUR" || state.source != "flag" {
		t.Fatalf("flag case = {%q, %q}, want {EUR, flag}", state.requested, state.source)
	}
}

// TestResolveScanCurrency_OfflineDefaultsToUSD: an offline scan prices
// everything from the embedded USD table and has no cloud client to ask, so
// the only honest answer is the USD default — quietly, exactly like today's
// output.
func TestResolveScanCurrency_OfflineDefaultsToUSD(t *testing.T) {
	cfg := config.Scan{Project: "my-project"}
	state := resolveScanCurrency(context.Background(), cfg, stubPricer{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), true)
	if state.requested != "" || state.source != "default" {
		t.Fatalf("offline case = {%q, %q}, want {, default}", state.requested, state.source)
	}
}

// TestResolveScanCurrency_NoProjectFallsBackToUSD: detection is best effort —
// when no project in scope answers, the scan degrades to USD quietly (a
// missing billing role is a normal state for an otherwise-healthy scan).
func TestResolveScanCurrency_NoProjectFallsBackToUSD(t *testing.T) {
	cfg := config.Scan{Project: "my-project"}
	state := resolveScanCurrency(context.Background(), cfg, stubPricer{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	if state.requested != "" || state.source != "default" {
		t.Fatalf("no-candidate case = {%q, %q}, want {, default}", state.requested, state.source)
	}
}

// TestReportCurrency_BarePricerRequestedEURIsTheTrap: a pricer without
// pricing.CurrencyReporter is a bare embedded-table pricer (an offline scan
// or a --price-file-only build) and always answers in USD. When a non-USD
// currency was requested, every figure is in the wrong currency and the scan
// must say so loudly: effective USD, mixed true.
func TestReportCurrency_BarePricerRequestedEURIsTheTrap(t *testing.T) {
	effective, mixed := reportCurrency(stubPricer{}, currencyResolution{requested: "EUR", source: "flag"})
	if effective != "USD" || !mixed {
		t.Fatalf("bare pricer with EUR requested = {%q, %v}, want {USD, true} (the embedded USD trap)", effective, mixed)
	}
}

// TestReportCurrency_BarePricerDefaultUSDIsQuiet: the default scan requests
// nothing, so a bare pricer answering USD is not a mix — it is today's
// behaviour, unchanged.
func TestReportCurrency_BarePricerDefaultUSDIsQuiet(t *testing.T) {
	effective, mixed := reportCurrency(stubPricer{}, currencyResolution{requested: "", source: "default"})
	if effective != "USD" || mixed {
		t.Fatalf("bare pricer default = {%q, %v}, want {USD, false}", effective, mixed)
	}
}

// TestReportCurrency_DelegatesToPricerReporter: when the pricer can report its
// currency (CatalogPricer after a live load), the effective currency and mix
// come from the pricer — the only component that knows whether the live
// catalogue answered in the requested currency or the embedded USD table did.
func TestReportCurrency_DelegatesToPricerReporter(t *testing.T) {
	live := reporterPricer{info: pricing.CurrencyInfo{Requested: "EUR", Effective: "EUR", Mixed: false}}
	effective, mixed := reportCurrency(live, currencyResolution{requested: "EUR", source: "flag"})
	if effective != "EUR" || mixed {
		t.Fatalf("live EUR pricer = {%q, %v}, want {EUR, false}", effective, mixed)
	}

	contaminated := reporterPricer{info: pricing.CurrencyInfo{Requested: "EUR", Effective: "EUR", Mixed: true}}
	effective, mixed = reportCurrency(contaminated, currencyResolution{requested: "EUR", source: "flag"})
	if effective != "EUR" || !mixed {
		t.Fatalf("contaminated EUR pricer = {%q, %v}, want {EUR, true}", effective, mixed)
	}
}

// TestProjectsInScope_FolderScopeUsesGraphProjects: for a folder/organization
// scope there is no single project to ask, so detection uses the distinct
// projects present in the ingested graph and stops at the first that answers.
func TestProjectsInScope_FolderScopeUsesGraphProjects(t *testing.T) {
	cfg := config.Scan{Folder: "folders/123"}
	gr := projectGraph(t, "alpha-proj", "beta-proj")
	got := projectsInScope(cfg, gr)
	if len(got) != 2 {
		t.Fatalf("projectsInScope(folder) = %v, want the two projects present in the graph", got)
	}
	seen := map[string]bool{}
	for _, p := range got {
		seen[p] = true
	}
	if !seen["alpha-proj"] || !seen["beta-proj"] {
		t.Fatalf("projectsInScope(folder) = %v, want {alpha-proj, beta-proj}", got)
	}
}

// TestProjectsInScope_ExplicitProjectWins: an explicit --gcp-project scope has
// exactly one project to ask; the graph is not consulted.
func TestProjectsInScope_ExplicitProjectWins(t *testing.T) {
	cfg := config.Scan{Project: "my-project"}
	got := projectsInScope(cfg, nil)
	if len(got) != 1 || got[0] != "my-project" {
		t.Fatalf("projectsInScope(project) = %v, want [my-project]", got)
	}
}

// projectGraph builds a frozen graph with one resource node per project (plus
// a project container per project), shaped the way ingestion leaves it.
func projectGraph(t *testing.T, projects ...string) *graph.Graph {
	t.Helper()
	g := graph.New()
	for i, p := range projects {
		proj := &graph.Node{ID: graph.Ref("projects/" + p), Kind: graph.KindProject, Name: p, Project: p}
		if err := g.AddNode(proj); err != nil {
			t.Fatalf("AddNode(project %s): %v", p, err)
		}
		leaf := &graph.Node{
			ID:      graph.Ref(string(rune('a'+i)) + "-leaf"),
			Kind:    graph.KindDisk,
			Name:    "disk-" + p,
			Project: p,
		}
		if err := g.AddNode(leaf); err != nil {
			t.Fatalf("AddNode(leaf %s): %v", p, err)
		}
		if err := g.AddEdge(graph.Edge{From: leaf.ID, To: proj.ID, Kind: graph.EdgeContains}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	g.Freeze()
	return g
}
