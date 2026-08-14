package azure

import (
	"sort"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// azureImageReference is one VM or VMSS imageReference.id read from the same
// ARG rows that produced the image nodes. No hydration call is needed: ARG
// returns `properties.storageProfile.imageReference.id` (VM) or
// `properties.virtualMachineProfile.storageProfile.imageReference.id` (VMSS)
// directly in the projected `properties` column.
type azureImageReference struct {
	id     string
	source string // "vm" or "vmss"
}

// azureImageReferenceSet is the per-subscription image-reference inventory
// used to stamp reference_count, reference_sources and references_complete on
// gallery image-version nodes.
type azureImageReferenceSet struct {
	refs     []azureImageReference
	complete bool
}

// CountFor computes the reference count and distinct source labels for one
// gallery image-version node. A reference matches in either of two ways:
//
//  1. exact version ID: the reference equals the version's ARM resource ID.
//  2. parent definition ID: the reference equals the parent gallery image
//     definition ID, in which case it counts every version under that
//     definition (a scale set using "latest" may resolve to any of them).
//
// Managed-image IDs and platform-image IDs never match either form, so they
// are naturally ignored for this rule.
func (s azureImageReferenceSet) CountFor(n *graph.Node) (float64, []string) {
	if len(s.refs) == 0 {
		return 0, []string{}
	}
	resourceID, _ := n.Str(AttrResourceID)
	galleryImageID, _ := n.Str(AttrGalleryImageID)

	var count float64
	sources := map[string]bool{}
	for _, ref := range s.refs {
		if !strings.EqualFold(ref.id, resourceID) && !strings.EqualFold(ref.id, galleryImageID) {
			continue
		}
		count++
		if ref.source != "" {
			sources[ref.source] = true
		}
	}

	out := make([]string, 0, len(sources))
	for source := range sources {
		out = append(out, source)
	}
	sort.Strings(out)
	return count, out
}

// collectImageReferences reads VM and VMSS rows from one ARG page and returns
// the reference set. complete is false when the inventory cannot be trusted to
// be exhaustive, in which case the rule skips with SkipReferencesUnknown rather
// than concluding an image is unreferenced.
//
// TWO WAYS THE INVENTORY IS UNTRUSTWORTHY, and the second is the dangerous one.
//
// A structurally malformed row — `properties` not the object ARG documents — is
// obvious and handled below.
//
// The second is invisible: RESOURCE GRAPH IS RBAC-FILTERED AND OMITS ROWS
// SILENTLY. An identity with Reader on a Compute Gallery but no read access to
// virtual machines gets a SUCCESSFUL query returning zero VM rows — identical
// to a subscription that genuinely runs nothing. Concluding "unreferenced" from
// that reports an image a scale set depends on as deletable, which is the worst
// advice this tool can give. It is the same property that makes an Azure scan
// with a narrow role report zero resources instead of an error, documented in
// docs/azure-setup.md.
//
// So: zero VM AND zero VMSS rows means the inventory is not trusted. A
// subscription that genuinely has no compute loses a true finding, which is the
// right side of that trade — a missed finding costs money, a false one costs an
// outage.
func collectImageReferences(rows []map[string]any) (azureImageReferenceSet, bool) {
	refs := azureImageReferenceSet{complete: true}
	sawCompute := false
	for _, row := range rows {
		typ := strings.ToLower(stringOf(row["type"]))
		var source string
		switch typ {
		case argTypeVM:
			source = "vm"
		case argTypeVMSS:
			source = "vmss"
		default:
			continue
		}
		sawCompute = true

		properties := mapOf(row["properties"])
		if properties == nil {
			refs.complete = false
			continue
		}

		var storageProfile map[string]any
		switch source {
		case "vm":
			storageProfile = mapOf(properties["storageProfile"])
		case "vmss":
			vmProfile := mapOf(properties["virtualMachineProfile"])
			if vmProfile == nil {
				refs.complete = false
				continue
			}
			storageProfile = mapOf(vmProfile["storageProfile"])
		}
		if storageProfile == nil {
			refs.complete = false
			continue
		}

		imageReference := mapOf(storageProfile["imageReference"])
		if imageReference == nil {
			// A platform image has no `id` (it carries publisher/offer/sku
			// instead). It is not a reference to one of our gallery versions.
			continue
		}
		refID := stringOf(imageReference["id"])
		if refID == "" {
			continue
		}
		refs.refs = append(refs.refs, azureImageReference{id: refID, source: source})
	}
	// No compute at all in the ARG result: either the subscription runs nothing,
	// or the identity cannot read virtual machines. Those are indistinguishable
	// from here, so the inventory is not trusted.
	if !sawCompute {
		refs.complete = false
	}
	return refs, refs.complete
}
