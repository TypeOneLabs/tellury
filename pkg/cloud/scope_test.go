package cloud

import "testing"

// TestScopeValidate_GCPExactlyOne asserts the GCP block enforces exactly one
// of project/folder/organization — the historical Scope contract, unchanged.
func TestScopeValidate_GCPExactlyOne(t *testing.T) {
	valid := []Scope{
		{Provider: "gcp", GCP: &GCPScope{Project: "p"}},
		{Provider: "gcp", GCP: &GCPScope{Folder: "f"}},
		{Provider: "gcp", GCP: &GCPScope{Organization: "o"}},
	}
	for _, s := range valid {
		if err := s.Validate(); err != nil {
			t.Errorf("Validate(%v) failed: %v", s, err)
		}
	}
	invalid := []Scope{
		{Provider: "gcp", GCP: &GCPScope{}},                          // none set
		{Provider: "gcp", GCP: &GCPScope{Project: "p", Folder: "f"}}, // two set
		{Provider: "gcp"}, // nil block
	}
	for _, s := range invalid {
		if err := s.Validate(); err == nil {
			t.Errorf("Validate(%v) must fail", s)
		}
	}
}

// TestScopeValidate_AWSExactlyOne asserts the AWS block enforces exactly one
// of account/organizational-unit/organization, structurally the same rule as
// GCP's but in AWS vocabulary.
func TestScopeValidate_AWSExactlyOne(t *testing.T) {
	valid := []Scope{
		{Provider: "aws", AWS: &AWSScope{Account: "123456789012"}},
		{Provider: "aws", AWS: &AWSScope{OrganizationalUnit: "ou-abc"}},
		{Provider: "aws", AWS: &AWSScope{Organization: "o-abc"}},
	}
	for _, s := range valid {
		if err := s.Validate(); err != nil {
			t.Errorf("Validate(%v) failed: %v", s, err)
		}
	}
	invalid := []Scope{
		{Provider: "aws", AWS: &AWSScope{}},
		{Provider: "aws", AWS: &AWSScope{Account: "1", OrganizationalUnit: "ou-1"}},
		{Provider: "aws"},
	}
	for _, s := range invalid {
		if err := s.Validate(); err == nil {
			t.Errorf("Validate(%v) must fail", s)
		}
	}
}

// TestScopeValidate_UnknownProvider asserts an unrecognized provider is
// rejected as UnknownProviderError — the same gate config applies to
// --provider.
func TestScopeValidate_UnknownProvider(t *testing.T) {
	err := Scope{Provider: "mars"}.Validate()
	if err == nil {
		t.Fatal("Validate with an unknown provider must fail")
	}
	if err.Error() != `cloud: unknown provider "mars"` {
		t.Fatalf("unknown-provider error = %q, want cloud: unknown provider \"mars\"", err)
	}
}

// TestScopeParent_GCP pins the CAI parent rendering the GCP provider feeds to
// SearchAllResources: "projects/<id>", "folders/<n>", "organizations/<n>".
// This is the historical Parent() output, byte for byte.
func TestScopeParent_GCP(t *testing.T) {
	for _, tc := range []struct {
		scope Scope
		want  string
	}{
		{Scope{Provider: "gcp", GCP: &GCPScope{Project: "my-project"}}, "projects/my-project"},
		{Scope{Provider: "gcp", GCP: &GCPScope{Folder: "123"}}, "folders/123"},
		{Scope{Provider: "gcp", GCP: &GCPScope{Organization: "456"}}, "organizations/456"},
	} {
		if got := tc.scope.Parent(); got != tc.want {
			t.Errorf("Parent(%v) = %q, want %q", tc.scope, got, tc.want)
		}
	}
}

// TestScopeParent_AWS pins the AWS parent contract: AWS has no single API
// parent string — its provider drives Organizations and per-account calls
// from the scope fields directly — so Parent() renders "".
func TestScopeParent_AWS(t *testing.T) {
	for _, tc := range []struct {
		scope Scope
		want  string
	}{
		{Scope{Provider: "aws", AWS: &AWSScope{Account: "123"}}, ""},
		{Scope{Provider: "aws", AWS: &AWSScope{OrganizationalUnit: "ou-1"}}, ""},
		{Scope{Provider: "aws", AWS: &AWSScope{Organization: "o-1"}}, ""},
	} {
		if got := tc.scope.Parent(); got != tc.want {
			t.Errorf("Parent(%v) = %q, want %q", tc.scope, got, tc.want)
		}
	}
}

// TestScopeString pins the report/display rendering in each provider's own
// vocabulary. GCP renders exactly as Parent did before this redesign
// ("projects/my-project"); AWS renders "accounts/<id>",
// "organizational-units/<ou-id>" or "organizations/<org-id>".
func TestScopeString(t *testing.T) {
	for _, tc := range []struct {
		scope Scope
		want  string
	}{
		{Scope{Provider: "gcp", GCP: &GCPScope{Project: "my-project"}}, "projects/my-project"},
		{Scope{Provider: "gcp", GCP: &GCPScope{Folder: "123"}}, "folders/123"},
		{Scope{Provider: "gcp", GCP: &GCPScope{Organization: "456"}}, "organizations/456"},
		{Scope{Provider: "aws", AWS: &AWSScope{Account: "123456789012"}}, "accounts/123456789012"},
		{Scope{Provider: "aws", AWS: &AWSScope{OrganizationalUnit: "ou-abc"}}, "organizational-units/ou-abc"},
		{Scope{Provider: "aws", AWS: &AWSScope{Organization: "o-abc"}}, "organizations/o-abc"},
		{Scope{}, ""}, // no provider, no block: renders empty, never panics
	} {
		if got := tc.scope.String(); got != tc.want {
			t.Errorf("String(%v) = %q, want %q", tc.scope, got, tc.want)
		}
	}
}
