package azure

import "testing"

// TestCollectImageReferences_RBACFilteredInventoryIsNotTrusted pins the
// dangerous case: Resource Graph is RBAC-filtered and omits rows the identity
// cannot read, WITHOUT failing. An identity holding Reader on a Compute Gallery
// but not on virtual machines gets a successful query returning zero VM rows —
// byte-identical to a subscription that genuinely runs nothing.
//
// Concluding "unreferenced" from that reports an image a scale set depends on
// as deletable. A missed finding costs money; a false one costs an outage.
func TestCollectImageReferences_RBACFilteredInventoryIsNotTrusted(t *testing.T) {
	// A gallery image row is present, but no VM or VMSS row is — exactly what a
	// narrow role produces.
	rows := []map[string]any{
		{"type": "microsoft.compute/galleries/images/versions", "id": "/subscriptions/s/img/1.0.0"},
	}
	_, complete := collectImageReferences(rows)
	if complete {
		t.Error("reference inventory reported complete with no compute rows visible: " +
			"an image referenced by an unreadable scale set would be reported as unused")
	}
}

// TestCollectImageReferences_VisibleComputeIsTrusted is the other side: once any
// VM or VMSS row is visible, the identity can evidently read compute, so an
// image with no references really has none.
func TestCollectImageReferences_VisibleComputeIsTrusted(t *testing.T) {
	rows := []map[string]any{
		{"type": "microsoft.compute/virtualmachines", "properties": map[string]any{
			"storageProfile": map[string]any{"imageReference": map[string]any{"id": "/other/image"}},
		}},
	}
	_, complete := collectImageReferences(rows)
	if !complete {
		t.Error("reference inventory reported incomplete despite visible compute")
	}
}
