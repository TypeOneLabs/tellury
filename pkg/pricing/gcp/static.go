package gcp

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

//go:embed data/gcp_prices.json
var embeddedPriceTable []byte

// table is pricing.Kind -> SKU -> region -> price.
type table map[pricing.Kind]map[string]map[string]float64

// StaticPricer reads an embedded GCP price table, optionally overlaid by a
// user-supplied override file (see OverlayFile).
//
// It is the GCP-specific implementation of pricing.Pricer over the embedded
// price data. The parent package (pricing) owns the interfaces and money
// conventions; this package owns the actual USD values and the SKU/region
// spelling. A future AWS provider would keep its own StaticPricer in its own
// package rather than sharing this GCP-shaped one.
type StaticPricer struct {
	t table
}

type priceFile struct {
	Version         string                        `json:"version"`
	Currency        string                        `json:"currency"`
	DiskCapacity    map[string]map[string]float64 `json:"disk_capacity"`
	DiskIOPS        map[string]map[string]float64 `json:"disk_iops"`
	DiskThroughput  map[string]map[string]float64 `json:"disk_throughput"`
	VMInstance      map[string]map[string]float64 `json:"vm_instance"`
	VMCustomCPU     map[string]map[string]float64 `json:"vm_custom_cpu"`
	VMCustomRAM     map[string]map[string]float64 `json:"vm_custom_ram"`
	GCSStorage      map[string]map[string]float64 `json:"gcs_storage"`
	GCSRetrieval    map[string]map[string]float64 `json:"gcs_retrieval"`
	GCSOpsClassA    map[string]map[string]float64 `json:"gcs_ops_class_a"`
	StaticIP        map[string]map[string]float64 `json:"static_ip"`
	SnapshotStorage map[string]map[string]float64 `json:"snapshot_storage"`
}

func newTableFromFile(pf priceFile) table {
	t := table{
		pricing.KindDiskCapacity:    pf.DiskCapacity,
		pricing.KindDiskIOPS:        pf.DiskIOPS,
		pricing.KindDiskThroughput:  pf.DiskThroughput,
		pricing.KindVMInstance:      pf.VMInstance,
		pricing.KindVMCustomCPU:     pf.VMCustomCPU,
		pricing.KindVMCustomRAM:     pf.VMCustomRAM,
		pricing.KindGCSStorage:      pf.GCSStorage,
		pricing.KindGCSRetrieval:    pf.GCSRetrieval,
		pricing.KindGCSOpsClassA:    pf.GCSOpsClassA,
		pricing.KindStaticIP:        pf.StaticIP,
		pricing.KindSnapshotStorage: pf.SnapshotStorage,
	}
	for k, v := range t {
		if v == nil {
			t[k] = map[string]map[string]float64{}
		}
	}
	return t
}

// NewStaticPricer loads the embedded table.
func NewStaticPricer() (*StaticPricer, error) {
	var pf priceFile
	if err := json.Unmarshal(embeddedPriceTable, &pf); err != nil {
		return nil, fmt.Errorf("pricing: decode embedded table: %w", err)
	}
	return &StaticPricer{t: newTableFromFile(pf)}, nil
}

// LoadOverride replaces the entire table with the given JSON file contents.
// Deterministic, offline; used by --price-file.
func (p *StaticPricer) LoadOverride(data []byte) error {
	var pf priceFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return fmt.Errorf("pricing: decode override table: %w", err)
	}
	p.t = newTableFromFile(pf)
	return nil
}

// UnitPrice resolves exact region -> region prefix -> "default".
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
	// region prefix, e.g. "us-central1" -> "us"
	if idx := indexByte(region, '-'); idx > 0 {
		prefix := region[:idx]
		if v, ok := regions[prefix]; ok {
			return v, prefix, nil
		}
	}
	if v, ok := regions["default"]; ok {
		return v, "default", nil
	}
	return 0, "", pricing.ErrNoPrice
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// MonthlyCost is a convenience wrapper: price * quantity, using UnitPrice.
func (p *StaticPricer) MonthlyCost(it pricing.Item) (float64, error) {
	unit, _, err := p.UnitPrice(it.Kind, it.Provider, it.SKU, it.Region)
	if err != nil {
		return 0, err
	}
	return unit * it.Quantity, nil
}

// OverlayFile reads a JSON price file from disk and merges it on top of the
// currently loaded table (per Kind/SKU/region entry — an override file need
// only specify the SKUs it changes). This backs `tellury scan --price-file`.
func (p *StaticPricer) OverlayFile(path string) error {
	data, err := readFile(path)
	if err != nil {
		return err
	}
	var pf priceFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return fmt.Errorf("pricing: decode price file %q: %w", path, err)
	}
	overlay := newTableFromFile(pf)
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
