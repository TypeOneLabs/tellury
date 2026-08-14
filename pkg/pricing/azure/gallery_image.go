package azure

import (
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// galleryImageStorageProduct maps an ARM storageAccountType token carried by a
// Compute Gallery image-version replica to the Retail Prices API row that
// prices its storage. The ARM tokens are the design's SKU tokens exactly:
// Standard_LRS, StandardSSD_LRS and Premium_LRS.
//
// The Retail Prices API spells the same three families differently than the
// flat managed-disk tier matcher: here the meter is the per-GiB-month storage
// meter (skuName "Standard LRS", "Standard SSD LRS", "Premium LRS"), not a
// provisioned tier such as "S10 LRS". Those spellings are pinned by the
// recorded retail-prices-recorded.json fixture, exactly as the managed-disk
// matcher pins its serviceName/productName/meterName spellings.
func galleryImageStorageProduct(sku string) (productName, skuName, meterName string, ok bool) {
	switch strings.TrimSpace(sku) {
	case "Standard_LRS":
		return "Standard HDD Managed Disks", "Standard LRS", "Standard LRS Disk", true
	case "StandardSSD_LRS":
		return "Standard SSD Managed Disks", "Standard SSD LRS", "Standard SSD LRS Disk", true
	case "Premium_LRS":
		return "Premium SSD Managed Disks", "Premium LRS", "Premium LRS Disk", true
	default:
		return "", "", "", false
	}
}

// galleryImageStorageFilters returns the complete filter set for one gallery
// image-version replica storage lookup. The filter table is shared by the live
// OData request and the fixture row matcher, like every other Azure filter
// table.
//
// The filters deliberately omit spot/windows exclusions: this is a Storage
// meter, not a VM meter, so those rows can never collide with it. The
// serviceName/productName/meterName equality terms are what discriminate the
// one billable per-GiB-month row.
func galleryImageStorageFilters(sku, region string) ([]priceFilter, error) {
	region = LocationRegion(region)
	if region == "" {
		return nil, pricing.ErrNoPrice
	}
	productName, retailSKU, meterName, ok := galleryImageStorageProduct(sku)
	if !ok {
		return nil, pricing.ErrNoPrice
	}
	return []priceFilter{
		{Field: "serviceName", Value: "Storage", Op: filterEq},
		{Field: "armRegionName", Value: region, Op: filterEq},
		{Field: "skuName", Value: retailSKU, Op: filterEq},
		{Field: "type", Value: "Consumption", Op: filterEq},
		{Field: "productName", Value: productName, Op: filterEq},
		{Field: "meterName", Value: meterName, Op: filterEq},
		{Field: "isPrimaryMeterRegion", Value: "true", Op: filterEq},
		{Field: "tierMinimumUnits", Value: "0", Op: filterEq},
	}, nil
}

// bytesPerGiB is the storage conversion used by every image rule in the
// project. It is a constant here so the Azure pricing unit normalizer and the
// Azure gallery-image rule can never disagree about the GiB definition.
const bytesPerGiB = 1 << 30

// decimalGBPerGiB is the factor converting an Azure Retail Prices row quoted
// in decimal GB-month (10^9 bytes) to the canonical GiB-month (2^30 bytes)
// unit the rule uses. 1 GiB = 1.073741824 GB.
const decimalGBPerGiB = float64(bytesPerGiB) / 1_000_000_000

// normalizeGalleryImageUnitPrice normalizes a Retail Prices unitOfMeasure for
// the gallery image storage dimension to per-GiB-month.
//
// The recorded fixture uses "1 GiB/Month", which is the canonical unit already.
// "1 GB/Month" is also accepted and converted from decimal GB to GiB, because
// the Retail Prices API has historically used decimal GB for storage meters.
// Any other unit returns false; tellury never guesses a conversion factor.
func normalizeGalleryImageUnitPrice(unitOfMeasure string, unitPrice float64) (float64, bool) {
	switch strings.ToLower(strings.TrimSpace(unitOfMeasure)) {
	case "1 gib/month", "1 gib /month":
		return unitPrice, true
	case "1 gb/month", "1 gb /month":
		return unitPrice * decimalGBPerGiB, true
	default:
		return 0, false
	}
}
