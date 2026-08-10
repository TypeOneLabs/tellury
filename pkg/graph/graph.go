package graph

import (
	"fmt"
	"sort"
	"sync"
)

// Graph is an in-memory, single-scan resource graph.
//
// Concurrency contract:
//   - AddNode and AddEdge are NOT safe for concurrent use, and neither is
//     safe to call concurrently with SetMetric. Ingestion (building nodes
//     and edges) is single-goroutine by design, and both methods reject
//     calls made after Freeze().
//   - SetMetric IS safe for concurrent use by multiple goroutines, both
//     before and after Freeze(): it only ever writes into a per-node
//     Metrics map, guarded by an internal mutex, and it never touches the
//     graph's node/edge indexes. This is what lets metric enrichment run
//     in parallel across nodes.
//   - After Freeze(), the graph's topology (nodes, edges, indexes) is
//     immutable and safe for unlimited concurrent reads. Freeze() does NOT
//     make Node.Metrics immutable - concurrent SetMetric calls may still be
//     enriching it, guarded by the same mutex. Readers that need a stable
//     view of Metrics must wait until enrichment has finished
//     (i.e. synchronize with the enrichment goroutines themselves; Freeze
//     provides no such signal).
type Graph struct {
	nodes  map[Ref]*Node
	out    map[Ref][]Edge
	in     map[Ref][]Edge
	byKind map[ResourceKind][]Ref

	order    []Ref
	frozen   bool
	dangling int

	// metricsMu guards writes to every Node.Metrics map reachable from
	// nodes. It is the only synchronization SetMetric needs, since it never
	// touches order/byKind/out/in.
	metricsMu sync.Mutex
}

func New() *Graph {
	return &Graph{
		nodes:  make(map[Ref]*Node, 1024),
		out:    make(map[Ref][]Edge, 1024),
		in:     make(map[Ref][]Edge, 1024),
		byKind: make(map[ResourceKind][]Ref, 16),
	}
}

// AddNode inserts or replaces a node. Last write wins.
func (g *Graph) AddNode(n *Node) error {
	if g.frozen {
		return fmt.Errorf("graph: AddNode after Freeze")
	}
	if n == nil || n.ID == "" {
		return fmt.Errorf("graph: node requires non-empty ID")
	}
	// Container nodes are hierarchy scaffolding: they must never reach rule
	// evaluation. Rule evaluation enters only through ByKind, which skips
	// container kinds structurally (see ByKind), so adding them to byKind is
	// harmless but pointless; we keep the index uniform (every node appears
	// in byKind so ordering/selice invariants hold and debug output can
	// enumerate all kinds) and rely on ByKind's container exclusion.
	if _, exists := g.nodes[n.ID]; !exists {
		g.order = append(g.order, n.ID)
		g.byKind[n.Kind] = append(g.byKind[n.Kind], n.ID)
	}
	g.nodes[n.ID] = n
	return nil
}

// AddEdge records a relationship. Endpoints need not exist yet.
func (g *Graph) AddEdge(e Edge) error {
	if g.frozen {
		return fmt.Errorf("graph: AddEdge after Freeze")
	}
	if e.From == "" || e.To == "" || e.Kind == "" {
		return fmt.Errorf("graph: invalid edge %+v", e)
	}
	g.out[e.From] = append(g.out[e.From], e)
	g.in[e.To] = append(g.in[e.To], e)
	return nil
}

// SetMetric attaches an enrichment value. No-op for unknown nodes.
//
// Safe for concurrent use by multiple goroutines (see the Graph doc
// comment): it only mutates a node's Metrics map, under metricsMu, and
// never touches the node/edge indexes that Freeze() seals.
func (g *Graph) SetMetric(id Ref, key string, v MetricValue) {
	g.metricsMu.Lock()
	defer g.metricsMu.Unlock()
	n, ok := g.nodes[id]
	if !ok {
		return
	}
	if n.Metrics == nil {
		n.Metrics = make(map[string]MetricValue, 2)
	}
	n.Metrics[key] = v
}

// Freeze prunes dangling edges, sorts every index for determinism, and seals
// the graph for concurrent reads. Idempotent.
//
// Freeze() only seals topology (nodes, edges, byKind/order indexes) against
// further mutation; it does not synchronize or lock out concurrent
// SetMetric calls, which remain independently safe via metricsMu for as
// long as enrichment is running.
func (g *Graph) Freeze() {
	if g.frozen {
		return
	}
	prune := func(m map[Ref][]Edge, endpoint func(Edge) Ref) {
		for k, edges := range m {
			kept := edges[:0]
			for _, e := range edges {
				if _, ok := g.nodes[endpoint(e)]; ok {
					kept = append(kept, e)
				} else {
					g.dangling++
				}
			}
			sort.Slice(kept, func(i, j int) bool {
				if kept[i].Kind != kept[j].Kind {
					return kept[i].Kind < kept[j].Kind
				}
				return endpoint(kept[i]) < endpoint(kept[j])
			})
			m[k] = kept
		}
	}
	prune(g.out, func(e Edge) Ref { return e.To })
	prune(g.in, func(e Edge) Ref { return e.From })

	sort.Slice(g.order, func(i, j int) bool { return g.order[i] < g.order[j] })
	for k := range g.byKind {
		ids := g.byKind[k]
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		g.byKind[k] = ids
	}
	g.frozen = true
}

func (g *Graph) Frozen() bool       { return g.frozen }
func (g *Graph) NodeCount() int     { return len(g.nodes) }
func (g *Graph) DanglingEdges() int { return g.dangling }

// ResourceNodeCount is the count of leaf (non-container) nodes. Container
// nodes — organization, folder, project — are hierarchy scaffolding that must
// not inflate an operator's reading of "N resources", so the scan report uses
// this count, not NodeCount().
func (g *Graph) ResourceNodeCount() int {
	n := 0
	g.Nodes(func(node *Node) bool {
		if !node.Container() {
			n++
		}
		return true
	})
	return n
}

// ProjectCount reports how many distinct project IDs appear across the
// scan's resource (non-container) nodes. It is what the CLI uses to decide
// whether an organization-wide scan spans more than one project — and thus
// whether the table must surface the PROJECT column so an operator can tell
// which project a finding lives in. A single-project scan returns 0 or 1, and
// the table stays at its compact width.
func (g *Graph) ProjectCount() int {
	seen := map[string]bool{}
	g.Nodes(func(node *Node) bool {
		if !node.Container() && node.Project != "" {
			seen[node.Project] = true
		}
		return true
	})
	return len(seen)
}

// ProjectContainerCount reports how many project container nodes the graph
// carries — the hierarchy's own "projects/<id>" nodes, not the distinct
// project IDs derived from resource nodes (see ProjectCount). The scan
// report's "projects analyzed" figure uses this count: a project container
// node exists for every project that ingested at least one resource, so a
// scan with findings reports its projects exactly as a scan with none does.
// Deriving the count from the findings instead would report zero projects for
// a clean scan — precisely the "nothing wasteful" vs "nothing scanned"
// ambiguity the scan summary exists to resolve.
func (g *Graph) ProjectContainerCount() int {
	return len(g.byKind[KindProject])
}

func (g *Graph) EdgeCount() int {
	total := 0
	for _, e := range g.out {
		total += len(e)
	}
	return total
}
