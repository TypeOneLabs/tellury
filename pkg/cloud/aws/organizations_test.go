package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
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
