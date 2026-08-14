package cli

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	_ "github.com/TypeOneLabs/tellury/pkg/rules/all"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// The README's Rules section is a per-provider set of tables, and nothing kept
// it honest: it drifted five rules behind the registry — every image rule was
// missing — while the whole suite stayed green. A reader choosing the tool sees
// that table long before they run `tellury rules list`.
//
// This pins the tables to the registry in both directions: every registered
// rule appears exactly once under its own provider heading, and the tables list
// nothing that is not registered. Severity and service are checked too, because
// a stale severity is a quieter lie than a missing row.

// The service group allows digits: AWS's service is "ec2". Without them the
// entire AWS table parses as zero rows and the test reports it as undocumented,
// which is a confusing way to learn about a regex bug.
var readmeRuleRow = regexp.MustCompile(`^\|\s*` + "`" + `([a-z0-9_]+)` + "`" + `\s*\|\s*([a-z0-9]+)\s*\|\s*([a-z]+)\s*\|`)

// readmeRuleTables parses the README's per-provider rule tables, returning
// ruleID -> (provider, service, severity). The provider comes from the "### "
// heading each table sits under.
func readmeRuleTables(t *testing.T) map[string]readmeRule {
	t.Helper()
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}

	// Only the Rules section; other sections have tables too.
	body := string(raw)
	start := strings.Index(body, "\n## Rules\n")
	if start < 0 {
		t.Fatal("README has no '## Rules' section")
	}
	rest := body[start+len("\n## Rules\n"):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}

	headings := map[string]string{
		"### AWS":   "aws",
		"### Azure": "azure",
		"### GCP":   "gcp",
	}

	out := map[string]readmeRule{}
	provider := ""
	for _, line := range strings.Split(rest, "\n") {
		if p, ok := headings[strings.TrimSpace(line)]; ok {
			provider = p
			continue
		}
		m := readmeRuleRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if provider == "" {
			t.Errorf("README rule row %q appears before any provider heading", m[1])
			continue
		}
		if prev, dup := out[m[1]]; dup {
			t.Errorf("README lists rule %q twice (under %s and %s)", m[1], prev.provider, provider)
		}
		out[m[1]] = readmeRule{provider: provider, service: m[2], severity: m[3]}
	}
	return out
}

type readmeRule struct{ provider, service, severity string }

func (r readmeRule) String() string {
	return fmt.Sprintf("%s/%s %s", r.provider, r.service, r.severity)
}

func TestReadmeRuleTablesMatchRegistry(t *testing.T) {
	documented := readmeRuleTables(t)
	if len(documented) == 0 {
		t.Fatal("parsed no rule rows from the README's Rules section")
	}

	registered := map[string]readmeRule{}
	for _, r := range rules.List() {
		m := r.Meta()
		registered[m.ID] = readmeRule{
			provider: m.Provider,
			service:  m.Service,
			severity: string(m.Severity),
		}
	}

	for id, want := range registered {
		got, ok := documented[id]
		if !ok {
			t.Errorf("rule %q is registered but missing from the README's %s table",
				id, strings.ToUpper(want.provider))
			continue
		}
		if got != want {
			t.Errorf("README documents rule %q as %s, registry says %s", id, got, want)
		}
	}

	for id := range documented {
		if _, ok := registered[id]; !ok {
			t.Errorf("README documents rule %q, which is not registered — a reader "+
				"cannot run it", id)
		}
	}

	if len(documented) != len(registered) {
		t.Errorf("README lists %d rules, registry has %d", len(documented), len(registered))
	}
}

// The section opens by counting the rules. A number in prose goes stale exactly
// as quietly as a missing table row.
func TestReadmeRuleCountMatchesRegistry(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	want := len(rules.List())
	words := map[int]string{
		11: "Eleven", 12: "Twelve", 13: "Thirteen", 14: "Fourteen", 15: "Fifteen",
		16: "Sixteen", 17: "Seventeen", 18: "Eighteen", 19: "Nineteen", 20: "Twenty",
	}
	word, ok := words[want]
	if !ok {
		t.Skipf("no spelled-out form for %d rules; extend the table above", want)
	}
	if !strings.Contains(string(raw), word+" rules") {
		t.Errorf("README's Rules section does not say %q; the registry has %d rules",
			word+" rules", want)
	}
}
