// Package gcp is the Google Cloud implementation of the pricing layer. It
// owns the Cloud Billing Catalog client (CatalogPricer), the embedded
// machine-type catalog (StaticSizer), the embedded price table (StaticPricer)
// and the --price-file overlay, all built on the provider-agnostic core in
// the parent package (pkg/pricing). The parent package holds only the Pricer
// interface, the money/routing helpers, the provenance types, the SKU key
// types and the Sizer/MachineSpec contracts.
//
// Anything that names a concrete GCP concept — a machine-type name, an
// embedded GCP price/machine data JSON — lives here. The agnostic core never
// imports this package or mentions a GCP SKU shape.
package gcp

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

//go:embed data/gcp_machine_types.json
var embeddedMachineCatalog []byte

// Machine-spec provenance tokens (spec §1.2 "machine_spec_source"): a shape
// resolved from the embedded catalog is fully trusted; a shape recovered by
// parsing a custom machine-type name is marked distinctly so a rule can, if it
// chooses, treat the two differently.
const (
	SpecSourceCatalog = "catalog"
	SpecSourceCustom  = "custom_parse"
)

// MachineSpec is the GCP-vocabulary alias over the agnostic shape struct.
// GCP rules and the normalizer speak pkg/pricing/gcp.MachineSpec directly,
// while the compiler and the agnostic core address the underlying
// pricing.MachineSpec (identical field layout).
type MachineSpec = pricing.MachineSpec

// Sizer resolves machine-type specs offline. Pure; no I/O. It is the GCP
// surface over the agnostic pricing.Sizer contract so rule packages that
// depend on the GCP shape need not import the parent package's interface for
// this pairing.
type Sizer = pricing.Sizer

// IsCustom reports whether a machine-type name is a custom shape (i.e. would
// be resolved via ParseCustomMachineType rather than the embedded catalog).
func IsCustom(machineType string) bool {
	return strings.Contains(machineType, "custom-")
}

type familyMeta struct {
	MinVCPU       float64 `json:"min_vcpu"`
	StepVCPU      float64 `json:"step_vcpu"`
	RAMPerVCPUMin float64 `json:"ram_per_vcpu_min"`
	RAMPerVCPUMax float64 `json:"ram_per_vcpu_max"`
}

type machineCatalogFile struct {
	Version  string                    `json:"version"`
	Families map[string]familyMeta     `json:"families"`
	Types    []pricing.MachineSpec     `json:"types"`
}

// StaticSizer implements pricing.Sizer from the embedded catalog plus a
// deterministic custom-machine-type parser (no catalog entry required for
// custom shapes).
type StaticSizer struct {
	byName   map[string]pricing.MachineSpec
	byFamily map[string][]pricing.MachineSpec
}

// NewMachineCatalog loads the embedded machine catalog. Alias of
// NewStaticSizer kept for callers that speak in "catalog" terms (e.g.
// pkg/cloud/gcp.New).
func NewMachineCatalog() (*StaticSizer, error) { return NewStaticSizer() }

// NewStaticSizer loads the embedded machine catalog.
func NewStaticSizer() (*StaticSizer, error) {
	var mc machineCatalogFile
	if err := json.Unmarshal(embeddedMachineCatalog, &mc); err != nil {
		return nil, fmt.Errorf("pricing: decode machine catalog: %w", err)
	}
	s := &StaticSizer{
		byName:   make(map[string]pricing.MachineSpec, len(mc.Types)),
		byFamily: make(map[string][]pricing.MachineSpec, len(mc.Families)),
	}
	for _, t := range mc.Types {
		s.byName[t.Name] = t
		s.byFamily[t.Family] = append(s.byFamily[t.Family], t)
	}
	for fam, specs := range s.byFamily {
		sort.Slice(specs, func(i, j int) bool {
			if specs[i].VCPU != specs[j].VCPU {
				return specs[i].VCPU < specs[j].VCPU
			}
			if specs[i].MemoryGiB != specs[j].MemoryGiB {
				return specs[i].MemoryGiB < specs[j].MemoryGiB
			}
			return specs[i].Name < specs[j].Name
		})
		s.byFamily[fam] = specs
	}
	return s, nil
}

func (s *StaticSizer) Spec(machineType string) (pricing.MachineSpec, bool) {
	if sp, ok := s.byName[machineType]; ok {
		return sp, true
	}
	if sp, ok := ParseCustomMachineType(machineType); ok {
		return sp, true
	}
	return pricing.MachineSpec{}, false
}

func (s *StaticSizer) Family(machineType string) string {
	if sp, ok := s.Spec(machineType); ok {
		return sp.Family
	}
	return ""
}

func (s *StaticSizer) Ladder(family string) []pricing.MachineSpec {
	return s.byFamily[family]
}

// ParseCustomMachineType is a pure, total function:
//
//	grammar:  [ <family> "-" ] "custom-" <vcpu:int> "-" <memMiB:int> [ "-ext" ]
//	examples: "custom-4-16384"        -> family "n1-custom",  vCPU 4, RAM 16 GiB
//	          "e2-custom-2-4096"      -> family "e2-custom",  vCPU 2, RAM  4 GiB
//	          "n2-custom-8-32768-ext" -> family "n2-custom",  vCPU 8, RAM 32 GiB
func ParseCustomMachineType(s string) (pricing.MachineSpec, bool) {
	idx := strings.Index(s, "custom-")
	if idx < 0 {
		return pricing.MachineSpec{}, false
	}
	prefix := strings.TrimSuffix(s[:idx], "-")
	family := "n1-custom"
	if prefix != "" {
		family = prefix + "-custom"
	}
	rest := s[idx+len("custom-"):]
	rest = strings.TrimSuffix(rest, "-ext")
	parts := strings.Split(rest, "-")
	if len(parts) != 2 {
		return pricing.MachineSpec{}, false
	}
	vcpu, err := strconv.Atoi(parts[0])
	if err != nil || vcpu <= 0 {
		return pricing.MachineSpec{}, false
	}
	memMiB, err := strconv.Atoi(parts[1])
	if err != nil || memMiB <= 0 {
		return pricing.MachineSpec{}, false
	}
	return pricing.MachineSpec{
		Name:      s,
		Family:    family,
		VCPU:      float64(vcpu),
		MemoryGiB: float64(memMiB) / 1024.0,
	}, true
}
