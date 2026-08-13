package azure

import (
	"context"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// VMPricer is the Azure VM price seam used by the Azure compute rule. It is
// separate from pricing.Pricer because a VM price is keyed by size + region +
// OS, while pricing.Pricer.UnitPrice has no OS axis and therefore cannot tell
// a Linux row from a Windows row for the same armSkuName.
type VMPricer interface {
	VMPrice(ctx context.Context, region, size, osType string) (float64, error)
}

type vmPriceKey struct {
	size   string
	region string
	osType string
}

// vmProductNameAllowlist is the measured discriminator that separates the one
// billable Virtual Machines row from the legacy look-alike rows that share the
// same serviceName, armRegionName and armSkuName. Measured across nine
// SKU-region pairs, every row with serviceName "Virtual Machines" also carries
// a productName beginning "Virtual Machines"; the legacy lines ("Dasv5 Series
// Cloud Services", "HDInsight FSv2 Series") do not. serviceName is
// deliberately NOT used as a filter: every one of the ambiguous rows carries
// serviceName "Virtual Machines".
const vmProductNameAllowlist = "Virtual Machines"

// VMPrice resolves one Azure VM size in one region for one OS family and
// returns the canonical hourly unit price. It shares the CatalogPricer's
// filter-table, fixture and cache discipline: one table builds the OData
// request and re-asserts every predicate against the returned or recorded
// rows.
func (c *CatalogPricer) VMPrice(ctx context.Context, region, size, osType string) (float64, error) {
	region = LocationRegion(region)
	osType = normalizeVMOSType(osType)
	key := vmPriceKey{size: size, region: region, osType: osType}

	filters, err := vmPriceFilters(region, size, osType)
	if err != nil {
		return 0, err
	}

	c.mu.Lock()
	if res, ok := c.vmCache[key]; ok {
		c.mu.Unlock()
		return res.unitPrice, nil
	}
	if c.vmUnpriceable[key] {
		c.mu.Unlock()
		return 0, pricing.ErrNoPrice
	}
	c.mu.Unlock()

	var rows []retailItem
	if path := priceFixturePath(); path != "" {
		entries, err := c.loadFixture(path)
		if err != nil {
			return 0, err
		}
		for _, entry := range entries {
			page, err := parseRetailPage(entry.Body)
			if err != nil {
				c.log.Debug("azure: could not parse recorded price response; skipping", "name", entry.Name, "err", err)
				continue
			}
			rows = append(rows, page.Items...)
		}
	} else {
		rows, err = c.fetchLiveContext(ctx, filters)
		if err != nil {
			c.log.Warn("azure: Retail Prices API unavailable; resources requiring prices will skip", "err", err)
			return 0, err
		}
	}

	res, ok := selectPrice(rows, filters, pricing.KindVMInstance)
	if !ok {
		c.mu.Lock()
		c.vmUnpriceable[key] = true
		c.mu.Unlock()
		return 0, pricing.ErrNoPrice
	}

	c.mu.Lock()
	c.vmCache[key] = res
	c.mu.Unlock()
	return res.unitPrice, nil
}

func normalizeVMOSType(osType string) string {
	osType = strings.ToLower(strings.TrimSpace(osType))
	switch osType {
	case "linux", "windows":
		return osType
	default:
		return ""
	}
}

// vmPriceFilters returns the complete filter set for one VM size/region/OS
// lookup. It is the single source for both the live OData request and the
// fixture row matcher.
//
// The measured rule is an allowlist: productName must start with
// "Virtual Machines". serviceName is deliberately not in the table — every
// ambiguous row measured carries serviceName "Virtual Machines", so it does
// not discriminate. armSkuName is the ARM size token (for example
// Standard_D2as_v5); the Retail Prices skuName is not the ARM SKU name.
func vmPriceFilters(region, size, osType string) ([]priceFilter, error) {
	region = LocationRegion(region)
	size = strings.TrimSpace(size)
	if region == "" || size == "" {
		return nil, pricing.ErrNoPrice
	}

	base := []priceFilter{
		{Field: "armRegionName", Value: region, Op: filterEq},
		{Field: "armSkuName", Value: size, Op: filterEq},
		{Field: "type", Value: "Consumption", Op: filterEq},
		{Field: "tierMinimumUnits", Value: "0", Op: filterEq},
		{Field: "productName", Value: vmProductNameAllowlist, Op: filterStartsWith},
	}

	switch osType {
	case "linux":
		return append(base, spotAndWindowsExclusions()...), nil
	case "windows":
		base = append(base, priceFilter{Field: "productName", Value: "Windows", Op: filterContains})
		return append(base, spotExclusions()...), nil
	default:
		return nil, pricing.ErrNoPrice
	}
}
