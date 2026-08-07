package gcp

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	pricinggcp "github.com/TypeOneLabs/tellury/pkg/pricing/gcp"
)

// normalizer converts one CAI asset into a node. Pure, table-dispatched and
// unit-testable without any client. Unknown asset types return (nil, nil).
type normalizer func(*RawAsset, pricing.Sizer) (*graph.Node, error)

var normalizers = map[string]normalizer{
	TypeInstance: normalizeInstance,
	TypeDisk:     normalizeDisk,
	TypeBucket:   normalizeBucket,
	TypeSnapshot: normalizeGeneric(graph.KindSnapshot, ServiceCompute),
	TypeAddress:  normalizeAddress,
	TypeNetwork:  normalizeGeneric(graph.KindNetwork, ServiceCompute),
}

// Normalize dispatches on asset type. (nil, nil) means "not a type we model".
func Normalize(a *RawAsset, sz pricing.Sizer) (*graph.Node, error) {
	if a == nil || a.Name == "" {
		return nil, nil
	}
	fn, ok := normalizers[a.AssetType]
	if !ok {
		return nil, nil
	}
	return fn(a, sz)
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared scaffolding
// ─────────────────────────────────────────────────────────────────────────────

func baseNode(a *RawAsset, kind graph.ResourceKind, service string) *graph.Node {
	return &graph.Node{
		ID:        graph.Ref(a.Name),
		Kind:      kind,
		Name:      lastSegment(a.Name),
		Provider:  "gcp",
		Service:   service,
		AssetType: a.AssetType,
		Project:   projectOf(a),
		Location:  a.Location(),
		Attrs:     make(map[string]any, 12),
		Raw:       a.Data(),
	}
}

// lastSegment returns the trailing path element of a self-link or asset name.
func lastSegment(s string) string {
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// projectFromAssetName extracts the project ID from a CAI asset name:
// "//compute.googleapis.com/projects/p/zones/z/disks/d" → "p".
// projectOf resolves the owning project for an asset.
//
// Most GCP asset names embed the project ("//compute.googleapis.com/projects/p/zones/..."),
// but some services do not: a GCS bucket is named "//storage.googleapis.com/<bucket>" with
// no project segment at all. For those, the CAI envelope's parent
// ("//cloudresourcemanager.googleapis.com/projects/<p>") is the only source. Without this
// fallback every bucket carries an empty project, which silently disables metric enrichment
// for the whole scan — EnrichMetrics derives its project list from these fields.
func projectOf(a *RawAsset) string {
	if p := projectFromAssetName(a.Name); p != "" {
		return p
	}
	return projectFromAssetName(a.Parent())
}

func projectFromAssetName(name string) string {
	parts := strings.Split(name, "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "projects" {
			return parts[i+1]
		}
	}
	return ""
}

// NormalizeSelfLink rewrites a Compute Engine self-link into a CAI asset name:
//
//	"https://www.googleapis.com/compute/v1/projects/p/zones/z/instances/i"
//	→ "//compute.googleapis.com/projects/p/zones/z/instances/i"
//
// Pure string rewrite: strip scheme+host+API prefix, then prefix the service.
// Inputs already in CAI form are returned unchanged.
func NormalizeSelfLink(link string) string {
	s := strings.TrimSpace(link)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "//") {
		return s
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "/"); j >= 0 {
			s = s[j:] // drop the host, keep the leading slash
		} else {
			return ""
		}
	}
	// Drop any "/compute/v1", "/compute/beta", "/v1" style API prefix so only
	// the resource path remains.
	s = strings.TrimPrefix(s, "/")
	for _, prefix := range []string{"compute/v1/", "compute/beta/", "compute/alpha/", "v1/"} {
		s = strings.TrimPrefix(s, prefix)
	}
	if !strings.HasPrefix(s, "projects/") {
		if i := strings.Index(s, "projects/"); i >= 0 {
			s = s[i:]
		}
	}
	if s == "" {
		return ""
	}
	return "//" + ServiceCompute + "/" + s
}

// numOf parses a JSON value that GCP may render as either a string or a number.
func numOf(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func strOf(v any) (string, bool) { s, ok := v.(string); return s, ok }

func boolOf(v any) (bool, bool) { b, ok := v.(bool); return b, ok }

func mapOf(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok }

func sliceOf(v any) ([]any, bool) { s, ok := v.([]any); return s, ok }

// decodeData unmarshals a resource payload into a generic map. A payload that
// is absent or malformed yields an empty map, never nil, so every normalizer
// can read from it unconditionally.
func decodeData(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func labelsOf(data map[string]any) map[string]string {
	raw, ok := mapOf(data["labels"])
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := strOf(v); ok {
			out[k] = s
		}
	}
	return out
}

// locationOf prefers the resource's own zone/region field and falls back to the
// authoritative CAI envelope location when parsing fails.
func locationOf(data map[string]any, envelope string) string {
	for _, key := range []string{"zone", "region"} {
		if s, ok := strOf(data[key]); ok && s != "" {
			if seg := lastSegment(s); seg != "" {
				return seg
			}
		}
	}
	return envelope
}

// ─────────────────────────────────────────────────────────────────────────────
// compute.googleapis.com/Address
// ─────────────────────────────────────────────────────────────────────────────

// normalizeAddress is the real Address normalizer. CAI's Address payload is
// the Compute Engine Address resource: status, addressType, purpose, address,
// region, and users[] (the instances/forwarding rules the address is attached
// to). The node must carry enough of these for `unused_reserved_ip` to decide
// "external, reserved, attached to nothing" without needing any metric.
func normalizeAddress(a *RawAsset, _ pricing.Sizer) (*graph.Node, error) {
	data := decodeData(a.Data())
	n := baseNode(a, graph.KindAddress, ServiceCompute)
	n.Labels = labelsOf(data)
	n.Location = locationOf(data, a.Location())
	if name, ok := strOf(data["name"]); ok && name != "" {
		n.Name = name
	}

	if id, ok := strOf(data["id"]); ok && id != "" {
		n.SetAttr(AttrResourceID, id)
	}
	if status, ok := strOf(data["status"]); ok && status != "" {
		// CAI returns "RESERVED" | "IN_USE". Normalize to upper like every
		// other status field so the rule never has to handle casing drift.
		n.SetAttr(AttrStatus, strings.ToUpper(status))
	}
	if typ, ok := strOf(data["addressType"]); ok && typ != "" {
		n.SetAttr(AttrAddrType, strings.ToUpper(typ))
	}
	if purpose, ok := strOf(data["purpose"]); ok && purpose != "" {
		// Purpose "GCE_ENDPOINT" (default), "VPC_PEERING", "SHARED_LOADBALANCER_VIP",
		// "GCS_ENDPOINT", "IPSEC_INTERCONNECT", etc. External standard addresses
		// carry "GCE_ENDPOINT"; only these are eligible for the waste rule.
		n.SetAttr(AttrAddrPurpose, strings.ToUpper(purpose))
	}
	if ip, ok := strOf(data["address"]); ok && ip != "" {
		n.SetAttr(AttrAddrIP, ip)
	}
	// users[] is always written, even when empty: the rule needs to tell
	// "no users" from "payload not parsed", exactly like a disk's users[].
	users, _ := sliceOf(data["users"])
	n.SetAttr(AttrAddrUserCount, float64(len(users)))

	if ts, ok := strOf(data["creationTimestamp"]); ok && ts != "" {
		n.SetAttr(AttrCreationTime, ts)
	}
	return n, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// compute.googleapis.com/Disk
// ─────────────────────────────────────────────────────────────────────────────

func normalizeDisk(a *RawAsset, _ pricing.Sizer) (*graph.Node, error) {
	data := decodeData(a.Data())
	n := baseNode(a, graph.KindDisk, ServiceCompute)
	n.Labels = labelsOf(data)
	n.Location = locationOf(data, a.Location())
	if name, ok := strOf(data["name"]); ok && name != "" {
		n.Name = name
	}

	if id, ok := strOf(data["id"]); ok {
		n.SetAttr(AttrResourceID, id)
	}
	if size, ok := numOf(data["sizeGb"]); ok && size > 0 {
		n.SetAttr(AttrSizeGB, size)
	}
	if t, ok := strOf(data["type"]); ok && t != "" {
		n.SetAttr(AttrDiskType, lastSegment(t))
	}
	if status, ok := strOf(data["status"]); ok && status != "" {
		n.SetAttr(AttrStatus, strings.ToUpper(status))
	}

	// users[] is always written, even when empty: the rule needs to tell "no
	// users" from "payload not parsed".
	users, _ := sliceOf(data["users"])
	n.SetAttr(AttrUserCount, float64(len(users)))

	replicas, _ := sliceOf(data["replicaZones"])
	n.SetAttr(AttrReplicaZoneCount, float64(len(replicas)))

	for attr, key := range map[string]string{
		AttrLastAttachTime: "lastAttachTimestamp",
		AttrLastDetachTime: "lastDetachTimestamp",
		AttrCreationTime:   "creationTimestamp",
		AttrArchitecture:   "architecture",
	} {
		if s, ok := strOf(data[key]); ok && s != "" {
			n.SetAttr(attr, s)
		}
	}

	iops, _ := numOf(data["provisionedIops"])
	n.SetAttr(AttrProvisionedIOPS, iops)
	thr, _ := numOf(data["provisionedThroughput"])
	n.SetAttr(AttrProvisionedThroughput, thr)

	// disk_sku is derived once, here, so pricing and evidence agree.
	diskType, _ := n.Str(AttrDiskType)
	if diskType != "" {
		n.SetAttr(AttrDiskSKU, pricing.DiskSKU(diskType, float64(len(replicas))))
	}
	return n, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// compute.googleapis.com/Instance
// ─────────────────────────────────────────────────────────────────────────────

func normalizeInstance(a *RawAsset, sz pricing.Sizer) (*graph.Node, error) {
	data := decodeData(a.Data())
	n := baseNode(a, graph.KindInstance, ServiceCompute)
	n.Labels = labelsOf(data)
	n.Location = locationOf(data, a.Location())
	if name, ok := strOf(data["name"]); ok && name != "" {
		n.Name = name
	}

	// instance_id is the join key to Monitoring resource.labels.instance_id.
	if id, ok := strOf(data["id"]); ok && id != "" {
		n.SetAttr(AttrInstanceID, id)
		n.SetAttr(AttrResourceID, id)
	} else if id, ok := numOf(data["id"]); ok {
		s := strconv.FormatFloat(id, 'f', -1, 64)
		n.SetAttr(AttrInstanceID, s)
		n.SetAttr(AttrResourceID, s)
	}

	machineType := ""
	if mt, ok := strOf(data["machineType"]); ok && mt != "" {
		machineType = lastSegment(mt)
		n.SetAttr(AttrMachineType, machineType)
	}
	if status, ok := strOf(data["status"]); ok && status != "" {
		n.SetAttr(AttrStatus, strings.ToUpper(status))
	}

	model := "STANDARD"
	preemptible := false
	if sched, ok := mapOf(data["scheduling"]); ok {
		if m, ok := strOf(sched["provisioningModel"]); ok && m != "" {
			model = strings.ToUpper(m)
		}
		if b, ok := boolOf(sched["preemptible"]); ok {
			preemptible = b
		}
	}
	n.SetAttr(AttrProvisioningModel, model)
	n.SetAttr(AttrPreemptible, preemptible)

	accelerators := 0.0
	if accs, ok := sliceOf(data["guestAccelerators"]); ok {
		for _, raw := range accs {
			if acc, ok := mapOf(raw); ok {
				if c, ok := numOf(acc["acceleratorCount"]); ok {
					accelerators += c
				}
			}
		}
	}
	n.SetAttr(AttrAcceleratorCount, accelerators)

	for attr, key := range map[string]string{
		AttrMinCPUPlatform: "minCpuPlatform",
		AttrCreationTime:   "creationTimestamp",
		AttrLastStartTime:  "lastStartTimestamp",
	} {
		if s, ok := strOf(data[key]); ok && s != "" {
			n.SetAttr(attr, s)
		}
	}

	if createdBy, ok := metadataItem(data, "created-by"); ok {
		n.SetAttr(AttrCreatedBy, createdBy)
	}

	// Derived shape: catalog first, deterministic custom parse second.
	if machineType != "" && sz != nil {
		if spec, ok := sz.Spec(machineType); ok {
			n.SetAttr(AttrVCPUCount, spec.VCPU)
			n.SetAttr(AttrMemoryGiB, spec.MemoryGiB)
			n.SetAttr(AttrMachineFamily, spec.Family)
			source := pricinggcp.SpecSourceCatalog
			if pricinggcp.IsCustom(machineType) {
				source = pricinggcp.SpecSourceCustom
			}
			n.SetAttr(AttrMachineSpecSource, source)
		} else {
			// Family is still useful for grouping even when the shape is
			// unknown; vcpu_count stays absent, which gates the rule.
			n.SetAttr(AttrMachineFamily, sz.Family(machineType))
		}
	}
	return n, nil
}

// metadataItem reads metadata.items[key=want].value.
func metadataItem(data map[string]any, want string) (string, bool) {
	md, ok := mapOf(data["metadata"])
	if !ok {
		return "", false
	}
	items, ok := sliceOf(md["items"])
	if !ok {
		return "", false
	}
	for _, raw := range items {
		item, ok := mapOf(raw)
		if !ok {
			continue
		}
		if k, _ := strOf(item["key"]); k == want {
			if v, ok := strOf(item["value"]); ok {
				return v, true
			}
		}
	}
	return "", false
}

// ─────────────────────────────────────────────────────────────────────────────
// storage.googleapis.com/Bucket
// ─────────────────────────────────────────────────────────────────────────────

func normalizeBucket(a *RawAsset, _ pricing.Sizer) (*graph.Node, error) {
	data := decodeData(a.Data())
	n := baseNode(a, graph.KindBucket, ServiceStorage)
	n.Labels = labelsOf(data)
	if name, ok := strOf(data["name"]); ok && name != "" {
		n.Name = name
	}
	if loc, ok := strOf(data["location"]); ok && loc != "" {
		n.Location = loc
	}

	// bucket_name is the join key to Monitoring resource.labels.bucket_name.
	n.SetAttr(AttrBucketName, n.Name)
	if id, ok := strOf(data["id"]); ok && id != "" {
		n.SetAttr(AttrResourceID, id)
	}
	if class, ok := strOf(data["storageClass"]); ok && class != "" {
		n.SetAttr(AttrStorageClass, strings.ToUpper(class))
	}
	if lt, ok := strOf(data["locationType"]); ok && lt != "" {
		n.SetAttr(AttrLocationType, lt)
	}
	if ts, ok := strOf(data["timeCreated"]); ok && ts != "" {
		n.SetAttr(AttrCreationTime, ts)
	}

	// Determinism: CAI omits `lifecycle` entirely when no rules exist, so the
	// count is written unconditionally first and only then overwritten. The
	// attribute is never absent, which is what lets the rule distinguish
	// "no rules" from "unparsed payload".
	n.SetAttr(AttrLifecycleRuleCnt, 0.0)
	if lc, ok := mapOf(data["lifecycle"]); ok {
		if lrules, ok := sliceOf(lc["rule"]); ok {
			n.SetAttr(AttrLifecycleRuleCnt, float64(len(lrules)))
			if actions := lifecycleActions(lrules); actions != "" {
				n.SetAttr(AttrLifecycleActions, actions)
			}
		}
	}

	versioning := false
	if v, ok := mapOf(data["versioning"]); ok {
		if b, ok := boolOf(v["enabled"]); ok {
			versioning = b
		}
	}
	n.SetAttr(AttrVersioning, versioning)

	autoclass := false
	if ac, ok := mapOf(data["autoclass"]); ok {
		if b, ok := boolOf(ac["enabled"]); ok {
			autoclass = b
		}
	}
	n.SetAttr(AttrAutoclass, autoclass)

	retention := 0.0
	locked := false
	if rp, ok := mapOf(data["retentionPolicy"]); ok {
		if v, ok := numOf(rp["retentionPeriod"]); ok {
			retention = v
		}
		if b, ok := boolOf(rp["isLocked"]); ok {
			locked = b
		}
	}
	n.SetAttr(AttrRetentionSeconds, retention)
	n.SetAttr(AttrRetentionLocked, locked)

	softDelete := 0.0
	if sd, ok := mapOf(data["softDeletePolicy"]); ok {
		if v, ok := numOf(sd["retentionDurationSeconds"]); ok {
			softDelete = v
		}
	}
	n.SetAttr(AttrSoftDeleteSeconds, softDelete)

	return n, nil
}

// lifecycleActions renders the sorted, comma-joined set of action types.
func lifecycleActions(lrules []any) string {
	seen := map[string]bool{}
	for _, raw := range lrules {
		r, ok := mapOf(raw)
		if !ok {
			continue
		}
		action, ok := mapOf(r["action"])
		if !ok {
			continue
		}
		if t, ok := strOf(action["type"]); ok && t != "" {
			seen[t] = true
		}
	}
	if len(seen) == 0 {
		return ""
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sortStrings(out)
	return strings.Join(out, ",")
}

// ─────────────────────────────────────────────────────────────────────────────
// Generic
// ─────────────────────────────────────────────────────────────────────────────

func normalizeGeneric(kind graph.ResourceKind, service string) normalizer {
	return func(a *RawAsset, _ pricing.Sizer) (*graph.Node, error) {
		data := decodeData(a.Data())
		n := baseNode(a, kind, service)
		n.Labels = labelsOf(data)
		n.Location = locationOf(data, a.Location())
		if name, ok := strOf(data["name"]); ok && name != "" {
			n.Name = name
		}
		if status, ok := strOf(data["status"]); ok && status != "" {
			n.SetAttr(AttrStatus, strings.ToUpper(status))
		}
		if ts, ok := strOf(data["creationTimestamp"]); ok && ts != "" {
			n.SetAttr(AttrCreationTime, ts)
		}
		return n, nil
	}
}
