package graph

import (
	"encoding/json"
	"io"
	"time"
)

// SnapshotVersion is bumped to 2 for the additive MetricValue fields
// (Coverage, Source, ...). v1 snapshots still decode: Go zero-values the
// new fields, and Coverage==0 is treated as "unknown" by rules, not "zero".
const SnapshotVersion = 2

// Snapshot is the on-disk / fixture representation of a Graph.
type Snapshot struct {
	Version    int       `json:"version"`
	Provider   string    `json:"provider"`
	Scope      string    `json:"scope"`
	CapturedAt time.Time `json:"captured_at"`
	Nodes      []*Node   `json:"nodes"`
	Edges      []Edge    `json:"edges"`
}

// WriteSnapshot serializes a frozen graph.
func (g *Graph) WriteSnapshot(w io.Writer, provider, scope string) error {
	g.Freeze()
	snap := Snapshot{
		Version:    SnapshotVersion,
		Provider:   provider,
		Scope:      scope,
		CapturedAt: time.Now().UTC(),
		Nodes:      make([]*Node, 0, len(g.nodes)),
	}
	g.Nodes(func(n *Node) bool { snap.Nodes = append(snap.Nodes, n); return true })
	for _, id := range g.order {
		snap.Edges = append(snap.Edges, g.out[id]...)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}

// LoadSnapshot rebuilds a frozen graph from JSON. Used by --cache-file and tests.
func LoadSnapshot(r io.Reader) (*Graph, *Snapshot, error) {
	var snap Snapshot
	if err := json.NewDecoder(r).Decode(&snap); err != nil {
		return nil, nil, err
	}
	g := New()
	for _, n := range snap.Nodes {
		if err := g.AddNode(n); err != nil {
			return nil, nil, err
		}
	}
	for _, e := range snap.Edges {
		if err := g.AddEdge(e); err != nil {
			return nil, nil, err
		}
	}
	g.Freeze()
	return g, &snap, nil
}
