package graph

// EdgeKind is the relationship semantics. Direction is always
// "dependent -> dependency" so that in-degree answers "who uses me?".
type EdgeKind string

const (
	// EdgeAttachedTo: instance --attached_to--> disk
	EdgeAttachedTo EdgeKind = "attached_to"
	// EdgeUses: instance --uses--> network / address / service_account
	EdgeUses EdgeKind = "uses"
	// EdgeContains: project --contains--> instance (and, during hierarchy
	// building, folder --contains--> project / organization --contains-->
	// folder). The direction is always "container -> contained", i.e. the
	// same "dependent -> dependency" convention as every other edge: the
	// contained resource points at the node that owns it, so in-degree on a
	// container answers "what lives under me?".
	EdgeContains EdgeKind = "contains"
	// EdgeCreatedFrom: disk --created_from--> snapshot|image
	EdgeCreatedFrom EdgeKind = "created_from"
	// EdgeTargets: forwarding_rule --targets--> backend
	EdgeTargets EdgeKind = "targets"
)

// Edge is a directed, typed relationship. Value type - stored inline in slices.
type Edge struct {
	From Ref      `json:"from"`
	To   Ref      `json:"to"`
	Kind EdgeKind `json:"kind"`
}
