package gcp

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

func init() {
	normalizers[TypeImage] = normalizeImage
	normalizers[TypeMachineImage] = normalizeMachineImage
}

// normalizeImage is the GCP custom-image normalizer. CAI's Image payload is
// the Compute Engine Image resource: archiveSizeBytes, diskSizeGb,
// storageLocations[], family, status and creationTimestamp.
//
// The billable quantity is archiveSizeBytes — the size of the image archive
// stored in Cloud Storage. diskSizeGb is the SOURCE DISK size at image
// creation time; it is kept as evidence (source_disk_size_gb) and must never
// be priced. This is the same stored-bytes vs source-size distinction that
// made old_snapshot wrong once, and the cost model here refuses to repeat it.
func normalizeImage(a *RawAsset, _ pricing.Sizer) (*graph.Node, error) {
	data := decodeData(a.Data())
	n := baseNode(a, graph.KindImage, ServiceCompute)
	n.Labels = labelsOf(data)
	if name, ok := strOf(data["name"]); ok && name != "" {
		n.Name = name
	}
	if id := idString(data["id"]); id != "" {
		n.SetAttr(AttrImageID, id)
	}
	if family, ok := strOf(data["family"]); ok && family != "" {
		n.SetAttr(AttrFamily, family)
	}
	if status, ok := strOf(data["status"]); ok && status != "" {
		n.SetAttr(AttrStatus, strings.ToUpper(status))
	}
	if ts, ok := strOf(data["creationTimestamp"]); ok && ts != "" {
		n.SetAttr(AttrCreationTime, ts)
	}

	// storage_bytes is written ONLY when archiveSizeBytes was parsed. Absent
	// means the payload was not read; present-but-zero is a real (free) image,
	// which falls out below the minimum-waste floor rather than being skipped
	// as missing data.
	if storageBytes, ok := numOf(data["archiveSizeBytes"]); ok {
		n.SetAttr(AttrStorageBytes, storageBytes)
	}
	size, _ := numOf(data["diskSizeGb"])
	n.SetAttr(AttrSourceDiskSizeGB, size)

	// Location is the first canonical storageLocations[] entry. Multiple
	// DISTINCT locations mean the archive is replicated across regions and the
	// rule refuses to price a single-region approximation; the normalizer
	// leaves storage_location absent in that case.
	if loc, ok := canonicalStorageLocation(data); ok {
		n.SetAttr(AttrStorageLocation, loc)
		n.Location = loc
	} else {
		n.Location = ""
	}

	// Reference fields are always written, so absence of reference_count means
	// zero. references_complete starts false and is set by the provider's
	// instance-template reference pass.
	n.SetAttr(AttrReferenceCount, 0.0)
	n.SetAttr(AttrReferenceSources, []string{})
	n.SetAttr(AttrReferencesComplete, false)
	return n, nil
}

// normalizeMachineImage is the GCP machine-image normalizer. CAI's
// MachineImage payload is the Compute Engine MachineImage resource:
// totalStorageBytes, storageLocations[], status and creationTimestamp.
//
// Machine images have no persistent reference path in the GA Compute API, so
// this node carries no reference fields — old_machine_image is age-only,
// exactly like old_snapshot.
func normalizeMachineImage(a *RawAsset, _ pricing.Sizer) (*graph.Node, error) {
	data := decodeData(a.Data())
	n := baseNode(a, graph.KindImage, ServiceCompute)
	n.Labels = labelsOf(data)
	if name, ok := strOf(data["name"]); ok && name != "" {
		n.Name = name
	}
	if id := idString(data["id"]); id != "" {
		n.SetAttr(AttrMachineImageID, id)
	}
	if status, ok := strOf(data["status"]); ok && status != "" {
		n.SetAttr(AttrStatus, strings.ToUpper(status))
	}
	if ts, ok := strOf(data["creationTimestamp"]); ok && ts != "" {
		n.SetAttr(AttrCreationTime, ts)
	}

	// totalStorageBytes is the billable quantity for a machine image.
	if storageBytes, ok := numOf(data["totalStorageBytes"]); ok {
		n.SetAttr(AttrStorageBytes, storageBytes)
	}

	if loc, ok := canonicalStorageLocation(data); ok {
		n.SetAttr(AttrStorageLocation, loc)
		n.Location = loc
	} else {
		n.Location = ""
	}
	return n, nil
}

// canonicalStorageLocation returns the single canonical region from a
// resource's storageLocations[]. ok=false when there are zero locations or
// more than one DISTINCT canonical location; in both cases the rule must
// skip rather than guess a region.
func canonicalStorageLocation(data map[string]any) (string, bool) {
	locs := stringSliceOf(data["storageLocations"])
	if len(locs) == 0 {
		return "", false
	}
	first := ""
	for _, loc := range locs {
		c := locationRegion(loc)
		if c == "" {
			continue
		}
		if first == "" {
			first = c
			continue
		}
		if c != first {
			return "", false
		}
	}
	if first == "" {
		return "", false
	}
	return first, true
}

// stringSliceOf reads a field that is either []string (the normal typed JSON
// shape) or []any (the shape decodeData produces for a bare untyped fixture).
func stringSliceOf(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := strOf(item); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// idString renders a GCP numeric/string resource id as the stable string the
// graph stores. Compute Engine returns ids as strings in JSON for some API
// versions and numbers in others; normalizers must accept both.
func idString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}
