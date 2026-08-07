package pricing

// DiskSKU derives the pricing SKU from disk_type + replica_zone_count (spec
// §5.3): a disk replicated across >=2 zones bills at the regional rate.
//
// This is a normalize-time concern (pkg/cloud/gcp/normalize.go writes the
// result into Attrs["disk_sku"]) so every rule and the pricer agree on one
// spelling; it is intentionally duplicated as an unexported helper in
// pkg/rules/gcp/compute/detached_disk for pure-Go unit tests that have no
// cloud import.
func DiskSKU(diskType string, replicaZoneCount float64) string {
	if replicaZoneCount >= 2 {
		return diskType + "-regional"
	}
	return diskType
}
