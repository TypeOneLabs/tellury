package gcp

import (
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// Hierarchy builder: turns the resource-hierarchy fields already carried by a
// SearchAllResources result (RawAsset.Project / RawAsset.Folders /
// RawAsset.Organization) into container nodes and containment edges. It is a
// pure pass over the ingested fixture/assets — no extra Cloud Resource Manager
// API call is ever made; the hierarchy comes entirely from the fields the
// search result already carries.
//
// Containment edge convention (following pkg/graph):
//
//	resource  --contains-->  project      (the resource's owning project)
//	project   --contains-->  folder       (each folder that contains the project)
//	folder    --contains-->  organization (the folder's owning organization)
//
// Direction is the same "dependent -> dependency" convention as every other
// edge in the graph: the contained resource points at the node that owns it,
// so walking out from a resource reaches its project, folder(s), then
// organization, and walking in from an organization reaches everything under
// it. EdgeContains is a single shared kind; a project directly contains a
// resource and a folder directly contains a project both use it.

// hierarchyNode builds a container node for one level of the hierarchy. The
// node ID is the qualified CAI-style token ("projects/<id>", "folders/<n>",
// "organizations/<n>"), which is also the natural edge endpoint for a
// containment edge. Container nodes are marked with the container kind so
// rules can never reach them (see graph.Node.Container and graph.ByKind) and
// the scan resource count excludes them (graph.ResourceNodeCount).
func hierarchyNode(kind graph.ResourceKind, token, projectID string) *graph.Node {
	return &graph.Node{
		ID:       graph.Ref(token),
		Kind:     kind,
		Name:     lastSegment(token),
		Provider: "gcp",
		// Project/folder/organization nodes expose the project they belong to
		// (a project node owns itself) so hierarchy queries and rollups can
		// match consistently.
		Project: projectID,
		Service: "cloudresourcemanager",
		Attrs:   map[string]any{},
	}
}

// ParseHierarchyToken splits a qualified container token ("projects/<id>",
// "folders/<n>", "organizations/<n>") into its kind-qualified parts.
// Returns ("", "") for a token that is not one of the three known prefixes.
func ParseHierarchyToken(token string) (kind string, name string) {
	if token == "" {
		return "", ""
	}
	switch {
	case strings.HasPrefix(token, "projects/"):
		return "projects", strings.TrimPrefix(token, "projects/")
	case strings.HasPrefix(token, "folders/"):
		return "folders", strings.TrimPrefix(token, "folders/")
	case strings.HasPrefix(token, "organizations/"):
		return "organizations", strings.TrimPrefix(token, "organizations/")
	default:
		return "", ""
	}
}

// buildHierarchy is called once per ingested leaf asset. It owns the
// project/folder/organization container nodes that asset is contained in and
// emits the containment edges from that asset up to the organization.
//
// Container nodes are only ever created for a project that actually has a
// normalized leaf. When leaf is nil (Normalize returned nil for an unmapped
// asset type) the whole hierarchy pass is skipped: a project whose only
// assets are unmodelled types must not end up with container nodes and no
// resources under them. The containment edges all hinge on the leaf on one
// end (resource -> project) or on the project that only exists because of it
// (project -> folder, folder -> organization), so no leaf means no containers
// and no edges.
//
// Edge set, in order (all EdgeContains):
//   - asset --contains--> its project (from RawAsset.Project, "projects/<N>")
//   - project --contains--> each of RawAsset.Folders (each "folders/<N>")
//   - each folder --contains--> the organization (RawAsset.Organization,
//     "organizations/<N>")
//
// The hierarchy builder derives the project ID from the RawAsset.Project token
// when present; otherwise the leaf node's normalized Project field is the
// fallback (that field already resolves the name-vs-parent fallback). Folder
// and organization nodes carry an empty project ID because they span multiple
// projects.
//
// A token that is empty or malformed is skipped; the edges whose other
// endpoint is a never-seen node are pruned by Freeze, so an asset that names
// a folder the scan never materializes simply leaves that folder unlinked
// rather than producing a dangling reference.
func buildHierarchy(a *RawAsset, leaf *graph.Node, add func(*graph.Node), emit func(graph.Edge)) {
	if a == nil || leaf == nil {
		return
	}

	// Project node.
	projectToken := a.Project
	projectID := projectIDFrom(projectToken)
	if projectID == "" {
		projectID = leaf.Project
		projectToken = "projects/" + projectID
	}
	if projectID == "" {
		// No project anywhere: nothing to contain, so there is nothing to
		// build a hierarchy for.
		return
	}
	pn := hierarchyNode(graph.KindProject, projectToken, projectID)
	add(pn)
	// Leaf -> project containment. The endpoint is the leaf ref (the
	// normalized node ID) so the edge joins the resource to its container
	// exactly once per leaf.
	emit(graph.Edge{From: leaf.ID, To: pn.ID, Kind: graph.EdgeContains})

	// Folder nodes: a resource may belong to multiple folders (its full
	// ancestor chain), which is why RawAsset.Folders is a slice. Each folder
	// is linked as a direct containment edge from the project (the folder is
	// a container above the project).
	for _, f := range a.Folders {
		if _, name := ParseHierarchyToken(f); name == "" {
			continue
		}
		fn := hierarchyNode(graph.KindFolder, f, "")
		add(fn)
		emit(graph.Edge{From: graph.Ref(projectToken), To: fn.ID, Kind: graph.EdgeContains})
	}

	// Organization node.
	if a.Organization != "" {
		if _, name := ParseHierarchyToken(a.Organization); name != "" {
			on := hierarchyNode(graph.KindOrganization, a.Organization, "")
			add(on)
			// Folder -> organization, one edge per distinct folder the asset
			// names. A resource with no folders still ends up under the org
			// via a direct-ish link only when a project links the org; to
			// keep the model uniform we always link the project to the org
			// when no folder lies between.
			if len(a.Folders) == 0 {
				emit(graph.Edge{From: graph.Ref(projectToken), To: on.ID, Kind: graph.EdgeContains})
			} else {
				for _, f := range a.Folders {
					if _, name := ParseHierarchyToken(f); name != "" {
						emit(graph.Edge{From: graph.Ref(f), To: on.ID, Kind: graph.EdgeContains})
					}
				}
			}
		}
	}
}

// projectIDFrom extracts the ID from a "projects/<N>" token. An unqualified
// or empty token resolves to "".
func projectIDFrom(token string) string {
	if kind, name := ParseHierarchyToken(token); kind == "projects" {
		return name
	}
	return ""
}
