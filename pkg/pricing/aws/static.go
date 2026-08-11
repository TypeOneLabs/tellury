// Package aws is the AWS pricing model for tellury. It is the AWS analog of
// pkg/pricing/gcp: the parent package owns the pricing interfaces and money
// conventions, this package owns the AWS SKU/region spelling. It has one
// implementation of pricing.Pricer:
//
//   - CatalogPricer (catalog.go): the live AWS Price List API (pricing::
//     GetProducts), lazy-loaded. When TELLURY_PRICE_FIXTURE is set, the
//     catalogue loads from that file instead of calling the API — a
//     test-only hook, never a user-facing flag.
//
//   - StaticPricer (this file): reads a price table from a JSON file on
//     disk. It is reached ONLY through TELLURY_PRICE_FIXTURE, which is a
//     test hook — there is no embedded table and no automatic fallback, so
//     a price that cannot be resolved skips the resource instead of being
//     guessed at. The SKU tokens are the EC2
//     SDK's own volume-type strings ("gp3", "io2", "st1", "sc1",
//     "standard", ...) and the Elastic-IP operation token
//     ("AdditionalAddress") — the same tokens the rules query and the live
//     catalogue indexes, so a live answer and a fixture-loaded price
//     resolve the same key.
package aws

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// table is pricing.Kind -> SKU -> region -> price.
type table map[pricing.Kind]map[string]map[string]float64

// priceFile is the JSON shape of an AWS price table. The AWS catalogue is
// USD only, so the file carries no currency choice; Currency is recorded for
// provenance.
type priceFile struct {
	Version        string                        `json:"version"`
	Currency       string                        `json:"currency"`
	DiskCapacity   map[string]map[string]float64 `json:"disk_capacity"`
	DiskIOPS       map[string]map[string]float64 `json:"disk_iops"`
	DiskThroughput map[string]map[string]float64 `json:"disk_throughput"`
	StaticIP       map[string]map[string]float64 `json:"static_ip"`
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

// StaticPricer reads an AWS price table from a JSON file on disk. It is used
// by tests (via TELLURY_PRICE_FIXTURE) and by offline scans that still need
// pricing.
//
// There is no embedded fallback table. A price that cannot be resolved from
// this file returns ErrNoPrice, and the rule skips rather than guessing.
type StaticPricer struct {
	t table
}

// NewStaticPricerFromFile loads an AWS price table from the given JSON file.
func NewStaticPricerFromFile(path string) (*StaticPricer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pricing: read AWS price file %q: %w", path, err)
	}
	var pf priceFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, fmt.Errorf("pricing: decode AWS price file %q: %w", path, err)
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
