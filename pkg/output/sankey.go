// Sankey flow diagram for the self-contained HTML report.
//
// The report opens on an air-gapped machine and must survive being emailed
// around, so the diagram is computed entirely in Go and emitted as inline
// SVG: rectangles for nodes, filled cubic-Bézier paths for flows, <text> for
// labels. There is no <script> element, no external font and no remote image
// anywhere in the document.
//
// Model: each finding contributes its monthly waste to exactly one
// project->rule flow, and — when the rollup tree carries the containment
// chain — one folder->project and one organization->folder flow on top, so
// the flows of the diagram total exactly the findings total. The tree is
// already de-duplicated (a project reachable from two folders is attached at
// its first occurrence only), so a fan-out hierarchy can never double-count a
// project. Aggregation is additive at every step, so capping a tier into an
// "other" node preserves the total.
//
// Layout: tiers are columns (organization -> folder -> project -> rule) and a
// tier that has no nodes — a single-project scan has no folder or
// organization tier — is simply skipped. Node heights are proportional to the
// monthly waste flowing through them (with a minimum tall enough for a
// two-line label), and each flow occupies a value-proportional sub-range of
// its source and target node, packed top-down, so a band's width reads
// directly as its monthly waste. A tier with more than sankeyMaxNodesPerTier
// nodes keeps the top nodes by value and aggregates the remainder into an
// explicit "other (N projects)" node, so a 200-project scan stays legible
// without dropping a cent of flow.
package output

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// Sankey layout tiers, in flow order: waste starts at the organization and
// ends at the rule that flagged it. A tier with no nodes is skipped, so a
// single-project scan renders only the project and rule columns.
const (
	sankeyTierOrg = iota
	sankeyTierFolder
	sankeyTierProject
	sankeyTierRule
)

// Sankey geometry and layout constants. The layout is computed in Go and
// emitted as inline SVG, so every figure here is a plain number in the
// emitted document.
const (
	sankeyNodeWidth     = 130.0 // node rect width; the label renders inside it
	sankeyColGap        = 150.0 // horizontal gap between adjacent tier columns
	sankeyPadX          = 16.0
	sankeyPadY          = 12.0
	sankeyTargetHeight  = 460.0 // desired content height when a tier carries the whole findings total
	sankeyMinNodeHeight = 30.0  // tall enough for a two-line label (name + amount)
	sankeyNodeGap       = 5.0
	sankeyLineH         = 13.0
	sankeyMaxLabelRunes = 18 // labels are truncated to fit inside the node rect
	// sankeyMaxNodesPerTier caps how many nodes a tier may render. A scan with
	// 200 projects cannot draw 200 legible rows; the excess is aggregated into
	// one explicit "other (N projects)" node per tier so the diagram stays
	// readable and the flows still total the findings.
	sankeyMaxNodesPerTier = 12
)

// SankeyNode is one node of the flow diagram: an organization, folder,
// project or rule. Value is the monthly waste that flows through the node —
// for a rule it is the finding total for that rule, for a container it is
// the total of everything beneath it.
type SankeyNode struct {
	Key   string
	Label string
	Kind  graph.ResourceKind
	Tier  int
	Value float64

	// Geometry, filled by layout().
	x, y, height float64
}

// SankeyLink is one flow between adjacent tiers. Value is the monthly waste
// the flow carries; the rendered band's width is proportional to Value.
type SankeyLink struct {
	From  string
	To    string
	Value float64

	// Geometry: the vertical sub-range of the source node (s0..s1) and of the
	// target node (t0..t1) this flow occupies, filled by layout().
	s0, s1, t0, t1 float64
}

// Sankey is a computed flow diagram over the resource hierarchy.
type Sankey struct {
	Nodes []*SankeyNode
	Links []*SankeyLink
	Total float64 // sum of the finding values the diagram was built from

	byKey               map[string]*SankeyNode
	svgWidth, svgHeight float64
	laidOut             bool
}

// linkPair is the aggregate key of a (from, to) flow: several findings
// between the same two nodes merge into one flow whose value is the sum.
type linkPair struct{ from, to string }

// buildSankey computes the flow diagram for a report from its rollup tree and
// finding list. The tree carries the de-duplicated containment hierarchy (a
// project reachable through two folders appears once), so every finding flows
// through exactly one chain, and the diagram's flows total exactly the
// findings total. The returned Sankey is capped and laid out, ready to render.
func buildSankey(root *TreeNode, findings []rules.Finding) *Sankey {
	sk := &Sankey{}
	nodes := map[string]*SankeyNode{}
	links := map[linkPair]float64{}
	var linkOrder []linkPair

	addNode := func(key, label string, kind graph.ResourceKind, tier int) *SankeyNode {
		if n, ok := nodes[key]; ok {
			return n
		}
		n := &SankeyNode{Key: key, Label: label, Kind: kind, Tier: tier}
		nodes[key] = n
		sk.Nodes = append(sk.Nodes, n)
		return n
	}
	addLink := func(from, to string, v float64) {
		p := linkPair{from, to}
		if _, ok := links[p]; !ok {
			linkOrder = append(linkOrder, p)
		}
		links[p] += v
	}

	if root != nil {
		// Index the rollup tree and its parent pointers so every finding's
		// resource node can be walked up to its project, folder and org.
		byRef := map[graph.Ref]*TreeNode{}
		parent := map[graph.Ref]*TreeNode{}
		var walk func(n, p *TreeNode)
		walk = func(n, p *TreeNode) {
			byRef[n.ID] = n
			if p != nil {
				parent[n.ID] = p
			}
			for _, c := range n.Children {
				walk(c, n)
			}
		}
		walk(root, nil)

		for _, f := range findings {
			sk.Total += f.MonthlyWasteUSD
			ruleKey := "rule:" + f.RuleID
			ruleNode := addNode(ruleKey, f.RuleID, graph.KindUnknown, sankeyTierRule)
			ruleNode.Value += f.MonthlyWasteUSD

			tn := byRef[f.ResourceID]
			if tn == nil {
				// Defensive fallback: a finding whose resource is not in the
				// rollup tree still flows. Attach it to a project node derived
				// from the finding's project so the diagram total always
				// equals the findings total.
				pk := "project:projects/" + f.Project
				pnode := addNode(pk, f.Project, graph.KindProject, sankeyTierProject)
				pnode.Value += f.MonthlyWasteUSD
				addLink(pk, ruleKey, f.MonthlyWasteUSD)
				continue
			}

			// Nearest ancestor of each container kind. The tree is a tree, so
			// each kind is found at most once per finding; the first one met
			// walking up is the nearest.
			var proj, fol, org *TreeNode
			for cur := tn; cur != nil; cur = parent[cur.ID] {
				switch cur.Kind {
				case graph.KindProject:
					if proj == nil {
						proj = cur
					}
				case graph.KindFolder:
					if fol == nil {
						fol = cur
					}
				case graph.KindOrganization:
					if org == nil {
						org = cur
					}
				}
			}
			if proj == nil {
				pk := "project:projects/" + f.Project
				pnode := addNode(pk, f.Project, graph.KindProject, sankeyTierProject)
				pnode.Value += f.MonthlyWasteUSD
				addLink(pk, ruleKey, f.MonthlyWasteUSD)
				continue
			}

			pk := "project:" + string(proj.ID)
			pnode := addNode(pk, treeLabel(proj), graph.KindProject, sankeyTierProject)
			pnode.Value += f.MonthlyWasteUSD
			addLink(pk, ruleKey, f.MonthlyWasteUSD)

			// Chain the containers above the project: org -> folder -> project
			// when both exist, org -> project when the folder tier is absent,
			// and folder -> project when there is no organization.
			switch {
			case fol != nil && org != nil:
				fk := "folder:" + string(fol.ID)
				fnode := addNode(fk, treeLabel(fol), graph.KindFolder, sankeyTierFolder)
				fnode.Value += f.MonthlyWasteUSD
				addLink(fk, pk, f.MonthlyWasteUSD)
				ok := "org:" + string(org.ID)
				onode := addNode(ok, treeLabel(org), graph.KindOrganization, sankeyTierOrg)
				onode.Value += f.MonthlyWasteUSD
				addLink(ok, fk, f.MonthlyWasteUSD)
			case fol != nil:
				fk := "folder:" + string(fol.ID)
				fnode := addNode(fk, treeLabel(fol), graph.KindFolder, sankeyTierFolder)
				fnode.Value += f.MonthlyWasteUSD
				addLink(fk, pk, f.MonthlyWasteUSD)
			case org != nil:
				ok := "org:" + string(org.ID)
				onode := addNode(ok, treeLabel(org), graph.KindOrganization, sankeyTierOrg)
				onode.Value += f.MonthlyWasteUSD
				addLink(ok, pk, f.MonthlyWasteUSD)
			}
		}
	}

	for _, p := range linkOrder {
		sk.Links = append(sk.Links, &SankeyLink{From: p.from, To: p.to, Value: links[p]})
	}
	if sk.Total <= 0 {
		// Nothing flows: return an empty diagram so the renderer skips the
		// section entirely instead of drawing zero-width bands.
		return &Sankey{}
	}

	sk.cap()
	sk.layout()
	return sk
}

// treeLabel returns the human label of a rollup tree node, falling back to
// the node's ID when the graph carried no display name.
func treeLabel(n *TreeNode) string {
	if n.Label != "" {
		return n.Label
	}
	return string(n.ID)
}

// cap keeps at most sankeyMaxNodesPerTier nodes per tier, ordered by value
// (desc), and aggregates the remainder into one explicit "other (N …)" node
// per tier. Because capping only merges node values and re-links every flow
// (summing duplicates), the diagram's total flow is unchanged: nothing is
// dropped, it is simply drawn under an "other" band.
func (s *Sankey) cap() {
	for tier := sankeyTierOrg; tier <= sankeyTierRule; tier++ {
		nodes := s.nodesInTier(tier)
		if len(nodes) <= sankeyMaxNodesPerTier {
			continue
		}
		merged := nodes[sankeyMaxNodesPerTier:]

		remap := make(map[string]string, len(merged))
		otherVal := 0.0
		for _, m := range merged {
			remap[m.Key] = sankeyOtherKey(tier)
			otherVal += m.Value
		}

		agg := make(map[linkPair]float64)
		var order []linkPair
		for _, l := range s.Links {
			from, to := l.From, l.To
			if r, ok := remap[from]; ok {
				from = r
			}
			if r, ok := remap[to]; ok {
				to = r
			}
			p := linkPair{from, to}
			if _, seen := agg[p]; !seen {
				order = append(order, p)
			}
			agg[p] += l.Value
		}

		var newNodes []*SankeyNode
		for _, n := range s.Nodes {
			if _, mergedAway := remap[n.Key]; mergedAway {
				continue
			}
			newNodes = append(newNodes, n)
		}
		if otherVal > 0 {
			newNodes = append(newNodes, &SankeyNode{
				Key:   sankeyOtherKey(tier),
				Label: sankeyOtherLabel(tier, len(merged)),
				Kind:  merged[0].Kind,
				Tier:  tier,
				Value: otherVal,
			})
		}

		newLinks := make([]*SankeyLink, 0, len(order))
		for _, p := range order {
			newLinks = append(newLinks, &SankeyLink{From: p.from, To: p.to, Value: agg[p]})
		}
		s.Nodes = newNodes
		s.Links = newLinks
	}
}

// layout assigns geometry to every node and flow: tier columns,
// value-proportional node heights, and per-flow sub-ranges packed top-down
// within each node. It is idempotent and fully deterministic, so the same
// Sankey always renders the same SVG. Called once by buildSankey; renderSVG
// then only emits.
func (s *Sankey) layout() {
	if s.laidOut {
		return
	}
	s.laidOut = true
	if len(s.Nodes) == 0 || s.Total <= 0 {
		return
	}

	s.byKey = make(map[string]*SankeyNode, len(s.Nodes))
	for _, n := range s.Nodes {
		s.byKey[n.Key] = n
	}

	tiers := s.presentTiers()
	colOf := make(map[int]int, len(tiers))
	for i, t := range tiers {
		colOf[t] = i
	}
	scale := sankeyTargetHeight / s.Total

	maxBottom := 0.0
	for _, t := range tiers {
		x := sankeyPadX + float64(colOf[t])*(sankeyNodeWidth+sankeyColGap)
		y := sankeyPadY
		for _, n := range s.nodesInTier(t) {
			h := n.Value * scale
			if h < sankeyMinNodeHeight {
				h = sankeyMinNodeHeight
			}
			n.x, n.y, n.height = x, y, h
			y += h + sankeyNodeGap
		}
		bottom := y - sankeyNodeGap + sankeyPadY
		if bottom > maxBottom {
			maxBottom = bottom
		}
	}
	s.svgHeight = maxBottom
	s.svgWidth = sankeyPadX*2 + float64(len(tiers))*sankeyNodeWidth + float64(len(tiers)-1)*sankeyColGap

	// Per-flow sub-ranges. A node's out-flows are packed top-down in (target
	// y, target key) order and its in-flows in (source y, source key) order,
	// so flows fan out and join without gratuitous crossing. Each flow's
	// height is exactly its value share of the node, so its width is
	// proportional to its monthly waste.
	for _, n := range s.Nodes {
		outs := s.outLinks(n.Key)
		sort.SliceStable(outs, func(i, j int) bool {
			a, b := outs[i], outs[j]
			ta, tb := s.node(a.To), s.node(b.To)
			if ta.y != tb.y {
				return ta.y < tb.y
			}
			return ta.Key < tb.Key
		})
		off := 0.0
		for _, l := range outs {
			lh := l.Value * scale
			l.s0, l.s1 = n.y+off, n.y+off+lh
			off += lh
		}

		ins := s.inLinks(n.Key)
		sort.SliceStable(ins, func(i, j int) bool {
			a, b := ins[i], ins[j]
			sa, sb := s.node(a.From), s.node(b.From)
			if sa.y != sb.y {
				return sa.y < sb.y
			}
			return sa.Key < sb.Key
		})
		off = 0.0
		for _, l := range ins {
			lh := l.Value * scale
			l.t0, l.t1 = n.y+off, n.y+off+lh
			off += lh
		}
	}

	// Stable document order for rendering, independent of input order.
	sort.SliceStable(s.Nodes, func(i, j int) bool {
		a, b := s.Nodes[i], s.Nodes[j]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if a.Value != b.Value {
			return a.Value > b.Value
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.Key < b.Key
	})
	sort.SliceStable(s.Links, func(i, j int) bool {
		a, b := s.Links[i], s.Links[j]
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
}

// presentTiers returns the tiers that actually carry nodes, in ascending
// order. Missing tiers (no organization, no folder) simply disappear from the
// column layout.
func (s *Sankey) presentTiers() []int {
	seen := map[int]bool{}
	for _, n := range s.Nodes {
		seen[n.Tier] = true
	}
	var out []int
	for t := sankeyTierOrg; t <= sankeyTierRule; t++ {
		if seen[t] {
			out = append(out, t)
		}
	}
	return out
}

// nodesInTier returns a tier's nodes ordered by (value desc, label, key) —
// the order used both for capping (keep the biggest) and for column stacking.
func (s *Sankey) nodesInTier(tier int) []*SankeyNode {
	var out []*SankeyNode
	for _, n := range s.Nodes {
		if n.Tier == tier {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Value != b.Value {
			return a.Value > b.Value
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.Key < b.Key
	})
	return out
}

func (s *Sankey) outLinks(key string) []*SankeyLink {
	var out []*SankeyLink
	for _, l := range s.Links {
		if l.From == key {
			out = append(out, l)
		}
	}
	return out
}

func (s *Sankey) inLinks(key string) []*SankeyLink {
	var out []*SankeyLink
	for _, l := range s.Links {
		if l.To == key {
			out = append(out, l)
		}
	}
	return out
}

func (s *Sankey) node(key string) *SankeyNode {
	return s.byKey[key]
}

// FlowTotal is the total waste that reaches the diagram's sink tier (the
// rules) — the invariant number: it must equal the findings total exactly,
// no matter how many tiers exist or how many nodes were capped into "other".
func (s *Sankey) FlowTotal() float64 {
	maxTier := -1
	for _, n := range s.Nodes {
		if n.Tier > maxTier {
			maxTier = n.Tier
		}
	}
	if maxTier < 0 {
		return 0
	}
	byKey := make(map[string]*SankeyNode, len(s.Nodes))
	for _, n := range s.Nodes {
		byKey[n.Key] = n
	}
	var total float64
	for _, l := range s.Links {
		if t, ok := byKey[l.To]; ok && t.Tier == maxTier {
			total += l.Value
		}
	}
	return total
}

// linkPath renders one flow as a filled cubic-Bézier band from the source
// node's right edge (s0..s1) to the target node's left edge (t0..t1).
func (s *Sankey) linkPath(l *SankeyLink, src, dst *SankeyNode) string {
	x0 := src.x + sankeyNodeWidth
	x1 := dst.x
	dx := (x1 - x0) / 2
	return fmt.Sprintf("M %s %s C %s %s, %s %s, %s %s L %s %s C %s %s, %s %s, %s %s Z",
		f2(x0), f2(l.s0),
		f2(x0+dx), f2(l.s0),
		f2(x1-dx), f2(l.t0),
		f2(x1), f2(l.t0),
		f2(x1), f2(l.t1),
		f2(x1-dx), f2(l.t1),
		f2(x0+dx), f2(l.s1),
		f2(x0), f2(l.s1))
}

// renderSVG emits the diagram as inline SVG. Flows are drawn first (behind
// the nodes), then node rectangles, then their labels. Every text value is
// passed through esc before it is written, so an untrusted project or rule
// name can never break the document.
func (s *Sankey) renderSVG(sb *strings.Builder, currency string) {
	if len(s.Nodes) == 0 {
		return
	}
	fmt.Fprintf(sb, "<svg viewBox=\"0 0 %s %s\" role=\"img\" aria-label=\"Sankey diagram of monthly waste flowing from organization to folder to project to rule, band widths proportional to waste\" xmlns=\"http://www.w3.org/2000/svg\">\n",
		f2(s.svgWidth), f2(s.svgHeight))

	sb.WriteString("<g class=\"flows\">\n")
	for _, l := range s.Links {
		src, okS := s.byKey[l.From]
		dst, okD := s.byKey[l.To]
		if !okS || !okD {
			continue
		}
		_, _, accent := tierStyle(src.Tier)
		fmt.Fprintf(sb, "<path d=\"%s\" fill=\"%s\" fill-opacity=\"0.38\" stroke=\"none\"/>\n",
			s.linkPath(l, src, dst), accent)
	}
	sb.WriteString("</g>\n")

	sb.WriteString("<g class=\"nodes\">\n")
	for _, n := range s.Nodes {
		fill, stroke, _ := tierStyle(n.Tier)
		sb.WriteString("<g class=\"sankey-node\">\n")
		// <title> is the hover/accessibility name; it carries the FULL label,
		// so a truncated on-rect label never loses information.
		fmt.Fprintf(sb, "<title>%s — %s</title>\n", esc(n.Label), esc(moneyHTML(n.Value, currency)))
		fmt.Fprintf(sb, "<rect x=\"%s\" y=\"%s\" width=\"%s\" height=\"%s\" rx=\"4\" ry=\"4\" fill=\"%s\" stroke=\"%s\" stroke-width=\"1.5\"/>\n",
			f2(n.x), f2(n.y), f2(sankeyNodeWidth), f2(n.height), fill, stroke)
		s.writeNodeLabel(sb, n, currency)
		sb.WriteString("</g>\n")
	}
	sb.WriteString("</g>\n")
	sb.WriteString("</svg>\n")
}

// writeNodeLabel renders the label text inside a node rect: the name on one
// line and the amount on a second line when the node is tall enough, the name
// alone otherwise. Both lines are truncated to fit the rect width and every
// value is escaped.
func (s *Sankey) writeNodeLabel(sb *strings.Builder, n *SankeyNode, currency string) {
	name := truncateLabel(n.Label, sankeyMaxLabelRunes)
	if n.height >= 2*sankeyLineH+2 {
		cy := n.y + n.height/2
		fmt.Fprintf(sb, "<text x=\"%s\" y=\"%s\" class=\"sankey-name\">%s</text>\n",
			f2(n.x+10), f2(cy-6), esc(name))
		fmt.Fprintf(sb, "<text x=\"%s\" y=\"%s\" class=\"sankey-amt\">%s</text>\n",
			f2(n.x+10), f2(cy+7), esc(moneyHTML(n.Value, currency)))
		return
	}
	fmt.Fprintf(sb, "<text x=\"%s\" y=\"%s\" class=\"sankey-name\">%s</text>\n",
		f2(n.x+10), f2(n.y+n.height/2+4), esc(name))
}

// renderSankeySection writes the "Waste flow" overview section: a hint line,
// the inline-SVG diagram and a caption carrying the diagram total. It is the
// overview that sits above the collapsible hierarchy tree and the findings
// table — not a replacement for either.
func renderSankeySection(sb *strings.Builder, sk *Sankey, currency string, findingCount int) {
	if sk == nil || len(sk.Nodes) == 0 {
		return
	}
	sb.WriteString("<section id=\"sankey\">\n")
	sb.WriteString("<h2>Waste flow</h2>\n")
	sb.WriteString("<p class=\"hint\">Each band's width is proportional to its monthly waste, flowing from the top of the resource hierarchy down to the rule that flagged it. The tree below gives the per-resource detail.</p>\n")
	sb.WriteString("<figure class=\"sankey\">\n")
	sk.renderSVG(sb, currency)
	plural := "finding"
	if findingCount != 1 {
		plural = "findings"
	}
	fmt.Fprintf(sb, "<figcaption>Total monthly waste: %s across %d %s.</figcaption>\n",
		moneyHTML(sk.Total, currency), findingCount, plural)
	sb.WriteString("</figure>\n")
	sb.WriteString("</section>\n")
}

// tierStyle returns the (fill, stroke, flow accent) colors for a tier. Light
// fills with a saturated border print cleanly on paper while the semi-
// transparent flow bands read as a single flowing ribbon.
func tierStyle(tier int) (fill, stroke, accent string) {
	switch tier {
	case sankeyTierOrg:
		return "#eaf1f9", "#4E79A7", "#4E79A7"
	case sankeyTierFolder:
		return "#eaf6e8", "#59A14F", "#59A14F"
	case sankeyTierProject:
		return "#fdf1e0", "#d9830f", "#F28E2B"
	default:
		return "#fbeaea", "#c05050", "#E15759"
	}
}

func sankeyTierName(tier int) string {
	switch tier {
	case sankeyTierOrg:
		return "orgs"
	case sankeyTierFolder:
		return "folders"
	case sankeyTierProject:
		return "projects"
	default:
		return "rules"
	}
}

func sankeyOtherKey(tier int) string {
	return "other:" + sankeyTierName(tier)
}

func sankeyOtherLabel(tier int, n int) string {
	return fmt.Sprintf("other (%d %s)", n, sankeyTierName(tier))
}

// truncateLabel caps a label at max runes, appending an ellipsis when it was
// cut, so a long resource name never overflows its node rect.
func truncateLabel(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// f2 formats a coordinate/measure as a fixed 2-decimal string.
func f2(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}
