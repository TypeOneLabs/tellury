package rules

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

var (
	mu       sync.RWMutex
	registry = map[string]Rule{}
)

// Register is called from each rule package's init(). Panics on duplicate ID.
func Register(r Rule) {
	mu.Lock()
	defer mu.Unlock()
	id := r.Meta().ID
	if id == "" {
		panic("rules: Register with empty Meta.ID")
	}
	if _, dup := registry[id]; dup {
		panic("rules: duplicate rule ID " + id)
	}
	registry[id] = r
}

// List returns all registered rules sorted by ID.
func List() []Rule {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Rule, 0, len(registry))
	for _, r := range registry {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta().ID < out[j].Meta().ID })
	return out
}

// Get looks up a rule by ID.
func Get(id string) (Rule, bool) {
	mu.RLock()
	defer mu.RUnlock()
	r, ok := registry[id]
	return r, ok
}

// Select resolves the --rules/--skip-rules flags.
func Select(provider string, include, exclude []string) ([]Rule, error) {
	all := List()
	byID := map[string]Rule{}
	for _, r := range all {
		byID[r.Meta().ID] = r
	}
	var chosen []Rule
	if len(include) == 0 {
		for _, r := range all {
			if r.Meta().Provider == provider {
				chosen = append(chosen, r)
			}
		}
	} else {
		for _, id := range include {
			r, ok := byID[strings.TrimSpace(id)]
			if !ok {
				return nil, fmt.Errorf("unknown rule %q (see `tellury rules list`)", id)
			}
			chosen = append(chosen, r)
		}
	}
	if len(exclude) > 0 {
		skip := map[string]bool{}
		for _, id := range exclude {
			skip[strings.TrimSpace(id)] = true
		}
		kept := chosen[:0]
		for _, r := range chosen {
			if !skip[r.Meta().ID] {
				kept = append(kept, r)
			}
		}
		chosen = kept
	}
	return chosen, nil
}

// Plan aggregates the data requirements of a rule set.
func Plan(rs []Rule) (assetTypes, metricKeys []string) {
	at, mk := map[string]bool{}, map[string]bool{}
	for _, r := range rs {
		for _, t := range r.Meta().RequiredAssetTypes {
			at[t] = true
		}
		for _, k := range r.Meta().RequiredMetrics {
			mk[k] = true
		}
	}
	return sortedKeys(at), sortedKeys(mk)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
