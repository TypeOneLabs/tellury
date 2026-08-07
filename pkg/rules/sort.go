package rules

import (
	"fmt"
	"sort"
)

// SortOrder is a validated --sort value.
type SortOrder string

// Supported --sort values. SortWaste is the engine's own default ordering
// (waste desc, then resource, then rule); the other two exist so the
// documented CLI example ("--sort resource") is reproducible regardless of
// dollar amounts.
const (
	SortWaste    SortOrder = "waste"
	SortResource SortOrder = "resource"
	SortRule     SortOrder = "rule"
)

// ParseSortOrder validates a --sort flag value.
func ParseSortOrder(s string) (SortOrder, error) {
	switch SortOrder(s) {
	case SortWaste, SortResource, SortRule:
		return SortOrder(s), nil
	default:
		return "", fmt.Errorf("invalid --sort %q (want waste|resource|rule)", s)
	}
}

// SortFindings reorders findings in place per the given order. Engine.Run
// already leaves findings in SortWaste order; this is only a re-sort for the
// other two orders, and is a no-op (stable) for SortWaste.
func SortFindings(fs []Finding, order SortOrder) {
	switch order {
	case SortResource:
		sort.SliceStable(fs, func(i, j int) bool {
			if fs[i].Resource != fs[j].Resource {
				return fs[i].Resource < fs[j].Resource
			}
			return fs[i].RuleID < fs[j].RuleID
		})
	case SortRule:
		sort.SliceStable(fs, func(i, j int) bool {
			if fs[i].RuleID != fs[j].RuleID {
				return fs[i].RuleID < fs[j].RuleID
			}
			return fs[i].Resource < fs[j].Resource
		})
	default: // SortWaste — already the engine's default order.
		sort.SliceStable(fs, func(i, j int) bool {
			a, b := fs[i], fs[j]
			switch {
			case a.MonthlyWasteUSD != b.MonthlyWasteUSD:
				return a.MonthlyWasteUSD > b.MonthlyWasteUSD
			case a.Resource != b.Resource:
				return a.Resource < b.Resource
			default:
				return a.RuleID < b.RuleID
			}
		})
	}
}
