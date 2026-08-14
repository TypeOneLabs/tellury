package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A missing permission is the quietest failure this tool has.
//
// On Azure, Resource Graph returns an EMPTY RESULT SET for a resource type the
// identity cannot read, rather than an error — docs/azure-setup.md says so
// itself, and warns that the least-privilege role "must be extended whenever a
// rule that reads a new resource type is added". It then wasn't: the role was
// missing virtualMachines/read, skus/read and Insights/metrics/read while the
// suite stayed green. On AWS a missing Describe* surfaces as a reference
// enumeration marked incomplete, so the rule skips and reports nothing.
//
// In both cases the operator sees a clean scan. These tests make the warning
// mechanical: the setup guides must name a permission for every API the scanner
// actually calls.

// awsCallPattern matches an AWS SDK operation by its Input struct, e.g.
// `ec2.DescribeVolumesInput{` or `cloudwatch.GetMetricDataInput`.
var awsCallPattern = regexp.MustCompile(`\b(ec2|cloudwatch|pricing|organizations|autoscaling|sts)\.([A-Z][A-Za-z0-9]+)Input\b`)

// awsNoIAMAction are calls that need no IAM permission to be granted.
// sts:GetCallerIdentity is allowed for every authenticated principal and cannot
// be denied by an identity policy.
var awsNoIAMAction = map[string]bool{
	"sts:GetCallerIdentity": true,
}

// goSourceFiles walks roots and returns every non-test .go file.
func goSourceFiles(t *testing.T, roots ...string) []string {
	t.Helper()
	var out []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			out = append(out, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no Go sources under %v", roots)
	}
	return out
}

func TestAWSSetupDocumentsEveryAPICall(t *testing.T) {
	doc, err := os.ReadFile("../../docs/aws-setup.md")
	if err != nil {
		t.Fatalf("read aws-setup.md: %v", err)
	}
	documented := string(doc)

	called := map[string]string{} // "service:Operation" -> file it was found in
	for _, path := range goSourceFiles(t, "../../pkg/cloud/aws", "../../pkg/pricing/aws", "../../pkg/metrics/aws") {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range awsCallPattern.FindAllStringSubmatch(string(src), -1) {
			called[m[1]+":"+m[2]] = path
		}
	}
	if len(called) == 0 {
		t.Fatal("found no AWS SDK calls; the detection pattern has stopped working")
	}

	var missing []string
	for action, path := range called {
		if awsNoIAMAction[action] {
			continue
		}
		if !strings.Contains(documented, action) {
			missing = append(missing, action+"  (called in "+filepath.Base(path)+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("docs/aws-setup.md does not mention %s — without it the call fails "+
			"and the rule skips, which an operator sees as a clean scan", m)
	}
	t.Logf("%d AWS operations called, all documented", len(called))
}

// argTypePattern matches the Resource Graph type constants the Azure provider
// queries, e.g. "microsoft.compute/virtualmachines".
var argTypePattern = regexp.MustCompile(`"(microsoft\.[a-z]+/[a-z/]+)"`)

// argTypeNoAction are Resource Graph types with no distinct ARM read action to
// grant beyond the Resource Graph permissions themselves.
var argTypeNoAction = map[string]bool{}

func TestAzureSetupDocumentsEveryResourceType(t *testing.T) {
	doc, err := os.ReadFile("../../docs/azure-setup.md")
	if err != nil {
		t.Fatalf("read azure-setup.md: %v", err)
	}
	// Compare case-insensitively: the code spells ARG types lowercase
	// ("microsoft.compute/virtualmachines"), ARM actions are camel-cased
	// ("Microsoft.Compute/virtualMachines/read").
	documented := strings.ToLower(string(doc))

	queried := map[string]string{}
	for _, path := range goSourceFiles(t, "../../pkg/cloud/azure") {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range argTypePattern.FindAllStringSubmatch(string(src), -1) {
			// Skip partial constants that are not whole resource types.
			if strings.Count(m[1], "/") == 0 {
				continue
			}
			queried[m[1]] = path
		}
	}
	if len(queried) == 0 {
		t.Fatal("found no Resource Graph types; the detection pattern has stopped working")
	}

	var missing []string
	for argType, path := range queried {
		if argTypeNoAction[argType] {
			continue
		}
		if !strings.Contains(documented, argType+"/read") {
			missing = append(missing, argType+"  (queried in "+filepath.Base(path)+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("docs/azure-setup.md's custom role has no read action for %s — "+
			"Resource Graph returns an EMPTY SET for an unreadable type, so the scan "+
			"reports a clean bill of health instead of failing", m)
	}
	t.Logf("%d Resource Graph types queried, all documented", len(queried))
}
