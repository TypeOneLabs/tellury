package rules

import (
	"context"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// NodeRule is a rule the engine evaluates one node at a time. The engine owns
// the entire evaluation skeleton:
//
//	for each node of NodeRule.Kind():
//	    if node.Exempt() → skip SkipExemptLabel
//	    for each guard in NodeRule.Guards():
//	        if !guard.Check(node, nc, pass) → skip guard.SkipCode
//	    branches := NodeRule.Cost(ctx, node, nc, pass)
//	    if err → skip SkipNoPrice
//	    keep := branches with Waste >= NodeRule.MinWasteUSD()
//	    if no branch remains → skip SkipBelowMinWaste
//	    best := branch with highest Waste
//	    finding := Finding{ …, best.Waste, best.Confidence, … }
//	    finding.Evidence = autoCollect(NodeRule.EvidenceKeys(), node) +
//	                      NodeRule.ExtraEvidence(node, nc, best)
//
// A rule that cannot express itself per-node (ranking, cross-node
// aggregation) keeps using the original Rule interface. Both coexist in the
// registry; NodeRule is adapted to Rule via AdaptNodeRule.
type NodeRule interface {
	Meta() Meta

	// Kind returns the ResourceKind the engine iterates.
	Kind() graph.ResourceKind

	// Guards returns ordered predicates. The engine evaluates them in order;
	// the first to return false decides the SkipCode recorded for this node.
	// Return nil or an empty slice when there are no guards beyond the
	// engine's built-in exempt-label check.
	Guards() []Guard

	// Cost computes waste and confidence for a node that passed every guard.
	// It may return multiple branches — for example, a rightsizing delta and
	// a stop/delete full-cost fallback when no smaller shape exists. The
	// engine drops branches whose Waste < MinWasteUSD(), then picks the
	// highest-waste survivor. If no branch survives — including Cost
	// returning nil, nil — the node is skipped as SkipBelowMinWaste.
	//
	// A price lookup failure MUST be returned as an error, never assumed to
	// be $0 (Invariant I4); the engine records SkipNoPrice for the node.
	//
	// nc carries values guards stashed during their checks. It is never nil.
	// n is the node being evaluated; raw attributes are read from it
	// directly — only computed values flow through nc.
	Cost(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error)

	// MinWasteUSD is the per-rule noise floor. Branches whose Waste falls
	// below this value are dropped before a Finding is constructed.
	MinWasteUSD() float64

	// EvidenceKeys names node attrs and/or metric keys the engine
	// auto-collects as evidence. Each key is looked up first in Attrs
	// (rendered via "%v"), then in Metrics (rendered via "%g"). Keys not
	// found on the node are silently omitted.
	EvidenceKeys() []string

	// ExtraEvidence returns evidence the engine cannot derive from
	// EvidenceKeys alone — computed values like detached_days, age_basis,
	// delta_per_gb_month, or price-source entries. nc carries guard-computed
	// values; branch is the CostBranch the engine selected.
	ExtraEvidence(n *graph.Node, nc *NodeContext, branch CostBranch) []Evidence
}

// Guard is one ordered predicate. The engine evaluates Guards in slice order;
// the first failing guard terminates evaluation and its SkipCode is recorded.
type Guard struct {
	// SkipCode is recorded via Pass.SkipNode when Check returns false.
	// It MUST be a non-empty typed SkipCode — never "" — so --explain-skips
	// always renders a meaningful reason.
	SkipCode SkipCode
	// Name is a stable, human-readable identifier used in diagnostics
	// (e.g. "shape_valid", "not_attached", "old_enough"). It MUST be
	// distinct per guard so a future --explain-skips enhancement can report
	// exactly which guard failed.
	Name string
	// Check evaluates this guard. It may write computed values into nc for
	// later steps to read. Returns true when the node passes. Guards are
	// pure functions of (n, nc, p): no network, no disk, no randomness.
	Check func(n *graph.Node, nc *NodeContext, p *Pass) bool
}

// NodeContext carries per-node values from guards to Cost and ExtraEvidence.
// The engine allocates a fresh NodeContext for each node, so a guard's writes
// never leak between nodes.
type NodeContext struct {
	// Values is a general-purpose bag. Guards write to it; Cost and
	// ExtraEvidence read from it. Keys are convention strings like
	// "detached_days", "age_basis", "overprovision_ratio".
	Values map[string]any
}

// Set is a convenience method for guard functions. It lazily allocates the
// backing map so a guard never needs a nil check before its first write.
func (nc *NodeContext) Set(key string, value any) {
	if nc.Values == nil {
		nc.Values = make(map[string]any, 4)
	}
	nc.Values[key] = value
}

// Get retrieves a guard-computed value.
func (nc *NodeContext) Get(key string) (any, bool) {
	v, ok := nc.Values[key]
	return v, ok
}

// CostBranch is one way to price a node's waste. A rule that has exactly one
// cost formula returns a single-element slice. A rule with alternative
// formulas (e.g. rightsize-delta vs. full-cost stop/delete) returns one
// branch per alternative; the engine picks the highest-Waste branch above the
// noise floor.
type CostBranch struct {
	// Waste is the monthly waste in USD.
	Waste float64
	// Confidence is in [0,1].
	Confidence float64
	// Label disambiguates branches in diagnostics and evidence rendering.
	// Example values: "rightsize", "stop_delete".
	Label string
}
