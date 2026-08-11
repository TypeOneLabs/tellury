// Package aws is the AWS pricing model for tellury. It is the AWS analog of
// pkg/pricing/gcp: the parent package owns the pricing interfaces and money
// conventions, this package owns the AWS SKU/region spelling. It has two
// implementations of pricing.Pricer:
//
//   - StaticPricer (this file): the embedded EBS + Elastic IP price table,
//     USD only, used for offline scans and as the lowest-precedence fallback
//     of the live pricer. The SKU tokens are the EC2 SDK's OWN volume-type
//     strings ("gp3", "io2", "st1", "sc1", "standard", ...) and the price
//     list's Elastic-IP operation token ("AdditionalAddress") — the same
//     tokens the rules query and the live catalogue indexes, so a live answer
//     and the embedded fallback resolve the same key.
//
//   - CatalogPricer (catalog.go): the live AWS Price List API (pricing::
//     GetProducts), lazy-loaded, with this StaticPricer as its fallback.
package aws

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

//go:embed data/aws_prices.json
var embeddedPriceTable []byte

// table is pricing.Kind -> SKU -> region -> price, the same shape the GCP
// static table uses.
type table map[pricing.Kind]map[string]map[string]float64

// priceFile is the JSON shape of the embedded AWS price table. The AWS
// catalogue is USD only (the Price List API prices every dimension in USD), so
// the file carries no currency choice; Currency is recorded for provenance.
type priceFile struct {
	Version         string                        `json:"version"`
	Currency        string                        `json:"currency"`
	DiskCapacity    map[string]map[string]float64 `json:"disk_capacity"`
	DiskIOPS        map[string]map[string]float64 `json:"disk_iops"`
	DiskThroughput  map[string]map[string]float64 `json:"disk_throughput"`
	StaticIP        map[string]map[string]float64 `json:"static_ip"`
}

func newTableFromFile(pf priceFile) table {
	t := table{
		pricing.KindDiskCapacity:   pf.DiskCapacity,
		pricing.KindDiskIOPS:       pf.DiskIOPS,
		pricing.KindDiskThroughput: pf.DiskThroughput,
		pricing.KindStaticIP:       pf.StaticIP,
	}
	for k, v := range t {
		if v == nil {
			t[k] = map[string]map[string]float64{}
		}
	}
	return t
}

// StaticPricer reads the embedded AWS price table, optionally overlaid by a
// user-supplied override file (see OverlayFile).
//
// FALLBACK OF LAST RESORT. A rate in this table is the lowest-precedence
// source in the CatalogPricer stack (--price-file override > live Price List
// API > embedded table) and is a hand-maintained snapshot of the published
// commercial-region list prices (us-east-1 baseline, applied to every region
// through the "default" key), not ground truth: it can silently drift from
// the live catalogue. Treat any answer whose provenance reads SourceEmbedded
// as a stopgap to verify against the live catalogue, never as a number to
// trust on its own.
type StaticPricer struct {
	t table
}

// NewStaticPricer loads the embedded AWS price table.
func NewStaticPricer() (*StaticPricer, error) {
	var pf priceFile
	if err := json.Unmarshal(embeddedPriceTable, &pf); err != nil {
		return nil, fmt.Errorf("pricing: decode embedded AWS price table: %w", err)
	}
	return &StaticPricer{t: newTableFromFile(pf)}, nil
}

// UnitPrice resolves kind -> SKU -> region -> "default". AWS has no
// multi-region prefix pricing (there is no "us" or "eu" aggregate tier), so
// the fallback chain is exact region, then "default" — never a prefix.
func (p *StaticPricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	skus, ok := p.t[kind]
	if !ok {
		return 0, "", pricing.ErrNoPrice
	}
	regions, ok := skus[sku]
	if !ok {
		return 0, "", pricing.ErrNoPrice
	}
	if v, ok := regions[region]; ok {
		return v, region, nil
	}
	if v, ok := regions["default"]; ok {
		return v, "default", nil
	}
	return 0, "", pricing.ErrNoPrice
}

// MonthlyCost is a convenience wrapper: price * quantity, using UnitPrice.
func (p *StaticPricer) MonthlyCost(it pricing.Item) (float64, error) {
	unit, _, err := p.UnitPrice(it.Kind, it.Provider, it.SKU, it.Region)
	if err != nil {
		return 0, err
	}
	return unit * it.Quantity, nil
}

// OverlayFile merges a JSON price file on top of the current table. The file
// shape is the generic kind -> SKU -> region -> price table (e.g.
// {"disk_capacity": {"gp3": {"us-east-1": 0.08}}}), which is exactly the
// table's own shape and therefore the shape the live catalogue emits. This
// backs `tellury scan --price-file` for AWS.
func (p *StaticPricer) OverlayFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("pricing: read AWS price file %s: %w", path, err)
	}
	var overlay table
	if err := json.Unmarshal(data, &overlay); err != nil {
		return fmt.Errorf("pricing: decode AWS price file %q: %w", path, err)
	}
	if p.t == nil {
		p.t = table{}
	}
	for kind, skus := range overlay {
		if len(skus) == 0 {
			continue
		}
		dstSKUs, ok := p.t[kind]
		if !ok {
			dstSKUs = map[string]map[string]float64{}
			p.t[kind] = dstSKUs
		}
		for sku, regions := range skus {
			dstRegions, ok := dstSKUs[sku]
			if !ok {
				dstRegions = map[string]float64{}
				dstSKUs[sku] = dstRegions
			}
			for region, price := range regions {
				dstRegions[region] = price
			}
		}
	}
	return nil
}
