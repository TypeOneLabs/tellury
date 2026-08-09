// Package all_test contains the offline rule-registration test. It lives in
// the external test package (all_test) on purpose: it must import both the
// rule packages THEMSELVES (to keep every built-in rule registered regardless
// of what pkg/rules/all does, so a dropped blank import cannot silently shrink
// both sides of the comparison) AND parse the `all` package's imports (to know
// what `all` actually ships). Neither half may be assumed — the test's job is
// to verify the two agree and are non-empty.
package all_test

import (
	"go/build"
	"sort"
	"strings"
	"testing"

	// Keep every built-in rule registered INDEPENDENTLY of pkg/rules/all. A
	// plain _ import of pkg/rules/all would make both the expected set AND the
	// registered set shrink together when a rule is dropped from `all`, which
	// would let the test go green while a rule silently vanished. These direct
	// imports hold each rule alive, so removing a blank import from `all` shows
	// up as a rule that is registered but no longer shipped by `all`.
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/detached_disk"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/old_snapshot"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/underutilized_instance"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/unused_reserved_ip"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/gcs/no_lifecycle_policy"

	"github.com/TypeOneLabs/tellury/pkg/rules"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/all" // what `all` actually ships
)

// importedRulePackages returns the import paths of every rule package that
// pkg/rules/all blank-imports. It uses go/build to read the package's actual
// source Import declarations — the source of truth — rather than a hand-coded
// list, so a rule package added to (or removed from) `all` is reflected here
// automatically.
func importedRulePackages(t *testing.T) []string {
	t.Helper()
	pkg, err := build.Import("github.com/TypeOneLabs/tellury/pkg/rules/all", "", 0)
	if err != nil {
		t.Fatalf("build.Import(pkg/rules/all): %v", err)
	}

	ruleRoot := "github.com/TypeOneLabs/tellury/pkg/rules/"
	var out []string
	for _, imp := range pkg.Imports {
		if strings.HasPrefix(imp, ruleRoot) && imp != "github.com/TypeOneLabs/tellury/pkg/rules" {
			out = append(out, imp)
		}
	}
	sort.Strings(out)
	return out
}

// ruleIDOf maps an imported rule package path to the rule ID its package is
// required to register. The native convention (enforced by every shipped rule
// and the build) is 1:1: package basename == Meta().ID.
func ruleIDOf(importPath string) string {
	parts := strings.Split(importPath, "/")
	return parts[len(parts)-1]
}

// TestRuleRegistryContainsEveryRuleAllImports is the regression test for the
// silent failure where the rule registry drained to empty (or never filled),
// or a rule was dropped from `all`'s import list — in both cases rules silently
// stopped running while build/vet/test stayed green. It fails in that state
// three ways:
//
//   - the imported rule set (what `all` ships) is empty;
//   - the registry is empty while `all` imports non-empty rule packages;
//   - a registered rule exists that `all` no longer imports (dropped blank
//     import), or an imported rule package's init() never registered.
//
// Because this test package imports every rule package directly, the dropped
// blank-import case is NOT masked: the rule stays registered here even when
// `all` stops referencing it, so the reverse-direction check below flags it.
func TestRuleRegistryContainsEveryRuleAllImports(t *testing.T) {
	imported := importedRulePackages(t)
	if len(imported) == 0 {
		t.Fatal("pkg/rules/all imports no rule packages; the built-in rule set is empty — this is a silent failure, not a passing invariant")
	}

	registered := rules.List()
	if len(registered) == 0 {
		t.Fatalf("pkg/rules/all imports %d rule packages (%v) but the rule registry is EMPTY; "+
			"no rule init() ran — every rule silently skipped", len(imported), imported)
	}

	importedIDs := map[string]bool{}
	for _, imp := range imported {
		importedIDs[ruleIDOf(imp)] = true
	}

	// Forward: every package `all` imports must have a registered rule with a
	// matching ID. Catches an imported package whose init() never called
	// rules.Register (or registered a different ID).
	var unregistered []string
	for _, imp := range imported {
		if !importedIDs[ruleIDOf(imp)] {
			continue
		}
		found := false
		for _, r := range registered {
			if r.Meta().ID == ruleIDOf(imp) {
				found = true
				break
			}
		}
		if !found {
			unregistered = append(unregistered, imp)
		}
	}
	if len(unregistered) > 0 {
		t.Fatalf("pkg/rules/all imports rule packages whose register-side rule is MISSING from the registry: %v; "+
			"the imported package's init()/Register did not run or registered a different ID", unregistered)
	}

	// Reverse: every registered native rule ID must be accounted for by a
	// matching package in `all`'s import set. This is what catches a dropped
	// blank import: the rule stays registered (this test holds it alive) but
	// `all` no longer ships it, so it shows up as orphaned here.
	var orphaned []string
	for _, r := range registered {
		if !importedIDs[r.Meta().ID] {
			orphaned = append(orphaned, r.Meta().ID)
		}
	}
	if len(orphaned) > 0 {
		t.Fatalf("registered native rules with NO matching pkg/rules/all import: %v; "+
			"a blank import in pkg/rules/all is missing", orphaned)
	}

	// Duplicate-ID guard.
	seen := map[string]bool{}
	for _, r := range registered {
		if seen[r.Meta().ID] {
			t.Fatalf("duplicate rule ID %q in registry", r.Meta().ID)
		}
		seen[r.Meta().ID] = true
	}
}
