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
	// KindSnapshotStorage is the flat per-GiB-month charge for keeping a
	// persistent disk snapshot. A snapshot is never "attached" to anything,
	// so this is an idle, flat cost: it bills every month the snapshot
	// exists, regardless of whether anything ever restores from it.
	KindSnapshotStorage Kind = "snapshot_storage"
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

// NoPricePricer is a Pricer that returns ErrNoPrice for every lookup. It is
// used by offline scans that have no price source available (no
// TELLURY_PRICE_FIXTURE and no live API). Every resource priced through it
// skips with SkipNoPrice rather than guessing a dollar figure.
type NoPricePricer struct{}

// UnitPrice always returns ErrNoPrice.
func (NoPricePricer) UnitPrice(_ Kind, _, _, _ string) (float64, string, error) {
	return 0, "", ErrNoPrice
}

// MonthlyCost always returns ErrNoPrice.
func (NoPricePricer) MonthlyCost(_ Item) (float64, error) {
	return 0, ErrNoPrice
}

var _ Pricer = NoPricePricer{}

// CanonicalRegion is the ONE canonicaliser for "what place is this". It
// lowercases a location and flattens a zone to its region. Every caller —
// pricing's RegionOf and the graph location node in pkg/cloud/gcp — must
// resolve a location through this single function, never a second copy: when
// two implementations answer the same question they drift, and then the graph
// node and the price disagree about where a resource lives. This project
// shipped exactly that failure once — matchSKU and a rule disagreed about a
// SKU token and every lookup silently fell back for two releases — so the
// location answer has one implementation and two thin wrappers.
//
// The three zone/region shapes are told apart by WHAT the last dash-separated
// segment is, never by how long it is:
//
//	GCP zone               last segment is a single LETTER
//	                       "us-central1-a" -> "us-central1"
//	AWS region             last segment is DIGITS
//	                       "us-east-1" -> "us-east-1" (unchanged)
//	AWS availability zone  last segment is DIGITS FOLLOWED BY ONE LETTER
//	                       "us-east-1a" -> "us-east-1"
//
// The length-only heuristic "last segment is one character" is wrong for AWS:
// in "us-east-1" that character is the digit 1, which is PART OF the region
// name, and dropping it produces a region AWS has never had — every price
// lookup then misses and falls back to the embedded table without saying so.
// In "us-east-1a" the one-character test misses the zone suffix entirely.
//
// Multi-region and global locations are single tokens and pass through
// lowercased: "US" -> "us", "EU" -> "eu", "global" -> "global".
//
// The empty string stays empty. The two callers differ ONLY in what an empty
// location means — pricing keys it as "default" (RegionOf), a location node
// keeps "" — so that difference lives in the caller's thin wrapper, never
// here.
func CanonicalRegion(location string) string {
	loc := strings.ToLower(strings.TrimSpace(location))
	if loc == "" {
		return ""
	}
	parts := strings.Split(loc, "-")
	if len(parts) >= 2 {
		last := parts[len(parts)-1]
		switch {
		case isAWSZoneSuffix(last):
			// "us-east-1a": strip the trailing zone letter, keep the region.
			return strings.Join(parts[:len(parts)-1], "-") + "-" + last[:len(last)-1]
		case isGCPSingleLetter(last):
			// "us-central1-a": drop the zone segment entirely.
			return strings.Join(parts[:len(parts)-1], "-")
		}
	}
	return loc
}

// isGCPSingleLetter reports whether s is exactly one ASCII letter — the GCP
// zone suffix ("us-central1-a"). A single digit ("us-east-1") is NOT a GCP
// zone; it is part of the AWS region name and must be kept.
func isGCPSingleLetter(s string) bool {
	return len(s) == 1 && isASCIILetter(s[0])
}

// isAWSZoneSuffix reports whether s is digits followed by exactly one letter —
// the AWS availability-zone suffix ("us-east-1a"). "1" alone is an AWS region
// suffix, not an AZ, and is deliberately excluded (len < 2).
func isAWSZoneSuffix(s string) bool {
	if len(s) < 2 {
		return false
	}
	for i := 0; i < len(s)-1; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return isASCIILetter(s[len(s)-1])
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// RegionOf normalizes a zone/location to a pricing region: "us-central1-a" ->
// "us-central1"; "US" -> "us"; "" -> "default". It is a thin wrapper over the
// single CanonicalRegion canonicaliser; the only difference is the empty
// input, which pricing keys as "default" (the regionless/global key every
// price table indexes).
func RegionOf(location string) string {
	if region := CanonicalRegion(location); region != "" {
		return region
	}
	return "default"
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
