package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/managementgroups/armmanagementgroups"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resourcegraph/armresourcegraph"
)

// Fixture is the offline data source for an Azure scan. It replays the
// management-groups hierarchy and per-subscription Azure Resource Graph rows
// from local JSON, so ingestion is testable and `tellury scan --fixture
// azure.json --provider azure --azure-tenant <tenant>` runs with no Azure
// credentials and no network.
//
// The JSON shape is:
//
//	{
//	  "management_groups": {
//	    "<mg-id>": {
//	      "display_name": "Tenant Root Group",
//	      "children": [
//	        {"type": "Microsoft.Management/managementGroups", "name": "<child-mg-id>", "display_name": "Child MG"},
//	        {"type": "/subscriptions", "name": "<subscription-id>", "display_name": "Sub One"}
//	      ]
//	    }
//	  },
//	  "subscriptions": {
//	    "<subscription-id>": {
//	      "resources": [ ... Azure Resource Graph rows ... ]
//	    }
//	  }
//	}
//
// Each resource element is the JSON object form of one ARG row (the same
// `project id, name, type, location, resourceGroup, subscriptionId, sku,
// managedBy, tags, properties` columns the live query asks for), so a capture
// produced by the ARG API feeds the fixture unedited and the normalizers read
// exactly the fields the live query returns.
type Fixture struct {
	ManagementGroups map[string]*ManagementGroupFixture `json:"management_groups"`
	Subscriptions    map[string]*SubscriptionFixture    `json:"subscriptions"`
}

// ManagementGroupFixture is one management group's direct children, exactly
// what a management-groups Get with `$expand=children` returns.
type ManagementGroupFixture struct {
	DisplayName string         `json:"display_name,omitempty"`
	Children    []ChildFixture `json:"children,omitempty"`
}

// ChildFixture is one child management group or subscription.
type ChildFixture struct {
	// Type is either "Microsoft.Management/managementGroups" or
	// "/subscriptions" — the ARG/management-groups API's own child type
	// strings, never invented values.
	Type string `json:"type"`
	// Name is the child management-group ID or subscription ID.
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
}

// SubscriptionFixture holds one subscription's captured ARG rows.
type SubscriptionFixture struct {
	Resources []map[string]any `json:"resources"`
}

// LoadFixture reads one or more Azure fixture files and merges them. Later
// files override earlier management-group entries and subscription entries of
// the same ID, so a multi-file fixture set can layer data the way the AWS and
// GCP fixture loaders do.
func LoadFixture(paths ...string) (*Fixture, error) {
	f := &Fixture{
		ManagementGroups: map[string]*ManagementGroupFixture{},
		Subscriptions:    map[string]*SubscriptionFixture{},
	}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("azure: read fixture %s: %w", path, err)
		}
		var envelope Fixture
		if err := json.Unmarshal(b, &envelope); err != nil {
			return nil, fmt.Errorf("azure: fixture %s: expected a {\"management_groups\":{...},\"subscriptions\":{...}} envelope: %w", path, err)
		}
		mergeFixture(f, &envelope)
	}
	return f, nil
}

func mergeFixture(dst, src *Fixture) {
	for id, mg := range src.ManagementGroups {
		if mg == nil {
			continue
		}
		dst.ManagementGroups[id] = mg
	}
	for id, sub := range src.Subscriptions {
		if sub == nil {
			continue
		}
		dst.Subscriptions[id] = sub
	}
}

// fakeResourceGraph is the fixture-backed ARG client. It stands in for the
// live SDK client on the offline path so ingestion exercises the same
// per-subscription Resources call with no network and no credentials.
type fakeResourceGraph struct {
	f *Fixture
}

var _ resourceGraphAPI = (*fakeResourceGraph)(nil)

func (c *fakeResourceGraph) Resources(_ context.Context, query armresourcegraph.QueryRequest, _ *armresourcegraph.ClientResourcesOptions) (armresourcegraph.ClientResourcesResponse, error) {
	resp := armresourcegraph.ClientResourcesResponse{
		QueryResponse: armresourcegraph.QueryResponse{
			Count:           to.Ptr(int64(0)),
			TotalRecords:    to.Ptr(int64(0)),
			Data:            []map[string]any{},
			ResultTruncated: to.Ptr(armresourcegraph.ResultTruncatedFalse),
		},
	}
	if c.f == nil {
		return resp, nil
	}

	subscriptionID := ""
	if len(query.Subscriptions) > 0 && query.Subscriptions[0] != nil {
		subscriptionID = *query.Subscriptions[0]
	}
	if subscriptionID == "" {
		return resp, nil
	}

	resourceGroup := fixtureResourceGroup(queryString(query.Query))
	var rows []map[string]any
	if sf := c.f.Subscriptions[subscriptionID]; sf != nil {
		for _, row := range sf.Resources {
			if resourceGroup != "" && stringOf(row["resourceGroup"]) != resourceGroup {
				continue
			}
			rows = append(rows, row)
		}
	}

	count := int64(len(rows))
	resp.Count = to.Ptr(count)
	resp.TotalRecords = to.Ptr(count)
	resp.Data = rows
	return resp, nil
}

// fakeManagementGroups is the fixture-backed management-groups client. It
// replays Get for one management group at a time, exactly like the live walk.
type fakeManagementGroups struct {
	f *Fixture
}

var _ managementGroupsAPI = (*fakeManagementGroups)(nil)

func (c *fakeManagementGroups) Get(_ context.Context, groupID string, _ *armmanagementgroups.ClientGetOptions) (armmanagementgroups.ClientGetResponse, error) {
	resp := armmanagementgroups.ClientGetResponse{}
	if c.f == nil {
		return resp, fmt.Errorf("azure: fixture has no management groups")
	}
	mg, ok := c.f.ManagementGroups[groupID]
	if !ok {
		return resp, fmt.Errorf("azure: fixture has no management group %s", groupID)
	}

	displayName := mg.DisplayName
	if displayName == "" {
		displayName = groupID
	}

	properties := &armmanagementgroups.ManagementGroupProperties{
		DisplayName: to.Ptr(displayName),
	}
	for _, child := range mg.Children {
		info := armmanagementgroups.ManagementGroupChildInfo{
			Name: to.Ptr(child.Name),
			ID:   to.Ptr(childARMID(child)),
			Type: to.Ptr(armmanagementgroups.ManagementGroupChildType(child.Type)),
		}
		if child.DisplayName != "" {
			info.DisplayName = to.Ptr(child.DisplayName)
		}
		properties.Children = append(properties.Children, &info)
	}

	resp.ManagementGroup = armmanagementgroups.ManagementGroup{
		ID:         to.Ptr("/providers/Microsoft.Management/managementGroups/" + groupID),
		Name:       to.Ptr(groupID),
		Type:       to.Ptr("Microsoft.Management/managementGroups"),
		Properties: properties,
	}
	return resp, nil
}

func childARMID(child ChildFixture) string {
	if child.Type == string(armmanagementgroups.ManagementGroupChildTypeSubscriptions) {
		return "/subscriptions/" + child.Name
	}
	return "/providers/Microsoft.Management/managementGroups/" + child.Name
}

func queryString(q *string) string {
	if q == nil {
		return ""
	}
	return *q
}

// fixtureResourceGroup extracts the resource-group filter from a generated
// KQL query so the offline client can replay --azure-resource-group without
// duplicating the live server's filter evaluation.
func fixtureResourceGroup(query string) string {
	const marker = "| where resourceGroup =~ '"
	idx := strings.Index(query, marker)
	if idx < 0 {
		return ""
	}
	rest := query[idx+len(marker):]
	end := strings.Index(rest, "'")
	if end < 0 {
		return ""
	}
	return strings.ReplaceAll(rest[:end], "''", "'")
}
