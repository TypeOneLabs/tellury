package output

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// ---------------------------------------------------------------------------
// Determining the root for a scope and the rollup arithmetic
// ---------------------------------------------------------------------------

// TestBuildHierarchy_SingleProjectRollsLeavesUp verifies the rollup for the
// canonical single-project shape (the README fixture's product): one project
// container containing one resource with one finding. The container's total
// must equal the leaf's finding waste.
func TestBuildHierarchy_SingleProjectRollsLeavesUp(t *testing.T) {
	g := graph.New()
	project := &graph.Node{
		ID:      graph.Ref("projects/my-project"),
		Kind:    graph.KindProject,
		Name:    "my-project",
		Project: "my-project",
	}
	leaf := &graph.Node{
		ID:       graph.Ref("//compute.googleapis.com/projects/my-project/zones/us-central1-a/disks/pd-standard-01"),
		Kind:     graph.KindDisk,
		Name:     "pd-standard-01",
		Project:  "my-project",
		Location: "us-central1-a",
	}
	for _, n := range []*graph.Node{project, leaf} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := g.AddEdge(graph.Edge{From: leaf.ID, To: project.ID, Kind: graph.EdgeContains}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.Freeze()

	findings := []rules.Finding{
		{
			RuleID:          "detached_disk",
			ResourceID:      leaf.ID,
			Resource:        "disk/pd-standard-01",
			Project:         "my-project",
			MonthlyWasteUSD: 8,
		},
	}

	root := BuildHierarchy(g, findings, "projects/my-project")
	if root == nil {
		t.Fatal("BuildHierarchy returned nil")
	}
	if root.Kind != graph.KindProject {
		t.Errorf("root kind = %q, want project", root.Kind)
	}
	if root.TotalUSD != 8.00 {
		t.Errorf("project total = %.2f, want 8.00 (leaf waste rolls up)", root.TotalUSD)
	}
	if len(root.Children) != 1 {
		t.Fatalf("project has %d children, want 1 (the disk)", len(root.Children))
	}
	if root.Children[0].TotalUSD != 8.00 {
		t.Errorf("disk total = %.2f, want 8.00", root.Children[0].TotalUSD)
	}
	if len(root.Children[0].Findings) != 1 {
		t.Errorf("disk has %d findings, want 1", len(root.Children[0].Findings))
	}
}

// TestBuildHierarchy_MultiProjectRollupAgainstFixture verifies rollup
// arithmetic against a multi-project fixture: an organization containing two
// folders, each containing a project that in turn contains a billable
// resource. Every container's figure must equal the sum of everything beneath
// it, and the organization root must equal the grand total of all findings.
func TestBuildHierarchy_MultiProjectRollupAgainstFixture(t *testing.T) {
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
	// Containment edges, "contained -> container" direction.
	edges := []graph.Edge{
		{From: leafA1.ID, To: projA.ID, Kind: graph.EdgeContains},
		{From: leafA2.ID, To: projA.ID, Kind: graph.EdgeContains},
		{From: leafB.ID, To: projB.ID, Kind: graph.EdgeContains},
		{From: projA.ID, To: folA.ID, Kind: graph.EdgeContains},
		{From: projB.ID, To: folB.ID, Kind: graph.EdgeContains},
		{From: folA.ID, To: org.ID, Kind: graph.EdgeContains},
		{From: folB.ID, To: org.ID, Kind: graph.EdgeContains},
	}
	for _, e := range edges {
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
	if root == nil {
		t.Fatal("BuildHierarchy returned nil")
	}
	if root.TotalUSD != 17.80 {
		t.Errorf("org total = %.2f, want 17.80 (8.00 + 2.50 + 7.30)", root.TotalUSD)
	}
	if len(root.Children) != 2 {
		t.Fatalf("org has %d children, want 2 folders", len(root.Children))
	}
	gotA := root.Children[0]
	if gotA.TotalUSD != 10.50 {
		t.Errorf("folder A total = %.2f, want 10.50", gotA.TotalUSD)
	}
	if len(gotA.Children) != 1 {
		t.Fatalf("folder A has %d children, want 1 project", len(gotA.Children))
	}
	if gotA.Children[0].ID != projA.ID {
		t.Errorf("folder A child = %q, want proj-a", gotA.Children[0].ID)
	}
	if gotA.Children[0].TotalUSD != 10.50 {
		t.Errorf("project A total = %.2f, want 10.50", gotA.Children[0].TotalUSD)
	}
	gotB := root.Children[1]
	if gotB.TotalUSD != 7.30 {
		t.Errorf("folder B total = %.2f, want 7.30", gotB.TotalUSD)
	}
}

// TestBuildHierarchy_UnknownScopeRootProducesSyntheticZero verifies the
// zero-findings edge case: a project scope whose node is absent from the
// graph yields a synthetic root labelled with the scope and a $0.00 total,
// so the report still renders.
func TestBuildHierarchy_UnknownScopeRootProducesSyntheticZero(t *testing.T) {
	g := graph.New()
	g.Freeze()

	root := BuildHierarchy(g, nil, "projects/empty-project")
	if root == nil {
		t.Fatal("BuildHierarchy returned nil")
	}
	if root.Kind != graph.KindProject {
		t.Errorf("synthetic root kind = %q, want project", root.Kind)
	}
	if root.Label != "projects/empty-project" {
		t.Errorf("synthetic root label = %q, want the full scope token", root.Label)
	}
	if root.TotalUSD != 0 {
		t.Errorf("synthetic root total = %.2f, want 0.00", root.TotalUSD)
	}
	if len(root.Children) != 0 || len(root.Findings) != 0 {
		t.Errorf("synthetic root must have no children or findings")
	}
}

// Escaping of hostile names
// ---------------------------------------------------------------------------

// TestRenderHTML_EscapesHostileNames is the security regression test: resource
// names, project IDs, rule IDs and evidence values come from a cloud API and
// are interpolated into HTML. A bucket named with a <script> tag must render
// inert (the literal script text appears, but never as live markup).
func TestRenderHTML_EscapesHostileNames(t *testing.T) {
	g := graph.New()
	project := &graph.Node{ID: "projects/proj-x", Kind: graph.KindProject, Name: "<script>project</script>", Project: "proj-x"}
	leaf := &graph.Node{ID: "//…/buckets/<img src=x onerror=alert(1)>", Kind: graph.KindBucket, Name: "<img src=x onerror=alert(1)>", Project: "proj-x", Location: "us"}
	for _, n := range []*graph.Node{project, leaf} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := g.AddEdge(graph.Edge{From: leaf.ID, To: project.ID, Kind: graph.EdgeContains}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.Freeze()

	hostile := rules.Finding{
		RuleID:          "no_lifecycle_policy<script>alert('x')</script>",
		ResourceID:      leaf.ID,
		Resource:        "bucket/<script>alert('x')</script>",
		Kind:            graph.KindBucket,
		Project:         "<svg/onload=alert(1)>",
		Location:        "<b>loc</b>",
		MonthlyWasteUSD: 3.30,
		Evidence: []rules.Evidence{
			{Key: "bucket_name", Value: "<img src=x onerror=alert(1)>"},
			{Key: "price_source", Value: "embedded_fallback sku=<script>alert(9)</script>"},
		},
	}

	report := Report{
		Scope:                "projects/proj-x",
		Provider:             "gcp",
		GeneratedAt:          time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC),
		WindowDays:           14,
		Findings:             []rules.Finding{hostile},
		TotalMonthlyWasteUSD: 3.30,
		FindingCount:         1,
		ResourcesScanned:     1,
		RulesEvaluated:       3,
	}

	var buf bytes.Buffer
	if err := RenderHTML(&buf, report, BuildHierarchy(g, report.Findings, "projects/proj-x")); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	got := buf.String()

	// The raw hostile markup must NEVER appear verbatim.
	for _, bad := range []string{
		"<script>alert('x')</script>",
		"<img src=x onerror=alert(1)>",
		"<svg/onload=alert(1)>",
		"<script>project</script>",
		"<b>loc</b>",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("hostile markup %q appears raw (unescaped) in the rendered HTML", bad)
		}
	}

	// The escaped forms MUST appear, proving the values are present but inert.
	for _, want := range []string{
		"&lt;script&gt;alert(&#39;x&#39;)&lt;/script&gt;",
		"&lt;img src=x onerror=alert(1)&gt;",
		"&lt;svg/onload=alert(1)&gt;",
		"&lt;script&gt;project&lt;/script&gt;",
		"&lt;b&gt;loc&lt;/b&gt;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("escaped form %q missing from rendered HTML", want)
		}
	}
}

// Zero-findings render
// ---------------------------------------------------------------------------

// TestRenderHTML_ZeroFindingsRenders is the zero-findings acceptance case:
// a project scope with a resource but no findings must render a complete,
// well-formed document whose hierarchy shows a $0.00 root and whose findings
// table says exactly nothing was found.
func TestRenderHTML_ZeroFindingsRenders(t *testing.T) {
	g := graph.New()
	project := &graph.Node{ID: "projects/my-project", Kind: graph.KindProject, Name: "my-project", Project: "my-project"}
	leaf := &graph.Node{ID: "//…/disks/pd-ok", Kind: graph.KindDisk, Name: "pd-ok", Project: "my-project"}
	for _, n := range []*graph.Node{project, leaf} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := g.AddEdge(graph.Edge{From: leaf.ID, To: project.ID, Kind: graph.EdgeContains}); err != nil {
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

	must := []string{
		"<!DOCTYPE html>",
		"$0.00",          // zero rollup root
		"No findings in this scan.",
		`<time datetime="2024-01-20T00:00:00Z">`,
	}
	for _, m := range must {
		if !strings.Contains(got, m) {
			t.Errorf("zero-findings report missing %q", m)
		}
	}
	if strings.Contains(got, "detached_disk") {
		t.Errorf("zero-findings report must not mention any rule id")
	}
}

// Determinism
// ---------------------------------------------------------------------------

// TestRenderHTML_IsDeterministic is the diffability guarantee: the same scan
// (same graph, same findings, same generated-at instant) must render a
// byte-identical document, so consecutive runs can be diffed.
func TestRenderHTML_IsDeterministic(t *testing.T) {
	g := graph.New()
	project := &graph.Node{ID: "projects/my-project", Kind: graph.KindProject, Name: "my-project", Project: "my-project"}
	a := &graph.Node{ID: "//…/disks/za", Kind: graph.KindDisk, Name: "za", Project: "my-project"}
	b := &graph.Node{ID: "//…/addresses/ab", Kind: graph.KindAddress, Name: "ab", Project: "my-project"}
	for _, n := range []*graph.Node{project, a, b} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	// Deliberately add edges in a non-canonical order to prove the renderer
	// normalizes them (children are re-sorted in BuildHierarchy).
	if err := g.AddEdge(graph.Edge{From: b.ID, To: project.ID, Kind: graph.EdgeContains}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge(graph.Edge{From: a.ID, To: project.ID, Kind: graph.EdgeContains}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.Freeze()

	findings := []rules.Finding{
		{RuleID: "detached_disk", ResourceID: a.ID, Resource: "disk/za", Project: "my-project", MonthlyWasteUSD: 8.00},
		{RuleID: "unused_reserved_ip", ResourceID: b.ID, Resource: "address/ab", Project: "my-project", MonthlyWasteUSD: 7.30},
	}

	report := Report{
		Scope:                "projects/my-project",
		Provider:             "gcp",
		GeneratedAt:          time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC),
		WindowDays:           14,
		Findings:             findings,
		TotalMonthlyWasteUSD: 15.30,
		FindingCount:         2,
		ResourcesScanned:     2,
		RulesEvaluated:       4,
	}

	render := func() string {
		var buf bytes.Buffer
		if err := RenderHTML(&buf, report, BuildHierarchy(g, report.Findings, "projects/my-project")); err != nil {
			t.Fatalf("RenderHTML: %v", err)
		}
		return buf.String()
	}
	first := render()
	for i := 0; i < 5; i++ {
		got := render()
		if got != first {
			t.Fatalf("render #%d differs from the first; reports must be byte-identical", i+1)
		}
	}
}

// Tree-total == findings-total invariant
// ---------------------------------------------------------------------------
//
// The one invariant that matters here: the rollup tree must neither drop nor
// double-count a finding, so the sum of every node's own findings in the tree
// equals the sum of the findings list, over a multi-project scope AND over a
// fan-out folder layout (one project under two folders) — the two ways the
// tree can disagree with the findings table, in opposite directions.

// sumTreeFindings walks a rollup tree and sums every finding attached to any
// node. Because BuildHierarchy attaches each finding to exactly one node, and
// that node appears exactly once in the tree, this double-checks that the tree
// has neither dropped a finding (under-count) nor rendered the same finding
// under two branches (over-count).
func sumTreeFindings(root *TreeNode) float64 {
	total := 0.0
	for _, f := range root.Findings {
		total += f.MonthlyWasteUSD
	}
	for _, c := range root.Children {
		total += sumTreeFindings(c)
	}
	return total
}

func sumFindings(fs []rules.Finding) float64 {
	total := 0.0
	for _, f := range fs {
		total += f.MonthlyWasteUSD
	}
	return total
}

// assertTreeMatchesFindings is THE invariant test: the sum of findings
// attached to the rollup tree equals the sum of the findings list. It is the
// single assertion that prevents the whole class of under-count/over-count
// disagreement, and it is what the two scenario tests below drive.
func assertTreeMatchesFindings(t *testing.T, g *graph.Graph, fs []rules.Finding, scope string) {
	t.Helper()
	root := BuildHierarchy(g, fs, scope)
	if root == nil {
		t.Fatal("BuildHierarchy returned nil")
	}
	treeTotal := sumTreeFindings(root)
	findingsTotal := sumFindings(fs)
	if treeTotal != findingsTotal {
		t.Fatalf("tree total %.2f != findings total %.2f: the rollup tree must render every finding exactly once",
			treeTotal, findingsTotal)
	}
	// The root's own figure must agree with what the tree actually carries.
	if root.TotalUSD != treeTotal {
		t.Errorf("root.TotalUSD %.2f != tree-internal findings sum %.2f", root.TotalUSD, treeTotal)
	}
	if root.TotalUSD != findingsTotal {
		t.Errorf("root.TotalUSD %.2f != findings total %.2f", root.TotalUSD, findingsTotal)
	}
}

// TestInvariant_TreeTotalEqualsFindingsTotal_MultiProjectScope verifies the
// invariant against the under-count shape: a project-scoped scan whose fixture
// spans two projects (`alpha-proj` and `beta-proj`), each holding one disk. The
// scope root is `projects/alpha-proj`; the tree descends from it and reaches
// `beta-proj` because beta's disk carries a finding. The tree total must equal
// the findings total ($16.00), never the scope-root subtree sum alone ($8.00).
func TestInvariant_TreeTotalEqualsFindingsTotal_MultiProjectScope(t *testing.T) {
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

	assertTreeMatchesFindings(t, g, findings, "projects/alpha-proj")
}

// TestInvariant_TreeTotalEqualsFindingsTotal_FanOut verifies the invariant
// against the over-count shape: one project under two folders in one
// organization. buildHierarchy emits a project->folder edge per entry in the
// asset's folders list, so the project is reachable from both folders. The tree
// must attach and sum it exactly once (left-root total = findings total =
// $8.00), never once per folder ($16.00).
func TestInvariant_TreeTotalEqualsFindingsTotal_FanOut(t *testing.T) {
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
		// One disk in one project; the project is under BOTH folders.
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

	assertTreeMatchesFindings(t, g, findings, "organizations/99")
}
