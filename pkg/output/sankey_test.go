package output

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// ---------------------------------------------------------------------------
// Degradation: single finding / single project
// ---------------------------------------------------------------------------

// TestSankey_SingleFindingIsSingleBand verifies the smallest possible diagram:
// one finding under one org/folder/project chain renders exactly one node per
// tier and one flow per adjacency, and every flow spans its source node's full
// height — a single band whose width is the whole column.
func TestSankey_SingleFindingIsSingleBand(t *testing.T) {
	g := graph.New()
	org := &graph.Node{ID: "organizations/1", Kind: graph.KindOrganization, Name: "org-1"}
	fol := &graph.Node{ID: "folders/10", Kind: graph.KindFolder, Name: "10"}
	proj := &graph.Node{ID: "projects/my-project", Kind: graph.KindProject, Name: "my-project", Project: "my-project"}
	disk := &graph.Node{ID: "//…/disks/pd-01", Kind: graph.KindDisk, Name: "pd-01", Project: "my-project"}
	for _, n := range []*graph.Node{org, fol, proj, disk} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for _, e := range []graph.Edge{
		{From: disk.ID, To: proj.ID, Kind: graph.EdgeContains},
		{From: proj.ID, To: fol.ID, Kind: graph.EdgeContains},
		{From: fol.ID, To: org.ID, Kind: graph.EdgeContains},
	} {
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	g.Freeze()

	findings := []rules.Finding{
		{RuleID: "detached_disk", ResourceID: disk.ID, Resource: "disk/pd-01", Project: "my-project", MonthlyWasteUSD: 8.00},
	}
	root := BuildHierarchy(g, findings, "organizations/1")
	sk := buildSankey(root, findings)

	if sk.Total != 8.00 {
		t.Errorf("sankey total = %.2f, want 8.00", sk.Total)
	}
	if len(sk.Nodes) != 4 {
		t.Fatalf("single finding must render 4 nodes (org, folder, project, rule), got %d", len(sk.Nodes))
	}
	if len(sk.Links) != 3 {
		t.Fatalf("single finding must render 3 flows (org->folder, folder->project, project->rule), got %d", len(sk.Links))
	}
	// One node per tier, each carrying the full findings total, and each flow
	// spans the source node's full height (the single band).
	tierCount := map[int]int{}
	for _, n := range sk.Nodes {
		tierCount[n.Tier]++
		if n.Value != 8.00 {
			t.Errorf("node %s value = %.2f, want 8.00", n.Label, n.Value)
		}
	}
	for tier := sankeyTierOrg; tier <= sankeyTierRule; tier++ {
		if tierCount[tier] != 1 {
			t.Errorf("tier %d has %d nodes, want exactly 1 (single band)", tier, tierCount[tier])
		}
	}
	for _, l := range sk.Links {
		if l.Value != 8.00 {
			t.Errorf("flow %s->%s value = %.2f, want 8.00", l.From, l.To, l.Value)
		}
		src := sk.byKey[l.From]
		if l.s0 != src.y || l.s1 != src.y+src.height {
			t.Errorf("flow %s->%s spans %s..%s, want the full source band %s..%s",
				l.From, l.To, f2(l.s0), f2(l.s1), f2(src.y), f2(src.y+src.height))
		}
	}
}

// TestSankey_SingleProjectHasNoFolderTier verifies the documented degradation:
// a single-project scan has no folder (or organization) tier, so the diagram
// collapses to project -> rule.
func TestSankey_SingleProjectHasNoFolderTier(t *testing.T) {
	g := graph.New()
	proj := &graph.Node{ID: "projects/my-project", Kind: graph.KindProject, Name: "my-project", Project: "my-project"}
	disk := &graph.Node{ID: "//…/disks/pd-01", Kind: graph.KindDisk, Name: "pd-01", Project: "my-project"}
	for _, n := range []*graph.Node{proj, disk} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := g.AddEdge(graph.Edge{From: disk.ID, To: proj.ID, Kind: graph.EdgeContains}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.Freeze()

	findings := []rules.Finding{
		{RuleID: "detached_disk", ResourceID: disk.ID, Resource: "disk/pd-01", Project: "my-project", MonthlyWasteUSD: 8.00},
	}
	sk := buildSankey(BuildHierarchy(g, findings, "projects/my-project"), findings)

	for _, n := range sk.Nodes {
		if n.Tier == sankeyTierOrg || n.Tier == sankeyTierFolder {
			t.Errorf("single-project sankey must have no org/folder tier, found node %q in tier %d", n.Label, n.Tier)
		}
	}
	if tiers := sk.presentTiers(); len(tiers) != 2 || tiers[0] != sankeyTierProject || tiers[1] != sankeyTierRule {
		t.Errorf("single-project sankey tiers = %v, want [project rule]", tiers)
	}
	if len(sk.Nodes) != 2 {
		t.Errorf("single-project sankey must have exactly 2 nodes (project, rule), got %d", len(sk.Nodes))
	}
	if len(sk.Links) != 1 {
		t.Errorf("single-project sankey must have exactly 1 flow, got %d", len(sk.Links))
	}
}

// ---------------------------------------------------------------------------
// The invariant: flows total == findings total
// ---------------------------------------------------------------------------

// assertSankeyMatchesFindings is THE invariant test for the diagram: the total
// flow that reaches the rule tier equals the sum of the findings list, and
// every rendered tier carries the same full total (no tier drops or
// double-counts a finding). It is what prevents the two failure modes that
// broke the hierarchy tree before — dropping containers outside the scope
// root, and double-counting a project reachable from two folders.
func assertSankeyMatchesFindings(t *testing.T, root *TreeNode, findings []rules.Finding) {
	t.Helper()
	sk := buildSankey(root, findings)
	findingsTotal := sumFindings(findings)
	if math.Abs(sk.Total-findingsTotal) > 1e-9 {
		t.Fatalf("sankey total %.2f != findings total %.2f", sk.Total, findingsTotal)
	}
	if flow := sk.FlowTotal(); math.Abs(flow-findingsTotal) > 1e-9 {
		t.Fatalf("sankey flow total %.2f != findings total %.2f: the diagram's flows must total exactly the findings",
			flow, findingsTotal)
	}
	sums := map[int]float64{}
	for _, n := range sk.Nodes {
		sums[n.Tier] += n.Value
	}
	for tier, sum := range sums {
		if math.Abs(sum-findingsTotal) > 1e-9 {
			t.Errorf("tier %d sums to %.2f, want %.2f: every tier must carry the full findings total",
				tier, sum, findingsTotal)
		}
	}
}

// TestInvariant_SankeyFlowsTotalEqualsFindingsTotal_MultiProjectScope drives
// the under-count shape: a project-scoped scan whose fixture spans two
// projects (alpha-proj and beta-proj). The scope root is projects/alpha-proj,
// so the tree hangs beta-proj off it as an extra branch — the shape that once
// dropped a container outside the scope root. The diagram must render both
// projects and its flows must total $16.00, never the scope subtree's $8.00.
func TestInvariant_SankeyFlowsTotalEqualsFindingsTotal_MultiProjectScope(t *testing.T) {
	g := graph.New()

	alpha := &graph.Node{ID: "projects/alpha-proj", Kind: graph.KindProject, Name: "alpha-proj", Project: "alpha-proj"}
	beta := &graph.Node{ID: "projects/beta-proj", Kind: graph.KindProject, Name: "beta-proj", Project: "beta-proj"}
	diskA := &graph.Node{ID: "//…/disks/disk-a", Kind: graph.KindDisk, Name: "disk-a", Project: "alpha-proj"}
	diskB := &graph.Node{ID: "//…/disks/disk-b", Kind: graph.KindDisk, Name: "disk-b", Project: "beta-proj"}

	for _, n := range []*graph.Node{alpha, beta, diskA, diskB} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for _, e := range []graph.Edge{
		{From: diskA.ID, To: alpha.ID, Kind: graph.EdgeContains},
		{From: diskB.ID, To: beta.ID, Kind: graph.EdgeContains},
	} {
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	g.Freeze()

	findings := []rules.Finding{
		{RuleID: "detached_disk", ResourceID: diskA.ID, Resource: "disk/disk-a", Project: "alpha-proj", MonthlyWasteUSD: 8.00},
		{RuleID: "detached_disk", ResourceID: diskB.ID, Resource: "disk/disk-b", Project: "beta-proj", MonthlyWasteUSD: 8.00},
	}

	root := BuildHierarchy(g, findings, "projects/alpha-proj")
	assertSankeyMatchesFindings(t, root, findings)

	// Both projects must be present as separate project-tier nodes — this is
	// the regression guard for "dropping containers outside the scope root".
	sk := buildSankey(root, findings)
	projects := map[string]float64{}
	for _, n := range sk.Nodes {
		if n.Tier == sankeyTierProject {
			projects[n.Label] = n.Value
		}
	}
	if projects["alpha-proj"] != 8.00 || projects["beta-proj"] != 8.00 {
		t.Errorf("multi-project diagram must carry both projects ($8 each), got %v", projects)
	}
}

// TestInvariant_SankeyFlowsTotalEqualsFindingsTotal_FanOut drives the
// over-count shape: one project under two folders in one organization. The
// diagram must attach the project under exactly one folder (the tree's
// de-duplicated containment) and its flows must total $8.00 — never $16.00.
func TestInvariant_SankeyFlowsTotalEqualsFindingsTotal_FanOut(t *testing.T) {
	g := graph.New()

	org := &graph.Node{ID: "organizations/99", Kind: graph.KindOrganization, Name: "99"}
	folA := &graph.Node{ID: "folders/full-a", Kind: graph.KindFolder, Name: "full-a"}
	folB := &graph.Node{ID: "folders/full-b", Kind: graph.KindFolder, Name: "full-b"}
	proj := &graph.Node{ID: "projects/p", Kind: graph.KindProject, Name: "p", Project: "p"}
	disk := &graph.Node{ID: "//…/disks/pd", Kind: graph.KindDisk, Name: "pd", Project: "p"}

	for _, n := range []*graph.Node{org, folA, folB, proj, disk} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for _, e := range []graph.Edge{
		{From: disk.ID, To: proj.ID, Kind: graph.EdgeContains},
		{From: proj.ID, To: folA.ID, Kind: graph.EdgeContains},
		{From: proj.ID, To: folB.ID, Kind: graph.EdgeContains},
		{From: folA.ID, To: org.ID, Kind: graph.EdgeContains},
		{From: folB.ID, To: org.ID, Kind: graph.EdgeContains},
	} {
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	g.Freeze()

	findings := []rules.Finding{
		{RuleID: "detached_disk", ResourceID: disk.ID, Resource: "disk/pd", Project: "p", MonthlyWasteUSD: 8.00},
	}

	root := BuildHierarchy(g, findings, "organizations/99")
	assertSankeyMatchesFindings(t, root, findings)

	// The project must sit under exactly ONE folder — the over-count guard.
	sk := buildSankey(root, findings)
	folderLinks := 0
	for _, l := range sk.Links {
		if strings.HasPrefix(l.From, "folder:") && strings.HasPrefix(l.To, "project:") {
			folderLinks++
			if l.Value != 8.00 {
				t.Errorf("folder->project flow value = %.2f, want 8.00", l.Value)
			}
		}
	}
	if folderLinks != 1 {
		t.Errorf("fan-out project must flow from exactly one folder, got %d folder->project flows", folderLinks)
	}
}

// TestInvariant_SankeyFlowsTotalEqualsFindingsTotal_FullOrgChain exercises the
// full four-tier diagram: an organization with two folders, each holding a
// project, holding resources with findings under two different rules. Every
// tier (org, folder, project, rule) must carry the full $17.80 total.
func TestInvariant_SankeyFlowsTotalEqualsFindingsTotal_FullOrgChain(t *testing.T) {
	g := graph.New()

	org := &graph.Node{ID: "organizations/111", Kind: graph.KindOrganization, Name: "111"}
	folA := &graph.Node{ID: "folders/10", Kind: graph.KindFolder, Name: "10"}
	folB := &graph.Node{ID: "folders/20", Kind: graph.KindFolder, Name: "20"}
	projA := &graph.Node{ID: "projects/proj-a", Kind: graph.KindProject, Name: "proj-a", Project: "proj-a"}
	projB := &graph.Node{ID: "projects/proj-b", Kind: graph.KindProject, Name: "proj-b", Project: "proj-b"}
	leafA1 := &graph.Node{ID: "//…/disks/a1", Kind: graph.KindDisk, Name: "a1", Project: "proj-a"}
	leafA2 := &graph.Node{ID: "//…/disks/a2", Kind: graph.KindDisk, Name: "a2", Project: "proj-a"}
	leafB := &graph.Node{ID: "//…/addresses/b1", Kind: graph.KindAddress, Name: "b1", Project: "proj-b"}

	for _, n := range []*graph.Node{org, folA, folB, projA, projB, leafA1, leafA2, leafB} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for _, e := range []graph.Edge{
		{From: leafA1.ID, To: projA.ID, Kind: graph.EdgeContains},
		{From: leafA2.ID, To: projA.ID, Kind: graph.EdgeContains},
		{From: leafB.ID, To: projB.ID, Kind: graph.EdgeContains},
		{From: projA.ID, To: folA.ID, Kind: graph.EdgeContains},
		{From: projB.ID, To: folB.ID, Kind: graph.EdgeContains},
		{From: folA.ID, To: org.ID, Kind: graph.EdgeContains},
		{From: folB.ID, To: org.ID, Kind: graph.EdgeContains},
	} {
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	g.Freeze()

	findings := []rules.Finding{
		{RuleID: "detached_disk", ResourceID: leafA1.ID, Resource: "disk/a1", Project: "proj-a", MonthlyWasteUSD: 8.00},
		{RuleID: "detached_disk", ResourceID: leafA2.ID, Resource: "disk/a2", Project: "proj-a", MonthlyWasteUSD: 2.50},
		{RuleID: "unused_reserved_ip", ResourceID: leafB.ID, Resource: "address/b1", Project: "proj-b", MonthlyWasteUSD: 7.30},
	}

	root := BuildHierarchy(g, findings, "organizations/111")
	assertSankeyMatchesFindings(t, root, findings)

	// The full chain must be present: one org, two folders, two projects, two
	// rules, with the expected aggregate values on each.
	sk := buildSankey(root, findings)
	got := map[string]float64{}
	for _, n := range sk.Nodes {
		got[fmt.Sprintf("%d:%s", n.Tier, n.Label)] = n.Value
	}
	want := map[string]float64{
		"0:111":                17.80,
		"1:10":                 10.50,
		"1:20":                 7.30,
		"2:proj-a":             10.50,
		"2:proj-b":             7.30,
		"3:detached_disk":      10.50,
		"3:unused_reserved_ip": 7.30,
	}
	for key, v := range want {
		if math.Abs(got[key]-v) > 1e-9 {
			t.Errorf("node %s = %.2f, want %.2f", key, got[key], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("sankey has %d nodes, want %d (%v)", len(got), len(want), got)
	}
}

// ---------------------------------------------------------------------------
// Capping a large scan into an explicit "other" band
// ---------------------------------------------------------------------------

// TestSankey_CapsLargeTiersIntoOther verifies the degradation for a scan too
// large to draw: an organization holding 14 projects (above the per-tier cap
// of 12) keeps the top 12 projects and aggregates the remaining two into one
// explicit "other (2 projects)" node. The diagram's flows must still total
// the findings — nothing is dropped, it is only re-banded.
func TestSankey_CapsLargeTiersIntoOther(t *testing.T) {
	root := &TreeNode{
		ID:    graph.Ref("organizations/1"),
		Kind:  graph.KindOrganization,
		Label: "org-1",
	}
	var findings []rules.Finding
	for i := 0; i < 14; i++ {
		name := fmt.Sprintf("p%02d", i)
		projID := graph.Ref("projects/" + name)
		diskID := graph.Ref(fmt.Sprintf("//…/disks/d%02d", i))
		f := rules.Finding{
			RuleID:          "detached_disk",
			ResourceID:      diskID,
			Resource:        "disk/d" + name,
			Project:         name,
			MonthlyWasteUSD: 1.00,
		}
		disk := &TreeNode{ID: diskID, Kind: graph.KindDisk, Label: fmt.Sprintf("d%02d", i), Findings: []rules.Finding{f}}
		proj := &TreeNode{ID: projID, Kind: graph.KindProject, Label: name}
		proj.Children = []*TreeNode{disk}
		root.Children = append(root.Children, proj)
		findings = append(findings, f)
	}
	root.TotalUSD = 14

	sk := buildSankey(root, findings)
	if sk.Total != 14.00 {
		t.Errorf("capped sankey total = %.2f, want 14.00", sk.Total)
	}
	if flow := sk.FlowTotal(); flow != 14.00 {
		t.Errorf("capped sankey flow total = %.2f, want 14.00 (capping must never drop flow)", flow)
	}

	projects := 0
	otherFound := false
	var projectToRule float64
	for _, n := range sk.Nodes {
		if n.Tier != sankeyTierProject {
			continue
		}
		projects++
		if strings.HasPrefix(n.Label, "other (") {
			otherFound = true
			if n.Label != "other (2 projects)" {
				t.Errorf("capped aggregate label = %q, want \"other (2 projects)\"", n.Label)
			}
		}
		for _, l := range sk.Links {
			if l.From == n.Key {
				projectToRule += l.Value
			}
		}
	}
	if projects != sankeyMaxNodesPerTier+1 {
		t.Errorf("project tier has %d nodes, want %d (12 kept + 1 other)", projects, sankeyMaxNodesPerTier+1)
	}
	if !otherFound {
		t.Errorf("large scan must render an explicit \"other\" band")
	}
	if projectToRule != 14.00 {
		t.Errorf("project-tier out-flows sum to %.2f, want 14.00", projectToRule)
	}
}

// ---------------------------------------------------------------------------
// The SVG in the rendered document
// ---------------------------------------------------------------------------

// TestRenderHTML_IncludesSankeyOverview renders a full org->folder->project
// report and asserts the Sankey overview is present above the hierarchy tree,
// that the diagram is built from inline SVG (no script anywhere), and that
// every hostile label it carries is escaped exactly like the tree and table.
func TestRenderHTML_IncludesSankeyOverview(t *testing.T) {
	g := graph.New()
	org := &graph.Node{ID: "organizations/1", Kind: graph.KindOrganization, Name: "org-1"}
	fol := &graph.Node{ID: "folders/10", Kind: graph.KindFolder, Name: "10"}
	proj := &graph.Node{ID: "projects/my-project", Kind: graph.KindProject, Name: "<script>my-project</script>", Project: "my-project"}
	disk := &graph.Node{ID: "//…/disks/pd-01", Kind: graph.KindDisk, Name: "pd-01", Project: "my-project"}
	for _, n := range []*graph.Node{org, fol, proj, disk} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for _, e := range []graph.Edge{
		{From: disk.ID, To: proj.ID, Kind: graph.EdgeContains},
		{From: proj.ID, To: fol.ID, Kind: graph.EdgeContains},
		{From: fol.ID, To: org.ID, Kind: graph.EdgeContains},
	} {
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	g.Freeze()

	findings := []rules.Finding{
		{RuleID: "detached_disk<script>alert(9)</script>", ResourceID: disk.ID, Resource: "disk/pd-01", Project: "my-project", MonthlyWasteUSD: 8.00},
	}
	report := Report{
		Scope:                "organizations/1",
		Provider:             "gcp",
		GeneratedAt:          time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC),
		WindowDays:           14,
		Findings:             findings,
		TotalMonthlyWasteUSD: 8.00,
		FindingCount:         1,
		ResourcesScanned:     1,
		RulesEvaluated:       1,
	}

	var buf bytes.Buffer
	if err := RenderHTML(&buf, report, BuildHierarchy(g, findings, "organizations/1")); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		`id="sankey"`,
		"<svg",
		`<path`,
		`<rect`,
		`<text`,
		"Waste flow",
		"Total monthly waste: $8.00 across 1 finding.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing sankey element %q", want)
		}
	}
	// The diagram is an overview ABOVE the tree, never a replacement for it.
	if i, j := strings.Index(got, `id="sankey"`), strings.Index(got, `id="hierarchy"`); i < 0 || j < 0 || i > j {
		t.Errorf("sankey section must precede the hierarchy section (sankey@%d, hierarchy@%d)", i, j)
	}
	// The tree and table must still be there.
	for _, want := range []string{"Where the waste is", "Top findings", "<details>"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lost existing section %q", want)
		}
	}
	// No script element anywhere, and every hostile label escaped (SVG text is
	// element text and must be escaped exactly like HTML text).
	if strings.Contains(got, "<script") {
		t.Errorf("report must remain script-free; found a raw script tag")
	}
	for _, raw := range []string{"<script>my-project</script>", "<script>alert(9)</script>"} {
		if strings.Contains(got, raw) {
			t.Errorf("hostile label %q appears raw in the Sankey SVG", raw)
		}
	}
	for _, escForm := range []string{
		"&lt;script&gt;my-project&lt;/script&gt;",
		"&lt;script&gt;alert(9)&lt;/script&gt;",
	} {
		if !strings.Contains(got, escForm) {
			t.Errorf("escaped label %q missing from the Sankey SVG", escForm)
		}
	}
}

// TestRenderHTML_ZeroFindingsHasNoSankeySection pins the degradation for an
// empty scan: nothing wasteful was found, so there is no flow to draw and the
// report must not emit a Sankey section at all — the tree's "$0.00" root is
// the honest statement.
func TestRenderHTML_ZeroFindingsHasNoSankeySection(t *testing.T) {
	g := graph.New()
	proj := &graph.Node{ID: "projects/my-project", Kind: graph.KindProject, Name: "my-project", Project: "my-project"}
	disk := &graph.Node{ID: "//…/disks/pd-ok", Kind: graph.KindDisk, Name: "pd-ok", Project: "my-project"}
	for _, n := range []*graph.Node{proj, disk} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := g.AddEdge(graph.Edge{From: disk.ID, To: proj.ID, Kind: graph.EdgeContains}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.Freeze()

	report := Report{
		Scope:                "projects/my-project",
		Provider:             "gcp",
		GeneratedAt:          time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC),
		WindowDays:           14,
		Findings:             nil,
		TotalMonthlyWasteUSD: 0,
		FindingCount:         0,
		ResourcesScanned:     1,
		RulesEvaluated:       4,
	}

	var buf bytes.Buffer
	if err := RenderHTML(&buf, report, BuildHierarchy(g, report.Findings, "projects/my-project")); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, `id="sankey"`) {
		t.Errorf("zero-findings report must not render a Sankey section:\n%s", got)
	}
	if !strings.Contains(got, "$0.00") {
		t.Errorf("zero-findings report must still render the $0.00 rollup root")
	}
}
