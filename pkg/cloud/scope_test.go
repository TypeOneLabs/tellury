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

// TestScopeValidate_AzureExactlyOne asserts the Azure block enforces the four
// valid scope shapes and the resource-group dependency.
func TestScopeValidate_AzureExactlyOne(t *testing.T) {
	valid := []Scope{
		{Provider: "azure", Azure: &AzureScope{Tenant: "t"}},
		{Provider: "azure", Azure: &AzureScope{ManagementGroup: "mg"}},
		{Provider: "azure", Azure: &AzureScope{Subscription: "s"}},
		{Provider: "azure", Azure: &AzureScope{Subscription: "s", ResourceGroup: "rg"}},
	}
	for _, s := range valid {
		if err := s.Validate(); err != nil {
			t.Errorf("Validate(%v) failed: %v", s, err)
		}
	}

	invalid := []struct {
		name  string
		scope Scope
		want  string
	}{
		{
			name:  "resource group alone",
			scope: Scope{Provider: "azure", Azure: &AzureScope{ResourceGroup: "rg"}},
			want:  "cloud: azure scope: --azure-resource-group requires --azure-subscription",
		},
		{
			name:  "resource group with tenant",
			scope: Scope{Provider: "azure", Azure: &AzureScope{Tenant: "t", ResourceGroup: "rg"}},
			want:  "cloud: azure scope: --azure-resource-group can only be combined with --azure-subscription",
		},
		{
			name:  "resource group with management group",
			scope: Scope{Provider: "azure", Azure: &AzureScope{ManagementGroup: "mg", ResourceGroup: "rg"}},
			want:  "cloud: azure scope: --azure-resource-group can only be combined with --azure-subscription",
		},
		{
			name:  "no top level dimension",
			scope: Scope{Provider: "azure", Azure: &AzureScope{}},
			want:  "cloud: azure scope requires exactly one of --azure-tenant, --azure-management-group, or --azure-subscription",
		},
		{
			name:  "two top level dimensions",
			scope: Scope{Provider: "azure", Azure: &AzureScope{Tenant: "t", Subscription: "s"}},
			want:  "cloud: azure scope requires exactly one of --azure-tenant, --azure-management-group, or --azure-subscription",
		},
		{
			name:  "nil block",
			scope: Scope{Provider: "azure"},
			want:  "cloud: azure scope: no scope block",
		},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.scope.Validate()
			if err == nil {
				t.Fatalf("Validate(%v) must fail", tc.scope)
			}
			if err.Error() != tc.want {
				t.Fatalf("error = %q, want %q", err, tc.want)
			}
		})
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

// TestScopeParent_Azure pins the Azure parent contract: Azure has no single
// API parent string — its provider drives management-group traversal and
// per-subscription Resource Graph queries from the scope fields directly — so
// Parent() renders "".
func TestScopeParent_Azure(t *testing.T) {
	for _, tc := range []struct {
		scope Scope
	}{
		{Scope{Provider: "azure", Azure: &AzureScope{Tenant: "t"}}},
		{Scope{Provider: "azure", Azure: &AzureScope{ManagementGroup: "mg"}}},
		{Scope{Provider: "azure", Azure: &AzureScope{Subscription: "s"}}},
		{Scope{Provider: "azure", Azure: &AzureScope{Subscription: "s", ResourceGroup: "rg"}}},
	} {
		if got := tc.scope.Parent(); got != "" {
			t.Errorf("Parent(%v) = %q, want \"\"", tc.scope, got)
		}
	}
}

// TestScopeString pins the report/display rendering in each provider's own
// vocabulary. GCP renders exactly as Parent did before this redesign
// ("projects/my-project"); AWS renders "accounts/<id>",
// "organizational-units/<ou-id>" or "organizations/<org-id>"; Azure renders
// tenants, management groups, subscriptions, and the subscription +
// resource-group form.
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
		{Scope{Provider: "azure", Azure: &AzureScope{Tenant: "t"}}, "tenants/t"},
		{Scope{Provider: "azure", Azure: &AzureScope{ManagementGroup: "mg"}}, "management-groups/mg"},
		{Scope{Provider: "azure", Azure: &AzureScope{Subscription: "sub"}}, "subscriptions/sub"},
		{Scope{Provider: "azure", Azure: &AzureScope{Subscription: "sub", ResourceGroup: "rg"}}, "subscriptions/sub/resourceGroups/rg"},
		{Scope{}, ""}, // no provider, no block: renders empty, never panics
	} {
		if got := tc.scope.String(); got != tc.want {
			t.Errorf("String(%v) = %q, want %q", tc.scope, got, tc.want)
		}
	}
}
