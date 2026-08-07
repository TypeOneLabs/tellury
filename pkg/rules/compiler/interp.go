package compiler

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// Interpret turns a validated RuleSpec into a rules.Rule. The returned rule is
// indistinguishable from a native one: same Pass, same skip codes, same
// pricing path — which is what makes "reproduces the native rules" a testable
// correctness bar for generated logic.
func Interpret(s *RuleSpec) (rules.Rule, error) {
	if s == nil {
		return nil, ErrInvalidSpec
	}
	if HasError(s.Validate()) {
		return nil, ErrInvalidSpec
	}
	meta := s.ToMeta()
	spec := *s

	return rules.RuleFunc{
		M: meta,
		Fn: func(ctx context.Context, p *rules.Pass) ([]rules.Finding, error) {
			return evalSpec(ctx, &spec, p)
		},
	}, nil
}

func evalSpec(ctx context.Context, s *RuleSpec, p *rules.Pass) ([]rules.Finding, error) {
	var out []rules.Finding
	var evalErr error
	kind := graph.ResourceKind(s.Match.Kind)

	p.Graph.ByKind(kind, func(n *graph.Node) bool {
		if ctx.Err() != nil {
			return false
		}
		// Invariant I7 outranks every spec predicate.
		if n.Exempt() {
			p.SkipNode(s.ID, n.ID, rules.SkipExemptLabel)
			return true
		}
		for _, pred := range s.Where {
			ok, code := evalPredicate(pred, n, p)
			if !ok {
				p.SkipNode(s.ID, n.ID, code)
				return true
			}
		}
		waste, code, err := evalCost(&s.Cost, n, p)
		if err != nil {
			evalErr = err
			return false
		}
		if code != "" {
			p.SkipNode(s.ID, n.ID, code)
			return true
		}
		out = append(out, rules.Finding{
			RuleID:          s.ID,
			ResourceID:      n.ID,
			Resource:        n.Display(),
			Kind:            n.Kind,
			Project:         n.Project,
			Location:        n.Location,
			MonthlyWasteUSD: waste,
			Confidence:      s.Meta.Confidence,
			Evidence:        collectEvidence(s.Evidence, n),
			Remediation:     s.Meta.Remediation,
		})
		return true
	})
	return out, evalErr
}

// ─────────────────────────────────────────────────────────────────────────────
// Predicates
// ─────────────────────────────────────────────────────────────────────────────

// evalPredicate returns (satisfied, skipCode). The skip code is only
// meaningful when satisfied is false.
func evalPredicate(pred Predicate, n *graph.Node, p *rules.Pass) (bool, rules.SkipCode) {
	switch pred.Source {
	case SrcAttr:
		return evalAttr(pred, n, p)
	case SrcLabel:
		v, ok := n.Label(pred.Field)
		if pred.Op == OpExists {
			return ok == truthy(pred.Value), rules.SkipMissingAttr
		}
		if !ok {
			return false, rules.SkipMissingAttr
		}
		return compareString(v, pred.Op, pred.Value), rules.SkipBadAttrType
	case SrcMetric:
		minSamples := pred.MinSamples
		if minSamples < 1 {
			minSamples = 1
		}
		m, ok := n.MetricOK(pred.Field, minSamples, pred.MinCoverage)
		if pred.Op == OpExists {
			return ok == truthy(pred.Value), rules.SkipNoMetric
		}
		if !ok {
			if raw, present := n.Metrics[pred.Field]; present && raw.Samples > 0 {
				return false, rules.SkipLowCoverage
			}
			return false, rules.SkipNoMetric
		}
		return compareNumber(m.Value, pred.Op, pred.Value), rules.SkipBadAttrType
	case SrcInDegree:
		d := float64(p.Graph.InDegree(n.ID, graph.EdgeKind(pred.Field)))
		return compareNumber(d, pred.Op, pred.Value), rules.SkipAttached
	case SrcOutDegree:
		d := float64(p.Graph.OutDegree(n.ID, graph.EdgeKind(pred.Field)))
		return compareNumber(d, pred.Op, pred.Value), rules.SkipAttached
	default:
		return false, rules.SkipBadAttrType
	}
}

func evalAttr(pred Predicate, n *graph.Node, p *rules.Pass) (bool, rules.SkipCode) {
	raw, present := n.Attrs[pred.Field]
	if pred.Op == OpExists {
		return present == truthy(pred.Value), rules.SkipMissingAttr
	}
	if pred.Op == OpOlderThanDays {
		// A timestamp that is absent cannot be "recent"; the caller decides
		// with an explicit exists predicate if that matters.
		t, ok := n.Time(pred.Field)
		if !ok {
			return false, rules.SkipMissingAttr
		}
		want, ok := toFloat(pred.Value)
		if !ok {
			return false, rules.SkipBadAttrType
		}
		return p.Now.Sub(t).Hours()/24 >= want, rules.SkipRecentlyDetached
	}
	if !present {
		return false, rules.SkipMissingAttr
	}
	if s, ok := raw.(string); ok {
		return compareString(s, pred.Op, pred.Value), rules.SkipBadAttrType
	}
	if b, ok := raw.(bool); ok {
		return (b == truthy(pred.Value)) == (pred.Op != OpNe), rules.SkipBadAttrType
	}
	num, ok := n.Num(pred.Field)
	if !ok {
		return false, rules.SkipBadAttrType
	}
	return compareNumber(num, pred.Op, pred.Value), rules.SkipBadAttrType
}

func compareNumber(have float64, op Op, want any) bool {
	if op == OpIn {
		for _, v := range toSlice(want) {
			if f, ok := toFloat(v); ok && f == have {
				return true
			}
		}
		return false
	}
	w, ok := toFloat(want)
	if !ok {
		return false
	}
	switch op {
	case OpEq:
		return have == w
	case OpNe:
		return have != w
	case OpLt:
		return have < w
	case OpLte:
		return have <= w
	case OpGt:
		return have > w
	case OpGte:
		return have >= w
	default:
		return false
	}
}

func compareString(have string, op Op, want any) bool {
	switch op {
	case OpEq:
		s, ok := toString(want)
		return ok && have == s
	case OpNe:
		s, ok := toString(want)
		return ok && have != s
	case OpIn:
		for _, v := range toSlice(want) {
			if s, ok := toString(v); ok && s == have {
				return true
			}
		}
		return false
	default:
		// Ordering operators on a string compare its numeric form, if any.
		f, err := strconv.ParseFloat(have, 64)
		if err != nil {
			return false
		}
		return compareNumber(f, op, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Cost
// ─────────────────────────────────────────────────────────────────────────────

// evalCost returns (waste, skipCode, err). A non-empty skip code means the node
// is not a finding; err is reserved for a malformed spec that survived
// validation, which is a programmer error.
func evalCost(c *Cost, n *graph.Node, p *rules.Pass) (float64, rules.SkipCode, error) {
	kind := pricing.Kind(c.PriceKind)

	switch c.Kind {
	case CostFullResource, CostFraction:
		qty, ok := quantity(c, n)
		if !ok {
			return 0, rules.SkipMissingAttr, nil
		}
		sku, ok := skuOf(c, n)
		if !ok {
			return 0, rules.SkipMissingAttr, nil
		}
		cost, err := p.Price.MonthlyCost(pricing.Item{
			Kind:     kind,
			Provider: "gcp",
			SKU:      sku,
			Region:   pricing.RegionOf(n.Location),
			Quantity: qty,
		})
		if err != nil {
			return 0, rules.SkipNoPrice, nil
		}
		if c.Kind == CostFraction {
			cost *= c.Fraction
		}
		return cost, "", nil

	case CostClassDelta:
		qty, ok := quantity(c, n)
		if !ok {
			return 0, rules.SkipMissingAttr, nil
		}
		region := pricing.RegionOf(n.Location)
		from, _, err := p.Price.UnitPrice(kind, "gcp", c.FromClass, region)
		if err != nil {
			return 0, rules.SkipNoPrice, nil
		}
		to, _, err := p.Price.UnitPrice(kind, "gcp", c.ToClass, region)
		if err != nil {
			return 0, rules.SkipNoPrice, nil
		}
		delta := from - to
		if delta <= 0 {
			return 0, rules.SkipNoPrice, nil
		}
		return qty * c.Fraction * delta, "", nil

	case CostRightsize:
		return rightsize(c, n, p)

	default:
		return 0, "", fmt.Errorf("%w: unhandled cost kind %q", ErrInvalidSpec, c.Kind)
	}
}

// rightsize prices the delta to the smallest catalog shape in the same family
// that still meets the utilization target.
func rightsize(c *Cost, n *graph.Node, p *rules.Pass) (float64, rules.SkipCode, error) {
	if p.Sizer == nil {
		return 0, rules.SkipUnknownMachineType, nil
	}
	machineType, ok := n.Str(c.SKUField)
	if !ok || machineType == "" {
		return 0, rules.SkipMissingAttr, nil
	}
	util, ok := n.Metric(c.UtilMetric)
	if !ok {
		return 0, rules.SkipNoMetric, nil
	}
	spec, ok := p.Sizer.Spec(machineType)
	if !ok {
		return 0, rules.SkipUnknownMachineType, nil
	}
	region := pricing.RegionOf(n.Location)
	current, err := pricing.MachineCost(p.Price, p.Sizer, machineType, region)
	if err != nil {
		return 0, rules.SkipNoPrice, nil
	}
	needVCPU := util.Value * spec.VCPU / c.TargetUtil

	for _, cand := range p.Sizer.Ladder(spec.Family) {
		if cand.VCPU >= spec.VCPU || cand.VCPU < needVCPU {
			continue
		}
		cost, err := pricing.MachineCost(p.Price, p.Sizer, cand.Name, region)
		if err != nil || cost >= current {
			continue
		}
		return current - cost, "", nil
	}
	return 0, rules.SkipNoSmallerSize, nil
}

func quantity(c *Cost, n *graph.Node) (float64, bool) {
	var (
		q  float64
		ok bool
	)
	switch c.QuantitySource {
	case SrcMetric:
		m, found := n.Metric(c.QuantityField)
		q, ok = m.Value, found
	default:
		q, ok = n.Num(c.QuantityField)
	}
	if !ok {
		return 0, false
	}
	if c.QuantityScale != 0 {
		q *= c.QuantityScale
	}
	return q, true
}

func skuOf(c *Cost, n *graph.Node) (string, bool) {
	if c.SKUField == "" {
		return "", true // a kind with a single SKU needs no discriminator
	}
	s, ok := n.Str(c.SKUField)
	return s, ok
}

func collectEvidence(keys []string, n *graph.Node) []rules.Evidence {
	if len(keys) == 0 {
		return nil
	}
	out := make([]rules.Evidence, 0, len(keys))
	for _, k := range keys {
		if raw, ok := n.Attrs[k]; ok {
			out = append(out, rules.Ev(k, "%v", raw))
			continue
		}
		if m, ok := n.Metric(k); ok {
			out = append(out, rules.Ev(k, "%g", m.Value))
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// JSON value coercion
// ─────────────────────────────────────────────────────────────────────────────

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func toString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case fmt.Stringer:
		return t.String(), true
	default:
		return "", false
	}
}

func toSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	if v == nil {
		return nil
	}
	return []any{v}
}

// truthy interprets an operand for exists/bool predicates. A nil operand means
// "must be present/true", which is the common case in a hand-written spec.
func truthy(v any) bool {
	if v == nil {
		return true
	}
	if b, ok := v.(bool); ok {
		return b
	}
	f, ok := toFloat(v)
	return ok && f != 0
}
