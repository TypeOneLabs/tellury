package azure

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/managementgroups/armmanagementgroups"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// Azure hierarchy node IDs and the single containment convention used by every
// edge below:
//
//	contained -> container  (resource -> region -> subscription -> management group -> tenant)
//
// This is the same direction the AWS provider uses (resource -> region ->
// account -> organizational unit -> organization), so a rollup walking
// EdgeContains in-degree from a resource reaches the whole chain.
//
// Example IDs:
//
//	tenant:          tenants/<tenant-id>
//	management group: management-groups/<mg-id>
//	subscription:    subscriptions/<subscription-id>
//	region:          subscriptions/<subscription-id>/regions/<arm-region>
//	resource:        the ARM resource ID verbatim
func organizationRef(tenantID string) graph.Ref {
	return graph.Ref("tenants/" + tenantID)
}

func managementGroupRef(mgID string) graph.Ref {
	return graph.Ref("management-groups/" + mgID)
}

func subscriptionRef(subscriptionID string) graph.Ref {
	return graph.Ref("subscriptions/" + subscriptionID)
}

func regionRef(subscriptionID, region string) graph.Ref {
	return graph.Ref("subscriptions/" + subscriptionID + "/regions/" + region)
}

// organizationNode builds the Azure tenant container node (KindOrganization).
func organizationNode(tenantID string) *graph.Node {
	return &graph.Node{
		ID:       organizationRef(tenantID),
		Kind:     graph.KindOrganization,
		Name:     tenantID,
		Provider: "azure",
		Attrs:    map[string]any{},
	}
}

// managementGroupNode builds one Azure management group container node
// (KindFolder). The node ID is scoped to the management group ID; nested
// management groups hang off their parent with a contained->container edge.
func managementGroupNode(mgID, displayName string) *graph.Node {
	name := displayName
	if name == "" {
		name = mgID
	}
	return &graph.Node{
		ID:       managementGroupRef(mgID),
		Kind:     graph.KindFolder,
		Name:     name,
		Provider: "azure",
		Attrs:    map[string]any{},
	}
}

// subscriptionNode builds the Azure owner-tier container node
// (KindSubscription). The display name may be empty for a single-subscription
// scope where only the ID is known.
func subscriptionNode(subscriptionID, displayName string) *graph.Node {
	name := displayName
	if name == "" {
		name = subscriptionID
	}
	return &graph.Node{
		ID:       subscriptionRef(subscriptionID),
		Kind:     graph.KindSubscription,
		Name:     name,
		Provider: "azure",
		Project:  subscriptionID,
		Attrs:    map[string]any{},
	}
}

// regionNode builds the per-subscription region container node. The ID is
// scoped to the subscription ("subscriptions/<id>/regions/<arm-region>") so
// two subscriptions sharing a region never collide and the containment chain
// stays linear. The Name field is the canonical ARM region form, and the
// Project field carries the owning subscription ID.
func regionNode(subscriptionID, canonicalRegion string) *graph.Node {
	return &graph.Node{
		ID:       regionRef(subscriptionID, canonicalRegion),
		Kind:     graph.KindRegion,
		Name:     canonicalRegion,
		Provider: "azure",
		Project:  subscriptionID,
		Attrs:    map[string]any{},
	}
}

// walkManagementGroup recursively walks a management group and its
// descendants. mgID is the management group to start from; parent is the
// graph node this group is contained by (empty for a management-group scope
// root). It adds the group's folder node and every descendant subscription
// node, and appends each discovered subscription ID to subs (de-duplicated).
func (p *Provider) walkManagementGroup(
	ctx context.Context,
	g *graph.Graph,
	edges map[graph.Edge]struct{},
	mgID string,
	parent graph.Ref,
	subs map[string]bool,
	visited map[string]bool,
) error {
	if p.mgClient == nil {
		return fmt.Errorf("azure: management groups client is unavailable")
	}
	if visited[mgID] {
		return nil
	}
	visited[mgID] = true

	resp, err := p.mgClient.Get(ctx, mgID, &armmanagementgroups.ClientGetOptions{
		Expand: to.Ptr(armmanagementgroups.ManagementGroupExpandTypeChildren),
	})
	if err != nil {
		return fmt.Errorf("azure: management group %s: %w", mgID, err)
	}

	displayName := mgID
	if resp.Properties != nil && resp.Properties.DisplayName != nil && *resp.Properties.DisplayName != "" {
		displayName = *resp.Properties.DisplayName
	}

	mgNode := managementGroupNode(mgID, displayName)
	if err := g.AddNode(mgNode); err != nil {
		return err
	}
	if parent != "" {
		edges[graph.Edge{From: mgNode.ID, To: parent, Kind: graph.EdgeContains}] = struct{}{}
	}

	if resp.Properties == nil {
		return nil
	}
	for _, child := range resp.Properties.Children {
		if child == nil {
			continue
		}
		childID := childIDOf(child)
		if childID == "" {
			continue
		}

		switch childTypeOf(child) {
		case armmanagementgroups.ManagementGroupChildTypeMicrosoftManagementManagementGroups:
			if err := p.walkManagementGroup(ctx, g, edges, childID, mgNode.ID, subs, visited); err != nil {
				return err
			}
		case armmanagementgroups.ManagementGroupChildTypeSubscriptions:
			if subs[childID] {
				continue
			}
			subs[childID] = true
			subNode := subscriptionNode(childID, childDisplayNameOf(child))
			if err := g.AddNode(subNode); err != nil {
				return err
			}
			edges[graph.Edge{From: subNode.ID, To: mgNode.ID, Kind: graph.EdgeContains}] = struct{}{}
		}
	}
	return nil
}

func childTypeOf(child *armmanagementgroups.ManagementGroupChildInfo) armmanagementgroups.ManagementGroupChildType {
	if child == nil || child.Type == nil {
		return ""
	}
	return *child.Type
}

func childIDOf(child *armmanagementgroups.ManagementGroupChildInfo) string {
	if child.Name != nil && *child.Name != "" {
		return *child.Name
	}
	if child.ID != nil && *child.ID != "" {
		return lastARMIDSegment(*child.ID)
	}
	return ""
}

func childDisplayNameOf(child *armmanagementgroups.ManagementGroupChildInfo) string {
	if child.DisplayName != nil && *child.DisplayName != "" {
		return *child.DisplayName
	}
	return childIDOf(child)
}

// lastARMIDSegment returns the final segment of an ARM resource ID
// (/providers/Microsoft.Management/managementGroups/<id> -> <id>).
func lastARMIDSegment(id string) string {
	id = strings.TrimSuffix(id, "/")
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}
