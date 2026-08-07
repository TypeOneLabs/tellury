package pricing

import (
	"errors"
	"math"
	"strings"
)

// Kind enumerates billable dimensions.
type Kind string

const (
	KindDiskCapacity   Kind = "disk_capacity"
	KindDiskIOPS       Kind = "disk_iops"
	KindDiskThroughput Kind = "disk_throughput"
	KindVMInstance     Kind = "vm_instance"
	KindVMCustomCPU    Kind = "vm_custom_cpu"
	KindVMCustomRAM    Kind = "vm_custom_ram"
	KindGCSStorage     Kind = "gcs_storage"
	KindGCSRetrieval   Kind = "gcs_retrieval"
	KindGCSOpsClassA   Kind = "gcs_ops_class_a"
	KindStaticIP       Kind = "static_ip"
)

// HoursPerMonth / DaysPerMonth are the fixed conventions (Invariant I1).
// Never derived from time.Now() - documented so every number is reproducible.
const (
	HoursPerMonth = 730.0
	DaysPerMonth  = 30.0
)

// ErrNoPrice is returned when a SKU/region has no entry. Rules MUST skip the
// node on this error, never assume $0 (Invariant I4).
var ErrNoPrice = errors.New("pricing: no price for kind/sku/region")

// Item is a priceable quantity.
type Item struct {
	Kind     Kind
	Provider string
	SKU      string
	Region   string
	Quantity float64
}

// Pricer resolves monthly cost. Implementations MUST be pure and offline.
type Pricer interface {
	MonthlyCost(it Item) (float64, error)
	UnitPrice(kind Kind, provider, sku, region string) (float64, string, error)
}

// RegionOf normalizes a zone/location to a pricing region:
// "us-central1-a" -> "us-central1"; "US" -> "us"; "" -> "default".
func RegionOf(location string) string {
	loc := strings.ToLower(strings.TrimSpace(location))
	if loc == "" {
		return "default"
	}
	// zone form: <region>-<letter>, e.g. us-central1-a
	parts := strings.Split(loc, "-")
	if len(parts) >= 3 {
		// last part is a single letter zone suffix
		last := parts[len(parts)-1]
		if len(last) == 1 {
			return strings.Join(parts[:len(parts)-1], "-")
		}
	}
	return loc
}

// Round2 rounds half-away-from-zero to 2 decimal places (Invariant I3). It is
// the single rounding function every renderer and report total must use so
// money never disagrees between formats.
func Round2(v float64) float64 {
	if v < 0 {
		return -math.Round(-v*100) / 100
	}
	return math.Round(v*100) / 100
}

// MachineCost prices one machine type at the given region using sz to resolve
// its vCPU/RAM shape: catalog types use KindVMInstance directly, and custom
// shapes fall back to the per-vCPU/per-GiB custom rates for the type's family.
func MachineCost(p Pricer, sz Sizer, machineType, region string) (float64, error) {
	if unit, _, err := p.UnitPrice(KindVMInstance, "gcp", machineType, region); err == nil {
		return unit * HoursPerMonth, nil
	}
	if sz == nil {
		return 0, ErrNoPrice
	}
	spec, ok := sz.Spec(machineType)
	if !ok {
		return 0, ErrNoPrice
	}
	family := spec.Family
	cpuUnit, _, err := p.UnitPrice(KindVMCustomCPU, "gcp", family, region)
	if err != nil {
		return 0, err
	}
	ramUnit, _, err := p.UnitPrice(KindVMCustomRAM, "gcp", family, region)
	if err != nil {
		return 0, err
	}
	return (spec.VCPU*cpuUnit + spec.MemoryGiB*ramUnit) * HoursPerMonth, nil
}
