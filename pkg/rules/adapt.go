package rules

import (
	"context"
	"fmt"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// AdaptNodeRule wraps a NodeRule as a Rule using the standard skeleton. It is
// the single place the skeleton lives; every NodeRule gets it for free. The
// returned value is an ordinary Rule — indistinguishable from a hand-rolled
// cross-node Rule at the engine level — so both styles coexist in one registry
// and `tellury rules list` shows them all.
func AdaptNodeRule(nr NodeRule) Rule {
	return RuleFunc{
		M: nr.Meta(),
		Fn: func(ctx context.Context, p *Pass) ([]Finding, error) {
			return evalNodeRule(ctx, nr, p)
		},
	}
}

// RegisterNode is the convenience entry point for NodeRule authors. It adapts
// the rule via AdaptNodeRule and registers the resulting Rule under the same
// registry the native (cross-node) rules use.
func RegisterNode(nr NodeRule) {
	Register(AdaptNodeRule(nr))
}

// evalNodeRule is the evaluation skeleton every NodeRule shares. It is the
// engine-side driver: exempt-label short-circuit, ordered guards with typed
// skip codes, per-node cost, the minimum-waste floor, highest-waste branch
// selection, evidence assembly, and Finding construction.
func evalNodeRule(ctx context.Context, nr NodeRule, p *Pass) ([]Finding, error) {
	meta := nr.Meta()
	var out []Finding

	p.Graph.ByKind(nr.Kind(), func(n *graph.Node) bool {
		if ctx.Err() != nil {
			return false
		}

		// P0: exemption label (Invariant I7). Short-circuits before ANY
		// guard — a rule must never declare this check itself.
		if n.Exempt() {
			p.SkipNode(meta.ID, n.ID, SkipExemptLabel)
			return true
		}

		// Ordered guards. The first to fail terminates evaluation; its
		// SkipCode is the one recorded.
		nc := new(NodeContext)
		for _, g := range nr.Guards() {
			if !g.Check(n, nc, p) {
				p.SkipNode(meta.ID, n.ID, g.SkipCode)
				return true
			}
		}

		branches, err := nr.Cost(ctx, n, nc, p)
		if err != nil {
			// A cost error is a price problem, never a $0 assumption
			// (Invariant I4); the node is skipped, not fatal to the rule.
			// On context cancellation, stop iterating instead.
			if ctx.Err() != nil {
				return false
			}
			p.SkipNode(meta.ID, n.ID, SkipNoPrice)
			return true
		}

		// Minimum-waste floor: drop branches whose Waste falls below the
		// rule's declared noise floor. When no branch survives — including
		// Cost returning nil, nil — the node is skipped as
		// SkipBelowMinWaste rather than reported as a finding.
		keep := branches[:0]
		for _, b := range branches {
			if b.Waste >= nr.MinWasteUSD() {
				keep = append(keep, b)
			}
		}
		if len(keep) == 0 {
			p.SkipNode(meta.ID, n.ID, SkipBelowMinWaste)
			return true
		}

		// Pick the branch with the highest waste.
		best := keep[0]
		for _, b := range keep[1:] {
			if b.Waste > best.Waste {
				best = b
			}
		}

		// Evidence assembly: auto-collected keys first, then rule-computed
		// evidence.
		ev := autoCollect(nr.EvidenceKeys(), n)
		ev = append(ev, nr.ExtraEvidence(n, nc, best)...)

		out = append(out, Finding{
			RuleID:          meta.ID,
			ResourceID:      n.ID,
			Resource:        n.Display(),
			Kind:            n.Kind,
			Project:         n.Project,
			Location:        n.Location,
			MonthlyWasteUSD: best.Waste,
			Confidence:      best.Confidence,
			Evidence:        ev,
		})
		return true
	})
	return out, nil
}

// autoCollect renders a NodeRule's EvidenceKeys as Evidence entries. Each key
// is looked up first in the node's Attrs (rendered via "%v"), then in its
// Metrics (rendered via "%g", and only for sample-positive series). Keys not
// found on the node are silently omitted.
func autoCollect(keys []string, n *graph.Node) []Evidence {
	var ev []Evidence
	for _, k := range keys {
		if v, ok := n.Attrs[k]; ok {
			ev = append(ev, Evidence{Key: k, Value: fmt.Sprintf("%v", v)})
			continue
		}
		if m, ok := n.Metrics[k]; ok && m.Samples > 0 {
			ev = append(ev, Evidence{Key: k, Value: fmt.Sprintf("%g", m.Value)})
		}
	}
	return ev
}
