// Renderers for the self-contained HTML report.
//
// The HTML report is a single self-contained file (inline CSS, no JavaScript,
// no CDN, no network fetch at runtime) that is written INTO the artifact
// directory alongside the graph and findings JSON. It has two parts:
//
//   - Part 1: a collapsible rollup hierarchy built from the graph's
//     organization / folder / project container nodes and containment edges.
//     Waste rolls UP: a container's figure is the sum of everything beneath
//     it (structurally, containers can never themselves carry an amount —
//     rules only ever emit findings on leaf resource nodes). The hierarchy
//     shows the scope root and, beneath it, every container that has a finding
//     beneath it — including containers that live outside the scope root's own
//     subtree, so a scan whose fixture spans projects beyond the scope token
//     still renders all of its waste exactly once.
//   - Part 2: a table of the top findings by monthly waste, each row carrying
//     the evidence behind the figure — including which price source produced
//     it.
//
// Determinism: the generator sorts every child slice and every finding group
// under a stable, total ordering, and the single timestamp lives in exactly
// one clearly-marked place in the header. The same scan therefore renders a
// byte-identical report.
//
// Escaping: every string that originates from a cloud API (resource names,
// project IDs, labels, evidence values) is passed through html.EscapeString
// before it is written between tags, so a bucket named with a <script> tag
// cannot execute. Only element text is interpolated — no untrusted value ever
// reaches an attribute.
package output

import (
	"fmt"
	"html"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// TreeNode is one node of the rollup hierarchy. TotalUSD is the node's own
// finding waste plus every descendant's, rounded once (Invariant I3).
type TreeNode struct {
	ID       graph.Ref
	Kind     graph.ResourceKind
	Label    string
	TotalUSD float64
	Findings []rules.Finding
	Children []*TreeNode
}

// BuildHierarchy reconstructs the rollup tree for the given scope from a
// frozen graph's containment edges. Every resource node's findings are
// lifted onto it, and waste is summed bottom-up as the tree is built.
//
// The scope root is the container node whose ID equals the scope token
// ("projects/<id>" | "folders/<n>" | "organizations/<n>"). When the scan
// produced no such node (e.g. an empty scope with zero resources), a
// synthetic root carrying just the scope label is returned so the report
// still renders with a $0.00 total.
//
// The tree descends from the scope root and additionally renders every
// container that has a finding beneath it but does not already fall inside
// the scope root's subtree, as an extra top-level branch off the root. This
// is how the rollup total is kept equal to the findings total even when the
// scan's data spans containers outside the scope token (a --fixture replay
// carries whatever the fixture names, regardless of the scope flag). Ancestors
// above the scope root are never re-rendered, so a finding is never counted
// twice.
//
// A shared visited set is threaded through the recursive walk, so a container
// reachable through more than one containment path (e.g. a project that sits
// under two folders) is attached at its first occurrence only and summed
// exactly once — this is what prevents a fan-out folder layout from
// over-counting the same project.
//
// Determinism: children are surfaced in a stable (Label, then Kind, then ID)
// order and a node's findings are sorted by (waste desc, rule, resource), so
// the same graph and finding set always produce the same tree.
func BuildHierarchy(g *graph.Graph, findings []rules.Finding, scope string) *TreeNode {
	byResource := map[graph.Ref][]rules.Finding{}
	for _, f := range findings {
		byResource[f.ResourceID] = append(byResource[f.ResourceID], f)
	}

	// childrenOf[rootRef] = nodes contained directly by that node, via
	// EdgeContains in-edges (a contained resource points at its container).
	childrenOf := map[graph.Ref][]*graph.Node{}
	g.Nodes(func(n *graph.Node) bool {
		for _, e := range g.In(n.ID) {
			if e.Kind == graph.EdgeContains {
				if child, ok := g.Node(e.From); ok {
					childrenOf[n.ID] = append(childrenOf[n.ID], child)
				}
			}
		}
		return true
	})
	for id := range childrenOf {
		nodes := childrenOf[id]
		sort.Slice(nodes, func(i, j int) bool {
			ai, bi := nodes[i].Display(), nodes[j].Display()
			if ai != bi {
				return ai < bi
			}
			return nodes[i].ID < nodes[j].ID
		})
		childrenOf[id] = nodes
	}

	// The set of containers that have at least one finding somewhere beneath
	// them. Used to decide which containers the tree must render beyond the
	// scope root's own subtree.
	below := containersWithFindingsBelow(g, byResource)

	visited := make(map[graph.Ref]bool)
	rootNode, ok := g.Node(graph.Ref(scope))
	if !ok {
		// No scope node: render a synthetic shell labelled with the scope and
		// hang every top-most finding-bearing container beneath it, so a graph
		// whose scope token is absent still surfaces all of its findings.
		root := &TreeNode{
			Kind:  scopeKind(scope),
			Label: scope,
		}
		for _, id := range topContainers(below, visited, graph.Ref(scope), g, childrenOf) {
			n, present := g.Node(id)
			if !present {
				continue
			}
			if ct := buildTreeNode(n, childrenOf, byResource, visited); ct != nil {
				root.Children = append(root.Children, ct)
			}
		}
		sortChildren(root)
		root.TotalUSD = rollupTotal(root)
		return root
	}

	root := buildTreeNode(rootNode, childrenOf, byResource, visited)

	// Render every container that has a finding beneath it and was not part
	// of the scope root's subtree, as an additional top-level branch of the
	// root. The visited set guarantees no finding is summed twice even when
	// the extra containers overlap or sit above a scope-root descendant.
	for _, id := range topContainers(below, visited, rootNode.ID, g, childrenOf) {
		n, present := g.Node(id)
		if !present {
			continue
		}
		if ct := buildTreeNode(n, childrenOf, byResource, visited); ct != nil {
			root.Children = append(root.Children, ct)
		}
	}
	sortChildren(root)
	root.TotalUSD = rollupTotal(root)
	return root
}

// rollupTotal re-derives a node's TotalUSD as the sum of its own findings and
// every child node's TotalUSD, rounded once. Used on the root after extra
// top-level branches are appended, and on the synthetic root.
func rollupTotal(tn *TreeNode) float64 {
	total := 0.0
	for _, f := range tn.Findings {
		total += f.MonthlyWasteUSD
	}
	for _, c := range tn.Children {
		total += c.TotalUSD
	}
	return pricing.Round2(total)
}

// sortChildren orders a node's children under the stable (Label, then Kind,
// then ID) total ordering used for determinism.
func sortChildren(tn *TreeNode) {
	sort.Slice(tn.Children, func(i, j int) bool {
		a, b := tn.Children[i], tn.Children[j]
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.ID < b.ID
	})
}

// buildTreeNode materializes one node and its full containment subtree. The
// visited set is shared across the entire walk so a node reachable through
// more than one containment path is attached at the first occurrence and
// skipped everywhere else — returning nil for an already-attached node so the
// caller does not add a duplicate branch.
func buildTreeNode(
	n *graph.Node,
	childrenOf map[graph.Ref][]*graph.Node,
	byResource map[graph.Ref][]rules.Finding,
	visited map[graph.Ref]bool,
) *TreeNode {
	if visited[n.ID] {
		return nil
	}
	visited[n.ID] = true

	tn := &TreeNode{
		ID:       n.ID,
		Kind:     n.Kind,
		Label:    n.Name,
		Findings: byResource[n.ID],
	}
	// Stable finding order on this node: waste desc, then rule, then resource.
	fs := tn.Findings
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		switch {
		case a.MonthlyWasteUSD != b.MonthlyWasteUSD:
			return a.MonthlyWasteUSD > b.MonthlyWasteUSD
		case a.RuleID != b.RuleID:
			return a.RuleID < b.RuleID
		default:
			return a.Resource < b.Resource
		}
	})

	var total float64
	for _, f := range tn.Findings {
		total += f.MonthlyWasteUSD
	}
	for _, child := range childrenOf[n.ID] {
		ct := buildTreeNode(child, childrenOf, byResource, visited)
		if ct == nil {
			continue
		}
		total += ct.TotalUSD
		tn.Children = append(tn.Children, ct)
	}
	tn.TotalUSD = pricing.Round2(total)
	return tn
}

// containersWithFindingsBelow returns the set of container refs that have at
// least one finding somewhere beneath them in the containment hierarchy. For
// each finding-having resource the walk climbs the containment chain (out
// edges of kind EdgeContains) marking each container it reaches.
func containersWithFindingsBelow(g *graph.Graph, byResource map[graph.Ref][]rules.Finding) map[graph.Ref]bool {
	below := map[graph.Ref]bool{}
	for res := range byResource {
		seen := map[graph.Ref]bool{res: true}
		stack := []graph.Ref{res}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, e := range g.Out(cur) {
				if e.Kind != graph.EdgeContains {
					continue
				}
				if n, ok := g.Node(e.To); ok && n.Container() {
					below[e.To] = true
				}
				if !seen[e.To] {
					seen[e.To] = true
					stack = append(stack, e.To)
				}
			}
		}
	}
	return below
}

// topContainers returns the set of containers the tree should render as extra
// top-level branches: every container in below that is not yet in visited, is
// not the scope root itself, is not an ancestor of the scope root (a finding
// above the scope is already rolled into the root's subtree and must not be
// re-rendered), and is top-most among the extras (no other, higher unvisited
// container with a finding beneath it also needs rendering — buildTreeNode's
// visited set would already fold it in). The result is returned in stable
// (Ref-sorted) order.
func topContainers(
	below map[graph.Ref]bool,
	visited map[graph.Ref]bool,
	rootID graph.Ref,
	g *graph.Graph,
	childrenOf map[graph.Ref][]*graph.Node,
) []graph.Ref {
	var cands []graph.Ref
	for c := range below {
		if visited[c] || c == rootID {
			continue
		}
		if _, ok := childrenOf[c]; !ok {
			// A container with no children cannot itself carry findings (they
			// live on leaves); a finding beneath it therefore travels through
			// at least one child, which buildTreeNode will reach. Skip it so a
			// leaf-less shell is never rendered empty in place of its subtree.
			continue
		}
		if isAncestor(rootID, c, g) {
			continue // above the scope root: already rolled into the root
		}
		cands = append(cands, c)
	}

	// Keep only the top-most candidates: drop any candidate that has a
	// different, higher candidate as an ancestor (its subtree will be folded
	// into that higher one, so attaching both would render the lower one empty).
	var out []graph.Ref
	for _, c := range cands {
		dominated := false
		for _, c2 := range cands {
			if c2 == c {
				continue
			}
			if isAncestor(c, c2, g) {
				dominated = true
				break
			}
		}
		if !dominated {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// isAncestor reports whether desc can be reached, walking up the containment
// hierarchy (out edges of kind EdgeContains), to anc — i.e. whether anc is an
// ancestor of desc. A BFS explores every containment parent, so a project
// under multiple folders correctly identifies each folder as an ancestor.
func isAncestor(desc, anc graph.Ref, g *graph.Graph) bool {
	if desc == anc {
		return true
	}
	seen := map[graph.Ref]bool{desc: true}
	stack := []graph.Ref{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, e := range g.Out(cur) {
			if e.Kind != graph.EdgeContains {
				continue
			}
			if e.To == anc {
				return true
			}
			if !seen[e.To] {
				seen[e.To] = true
				stack = append(stack, e.To)
			}
		}
	}
	return false
}

// scopeKind maps a scope token prefix onto a container kind for the synthetic
// root that is produced when the scope's node is absent from the graph.
func scopeKind(scope string) graph.ResourceKind {
	switch {
	case strings.HasPrefix(scope, "organizations/"):
		return graph.KindOrganization
	case strings.HasPrefix(scope, "folders/"):
		return graph.KindFolder
	case strings.HasPrefix(scope, "projects/"):
		return graph.KindProject
	default:
		return graph.KindUnknown
	}
}

// RenderHTML writes a complete, self-contained HTML document for the report
// and its rollup hierarchy. The timestamp is emitted in exactly one place —
// the header — so the document is byte-identical for the same scan.
//
// Currency: a default USD scan renders exactly as before (every figure "$…").
// A non-USD scan names its currency in a header disclosure paragraph and
// renders every figure as "12.40 EUR"; when USD embedded-fallback prices
// contaminated the scan, the disclosure is a loud warning so an operator
// reading EUR figures is never silently handed USD numbers.
func RenderHTML(w io.Writer, r Report, root *TreeNode) error {
	if root == nil {
		return fmt.Errorf("html: nil hierarchy root")
	}
	sb := &strings.Builder{}
	sb.WriteString("<!DOCTYPE html>\n")
	sb.WriteString("<html lang=\"en\">\n")
	sb.WriteString("<head>\n")
	sb.WriteString("<meta charset=\"utf-8\">\n")
	fmt.Fprintf(sb, "<title>Tellury waste report — %s</title>\n", esc(r.Scope))
	sb.WriteString(htmlCSS)
	sb.WriteString("</head>\n")
	sb.WriteString("<body>\n")

	// Header — the ONE clearly-marked timestamp, plus the currency disclosure
	// for a non-USD scan (an operator must see which currency the figures are
	// in before reading a single number).
	sb.WriteString("<header>\n")
	sb.WriteString("<h1>Tellury waste report</h1>\n")
	sb.WriteString("<p class=\"meta\">\n")
	fmt.Fprintf(sb, "Scope: <code>%s</code> · Provider: %s · ", esc(r.Scope), esc(r.Provider))
	fmt.Fprintf(sb, "%d-day window · %d resources scanned · %d findings<br>\n",
		r.WindowDays, r.ResourcesScanned, r.FindingCount)
	fmt.Fprintf(sb, "Generated at <time datetime=\"%s\">%s</time> (UTC)\n",
		r.GeneratedAt.UTC().Format(time.RFC3339),
		esc(r.GeneratedAt.UTC().Format(time.RFC3339)))
	sb.WriteString("</p>\n")
	if lines := currencyDisclosure(r); len(lines) > 0 {
		sb.WriteString("<p class=\"currency\">\n")
		for i, line := range lines {
			if i > 0 {
				sb.WriteString("<br>\n")
			}
			fmt.Fprintf(sb, "%s", esc(line))
		}
		sb.WriteString("\n</p>\n")
	}
	sb.WriteString("</header>\n")

	// Part 1 — the hierarchy.
	sb.WriteString("<section id=\"hierarchy\">\n")
	sb.WriteString("<h2>Where the waste is</h2>\n")
	sb.WriteString("<p class=\"hint\">Expand a branch to reveal its children. " +
		"Each figure is the sum of everything below it.</p>\n")
	renderTree(sb, root, r.Currency)
	sb.WriteString("</section>\n")

	// Part 2 — the findings table.
	sb.WriteString("<section id=\"findings\">\n")
	sb.WriteString("<h2>Top findings</h2>\n")
	if len(r.Findings) == 0 {
		sb.WriteString("<p class=\"none\">No findings in this scan.</p>\n")
	} else {
		sorted := sortedFindings(r.Findings)
		limit := 10
		if limit > len(sorted) {
			limit = len(sorted)
		}
		sb.WriteString("<table class=\"findings\">\n")
		writeFindingsHeader(sb)
		sb.WriteString("<tbody>\n")
		for _, f := range sorted[:limit] {
			writeFindingRow(sb, f, r.Currency)
		}
		sb.WriteString("</tbody>\n")
		sb.WriteString("</table>\n")

		if len(sorted) > limit {
			fmt.Fprintf(sb, "<details class=\"more\">\n")
			fmt.Fprintf(sb, "<summary>Show all %d findings</summary>\n", len(sorted))
			sb.WriteString("<table class=\"findings\">\n")
			writeFindingsHeader(sb)
			sb.WriteString("<tbody>\n")
			for _, f := range sorted {
				writeFindingRow(sb, f, r.Currency)
			}
			sb.WriteString("</tbody>\n")
			sb.WriteString("</table>\n")
			sb.WriteString("</details>\n")
		}
	}
	sb.WriteString("</section>\n")

	sb.WriteString("</body>\n")
	sb.WriteString("</html>\n")
	_, err := io.WriteString(w, sb.String())
	return err
}

// sortedFindings returns a stable copy ordered by (waste desc, resource,
// rule), decoupled from whatever --sort the CLI applied so the "top by
// monthly waste" table is always exactly that.
func sortedFindings(fs []rules.Finding) []rules.Finding {
	out := append([]rules.Finding(nil), fs...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.MonthlyWasteUSD != b.MonthlyWasteUSD:
			return a.MonthlyWasteUSD > b.MonthlyWasteUSD
		case a.Resource != b.Resource:
			return a.Resource < b.Resource
		default:
			return a.RuleID < b.RuleID
		}
	})
	return out
}

func renderTree(sb *strings.Builder, root *TreeNode, currency string) {
	sb.WriteString("<ul class=\"tree\">\n")
	renderNodeLi(sb, root, currency)
	sb.WriteString("</ul>\n")
}

func renderNodeLi(sb *strings.Builder, tn *TreeNode, currency string) {
	sb.WriteString("<li>\n<details>\n<summary>\n")
	fmt.Fprintf(sb, "<span class=\"kind\">%s</span> ", esc(string(tn.Kind)))
	if tn.Label == "" {
		fmt.Fprintf(sb, "<span class=\"name\">%s</span>", esc(string(tn.ID)))
	} else {
		fmt.Fprintf(sb, "<span class=\"name\">%s</span>", esc(tn.Label))
	}
	fmt.Fprintf(sb, "<span class=\"cost\">%s</span>\n", moneyHTML(tn.TotalUSD, currency))
	sb.WriteString("</summary>\n")

	if len(tn.Children) > 0 {
		sb.WriteString("<ul>\n")
		for _, c := range tn.Children {
			renderNodeLi(sb, c, currency)
		}
		sb.WriteString("</ul>\n")
	}
	if len(tn.Findings) > 0 {
		sb.WriteString("<ul class=\"findings\">\n")
		for _, f := range tn.Findings {
			renderFindingLi(sb, f, currency)
		}
		sb.WriteString("</ul>\n")
	}
	sb.WriteString("</details>\n</li>\n")
}

func renderFindingLi(sb *strings.Builder, f rules.Finding, currency string) {
	sb.WriteString("<li class=\"finding\">\n")
	fmt.Fprintf(sb, "<span class=\"rule\">%s</span>", esc(f.RuleID))
	fmt.Fprintf(sb, "<span class=\"sev sev-%s\">%s</span>", esc(string(f.Severity)), esc(string(f.Severity)))
	fmt.Fprintf(sb, "<span class=\"cost\">%s</span>/month\n", moneyHTML(f.MonthlyWasteUSD, currency))
	if f.Project != "" {
		fmt.Fprintf(sb, "<span class=\"proj\">in %s</span>\n", esc(f.Project))
	}
	if f.Location != "" {
		fmt.Fprintf(sb, "<span class=\"loc\">· %s</span>\n", esc(f.Location))
	}
	if len(f.Evidence) > 0 {
		sb.WriteString("<ul class=\"evidence\">\n")
		for _, ev := range f.Evidence {
			fmt.Fprintf(sb, "<li><code>%s</code>: %s</li>\n", esc(ev.Key), esc(ev.Value))
		}
		sb.WriteString("</ul>\n")
	}
	sb.WriteString("</li>\n")
}

func writeFindingsHeader(sb *strings.Builder) {
	sb.WriteString("<thead>\n<tr>")
	for _, h := range []string{"Resource", "Project", "Rule", "Monthly waste", "Evidence"} {
		fmt.Fprintf(sb, "<th>%s</th>", h)
	}
	sb.WriteString("</tr>\n</thead>\n")
}

func writeFindingRow(sb *strings.Builder, f rules.Finding, currency string) {
	sb.WriteString("<tr>\n")
	fmt.Fprintf(sb, "<td>%s</td>\n", esc(f.Resource))
	fmt.Fprintf(sb, "<td>%s</td>\n", esc(f.Project))
	fmt.Fprintf(sb, "<td>%s</td>\n", esc(f.RuleID))
	fmt.Fprintf(sb, "<td class=\"num\">%s</td>\n", moneyHTML(f.MonthlyWasteUSD, currency))
	sb.WriteString("<td><ul class=\"evidence\">\n")
	if len(f.Evidence) == 0 {
		sb.WriteString("<li>—</li>\n")
	}
	for _, ev := range f.Evidence {
		fmt.Fprintf(sb, "<li><code>%s</code>: %s</li>\n", esc(ev.Key), esc(ev.Value))
	}
	sb.WriteString("</ul></td>\n")
	sb.WriteString("</tr>\n")
}

func esc(s string) string { return html.EscapeString(s) }

// moneyHTML renders an amount in the report's currency. USD (including the
// empty default) keeps the historical "$12.40" form so a default scan renders
// byte-identically; any other currency appends its code — "12.40 EUR" — so a
// EUR figure can never be mistaken for dollars.
func moneyHTML(v float64, currency string) string {
	v = pricing.Round2(v)
	if currency == "" || currency == "USD" {
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("%.2f %s", v, currency)
}

// htmlCSS is the inline stylesheet. No external resource is referenced, so
// the report is legible on an air-gapped machine and when printed to PDF.
const htmlCSS = `<style>
:root { color-scheme: light; }
* { box-sizing: border-box; }
body { margin: 0 auto; max-width: 1000px; padding: 1.5rem 2rem 3rem;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  color: #1a1d21; background: #fff; font-size: 14px; line-height: 1.45; }
h1 { font-size: 1.6rem; margin: 0 0 0.25rem; }
h2 { font-size: 1.2rem; margin: 1.5rem 0 0.5rem; border-bottom: 1px solid #e1e4e8; padding-bottom: 0.25rem; }
header p.meta { color: #57606a; font-size: 0.9rem; margin: 0 0 1rem; }
p.hint { color: #57606a; font-size: 0.85rem; margin: 0 0 0.75rem; }
p.none { color: #57606a; font-style: italic; }
p.currency { color: #0969da; font-size: 0.9rem; margin: -0.5rem 0 1rem;
  border-left: 3px solid #0969da; padding: 0.35rem 0.6rem; background: #f6f8fa; }
code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  background: #f6f8fa; padding: 0 0.25em; border-radius: 3px; font-size: 0.85em; }

ul.tree { list-style: none; margin: 0; padding-left: 0; }
ul.tree ul { list-style: none; margin: 0; padding-left: 1.4rem; }
ul.tree details { margin: 0.15rem 0; }
ul.tree summary { cursor: pointer; padding: 0.15rem 0; user-select: none;
  display: flex; align-items: baseline; flex-wrap: wrap; gap: 0.4rem; }
ul.tree summary::-webkit-details-marker { color: #8b949e; }
span.kind { font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.05em;
  color: #57606a; border: 1px solid #d0d7de; border-radius: 3px; padding: 0 0.3em; }
span.name { font-weight: 600; }
span.cost { margin-left: auto; font-variant-numeric: tabular-nums; font-weight: 600; }
span.rule { font-weight: 600; }
span.sev { font-size: 0.75rem; border-radius: 3px; padding: 0 0.35em; margin-left: 0.4rem; }
span.sev-high { background: #d73a49; color: #fff; }
span.sev-medium { background: #d4a72c; color: #fff; }
span.sev-low { background: #6f7378; color: #fff; }
span.proj, span.loc { color: #57606a; font-size: 0.85rem; }

ul.findings { list-style: none; margin: 0.25rem 0 0.5rem; padding-left: 1.6rem; }
li.finding { margin: 0.35rem 0; }
ul.evidence { list-style: none; margin: 0.15rem 0 0; padding-left: 0.75rem;
  color: #57606a; font-size: 0.82rem; }

table.findings { width: 100%; border-collapse: collapse; margin: 0.5rem 0 1rem; }
table.findings th, table.findings td { text-align: left; padding: 0.4rem 0.6rem;
  border-bottom: 1px solid #e1e4e8; vertical-align: top; }
table.findings thead th { background: #f6f8fa; font-size: 0.8rem; text-transform: uppercase;
  letter-spacing: 0.04em; color: #57606a; }
table.findings td.num, table.findings th.num { text-align: right; white-space: nowrap; }
table.findings ul.evidence { padding-left: 0; }

details.more { margin-top: 0.5rem; }
details.more summary { cursor: pointer; color: #0969da; font-size: 0.9rem; }

@media print {
  body { max-width: none; padding: 0; }
  details > *:not(summary) { display: block !important; }
  a, .hint { color: #000; }
}
</style>
`
