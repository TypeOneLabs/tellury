package graph

import (
	"encoding/json"
	"io"
	"time"
)

// SnapshotVersion is bumped to 3 for the region container tier: a v3
// snapshot carries region nodes ("projects/<id>/regions/<location>") and the
// resource -> region -> project containment edges that v2 predates. v2
// snapshots still load (the version is advisory); the provider re-constructs
// the missing region tier from each node's Location field — see
// gcp.Provider.MigrateV2ToV3 — so an operator's cached scan replays instead
// of being rejected. v1 snapshots still decode: Go zero-values the newer
// fields, and Coverage==0 is treated as "unknown" by rules, not "zero".
const SnapshotVersion = 3

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
// The version is advisory: an older snapshot loads fine and the caller
// decides whether provider-side migration is needed before the graph is used.
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
