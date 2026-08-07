// Package gcp_test contains the offline registration tests. They live in the
// external test package (gcp_test) on purpose: they must pull in BOTH the
// compute/storage spec subpackages (to make the registry non-empty) AND the
// shipped rule packages (to derive the expected keys from the rules), and
// neither side may be assumed — the whole point is to verify that the two
// are wired together. Being external keeps them free of any import cycle.
package gcp_test

import (
	"sort"
	"testing"

	metricsgcp "github.com/TypeOneLabs/tellury/pkg/metrics/gcp"
	// Import the PROVIDER, not the spec subpackages directly. The bug this test exists to
	// catch is production code failing to register the specs; a test that blank-imports
	// compute and storage itself populates the registry on its own and passes no matter what
	// production does — which is exactly how the original regression shipped green.
	// Registration must reach us only through pkg/cloud/gcp's own imports.
	_ "github.com/TypeOneLabs/tellury/pkg/cloud/gcp"
	// Pull in every shipped rule so expected metric keys are derived from
	// what the rules actually ask for.
	_ "github.com/TypeOneLabs/tellury/pkg/rules/all"
	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// ruleMetricKeys returns every metric key that the shipped built-in rules
// declare in RequiredMetrics, deduplicated and sorted. This is the source of
// truth: it comes straight from the rules, so a new metric-dependent rule
// joins the expected set purely by being registered — it cannot be added
// without its spec also being registered, or these tests fail.
func ruleMetricKeys(t *testing.T) []string {
	t.Helper()
	set := map[string]bool{}
	for _, r := range rules.List() {
		for _, k := range r.Meta().RequiredMetrics {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestGCPMetricRegistryHasEveryKeyRulesNeed is the regression test for the
// silent failure where a refactor moved the metric specs into
// pkg/metrics/gcp/compute and pkg/metrics/gcp/storage but nothing imported
// them: the registry stayed empty and every rule that asked for a metric key
// saw it reported unsupported and silently skipped (build/vet/test all green).
//
// It fails in that state: every key a shipped rule requires in RequiredMetrics
// must exist in Specs(). If the blank import of either spec subpackage is
// removed, its keys vanish and the test fails.
func TestGCPMetricRegistryHasEveryKeyRulesNeed(t *testing.T) {
	keys := ruleMetricKeys(t)
	if len(keys) == 0 {
		t.Fatal("no shipped rule declares a RequiredMetrics key; this test is vacuous")
	}

	registered := map[string]bool{}
	for _, s := range metricsgcp.Specs() {
		registered[s.Key] = true
	}

	var failures []string
	for _, key := range keys {
		if !registered[key] {
			failures = append(failures, key)
		}
	}
	if len(failures) > 0 {
		t.Fatalf("shipped rules require metric keys that are NOT in the GCP spec registry: %v; "+
			"the blank import registering them (pkg/metrics/gcp/compute or pkg/metrics/gcp/storage) is missing", failures)
	}
}

// TestConstructedProviderSupportsRuleMetricKeys drives the exact support check
// a constructed provider uses. metricsgcp.Client.Supports reports a key as
// supported iff its spec is registered — the mechanism EnrichMetrics consults
// when deciding whether a requested key is usable. Constructing a Client here
// is fully offline (Supports never touches Cloud Monitoring), so this is an
// ordinary offline test.
//
// In the broken (empty-registry) state, Supports returns false for every
// rule-required key and the test fails.
func TestConstructedProviderSupportsRuleMetricKeys(t *testing.T) {
	// A Client with a nil transport is fine: Supports reads only the spec
	// registry, never the connection.
	p := &metricsgcp.Client{}
	for _, key := range ruleMetricKeys(t) {
		if !p.Supports(key) {
			t.Errorf("constructed GCP provider reports metric key %q as unsupported; "+
				"its spec is not registered (missing blank import?)", key)
		}
	}
}
