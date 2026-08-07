package compiler

import (
	"fmt"
	"sort"

	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// SpecVersion is the only IR version this build accepts.
const SpecVersion = 1

// RuleSpec is the declarative IR: stable, versioned, human-reviewable.
//
// Deliberately NOT Turing-complete: no loops, no user functions, no eval. A rule
// that cannot be expressed here should be written as native Go and registered
// normally. That boundary is what keeps the compiler safe.
type RuleSpec struct {
	Version int      `json:"version"`
	ID      string   `json:"id"`
	Meta    SpecMeta `json:"meta"`

	// Match selects candidate nodes.
	Match Match `json:"match"`
	// Where are AND-ed predicates. Any unsatisfiable predicate ⇒ skip node.
	Where []Predicate `json:"where"`
	// Cost computes MonthlyWasteUSD.
	Cost Cost `json:"cost"`
	// Evidence names attrs/metrics to surface in output.
	Evidence []string `json:"evidence,omitempty"`
}

// SpecMeta is the reviewable, human-facing half of a spec.
type SpecMeta struct {
	Provider    string  `json:"provider"`
	Service     string  `json:"service"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Severity    string  `json:"severity"`
	Remediation string  `json:"remediation"`
	Confidence  float64 `json:"confidence"`
}

// Match selects the candidate node set.
type Match struct {
	Kind       string   `json:"kind"`
	AssetTypes []string `json:"asset_types,omitempty"`
}

// Op is the closed predicate operator set.
type Op string

// Predicate operators.
const (
	OpEq            Op = "eq"
	OpNe            Op = "ne"
	OpLt            Op = "lt"
	OpLte           Op = "lte"
	OpGt            Op = "gt"
	OpGte           Op = "gte"
	OpIn            Op = "in"
	OpExists        Op = "exists"
	OpOlderThanDays Op = "older_than_days"
)

var validOps = map[Op]bool{
	OpEq: true, OpNe: true, OpLt: true, OpLte: true, OpGt: true,
	OpGte: true, OpIn: true, OpExists: true, OpOlderThanDays: true,
}

// ValueSource selects where a Predicate reads its left-hand value.
//
// Named ValueSource (not Source) because compiler.Source already names the
// top-level "thing being compiled" (Text/Spec/Hints) in compiler.go; the two
// concepts are unrelated and must not share an identifier in this package.
type ValueSource string

// Predicate value sources.
const (
	SrcAttr      ValueSource = "attr"
	SrcMetric    ValueSource = "metric"
	SrcLabel     ValueSource = "label"
	SrcInDegree  ValueSource = "in_degree"
	SrcOutDegree ValueSource = "out_degree"
)

var validSources = map[ValueSource]bool{
	SrcAttr: true, SrcMetric: true, SrcLabel: true,
	SrcInDegree: true, SrcOutDegree: true,
}

// Predicate is one AND-ed condition.
type Predicate struct {
	Source ValueSource `json:"source"`
	Field  string      `json:"field"`
	Op     Op          `json:"op"`
	Value  any         `json:"value,omitempty"`
	// MinSamples guards metric sources against thin data. Default 1.
	MinSamples int `json:"min_samples,omitempty"`
	// MinCoverage guards metric sources against sparse windows.
	MinCoverage float64 `json:"min_coverage,omitempty"`
}

// CostKind enumerates the supported cost formulas. Extending this list is a
// deliberate, reviewed act: it is the only place money is computed.
type CostKind string

// Cost formulas.
const (
	// CostFullResource: waste = full monthly cost of the resource.
	CostFullResource CostKind = "full_resource"
	// CostFraction: waste = full monthly cost × Fraction.
	CostFraction CostKind = "fraction"
	// CostClassDelta: waste = quantity × Fraction × (price(From) − price(To)).
	CostClassDelta CostKind = "class_delta"
	// CostRightsize: waste = cost(current) − cost(smallest size meeting Target).
	CostRightsize CostKind = "rightsize"
)

var validCostKinds = map[CostKind]bool{
	CostFullResource: true, CostFraction: true,
	CostClassDelta: true, CostRightsize: true,
}

// Cost declares how MonthlyWasteUSD is computed.
type Cost struct {
	Kind CostKind `json:"kind"`
	// PriceKind maps to pricing.Item.Kind.
	PriceKind string `json:"price_kind"`
	// Quantity* name the billable quantity.
	QuantitySource ValueSource `json:"quantity_source,omitempty"`
	QuantityField  string      `json:"quantity_field,omitempty"`
	QuantityScale  float64     `json:"quantity_scale,omitempty"`
	// SKUField names the attr holding the SKU discriminator.
	SKUField string `json:"sku_field,omitempty"`

	Fraction   float64 `json:"fraction,omitempty"`
	FromClass  string  `json:"from_class,omitempty"`
	ToClass    string  `json:"to_class,omitempty"`
	TargetUtil float64 `json:"target_util,omitempty"`
	UtilMetric string  `json:"util_metric,omitempty"`
}

// Validate returns diagnostics. Anything at DiagError blocks interpretation.
func (s *RuleSpec) Validate() []Diagnostic {
	var d []Diagnostic
	if s == nil {
		return []Diagnostic{errorDiag("", "spec is nil")}
	}
	if s.Version != SpecVersion {
		d = append(d, errorDiag("/version",
			fmt.Sprintf("unsupported spec version %d (want %d)", s.Version, SpecVersion)))
	}
	if s.ID == "" {
		d = append(d, errorDiag("/id", "id is required"))
	}
	if s.Meta.Provider == "" {
		d = append(d, errorDiag("/meta/provider", "provider is required"))
	}
	switch rules.Severity(s.Meta.Severity) {
	case rules.SeverityLow, rules.SeverityMedium, rules.SeverityHigh:
	case "":
		d = append(d, warnDiag("/meta/severity", "severity empty, defaulting to medium"))
	default:
		d = append(d, errorDiag("/meta/severity",
			fmt.Sprintf("unknown severity %q", s.Meta.Severity)))
	}
	if s.Meta.Confidence < 0 || s.Meta.Confidence > 1 {
		d = append(d, errorDiag("/meta/confidence", "confidence must be in [0,1]"))
	}
	if s.Match.Kind == "" {
		d = append(d, errorDiag("/match/kind", "match.kind is required"))
	}

	for i, p := range s.Where {
		base := fmt.Sprintf("/where/%d", i)
		if !validSources[p.Source] {
			d = append(d, errorDiag(base+"/source", fmt.Sprintf("unknown source %q", p.Source)))
		}
		if !validOps[p.Op] {
			d = append(d, errorDiag(base+"/op", fmt.Sprintf("unknown op %q", p.Op)))
		}
		if p.Field == "" {
			d = append(d, errorDiag(base+"/field", "field is required"))
		}
		if p.Op != OpExists && p.Value == nil {
			d = append(d, errorDiag(base+"/value", fmt.Sprintf("op %q requires a value", p.Op)))
		}
		if p.Source == SrcMetric && !metrics.Known(p.Field) {
			d = append(d, errorDiag(base+"/field",
				fmt.Sprintf("metric %q is not in the metrics registry", p.Field)))
		}
	}

	if !validCostKinds[s.Cost.Kind] {
		d = append(d, errorDiag("/cost/kind", fmt.Sprintf("unknown cost kind %q", s.Cost.Kind)))
		return d
	}
	if s.Cost.PriceKind == "" {
		d = append(d, errorDiag("/cost/price_kind", "price_kind is required"))
	}
	switch s.Cost.Kind {
	case CostFullResource:
		if s.Cost.QuantityField == "" {
			d = append(d, errorDiag("/cost/quantity_field", "full_resource requires quantity_field"))
		}
	case CostFraction:
		if s.Cost.QuantityField == "" {
			d = append(d, errorDiag("/cost/quantity_field", "fraction requires quantity_field"))
		}
		if s.Cost.Fraction <= 0 || s.Cost.Fraction > 1 {
			d = append(d, errorDiag("/cost/fraction", "fraction must be in (0,1]"))
		}
	case CostClassDelta:
		if s.Cost.QuantityField == "" {
			d = append(d, errorDiag("/cost/quantity_field", "class_delta requires quantity_field"))
		}
		if s.Cost.Fraction <= 0 || s.Cost.Fraction > 1 {
			d = append(d, errorDiag("/cost/fraction", "fraction must be in (0,1]"))
		}
		if s.Cost.FromClass == "" || s.Cost.ToClass == "" {
			d = append(d, errorDiag("/cost", "class_delta requires from_class and to_class"))
		}
	case CostRightsize:
		if s.Cost.TargetUtil <= 0 || s.Cost.TargetUtil > 1 {
			d = append(d, errorDiag("/cost/target_util", "target_util must be in (0,1]"))
		}
		if !metrics.Known(s.Cost.UtilMetric) {
			d = append(d, errorDiag("/cost/util_metric",
				fmt.Sprintf("util_metric %q is not in the metrics registry", s.Cost.UtilMetric)))
		}
		if s.Cost.SKUField == "" {
			d = append(d, errorDiag("/cost/sku_field", "rightsize requires sku_field"))
		}
	}
	return d
}

// RequiredMetrics derives Meta.RequiredMetrics from Where + Cost, so the
// engine's enrichment planner works identically for compiled and native rules.
func (s *RuleSpec) RequiredMetrics() []string {
	seen := map[string]bool{}
	for _, p := range s.Where {
		if p.Source == SrcMetric && metrics.Known(p.Field) {
			seen[p.Field] = true
		}
	}
	if s.Cost.UtilMetric != "" && metrics.Known(s.Cost.UtilMetric) {
		seen[s.Cost.UtilMetric] = true
	}
	if s.Cost.QuantitySource == SrcMetric && metrics.Known(s.Cost.QuantityField) {
		seen[s.Cost.QuantityField] = true
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ToMeta renders the spec's declaration as engine Meta.
func (s *RuleSpec) ToMeta() rules.Meta {
	sev := rules.Severity(s.Meta.Severity)
	if sev == "" {
		sev = rules.SeverityMedium
	}
	return rules.Meta{
		ID:                 s.ID,
		Provider:           s.Meta.Provider,
		Service:            s.Meta.Service,
		Title:              s.Meta.Title,
		Description:        s.Meta.Description,
		Severity:           sev,
		RequiredAssetTypes: s.Match.AssetTypes,
		RequiredMetrics:    s.RequiredMetrics(),
		Remediation:        s.Meta.Remediation,
		Origin:             rules.OriginCompiled,
	}
}
