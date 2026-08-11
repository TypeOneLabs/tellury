package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
)

// fakeOrgAPI is a minimal Organizations stand-in: one organization, one root,
// no OUs and no accounts. Enough to exercise scope validation without a
// network or credentials.
type fakeOrgAPI struct{ orgID string }

func (f *fakeOrgAPI) DescribeOrganization(context.Context, *organizations.DescribeOrganizationInput, ...func(*organizations.Options)) (*organizations.DescribeOrganizationOutput, error) {
	return &organizations.DescribeOrganizationOutput{
		Organization: &orgtypes.Organization{Id: aws.String(f.orgID)},
	}, nil
}

func (f *fakeOrgAPI) ListRoots(context.Context, *organizations.ListRootsInput, ...func(*organizations.Options)) (*organizations.ListRootsOutput, error) {
	return &organizations.ListRootsOutput{
		Roots: []orgtypes.Root{{Id: aws.String("r-root"), Name: aws.String("Root")}},
	}, nil
}

func (f *fakeOrgAPI) ListOrganizationalUnitsForParent(context.Context, *organizations.ListOrganizationalUnitsForParentInput, ...func(*organizations.Options)) (*organizations.ListOrganizationalUnitsForParentOutput, error) {
	return &organizations.ListOrganizationalUnitsForParentOutput{}, nil
}

func (f *fakeOrgAPI) ListAccountsForParent(context.Context, *organizations.ListAccountsForParentInput, ...func(*organizations.Options)) (*organizations.ListAccountsForParentOutput, error) {
	return &organizations.ListAccountsForParentOutput{}, nil
}

// TestBuildOrgTree_RejectsMismatchedOrganization pins the scope check.
//
// DescribeOrganization takes no organization ID — it answers for whatever
// credentials called it. So a mistyped --aws-organization used to traverse a
// DIFFERENT organization while the report carried the requested name:
// `--aws-organization o-notreal` produced a summary reading
// "organizations/o-notreal" over another organization's accounts. A report
// labelled with a scope it never scanned is worse than an error, because
// nothing about it looks wrong.
func TestBuildOrgTree_RejectsMismatchedOrganization(t *testing.T) {
	client := &fakeOrgAPI{orgID: "o-real0000"}

	_, err := buildOrgTree(context.Background(), client, cloud.AWSScope{Organization: "o-typo00000"})
	if err == nil {
		t.Fatal("want an error when the requested organization is not the caller's")
	}
	for _, want := range []string{"o-typo00000", "o-real0000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name both organizations so the mismatch is visible;\n got: %v\n want substring: %s", err, want)
		}
	}

	// The matching case still works, and so does an unset organization — an OU
	// scan names no organization at all and must not be rejected.
	if _, err := buildOrgTree(context.Background(), client, cloud.AWSScope{Organization: "o-real0000"}); err != nil {
		t.Errorf("matching organization must be accepted: %v", err)
	}
	if _, err := buildOrgTree(context.Background(), client, cloud.AWSScope{OrganizationalUnit: "ou-abc"}); err != nil {
		t.Errorf("an OU scan names no organization and must be accepted: %v", err)
	}
}


// fakeSTS is an STS stand-in whose AssumeRole always fails, so a test can
// prove the caller's own account is scanned WITHOUT one.
type fakeSTS struct{ account string }

func (f *fakeSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account)}, nil
}

func (f *fakeSTS) AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	return nil, errors.New("AccessDenied: not authorized to perform sts:AssumeRole")
}

// TestCallerAccountID_ReadsOwnAccount pins the lookup that makes an
// organization scan usable at all.
//
// An organization almost always contains the account the credentials belong
// to, usually the management account. You cannot assume a role into yourself
// unless someone created one, and OrganizationAccountAccessRole exists in
// accounts Organizations CREATED, not in the management account. Before this,
// the one account an operator is guaranteed to have access to was the one
// account every organization scan reported as unreachable.
func TestCallerAccountID_ReadsOwnAccount(t *testing.T) {
	p := &Provider{log: newTestLogger(), stsClient: &fakeSTS{account: "111122223333"}}
	if got := p.callerAccountID(context.Background()); got != "111122223333" {
		t.Errorf("callerAccountID = %q, want %q", got, "111122223333")
	}

	// No STS client at all must not panic; it degrades to assuming into
	// every account, which is the pre-existing behaviour.
	empty := &Provider{log: newTestLogger()}
	if got := empty.callerAccountID(context.Background()); got != "" {
		t.Errorf("callerAccountID without an STS client = %q, want empty", got)
	}
}
