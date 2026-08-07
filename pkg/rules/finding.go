package rules

import "github.com/TypeOneLabs/tellury/pkg/graph"

// Finding is one unit of detected waste. Shape maps 1:1 to CLI columns.
type Finding struct {
	RuleID     string             `json:"rule_id"`
	ResourceID graph.Ref          `json:"resource_id"`
	Resource   string             `json:"resource"`
	Kind       graph.ResourceKind `json:"kind"`
	Project    string             `json:"project"`
	Location   string             `json:"location"`
	Severity   Severity           `json:"severity"`

	// MonthlyWasteUSD is the reclaimable spend per 30-day month.
	MonthlyWasteUSD float64 `json:"monthly_waste_usd"`
	// Confidence in [0,1]; drives --min-confidence filtering.
	Confidence float64 `json:"confidence"`

	Evidence    []Evidence `json:"evidence,omitempty"`
	Remediation string     `json:"remediation,omitempty"`
}

// Evidence is one supporting fact: {"p95_cpu", "3.10%"}.
type Evidence struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}
