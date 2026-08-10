package output

import (
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

// TestRenderHTML_EscapesHostileNames is the security regression test: resource
// names, project IDs, rule IDs and evidence values come from a cloud API and
// are interpolated into HTML element text and attributes. A resource named
// "</script><script>alert(1)</script>" — the JSON-in-script breaker — must
// render inert: the literal text appears, but never as live markup, and the
// document's only </script> is the inline script's own legitimate close tag.
// A hostile PROJECT name also reaches the SVG bar chart (two projects force
// the chart to render), so SVG element text is exercised as its own escaping
// context.
func TestRenderHTML_EscapesHostileNames(t *testing.T) {
	scriptName := "</script><script>alert(1)</script>"
	findings := []rules.Finding{
		{
			RuleID:          "no_lifecycle_policy<script>alert('x')</script>",
			Resource:        scriptName,
			Project:         "<svg/onload=alert(1)>",
			Severity:        rules.SeverityHigh,
			Confidence:      0.9,
			MonthlyWasteUSD: 3.30,
			Evidence: []rules.Evidence{
				{Key: "bucket_name", Value: "<img src=x onerror=alert(1)>"},
				{Key: "price_source", Value: "embedded_fallback sku=<script>alert(9)</script>"},
			},
		},
		{
			RuleID:          "detached_disk",
			Resource:        "disk/pd-02",
			Project:         "another-proj",
			Severity:        rules.SeverityLow,
			Confidence:      0.4,
			MonthlyWasteUSD: 1.00,
		},
	}
	r := baseReport(findings)
	r.MultiProject = true // hostile project name must reach the Project column AND the SVG chart
	got := renderHTML(t, r)

	// The raw hostile markup must NEVER appear verbatim anywhere in the
	// document — element text, attributes, or SVG text.
	for _, bad := range []string{
		scriptName, // the </script> breaker, in its full form
		"<script>alert('x')</script>",
		"<img src=x onerror=alert(1)>",
		"<svg/onload=alert(1)>",
		"<script>alert(9)</script>",
		"</script><script>", // a raw close-then-open is always hostile
	} {
		if strings.Contains(got, bad) {
			t.Errorf("hostile markup %q appears raw (unescaped) in the rendered HTML", bad)
		}
	}
	// The ONLY raw </script> in the document must be the inline script's own
	// close tag — the hostile resource name must not have injected another.
	if c := strings.Count(got, "</script>"); c != 1 {
		t.Errorf("document must contain exactly one </script> (the inline script close tag), got %d", c)
	}
	if c := strings.Count(got, "<script"); c != 1 {
		t.Errorf("document must contain exactly one <script> element, got %d", c)
	}

	// The escaped forms MUST appear, proving the values are present but inert
	// — in element text, in attributes, and in the SVG chart's text nodes.
	for _, want := range []string{
		"&lt;/script&gt;&lt;script&gt;alert(1)&lt;/script&gt;",
		"&lt;script&gt;alert(&#39;x&#39;)&lt;/script&gt;",
		"&lt;img src=x onerror=alert(1)&gt;",
		"&lt;svg/onload=alert(1)&gt;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("escaped form %q missing from rendered HTML", want)
		}
	}
	// The hostile project name must appear (escaped) inside the SVG chart,
	// proving SVG text is escaped like every other element-text context.
	if strings.Index(got, "<svg") > strings.Index(got, "&lt;svg/onload=alert(1)&gt;") {
		t.Errorf("hostile project name must be rendered (escaped) inside the SVG chart")
	}
}
