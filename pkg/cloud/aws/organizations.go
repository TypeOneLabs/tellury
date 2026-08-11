package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// orgAPI is the subset of the Organizations API this provider calls. It is
// what lets a fixture (offline replay) stand in for the live SDK client
// without touching the network.
type orgAPI interface {
	DescribeOrganization(ctx context.Context, params *organizations.DescribeOrganizationInput, optFns ...func(*organizations.Options)) (*organizations.DescribeOrganizationOutput, error)
	ListRoots(ctx context.Context, params *organizations.ListRootsInput, optFns ...func(*organizations.Options)) (*organizations.ListRootsOutput, error)
	ListOrganizationalUnitsForParent(ctx context.Context, params *organizations.ListOrganizationalUnitsForParentInput, optFns ...func(*organizations.Options)) (*organizations.ListOrganizationalUnitsForParentOutput, error)
	ListAccountsForParent(ctx context.Context, params *organizations.ListAccountsForParentInput, optFns ...func(*organizations.Options)) (*organizations.ListAccountsForParentOutput, error)
}

// newOrgClient builds an Organizations client pinned to us-east-1. The AWS
// Organizations API is a global service served from a single endpoint in
// us-east-1, regardless of which region a scan targets. A client built for
// any other region will fail to resolve. This mirrors the Price List API,
// which has the same constraint and is similarly pinned.
func newOrgClient(cfg aws.Config) orgAPI {
	return organizations.NewFromConfig(cfg, func(o *organizations.Options) {
		o.Region = "us-east-1"
	})
}

// orgTree holds the result of building an organization's container hierarchy
// from the Organizations API.
type orgTree struct {
	// Nodes is every container node (organization, root, OUs, accounts) the
	// tree walk produced, keyed by node ID.
	Nodes map[string]*graph.Node
	// Edges is every containment edge (root -> org, OU -> root/OU, account ->
	// root/OU) the tree walk produced.
	Edges []graph.Edge
	// AccountIDs is the list of active account IDs discovered, in the order
	// the API returned them.
	AccountIDs []string
}

// buildOrgTree walks the Organizations API from the given scope (organization
// root or named OU) and returns every container node and edge that make up
// the tree. It recurses from the root or from the named OU through
// ListOrganizationalUnitsForParent and ListAccountsForParent, building OU
// container nodes (KindOrganizationalUnit) and account container nodes
// (KindAccount), with EdgeContains edges linking child to parent.
//
// No single AWS API returns the whole tree. DescribeOrganization gives the
// org ID; ListRoots gives the root(s); ListOrganizationalUnitsForParent
// recurses through the OU hierarchy; and ListAccountsForParent returns the
// leaf accounts. This function combines all four into one pass.
//
// The organization ID (the "o-" prefix string) is used as the organization
// node's name, and the node ID is "organizations/<id>", matching the GCP
// naming convention. OU nodes carry the OU's ARN as their node ID so
// containment edges stay unambiguous even when two OUs at different levels
// share a name.
func buildOrgTree(ctx context.Context, client orgAPI, scope cloud.AWSScope) (*orgTree, error) {
	t := &orgTree{
		Nodes: make(map[string]*graph.Node),
	}

	// Step 1: get the organization ID via DescribeOrganization.
	desc, err := client.DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	if err != nil {
		return nil, fmt.Errorf("organizations DescribeOrganization: %w", err)
	}
	if desc.Organization == nil || desc.Organization.Id == nil {
		return nil, fmt.Errorf("organizations: DescribeOrganization returned no organization")
	}
	orgID := *desc.Organization.Id

	// Add the organization container node.
	orgNode := &graph.Node{
		ID:       graph.Ref("organizations/" + orgID),
		Kind:     graph.KindOrganization,
		Name:     orgID,
		Provider: "aws",
		Service:  "organizations",
		Attrs:    map[string]any{},
	}
	t.Nodes[string(orgNode.ID)] = orgNode

	// Step 2: list roots.
	rootsOut, err := client.ListRoots(ctx, &organizations.ListRootsInput{})
	if err != nil {
		return nil, fmt.Errorf("organizations ListRoots: %w", err)
	}

	// Collect root IDs for the walk.
	var rootIDs []string
	for _, r := range rootsOut.Roots {
		if r.Id != nil {
			rootIDs = append(rootIDs, *r.Id)
		}
	}
	if len(rootIDs) == 0 {
		return nil, fmt.Errorf("organizations: no root found")
	}

	// Walk from each root (or from the named OU if the scope specifies one).
	var walk func(parentID, parentType string) error
	walk = func(parentID, parentType string) error {
		// Paginate child OUs.
		var nextToken *string
		for {
			out, err := client.ListOrganizationalUnitsForParent(ctx, &organizations.ListOrganizationalUnitsForParentInput{
				ParentId:   aws.String(parentID),
				NextToken:  nextToken,
				MaxResults: aws.Int32(20),
			})
			if err != nil {
				return fmt.Errorf("organizations ListOrganizationalUnitsForParent(%s): %w", parentID, err)
			}
			for _, ou := range out.OrganizationalUnits {
				ouID := aws.ToString(ou.Id)
				ouARN := aws.ToString(ou.Arn)
				ouName := aws.ToString(ou.Name)
				if ouID == "" {
					continue
				}

				ouNode := &graph.Node{
					ID:       graph.Ref(ouARN),
					Kind:     graph.KindOrganizationalUnit,
					Name:     ouName,
					Provider: "aws",
					Service:  "organizations",
					Attrs: map[string]any{
						"ou_id":  ouID,
						"ou_arn": ouARN,
					},
				}
				t.Nodes[string(ouNode.ID)] = ouNode

				// Containment edge: child OU -> parent.
				parentRef := t.parentRef(parentID, parentType)
				t.Edges = append(t.Edges, graph.Edge{
					From: ouNode.ID,
					To:   graph.Ref(parentRef),
					Kind: graph.EdgeContains,
				})

				// Recurse into this OU.
				if err := walk(ouID, "ORGANIZATIONAL_UNIT"); err != nil {
					return err
				}
			}
			if out.NextToken == nil || *out.NextToken == "" {
				break
			}
			nextToken = out.NextToken
		}

		// Paginate child accounts.
		nextToken = nil
		for {
			out, err := client.ListAccountsForParent(ctx, &organizations.ListAccountsForParentInput{
				ParentId:   aws.String(parentID),
				NextToken:  nextToken,
				MaxResults: aws.Int32(20),
			})
			if err != nil {
				return fmt.Errorf("organizations ListAccountsForParent(%s): %w", parentID, err)
			}
			for _, acct := range out.Accounts {
				acctID := aws.ToString(acct.Id)
				acctName := aws.ToString(acct.Name)
				acctStatus := acct.Status
				if acctID == "" {
					continue
				}

				// Suspended accounts are recorded as container nodes but have
				// no resources to scan — they carry a status attribute so the
				// caller can report them as unreachable.
				if acctStatus == orgtypes.AccountStatusSuspended {
					t.Nodes["accounts/"+acctID] = &graph.Node{
						ID:       graph.Ref("accounts/" + acctID),
						Kind:     graph.KindAccount,
						Name:     acctName,
						Provider: "aws",
						Service:  "organizations",
						Attrs: map[string]any{
							"account_id":     acctID,
							"account_status": "SUSPENDED",
						},
					}
					continue
				}

				acctNode := &graph.Node{
					ID:       graph.Ref("accounts/" + acctID),
					Kind:     graph.KindAccount,
					Name:     acctName,
					Provider: "aws",
					Service:  "organizations",
					Attrs: map[string]any{
						"account_id":     acctID,
						"account_status": string(acctStatus),
					},
				}
				t.Nodes[string(acctNode.ID)] = acctNode

				// Containment edge: account -> parent.
				parentRef := t.parentRef(parentID, parentType)
				t.Edges = append(t.Edges, graph.Edge{
					From: acctNode.ID,
					To:   graph.Ref(parentRef),
					Kind: graph.EdgeContains,
				})

				if acctStatus == orgtypes.AccountStatusActive {
					t.AccountIDs = append(t.AccountIDs, acctID)
				}
			}
			if out.NextToken == nil || *out.NextToken == "" {
				break
			}
			nextToken = out.NextToken
		}

		return nil
	}

	// Determine the starting parent(s) for the tree walk.
	startType := "ROOT"
	startIDs := rootIDs

	if scope.OrganizationalUnit != "" {
		// The scope names an OU. We walk from the named OU only, grafting its
		// subtree under the organization node. The OU may be at any depth;
		// the walk starts from the OU ID and only enumerates its children.
		startType = "ORGANIZATIONAL_UNIT"
		startIDs = []string{scope.OrganizationalUnit}

		// We need to create the target OU node itself (since the walk only
		// creates children). We don't have the OU's ARN or name from just the
		// ID; we build a minimal node and the walk's first level links children
		// under it. The OU's ARN is derived as the organization's OU ARN
		// pattern: arn:aws:organizations::<management-account-id>:ou/<org-id>/<ou-id>
		// But since we don't have the management account ID here, we use the
		// OU ID as a fallback identifier.
		ouARN := "arn:aws:organizations:::ou/" + orgID + "/" + scope.OrganizationalUnit
		ouNode := &graph.Node{
			ID:       graph.Ref(ouARN),
			Kind:     graph.KindOrganizationalUnit,
			Name:     scope.OrganizationalUnit,
			Provider: "aws",
			Service:  "organizations",
			Attrs: map[string]any{
				"ou_id":  scope.OrganizationalUnit,
				"ou_arn": ouARN,
			},
		}
		t.Nodes[string(ouNode.ID)] = ouNode

		// Link the OU to the organization.
		t.Edges = append(t.Edges, graph.Edge{
			From: ouNode.ID,
			To:   orgNode.ID,
			Kind: graph.EdgeContains,
		})
	}

	for _, id := range startIDs {
		if startType == "ROOT" {
			// Link each root to the organization.
			t.Edges = append(t.Edges, graph.Edge{
				From: graph.Ref("roots/" + id),
				To:   orgNode.ID,
				Kind: graph.EdgeContains,
			})
		}
		if err := walk(id, startType); err != nil {
			return nil, err
		}
	}

	return t, nil
}

// parentRef returns the graph node Ref for a parent identified by ID and
// type. For a ROOT parent, it returns "roots/<id>". For an OU parent, it
// looks up the OU node in the tree by OU ID. For the organization, it
// finds the org node.
func (t *orgTree) parentRef(parentID, parentType string) string {
	switch parentType {
	case "ROOT":
		return "roots/" + parentID
	case "ORGANIZATIONAL_UNIT":
		// Find the OU node by its OU ID attribute.
		for id, n := range t.Nodes {
			if n.Kind == graph.KindOrganizationalUnit {
				if ouID, ok := n.Attrs["ou_id"].(string); ok && ouID == parentID {
					return id
				}
			}
		}
		return ""
	}
	// Organization node.
	for id, n := range t.Nodes {
		if n.Kind == graph.KindOrganization {
			return id
		}
	}
	return ""
}

// orgNodeName returns the organization ID string ("o-xxx"), or "".
func orgNodeName(t *orgTree) string {
	for _, n := range t.Nodes {
		if n.Kind == graph.KindOrganization {
			return strings.TrimPrefix(string(n.ID), "organizations/")
		}
	}
	return ""
}
