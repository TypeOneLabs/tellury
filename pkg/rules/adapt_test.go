package rules

import (
	"context"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// testNodeRule is a configurable NodeRule used to drive the adapter skeleton
// in isolation. Every field is optional except id/kind; nil hooks behave like
// a rule that produces no branches and no evidence.
type testNodeRule struct {
	id       string
	kind     graph.ResourceKind
	guards   []Guard
	costFn   func(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error)
	minWaste float64
	evKeys   []string
	extraFn  func(n *graph.Node, nc *NodeContext, branch CostBranch) []Evidence
}

func (r testNodeRule) Meta() Meta {
	return Meta{ID: r.id, Provider: "test", Service: "test", Title: "test rule", Severity: SeverityLow, Origin: OriginNative}
}

func (r testNodeRule) Kind() graph.ResourceKind { return r.kind }
func (r testNodeRule) Guards() []Guard          { return r.guards }
func (r testNodeRule) MinWasteUSD() float64     { return r.minWaste }
func (r testNodeRule) EvidenceKeys() []string   { return r.evKeys }

func (r testNodeRule) Cost(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error) {
	if r.costFn == nil {
		return nil, nil
	}
	return r.costFn(ctx, n, nc, p)
}

func (r testNodeRule) ExtraEvidence(n *graph.Node, nc *NodeContext, branch CostBranch) []Evidence {
	if r.extraFn == nil {
		return nil
	}
	return r.extraFn(n, nc, branch)
}

// runNodeRule builds a frozen graph from the given nodes, runs the rule
// through AdaptNodeRule, and returns findings plus the recorded skip tally.
// It exercises the exact entry point a NodeRule author gets for free, without
// going through Engine.Run.
func runNodeRule(t *testing.T, nr NodeRule, nodes []*graph.Node) ([]Finding, map[SkipCode]int) {
	t.Helper()
	g := graph.New()
	for _, n := range nodes {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	g.Freeze()

	skipCounts := map[SkipCode]int{}
	p := &Pass{
		Graph: g,
		Skip: func(ruleID string, id graph.Ref, code SkipCode) {
			skipCounts[code]++
		},
	}
	findings, err := AdaptNodeRule(nr).Eval(context.Background(), p)
	if err != nil {
		t.Fatalf("AdaptNodeRule(nr).Eval: %v", err)
	}
	return findings, skipCounts
}

// TestAdapt_TrivialRule_Fires is the skeleton's primary acceptance test: a
// NodeRule with ZERO guards and a Cost returning one branch must produce
// exactly one Finding carrying the branch's waste and confidence plus the
// node's identity fields. This is the shape every simple one-formula rule
// (e.g. detached_disk, unused_reserved_ip after porting) takes.
//
// MUTATION CHECK: adding a "skip the node when the rule declares no guards"
// gate before the guard loop (treating Guards()==nil as a failure) turned the
// firing node into a SkipMissingAttr skip, so this test failed with "want 1
// finding, got 0". The gate was removed; the test passes.
func TestAdapt_TrivialRule_Fires(t *testing.T) {
	n := &graph.Node{
		ID:       graph.Ref("//test/projects/p/zones/us-central1-a/disks/d1"),
		Kind:     graph.KindDisk,
		Name:     "d1",
		Project:  "p",
		Location: "us-central1-a",
	}

	nr := testNodeRule{
		id:       "test_trivial",
		kind:     graph.KindDisk,
		minWaste: 0.0, // no noise floor for the trivial rule
		costFn: func(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error) {
			return []CostBranch{{Waste: 17.0, Confidence: 0.9, Label: "detached"}}, nil
		},
	}

	findings, skips := runNodeRule(t, nr, []*graph.Node{n})
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.RuleID != "test_trivial" {
		t.Errorf("RuleID = %q, want test_trivial", f.RuleID)
	}
	if f.ResourceID != n.ID {
		t.Errorf("ResourceID = %q, want %q", f.ResourceID, n.ID)
	}
	if f.Resource != "disk/d1" {
		t.Errorf("Resource = %q, want disk/d1", f.Resource)
	}
	if f.Kind != graph.KindDisk {
		t.Errorf("Kind = %q, want disk", f.Kind)
	}
	if f.Project != "p" || f.Location != "us-central1-a" {
		t.Errorf("Project/Location = %q/%q, want p/us-central1-a", f.Project, f.Location)
	}
	if f.MonthlyWasteUSD != 17.0 {
		t.Errorf("MonthlyWasteUSD = %v, want 17.0 (the branch's Waste)", f.MonthlyWasteUSD)
	}
	if f.Confidence != 0.9 {
		t.Errorf("Confidence = %v, want 0.9 (the branch's Confidence)", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("a firing node must record zero skips, got %+v", skips)
	}
}

// TestAdapt_GuardsRunInDeclaredOrder pins the ordering contract: the engine
// must evaluate Guards in slice order, never sorted, reversed, or
// parallelized. Both guards here pass, so the observation is the order in
// which their Check functions ran.
//
// MUTATION CHECK: iterating the guard slice in reverse order in evalNodeRule
// (for i := len(gs)-1; i >= 0; i-- { g := gs[i] }) made the observed order
// [second first] and this test failed with `guards ran in order [second
// first], want [first second]`. The forward loop was restored; the test
// passes.
func TestAdapt_GuardsRunInDeclaredOrder(t *testing.T) {
	n := &graph.Node{
		ID:   graph.Ref("//test/projects/p/zones/z/instances/i1"),
		Kind: graph.KindInstance,
		Name: "i1",
	}
	var order []string

	nr := testNodeRule{
		id:       "test_order",
		kind:     graph.KindInstance,
		minWaste: 0.0,
		guards: []Guard{
			{
				Name:     "first",
				SkipCode: SkipMissingAttr,
				Check: func(n *graph.Node, nc *NodeContext, p *Pass) bool {
					order = append(order, "first")
					return true
				},
			},
			{
				Name:     "second",
				SkipCode: SkipMissingAttr,
				Check: func(n *graph.Node, nc *NodeContext, p *Pass) bool {
					order = append(order, "second")
					return true
				},
			},
		},
		costFn: func(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error) {
			return []CostBranch{{Waste: 5.0, Confidence: 1.0, Label: "x"}}, nil
		},
	}

	findings, skips := runNodeRule(t, nr, []*graph.Node{n})
	if len(findings) != 1 {
		t.Fatalf("want 1 finding (both guards passed), got %d (%+v)", len(findings), findings)
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("guards ran in order %v, want [first second]", order)
	}
	if len(skips) != 0 {
		t.Errorf("passing guards must not skip, got %+v", skips)
	}
}

// TestAdapt_FirstFailingGuardDecidesSkipCode pins the skip-code contract: the
// FIRST guard whose Check returns false decides the SkipCode recorded, and no
// later guard runs (evaluation terminates at the first failure). A later
// guard carrying a different code must never be recorded for this node.
//
// MUTATION CHECK: removing the `return true` that follows a failed guard's
// SkipNode call (so the chain kept evaluating after recording the skip) let
// the third guard run AND evaluation continue into Cost; this test failed
// with `Cost must not run when a guard fails` (the first assertion to fire —
// the mutated driver had already also recorded SkipAttached). The early
// return was restored; the test passes.
func TestAdapt_FirstFailingGuardDecidesSkipCode(t *testing.T) {
	n := &graph.Node{
		ID:   graph.Ref("//test/projects/p/zones/z/instances/i1"),
		Kind: graph.KindInstance,
		Name: "i1",
	}
	thirdRan := false

	nr := testNodeRule{
		id:       "test_first_fail",
		kind:     graph.KindInstance,
		minWaste: 0.0,
		guards: []Guard{
			{
				Name:     "passes",
				SkipCode: SkipMissingAttr,
				Check:    func(n *graph.Node, nc *NodeContext, p *Pass) bool { return true },
			},
			{
				Name:     "first_fail",
				SkipCode: SkipMissingAttr, // the code that MUST be recorded
				Check:    func(n *graph.Node, nc *NodeContext, p *Pass) bool { return false },
			},
			{
				Name:     "third",
				SkipCode: SkipAttached, // must never be reached
				Check: func(n *graph.Node, nc *NodeContext, p *Pass) bool {
					thirdRan = true
					return false
				},
			},
		},
		costFn: func(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error) {
			t.Fatal("Cost must not run when a guard fails")
			return nil, nil
		},
	}

	findings, skips := runNodeRule(t, nr, []*graph.Node{n})
	if len(findings) != 0 {
		t.Fatalf("a node failing a guard must not produce a finding, got %+v", findings)
	}
	if skips[SkipMissingAttr] != 1 {
		t.Errorf("SkipMissingAttr recorded %d times, want 1 (the first failing guard's code)", skips[SkipMissingAttr])
	}
	if skips[SkipAttached] != 0 {
		t.Errorf("SkipAttached recorded %d times, want 0 (a later guard must not decide the code)", skips[SkipAttached])
	}
	if thirdRan {
		t.Fatal("guard after the first failure must not run")
	}
}

// TestAdapt_ExemptLabelShortCircuitsBeforeGuards pins Invariant I7 in the
// skeleton: a node labeled tellury-exempt=true is skipped as SkipExemptLabel
// before ANY guard runs, whether or not the rule declares guards. A rule must
// never need to (and must never) declare this check itself.
//
// MUTATION CHECK: deleting the `if n.Exempt()` branch from evalNodeRule made
// the guard run and fail, so the node was recorded as SkipMissingAttr instead
// of SkipExemptLabel; this test failed with `SkipExemptLabel recorded 0
// times, want 1` and `no guard may run for an exempt node`. The exempt check
// was restored; the test passes.
func TestAdapt_ExemptLabelShortCircuitsBeforeGuards(t *testing.T) {
	t.Run("with guards declared", func(t *testing.T) {
		n := &graph.Node{
			ID:     graph.Ref("//test/projects/p/zones/z/instances/i1"),
			Kind:   graph.KindInstance,
			Name:   "i1",
			Labels: map[string]string{"tellury-exempt": "true"},
		}
		guardRan := false

		nr := testNodeRule{
			id:       "test_exempt",
			kind:     graph.KindInstance,
			minWaste: 0.0,
			guards: []Guard{
				{
					Name:     "must_not_run",
					SkipCode: SkipMissingAttr,
					Check: func(n *graph.Node, nc *NodeContext, p *Pass) bool {
						guardRan = true
						return false
					},
				},
			},
			costFn: func(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error) {
				t.Fatal("Cost must not run for an exempt node")
				return nil, nil
			},
		}

		findings, skips := runNodeRule(t, nr, []*graph.Node{n})
		if len(findings) != 0 {
			t.Fatalf("exempt node must not fire, got %+v", findings)
		}
		if skips[SkipExemptLabel] != 1 {
			t.Errorf("SkipExemptLabel recorded %d times, want 1", skips[SkipExemptLabel])
		}
		if skips[SkipMissingAttr] != 0 {
			t.Errorf("exempt node must be skipped for the label, not a guard, got %+v", skips)
		}
		if guardRan {
			t.Fatal("no guard may run for an exempt node")
		}
	})

	t.Run("no guards declared", func(t *testing.T) {
		n := &graph.Node{
			ID:     graph.Ref("//test/projects/p/disks/d1"),
			Kind:   graph.KindDisk,
			Name:   "d1",
			Labels: map[string]string{"tellury-exempt": "true"},
		}
		nr := testNodeRule{
			id:       "test_exempt_no_guards",
			kind:     graph.KindDisk,
			minWaste: 0.0,
			costFn: func(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error) {
				t.Fatal("Cost must not run for an exempt node")
				return []CostBranch{{Waste: 99.0, Confidence: 1.0, Label: "x"}}, nil
			},
		}

		findings, skips := runNodeRule(t, nr, []*graph.Node{n})
		if len(findings) != 0 {
			t.Fatalf("exempt node with zero guards must still be skipped, got %+v", findings)
		}
		if skips[SkipExemptLabel] != 1 {
			t.Errorf("SkipExemptLabel recorded %d times, want 1", skips[SkipExemptLabel])
		}
	})
}

// TestAdapt_BelowFloorWasteSkips pins the noise-floor contract: a branch whose
// Waste falls below MinWasteUSD is dropped, and when no branch survives the
// node is skipped as SkipBelowMinWaste — never reported as a finding.
//
// MUTATION CHECK: removing the floor filter from evalNodeRule (keeping every
// branch) made the 0.05 branch reach Finding construction, so this test
// failed with `want 0 findings, got 1` and `SkipBelowMinWaste recorded 0
// times, want 1`. The floor filter was restored; the test passes.
func TestAdapt_BelowFloorWasteSkips(t *testing.T) {
	n := &graph.Node{
		ID:   graph.Ref("//test/projects/p/disks/d1"),
		Kind: graph.KindDisk,
		Name: "d1",
	}

	nr := testNodeRule{
		id:       "test_floor",
		kind:     graph.KindDisk,
		minWaste: 1.00, // $1/month noise floor
		costFn: func(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error) {
			return []CostBranch{{Waste: 0.05, Confidence: 1.0, Label: "x"}}, nil
		},
	}

	findings, skips := runNodeRule(t, nr, []*graph.Node{n})
	if len(findings) != 0 {
		t.Fatalf("sub-floor waste must not produce a finding, got %+v", findings)
	}
	if skips[SkipBelowMinWaste] != 1 {
		t.Errorf("SkipBelowMinWaste recorded %d times, want 1", skips[SkipBelowMinWaste])
	}
}

// TestAdapt_SelectsHighestWasteBranch pins branch selection: when several
// branches survive the floor, the engine must pick the one with the highest
// Waste (and carry its Confidence with it) — this is what lets a rule return
// both a rightsize-delta and a stop/delete fallback and have the engine
// choose the larger saving.
//
// MUTATION CHECK: selecting keep[0] instead of the max in evalNodeRule made
// the finding carry Waste 3.0 / Confidence 0.5; this test failed with
// `MonthlyWasteUSD = 3, want 9`. The max-by-waste scan was restored; the test
// passes.
func TestAdapt_SelectsHighestWasteBranch(t *testing.T) {
	n := &graph.Node{
		ID:   graph.Ref("//test/projects/p/disks/d1"),
		Kind: graph.KindDisk,
		Name: "d1",
	}

	nr := testNodeRule{
		id:       "test_max",
		kind:     graph.KindDisk,
		minWaste: 0.0,
		costFn: func(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error) {
			return []CostBranch{
				{Waste: 3.0, Confidence: 0.5, Label: "rightsize"},
				{Waste: 9.0, Confidence: 0.8, Label: "stop_delete"},
			}, nil
		},
	}

	findings, _ := runNodeRule(t, nr, []*graph.Node{n})
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.MonthlyWasteUSD != 9.0 {
		t.Errorf("MonthlyWasteUSD = %v, want 9.0 (the highest-waste branch)", f.MonthlyWasteUSD)
	}
	if f.Confidence != 0.8 {
		t.Errorf("Confidence = %v, want 0.8 (the chosen branch's confidence)", f.Confidence)
	}
}

// TestAdapt_EvidenceAssembly pins evidence construction: auto-collected keys
// come first (attrs rendered via %v, metrics via %g, missing keys silently
// omitted), then the rule's ExtraEvidence entries. The order is stable — it
// becomes the CLI column order.
//
// MUTATION CHECK: dropping the metric branch of autoCollect (attrs only) made
// the p95_cpu entry disappear; this test failed with `want 3 evidence
// entries, got 2 (missing p95_cpu)`. The metric branch was restored; the test
// passes.
func TestAdapt_EvidenceAssembly(t *testing.T) {
	n := &graph.Node{
		ID:   graph.Ref("//test/projects/p/disks/d1"),
		Kind: graph.KindDisk,
		Name: "d1",
	}
	n.SetAttr("size_gb", 100.0)
	n.Metrics = map[string]graph.MetricValue{
		"p95_cpu": {Value: 0.10, Samples: 200, Coverage: 0.9, WindowDays: 7},
	}

	nr := testNodeRule{
		id:       "test_ev",
		kind:     graph.KindDisk,
		minWaste: 0.0,
		evKeys:   []string{"size_gb", "p95_cpu", "missing_key"}, // missing_key is not on the node
		costFn: func(ctx context.Context, n *graph.Node, nc *NodeContext, p *Pass) ([]CostBranch, error) {
			return []CostBranch{{Waste: 1.0, Confidence: 1.0, Label: "x"}}, nil
		},
		extraFn: func(n *graph.Node, nc *NodeContext, branch CostBranch) []Evidence {
			return []Evidence{{Key: "detached_days", Value: "19"}}
		},
	}

	findings, _ := runNodeRule(t, nr, []*graph.Node{n})
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	ev := findings[0].Evidence
	want := []Evidence{
		{Key: "size_gb", Value: "100"},      // attr, %v
		{Key: "p95_cpu", Value: "0.1"},      // metric, %g
		{Key: "detached_days", Value: "19"}, // ExtraEvidence, appended after
	}
	if len(ev) != len(want) {
		t.Fatalf("want %d evidence entries, got %d: %+v", len(want), len(ev), ev)
	}
	for i := range want {
		if ev[i].Key != want[i].Key || ev[i].Value != want[i].Value {
			t.Errorf("evidence[%d] = {%q %q}, want {%q %q}", i, ev[i].Key, ev[i].Value, want[i].Key, want[i].Value)
		}
	}
}

// TestRegisterNode_AppearsInRegistry proves the registration story end to
// end: RegisterNode adapts a NodeRule and places it in the SAME registry as
// the native rules, so `tellury rules list` shows both styles together. The ID
// is unique to this package's test binary, so it cannot collide with a
// shipped rule or with the external all_test registration check (which runs
// in a different test process).
//
// MUTATION CHECK: changing RegisterNode to a no-op (dropping the
// Register(AdaptNodeRule(nr)) call) left the rule out of List(); this test
// failed with `rule "test_node_rule_registration" missing from List()`. The
// Register call was restored; the test passes.
func TestRegisterNode_AppearsInRegistry(t *testing.T) {
	const id = "test_node_rule_registration"
	nr := testNodeRule{id: id, kind: graph.KindDisk}
	RegisterNode(nr)

	for _, r := range List() {
		if r.Meta().ID == id {
			return // found — new-style and existing-style rules share the registry
		}
	}
	t.Fatalf("rule %q registered via RegisterNode is missing from List(); "+
		"the NodeRule and native rules must share one registry", id)
}
