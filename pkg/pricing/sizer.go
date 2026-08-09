package pricing

// Sizer resolves machine-type specs offline. Pure; no I/O.
//
// It is the provider-neutral contract over the vCPU/RAM shape of a machine
// type. It deliberately speaks generic CPU/memory terms (VCPU, MemoryGiB,
// Family) and never names a concrete cloud SKU. The GCP-specific
// implementation — the embedded machine catalog, StaticSizer and the custom
// machine-type name parser — lives in pkg/pricing/gcp, which produces
// MachineSpec values through this interface so rules and the agnostic core
// never depend on a cloud package.
type Sizer interface {
	Spec(machineType string) (MachineSpec, bool)
	Family(machineType string) string
	// Ladder returns every catalog member of a family, sorted ascending by
	// (VCPU, MemoryGiB, Name). Deterministic; excludes the input itself.
	Ladder(family string) []MachineSpec
}

// MachineSpec is the vCPU/RAM shape of a machine type.
type MachineSpec struct {
	Name          string  `json:"name"`
	Family        string  `json:"family"`
	VCPU          float64 `json:"vcpu"`
	MemoryGiB     float64 `json:"memory_gib"`
	SharedCore    bool    `json:"shared_core"`
	MinVCPU       float64 `json:"min_vcpu,omitempty"`
	StepVCPU      float64 `json:"step_vcpu,omitempty"`
	RAMPerVCPUMin float64 `json:"ram_per_vcpu_min,omitempty"`
	RAMPerVCPUMax float64 `json:"ram_per_vcpu_max,omitempty"`
}
