package graph

// Node returns a node by ref.
func (g *Graph) Node(id Ref) (*Node, bool) { n, ok := g.nodes[id]; return n, ok }

// Nodes iterates every node in deterministic (Ref-sorted, post-Freeze) order.
func (g *Graph) Nodes(fn func(*Node) bool) {
	for _, id := range g.order {
		if !fn(g.nodes[id]) {
			return
		}
	}
}

// HasMetric reports whether ANY node carries a usable (sample-positive) value
// for the given metric key. It is what the offline summary uses to tell which
// metric-dependent rules could not evaluate for lack of data: a fixture replay
// materially has no series, so every metric key is absent here and every
// metric-requiring rule is reported as not evaluated rather than silently
// looking like "no waste".
func (g *Graph) HasMetric(key string) bool {
	found := false
	g.Nodes(func(n *Node) bool {
		m, ok := n.Metrics[key]
		if ok && m.Samples > 0 {
			found = true
			return false
		}
		return true
	})
	return found
}

// ByKind iterates nodes of one kind - the primary rule entrypoint.
//
// Container kinds (organization, folder, project) are excluded structurally:
// a rule asks for a leaf ResourceKind (KindInstance, KindDisk, ...), and a
// container node's Kind is always one of the container kinds, so invoking
// ByKind with any leaf kind can never surface a container node. A caller
// using ByKind(KindProject) to enumerate a hierarchy is allowed, but rules
// never do — rule evaluation only ever enters through these leaf kinds, which
// makes container exclusion structural rather than a convention each rule
// must remember.
func (g *Graph) ByKind(k ResourceKind, fn func(*Node) bool) {
	for _, id := range g.byKind[k] {
		if !fn(g.nodes[id]) {
			return
		}
	}
}

// Out / In return the adjacency slices. Callers MUST NOT mutate them.
func (g *Graph) Out(id Ref) []Edge { return g.out[id] }
func (g *Graph) In(id Ref) []Edge  { return g.in[id] }

// InDegree counts inbound edges, optionally filtered by kind.
func (g *Graph) InDegree(id Ref, kinds ...EdgeKind) int {
	return degree(g.in[id], kinds)
}

// OutDegree counts outbound edges, optionally filtered by kind.
func (g *Graph) OutDegree(id Ref, kinds ...EdgeKind) int {
	return degree(g.out[id], kinds)
}

func degree(edges []Edge, kinds []EdgeKind) int {
	if len(kinds) == 0 {
		return len(edges)
	}
	n := 0
	for _, e := range edges {
		for _, k := range kinds {
			if e.Kind == k {
				n++
				break
			}
		}
	}
	return n
}

type Direction uint8

const (
	Outbound Direction = iota
	Inbound
)

// Neighbors returns nodes reachable in one hop along the given edge kinds.
func (g *Graph) Neighbors(id Ref, dir Direction, kinds ...EdgeKind) []*Node {
	edges := g.out[id]
	pick := func(e Edge) Ref { return e.To }
	if dir == Inbound {
		edges, pick = g.in[id], func(e Edge) Ref { return e.From }
	}
	out := make([]*Node, 0, len(edges))
	for _, e := range edges {
		if len(kinds) > 0 && !matches(e.Kind, kinds) {
			continue
		}
		if n, ok := g.nodes[pick(e)]; ok {
			out = append(out, n)
		}
	}
	return out
}

func matches(k EdgeKind, kinds []EdgeKind) bool {
	for _, want := range kinds {
		if k == want {
			return true
		}
	}
	return false
}
