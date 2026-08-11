package gcp

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// table is pricing.Kind -> SKU -> region -> price.
type table map[pricing.Kind]map[string]map[string]float64

// StaticPricer reads a GCP price table from a JSON file on disk. It is used
// by tests (via TELLURY_PRICE_FIXTURE) and by offline scans that still need
// pricing.
//
// There is no embedded fallback table. A price that cannot be resolved from
// this file returns ErrNoPrice, and the rule skips rather than guessing.
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

// NewStaticPricerFromFile loads a GCP price table from the given JSON file.
func NewStaticPricerFromFile(path string) (*StaticPricer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pricing: read GCP price file %q: %w", path, err)
	}
	var pf priceFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, fmt.Errorf("pricing: decode GCP price file %q: %w", path, err)
	}
	return &StaticPricer{t: newTableFromFile(pf)}, nil
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
