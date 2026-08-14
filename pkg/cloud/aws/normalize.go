package aws

import (
	"sort"
	"strings"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// Normalizers: pure functions that convert one EC2 Describe result into a
// graph node. They are unit-testable without any client or credentials, and
// their Attrs keys are the EC2 SDK's own field names (see attrs.go), so a rule
// reads exactly what DescribeVolumes / DescribeAddresses / DescribeInstances /
// DescribeImages / DescribeSnapshots documented.

// Service and asset-type tokens. AWS has no Cloud Asset Inventory, so the
// asset-type token is the provider's own stable "aws.<service>.<resource>"
// spelling; the service token is the AWS service name.
const (
	serviceEC2        = "ec2"
	assetTypeVolume   = "aws.ec2.volume"
	assetTypeAddress  = "aws.ec2.address"
	assetTypeInstance = "aws.ec2.instance"
	assetTypeImage    = "aws.ec2.image"
	assetTypeSnapshot = "aws.ec2.snapshot"
)

// InstanceTypeInfo is the resolved shape of one EC2 instance type, from
// ec2:DescribeInstanceTypes. It carries only the fields a rule reads; the
// full SDK type is retained in the call path but not stored.
type InstanceTypeInfo struct {
	VCPU      float64 // VCpuInfo.DefaultVCpus
	MemoryGiB float64 // MemoryInfo.SizeInMiB / 1024
}

// amiSnapshotDescriptionPrefix is the AWS-generated description prefix on
// snapshots created by CreateImage. It is the single discriminator the
// orphaned_ami_snapshot rule uses.
const amiSnapshotDescriptionPrefix = "Created by CreateImage("

// locationRegion is the location node's answer to "what place is this" — a
// thin wrapper over the single pricing.CanonicalRegion canonicaliser, exactly
// like pkg/cloud/gcp's wrapper of the same name. AWS's own availability-zone
// form ("us-east-1a") is flattened to its region ("us-east-1") here so the
// graph node and the price resolve a location through the same code path and
// can never disagree. This is the ONLY canonicalisation point in the AWS
// provider.
func locationRegion(location string) string {
	return pricing.CanonicalRegion(location)
}

// accountNode builds the account container node — the AWS analog of GCP's
// "projects/<id>" project container. Its token is "accounts/<id>"; every
// region node in the scan hangs off it via a containment edge, and the scan
// summary's "N accounts analyzed" figure counts these nodes.
func accountNode(accountToken, account string) *graph.Node {
	return &graph.Node{
		ID:       graph.Ref(accountToken),
		Kind:     graph.KindAccount,
		Name:     account,
		Provider: "aws",
		Attrs:    map[string]any{},
	}
}

// regionNode builds the per-account region container node. The ID is scoped to
// the account ("accounts/<id>/regions/<region>") so two accounts sharing a
// region never collide and the containment chain stays linear — exactly as
// GCP's region node is scoped to its project. The Name field is the canonical
// region string (what a cross-account rollup groups on) and the Project field
// carries the owning account ID so the node is attributable.
//
// canonicalRegion is ALREADY canonicalised (normalize.go routes every node's
// location through the single pricing.CanonicalRegion wrapper); this function
// never canonicalises again, same rule as pkg/cloud/gcp/regionNode.
func regionNode(accountToken, account, canonicalRegion string) *graph.Node {
	return &graph.Node{
		ID:       graph.Ref(accountToken + "/regions/" + canonicalRegion),
		Kind:     graph.KindRegion,
		Name:     canonicalRegion,
		Provider: "aws",
		Project:  account,
		Attrs:    map[string]any{},
	}
}

// instanceNode is the minimal instance node an EBS attachment references. It
// exists so the graph's topology says "attached" exactly when the volume's
// Attachments list says so — a detached-volume rule can check either the
// node's attachment_count or the graph's attached_to edges. The DescribeInstances
// step enriches these nodes with full instance attributes (see NormalizeInstance);
// the ingestion loop processes instances after volumes so the enriched node
// is always the final write for the ID.
func instanceNode(instanceID, account, region string) *graph.Node {
	return &graph.Node{
		ID:        graph.Ref("accounts/" + account + "/regions/" + region + "/instances/" + instanceID),
		Kind:      graph.KindInstance,
		Name:      instanceID,
		Provider:  "aws",
		Service:   serviceEC2,
		AssetType: assetTypeInstance,
		Project:   account,
		Location:  region,
		Attrs:     map[string]any{},
	}
}

// NormalizeVolume converts one DescribeVolumes result into a graph node. A
// volume without an ID (which the real API never returns) is dropped.
//
// Attrs, all named from the EC2 SDK's own types.Volume fields (see attrs.go):
// state (State), size_gb (Size, in GiB), volume_type (VolumeType), iops
// (Iops, provisioned IOPS), throughput (Throughput, in MiB/s), create_time
// (CreateTime, RFC3339), availability_zone (AvailabilityZone), and the
// attachments (Attachments) — written as a structured list plus an
// always-written attachment_count so a rule can tell "no attachments" from
// "payload not parsed", exactly like the GCP normalizers' user_count.
func NormalizeVolume(v *ec2types.Volume, account, region string) *graph.Node {
	if v == nil || v.VolumeId == nil || *v.VolumeId == "" {
		return nil
	}
	id := *v.VolumeId
	n := &graph.Node{
		ID:        graph.Ref("accounts/" + account + "/regions/" + region + "/volumes/" + id),
		Kind:      graph.KindDisk,
		Name:      id,
		Provider:  "aws",
		Service:   serviceEC2,
		AssetType: assetTypeVolume,
		Project:   account,
		Location:  region,
		Attrs:     map[string]any{},
	}
	if v.AvailabilityZone != nil && *v.AvailabilityZone != "" {
		n.Location = locationRegion(*v.AvailabilityZone)
		n.SetAttr(AttrAvailabilityZone, *v.AvailabilityZone)
	}
	n.SetAttr(AttrState, string(v.State))
	if v.Size != nil {
		n.SetAttr(AttrSizeGB, float64(*v.Size))
	}
	n.SetAttr(AttrVolumeType, string(v.VolumeType))
	if v.Iops != nil {
		n.SetAttr(AttrIops, float64(*v.Iops))
	}
	if v.Throughput != nil {
		n.SetAttr(AttrThroughput, float64(*v.Throughput))
	}
	if v.CreateTime != nil {
		n.SetAttr(AttrCreateTime, v.CreateTime.UTC().Format(time.RFC3339))
	}
	// attachments and attachment_count are always written, even when the
	// volume has none, so the absence of attachments is distinguishable from
	// an unparsed payload.
	n.SetAttr(AttrAttachmentCount, float64(len(v.Attachments)))
	n.SetAttr(AttrAttachments, attachmentsOf(v.Attachments))
	return n
}

// attachmentsOf renders a volume's Attachments slice as rule-facing records
// keyed by the SDK's own VolumeAttachment field names: instance_id, device,
// state, attach_time and volume_id. Written unconditionally (empty when the
// volume has none).
func attachmentsOf(atts []ec2types.VolumeAttachment) []map[string]any {
	out := make([]map[string]any, 0, len(atts))
	for _, a := range atts {
		rec := map[string]any{"state": string(a.State)}
		if a.InstanceId != nil && *a.InstanceId != "" {
			rec["instance_id"] = *a.InstanceId
		}
		if a.Device != nil && *a.Device != "" {
			rec["device"] = *a.Device
		}
		if a.AttachTime != nil {
			rec["attach_time"] = a.AttachTime.UTC().Format(time.RFC3339)
		}
		if a.VolumeId != nil && *a.VolumeId != "" {
			rec["volume_id"] = *a.VolumeId
		}
		out = append(out, rec)
	}
	return out
}

// NormalizeAddress converts one DescribeAddresses result into a graph node. An
// address with neither a public IP nor an allocation ID (which the real API
// never returns) is dropped.
//
// Attrs, named from the EC2 SDK's own types.Address fields (see attrs.go):
// domain (Domain, the SDK's "vpc" | "standard" values verbatim), allocation_id
// (AllocationId), public_ip (PublicIp), association_id (AssociationId, present
// exactly when the address is associated), instance_id (InstanceId), and the
// derived association_state ("associated" | "unassociated") so a rule can
// decide "reserved but unused" without a metric.
func NormalizeAddress(a *ec2types.Address, account, region string) *graph.Node {
	if a == nil {
		return nil
	}
	name := ""
	if a.PublicIp != nil && *a.PublicIp != "" {
		name = *a.PublicIp
	} else if a.AllocationId != nil && *a.AllocationId != "" {
		name = *a.AllocationId
	}
	if name == "" {
		return nil
	}
	n := &graph.Node{
		ID:        graph.Ref("accounts/" + account + "/regions/" + region + "/addresses/" + name),
		Kind:      graph.KindAddress,
		Name:      name,
		Provider:  "aws",
		Service:   serviceEC2,
		AssetType: assetTypeAddress,
		Project:   account,
		Location:  region,
		Attrs:     map[string]any{},
	}
	n.SetAttr(AttrDomain, string(a.Domain))
	if a.PublicIp != nil {
		n.SetAttr(AttrPublicIP, *a.PublicIp)
	}
	if a.AllocationId != nil {
		n.SetAttr(AttrAllocationID, *a.AllocationId)
	}
	state := "unassociated"
	if a.AssociationId != nil && *a.AssociationId != "" {
		state = "associated"
		n.SetAttr(AttrAssociationID, *a.AssociationId)
	}
	n.SetAttr(AttrAssociationState, state)
	if a.InstanceId != nil && *a.InstanceId != "" {
		n.SetAttr(AttrInstanceID, *a.InstanceId)
	}
	return n
}

// NormalizeInstance converts one DescribeInstances result into a graph node.
// An instance without an InstanceId (which the real API never returns) is
// dropped. The resolved InstanceTypeInfo may be nil when DescribeInstanceTypes
// could not resolve the shape; in that case vcpu_count and memory_gib are
// absent and a rule that requires them skips the instance — never zero and
// never a guess.
//
// Attrs, named from the EC2 SDK's own types.Instance fields (see attrs.go):
//
//	instance_type    — types.Instance.InstanceType
//	state            — types.Instance.State.Name
//	launch_time      — types.Instance.LaunchTime (RFC3339)
//	platform         — types.Instance.Platform (empty string for Linux)
//	architecture     — types.Instance.Architecture
//	tenancy          — types.Instance.Placement.Tenancy
//	lifecycle        — types.Instance.InstanceLifecycle (always written;
//	                   empty string means on-demand / standard)
//	provisioning_model — derived from lifecycle: "SPOT" | "STANDARD"
//	machine_family   — derived from instance_type (prefix before ".")
//	availability_zone — types.Instance.Placement.AvailabilityZone
//	instance_id      — types.Instance.InstanceId
//	vcpu_count       — InstanceTypeInfo.VCPU (absent when shape unresolved)
//	memory_gib       — InstanceTypeInfo.MemoryGiB (absent when shape unresolved)
func NormalizeInstance(inst *ec2types.Instance, shape *InstanceTypeInfo, account, region string) *graph.Node {
	if inst == nil || inst.InstanceId == nil || *inst.InstanceId == "" {
		return nil
	}
	id := *inst.InstanceId
	n := &graph.Node{
		ID:        graph.Ref("accounts/" + account + "/regions/" + region + "/instances/" + id),
		Kind:      graph.KindInstance,
		Name:      id,
		Provider:  "aws",
		Service:   serviceEC2,
		AssetType: assetTypeInstance,
		Project:   account,
		Location:  region,
		Attrs:     map[string]any{},
	}

	// instance_id is written unconditionally so a rule can read it without
	// falling back to Name.
	n.SetAttr(AttrInstanceID, id)

	// EC2 tags become node Labels, which is where rules look for them. The
	// Auto Scaling guard depends on this: an ASG member carries the
	// aws:autoscaling:groupName tag, and without the tags reaching Labels the
	// guard reads an always-absent label, always passes, and every ASG member
	// is recommended for rightsizing an operator cannot act on.
	for _, t := range inst.Tags {
		if t.Key == nil || *t.Key == "" || t.Value == nil {
			continue
		}
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels[*t.Key] = *t.Value
	}

	// state — always written from the SDK's InstanceState.Name.
	if inst.State != nil {
		n.SetAttr(AttrState, string(inst.State.Name))
	} else {
		n.SetAttr(AttrState, "")
	}

	// instance_type — always written.
	n.SetAttr(AttrInstanceType, string(inst.InstanceType))

	// launch_time — always written when present (RFC3339).
	if inst.LaunchTime != nil {
		n.SetAttr(AttrLaunchTime, inst.LaunchTime.UTC().Format(time.RFC3339))
	}

	// platform — always written. Empty string means Linux/Unix.
	n.SetAttr(AttrPlatform, string(inst.Platform))

	// architecture — always written.
	n.SetAttr(AttrArchitecture, string(inst.Architecture))

	// tenancy — always written from Placement.Tenancy.
	// Default is "default" which means shared.
	if inst.Placement != nil {
		n.SetAttr(AttrTenancy, string(inst.Placement.Tenancy))
		if inst.Placement.AvailabilityZone != nil && *inst.Placement.AvailabilityZone != "" {
			n.SetAttr(AttrAvailabilityZone, *inst.Placement.AvailabilityZone)
			n.Location = locationRegion(*inst.Placement.AvailabilityZone)
		}
	} else {
		n.SetAttr(AttrTenancy, "")
	}

	// lifecycle — ALWAYS written, even when empty, so a rule's not-spot guard
	// can read a present value. Empty means on-demand (standard).
	n.SetAttr(AttrLifecycle, string(inst.InstanceLifecycle))

	// provisioning_model — derived from lifecycle.
	pm := ProvisioningStandard
	if string(inst.InstanceLifecycle) == LifecycleSpot {
		pm = ProvisioningSpot
	}
	n.SetAttr(AttrProvisioningModel, pm)

	// machine_family — derived from instance_type (prefix before ".").
	n.SetAttr(AttrMachineFamily, machineFamily(string(inst.InstanceType)))

	// vcpu_count and memory_gib — only written when the shape is resolved.
	if shape != nil {
		n.SetAttr(AttrVCpuCount, shape.VCPU)
		n.SetAttr(AttrMemoryGiB, shape.MemoryGiB)
	}

	return n
}

// machineFamily extracts the family prefix from an EC2 instance type.
// "t3.medium" → "t3", "m6i.xlarge" → "m6i", "c7g.large" → "c7g".
// If the instance type has no dot, the whole string is returned.
func machineFamily(instanceType string) string {
	if idx := strings.IndexByte(instanceType, '.'); idx >= 0 {
		return instanceType[:idx]
	}
	return instanceType
}

// imageReferenceSet accumulates the AMI references discovered by the EC2 and
// Auto Scaling reference passes. It deliberately stores every source label so
// a node can carry reference_sources, and a count so the rule can decide
// "referenced" without walking the source list.
type imageReferenceSet struct {
	counts   map[string]int
	sources  map[string]map[string]bool
	complete bool
}

// newImageReferenceSet returns an empty reference set whose complete flag is
// true. A failed reference API must set complete=false explicitly.
func newImageReferenceSet() imageReferenceSet {
	return imageReferenceSet{
		counts:   map[string]int{},
		sources:  map[string]map[string]bool{},
		complete: true,
	}
}

// add records one reference from source for imageID. Empty image IDs and
// empty source labels are ignored.
func (r *imageReferenceSet) add(imageID, source string) {
	if imageID == "" || source == "" {
		return
	}
	r.counts[imageID]++
	if r.sources[imageID] == nil {
		r.sources[imageID] = map[string]bool{}
	}
	r.sources[imageID][source] = true
}

// countFor returns the total reference count for an image.
func (r imageReferenceSet) countFor(imageID string) int {
	return r.counts[imageID]
}

// sourcesFor returns the distinct, sorted source labels for an image.
func (r imageReferenceSet) sourcesFor(imageID string) []string {
	set := r.sources[imageID]
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// imageSnapshotIDs extracts the EBS snapshot IDs from an image's block-device
// mappings, preserving SDK order. Instance-store mappings (no Ebs block) are
// ignored, exactly as the pricing design requires: only backing EBS snapshots
// bill.
func imageSnapshotIDs(img *ec2types.Image) []string {
	if img == nil {
		return nil
	}
	out := make([]string, 0, len(img.BlockDeviceMappings))
	for _, m := range img.BlockDeviceMappings {
		if m.Ebs == nil || m.Ebs.SnapshotId == nil || *m.Ebs.SnapshotId == "" {
			continue
		}
		out = append(out, *m.Ebs.SnapshotId)
	}
	return out
}

// NormalizeImage converts one DescribeImages result into a graph node. An
// image without an ImageId (which the real API never returns) is dropped.
//
// snapshots is the self-owned DescribeSnapshots inventory keyed by SnapshotId;
// snapshotRefCounts is the number of current self-owned AMI block-device
// mappings naming each snapshot. snapshotsComplete reports whether that
// inventory was successfully read; when false, backing_complete is false for
// every image. refs is the reference set built from the EC2 and Auto Scaling
// reference passes.
//
// Attrs, named from the EC2 SDK's own types.Image fields (see attrs.go):
//
//	image_id                    — types.Image.ImageId
//	image_name                  — types.Image.Name
//	creation_timestamp          — types.Image.CreationDate (RFC3339)
//	state                       — types.Image.State
//	root_device_type            — types.Image.RootDeviceType
//	block_device_mappings       — types.Image.BlockDeviceMappings
//	backing_snapshot_ids        — Ebs.SnapshotId for every EBS mapping
//	backing_snapshot_count      — len(backing_snapshot_ids), always written
//	backing_size_gb             — sum of backing snapshot VolumeSize, always written
//	backing_exclusive_size_gb   — sum of snapshots referenced only by this AMI, always written
//	backing_complete            — false when a backing snapshot is absent from the snapshot inventory
//	reference_count             — total AMI references, always written (0 means none)
//	reference_sources           — distinct reference source labels, always written
//	references_complete         — false when any reference API could not be read
func NormalizeImage(img *ec2types.Image, account, region string, snapshots map[string]ec2types.Snapshot, snapshotRefCounts map[string]int, refs imageReferenceSet, snapshotsComplete bool) *graph.Node {
	if img == nil || img.ImageId == nil || *img.ImageId == "" {
		return nil
	}
	id := *img.ImageId
	n := &graph.Node{
		ID:        graph.Ref("accounts/" + account + "/regions/" + region + "/images/" + id),
		Kind:      graph.KindImage,
		Name:      id,
		Provider:  "aws",
		Service:   serviceEC2,
		AssetType: assetTypeImage,
		Project:   account,
		Location:  region,
		Attrs:     map[string]any{},
	}

	n.SetAttr(AttrImageID, id)
	n.SetAttr(AttrImageName, stringValue(img.Name))
	if img.CreationDate != nil && *img.CreationDate != "" {
		n.SetAttr(AttrCreationTimestamp, *img.CreationDate)
	} else {
		n.SetAttr(AttrCreationTimestamp, "")
	}
	n.SetAttr(AttrState, string(img.State))
	n.SetAttr(AttrRootDeviceType, string(img.RootDeviceType))

	ids := imageSnapshotIDs(img)
	backingComplete := snapshotsComplete
	var backingSizeGB float64
	var exclusiveSizeGB float64
	for _, snapshotID := range ids {
		snap, ok := snapshots[snapshotID]
		if !ok {
			backingComplete = false
			continue
		}
		if snap.VolumeSize != nil {
			size := float64(*snap.VolumeSize)
			backingSizeGB += size
			if snapshotRefCounts[snapshotID] == 1 {
				exclusiveSizeGB += size
			}
		}
	}

	// The block-device mapping list and every countable attribute are written
	// unconditionally, exactly like the other normalizers: a missing key means
	// an unparsed payload, never "zero" or "unknown".
	n.SetAttr(AttrBlockDeviceMappings, blockDeviceMappingsOf(img.BlockDeviceMappings))
	n.SetAttr(AttrBackingSnapshotIDs, ids)
	n.SetAttr(AttrBackingSnapshotCount, float64(len(ids)))
	n.SetAttr(AttrBackingSizeGB, backingSizeGB)
	n.SetAttr(AttrBackingExclusiveSizeGB, exclusiveSizeGB)
	n.SetAttr(AttrBackingComplete, backingComplete)
	n.SetAttr(AttrReferenceCount, float64(refs.countFor(id)))
	n.SetAttr(AttrReferenceSources, refs.sourcesFor(id))
	n.SetAttr(AttrReferencesComplete, refs.complete)

	return n
}

// stringValue dereferences a *string, returning "" for nil.
func stringValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// blockDeviceMappingsOf renders an image's BlockDeviceMappings as rule-facing
// records keyed by the SDK's own field names. Written unconditionally (empty
// when the image has none).
func blockDeviceMappingsOf(mappings []ec2types.BlockDeviceMapping) []map[string]any {
	out := make([]map[string]any, 0, len(mappings))
	for _, m := range mappings {
		rec := map[string]any{}
		if m.DeviceName != nil {
			rec["device_name"] = *m.DeviceName
		}
		if m.Ebs != nil {
			if m.Ebs.SnapshotId != nil && *m.Ebs.SnapshotId != "" {
				rec["snapshot_id"] = *m.Ebs.SnapshotId
			}
			if m.Ebs.VolumeSize != nil {
				rec["volume_size"] = float64(*m.Ebs.VolumeSize)
			}
			rec["volume_type"] = string(m.Ebs.VolumeType)
			if m.Ebs.DeleteOnTermination != nil {
				rec["delete_on_termination"] = *m.Ebs.DeleteOnTermination
			}
			if m.Ebs.Iops != nil {
				rec["iops"] = float64(*m.Ebs.Iops)
			}
			if m.Ebs.Throughput != nil {
				rec["throughput"] = float64(*m.Ebs.Throughput)
			}
			if m.Ebs.Encrypted != nil {
				rec["encrypted"] = *m.Ebs.Encrypted
			}
		}
		out = append(out, rec)
	}
	return out
}

// NormalizeSnapshot converts one DescribeSnapshots result into a graph node.
// A snapshot without a SnapshotId (which the real API never returns) is
// dropped.
//
// Attrs, named from the EC2 SDK's own types.Snapshot fields (see attrs.go):
// snapshot_id (SnapshotId), volume_size_gb (VolumeSize, in GiB),
// creation_timestamp (StartTime, RFC3339), state (State), description
// (Description), ami_created (derived from the AWS CreateImage description
// prefix), referenced_by_ami_count (always written), and
// ami_reference_complete (false when DescribeImages could not be read).
func NormalizeSnapshot(s *ec2types.Snapshot, account, region string, referencedByAMICount int, amiReferenceComplete bool) *graph.Node {
	if s == nil || s.SnapshotId == nil || *s.SnapshotId == "" {
		return nil
	}
	id := *s.SnapshotId
	n := &graph.Node{
		ID:        graph.Ref("accounts/" + account + "/regions/" + region + "/snapshots/" + id),
		Kind:      graph.KindSnapshot,
		Name:      id,
		Provider:  "aws",
		Service:   serviceEC2,
		AssetType: assetTypeSnapshot,
		Project:   account,
		Location:  region,
		Attrs:     map[string]any{},
	}
	n.SetAttr(AttrSnapshotID, id)
	if s.VolumeSize != nil {
		n.SetAttr(AttrVolumeSizeGB, float64(*s.VolumeSize))
	}
	if s.StartTime != nil {
		n.SetAttr(AttrCreationTimestamp, s.StartTime.UTC().Format(time.RFC3339))
	}
	n.SetAttr(AttrState, string(s.State))
	n.SetAttr(AttrDescription, stringValue(s.Description))
	n.SetAttr(AttrAMICreated, strings.HasPrefix(stringValue(s.Description), amiSnapshotDescriptionPrefix))
	// Countable attributes are always written so absence is distinguishable
	// from zero, exactly as the other normalizers do.
	n.SetAttr(AttrReferencedByAMICount, float64(referencedByAMICount))
	n.SetAttr(AttrAMIReferenceComplete, amiReferenceComplete)
	return n
}
