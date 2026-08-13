package azure

import (
	"strings"
)

// ManagedDiskTierSKU maps an Azure managed-disk ARM SKU name plus the
// provisioned diskSizeGB to the Retail Prices API's billable tier token:
//
//	ManagedDiskTierSKU("Premium_LRS", 128) -> "P10 LRS", true
//
// The mapping is exact, not "next tier up". Azure provisions managed disks in
// fixed size tiers and resourceGraph's properties.diskSizeGB is one of those
// tier sizes; if a size does not match a known tier, the rule must skip
// rather than guessing at a larger billable tier. There is deliberately no
// fallback table for an unknown size.
func ManagedDiskTierSKU(armSKUName string, diskSizeGB float64) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(armSKUName))
	for _, family := range managedDiskFamilies {
		if strings.ToLower(family.armSKUName) != key {
			continue
		}
		for _, tier := range family.tiers {
			if tier.sizeGiB == diskSizeGB {
				return tier.skuName, true
			}
		}
		return "", false
	}
	return "", false
}

// managedDiskFamily is one ARM SKU family and its fixed provisioned-size
// tiers. The skuName tokens are exactly the Retail Prices API skuName values
// (e.g. "P10 LRS", "E10 LRS", "S10 LRS"), not a local invention.
type managedDiskFamily struct {
	armSKUName  string
	productName string
	tiers       []managedDiskTier
}

type managedDiskTier struct {
	skuName string
	sizeGiB float64
}

var managedDiskFamilies = []managedDiskFamily{
	{
		armSKUName:  "Premium_LRS",
		productName: "Premium SSD Managed Disks",
		tiers: []managedDiskTier{
			{"P1 LRS", 4},
			{"P2 LRS", 8},
			{"P3 LRS", 16},
			{"P4 LRS", 32},
			{"P6 LRS", 64},
			{"P10 LRS", 128},
			{"P15 LRS", 256},
			{"P20 LRS", 512},
			{"P30 LRS", 1024},
			{"P40 LRS", 2048},
			{"P50 LRS", 4096},
			{"P60 LRS", 8192},
			{"P70 LRS", 16384},
			{"P80 LRS", 32768},
		},
	},
	{
		armSKUName:  "Premium_ZRS",
		productName: "Premium SSD Managed Disks",
		tiers: []managedDiskTier{
			{"P1 ZRS", 4},
			{"P2 ZRS", 8},
			{"P3 ZRS", 16},
			{"P4 ZRS", 32},
			{"P6 ZRS", 64},
			{"P10 ZRS", 128},
			{"P15 ZRS", 256},
			{"P20 ZRS", 512},
			{"P30 ZRS", 1024},
			{"P40 ZRS", 2048},
			{"P50 ZRS", 4096},
			{"P60 ZRS", 8192},
			{"P70 ZRS", 16384},
			{"P80 ZRS", 32768},
		},
	},
	{
		armSKUName:  "StandardSSD_LRS",
		productName: "Standard SSD Managed Disks",
		tiers: []managedDiskTier{
			{"E1 LRS", 4},
			{"E2 LRS", 8},
			{"E3 LRS", 16},
			{"E4 LRS", 32},
			{"E6 LRS", 64},
			{"E10 LRS", 128},
			{"E15 LRS", 256},
			{"E20 LRS", 512},
			{"E30 LRS", 1024},
			{"E40 LRS", 2048},
			{"E50 LRS", 4096},
			{"E60 LRS", 8192},
			{"E70 LRS", 16384},
			{"E80 LRS", 32768},
		},
	},
	{
		armSKUName:  "StandardSSD_ZRS",
		productName: "Standard SSD Managed Disks",
		tiers: []managedDiskTier{
			{"E1 ZRS", 4},
			{"E2 ZRS", 8},
			{"E3 ZRS", 16},
			{"E4 ZRS", 32},
			{"E6 ZRS", 64},
			{"E10 ZRS", 128},
			{"E15 ZRS", 256},
			{"E20 ZRS", 512},
			{"E30 ZRS", 1024},
			{"E40 ZRS", 2048},
			{"E50 ZRS", 4096},
			{"E60 ZRS", 8192},
			{"E70 ZRS", 16384},
			{"E80 ZRS", 32768},
		},
	},
	{
		armSKUName:  "Standard_LRS",
		productName: "Standard HDD Managed Disks",
		tiers: []managedDiskTier{
			{"S4 LRS", 32},
			{"S6 LRS", 64},
			{"S10 LRS", 128},
			{"S15 LRS", 256},
			{"S20 LRS", 512},
			{"S30 LRS", 1024},
			{"S40 LRS", 2048},
			{"S50 LRS", 4096},
			{"S60 LRS", 8192},
			{"S70 LRS", 16384},
			{"S80 LRS", 32768},
		},
	},
}

// managedDiskProduct returns the Retail Prices API productName and meterName
// for a managed-disk tier SKU token such as "P10 LRS", "E10 LRS", or
// "S10 LRS". The meterName is always "<skuName> Disk" for the flat disk-month
// meter, never "<skuName> Disk Mount" (the mount/transactional meter).
func managedDiskProduct(skuName string) (productName, meterName string, ok bool) {
	skuName = strings.TrimSpace(skuName)
	if skuName == "" {
		return "", "", false
	}
	family, ok := familyForTierSKU(skuName)
	if !ok {
		return "", "", false
	}
	return family.productName, skuName + " Disk", true
}

// tierSKUIndex maps a Retail Prices tier token ("S4 LRS") to the family that
// declares it. It is built from managedDiskFamilies, which already lists every
// family's exact tokens, so the two can never disagree.
//
// It replaces deriving the family from the token's first letter, which was
// wrong in two of three families and silently so. "S" was matched as a prefix
// against the ARM SKU names and hit "StandardSSD_LRS" before "Standard_LRS",
// so every Standard HDD disk was priced as Standard SSD — a query that matches
// nothing, making the disk unpriceable. "E" matched no ARM name at all, since
// none begins with it, so Standard SSD never priced either. Only Premium
// worked. A letter prefix is a guess about a naming convention; the tier table
// is the fact.
var tierSKUIndex = func() map[string]managedDiskFamily {
	idx := make(map[string]managedDiskFamily)
	for _, family := range managedDiskFamilies {
		for _, tier := range family.tiers {
			idx[strings.ToLower(tier.skuName)] = family
		}
	}
	return idx
}()

func familyForTierSKU(skuName string) (managedDiskFamily, bool) {
	f, ok := tierSKUIndex[strings.ToLower(strings.TrimSpace(skuName))]
	return f, ok
}
