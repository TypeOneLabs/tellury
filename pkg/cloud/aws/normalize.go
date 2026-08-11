package aws

import (
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// Normalizers: pure functions that convert one EC2 Describe result into a
// graph node. They are unit-testable without any client or credentials, and
// their Attrs keys are the EC2 SDK's own field names (see attrs.go), so a rule
// reads exactly what DescribeVolumes / DescribeAddresses documented.

// Service and asset-type tokens. AWS has no Cloud Asset Inventory, so the
// asset-type token is the provider's own stable "aws.<service>.<resource>"
// spelling; the service token is the AWS service name.
const (
	serviceEC2        = "ec2"
	assetTypeVolume   = "aws.ec2.volume"
	assetTypeAddress  = "aws.ec2.address"
	assetTypeInstance = "aws.ec2.instance"
)

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
// Attachments list says so — a future detached-volume rule can check either
// the node's attachment_count or the graph's attached_to edges. A later
// DescribeInstances step can enrich these nodes; today they carry no rule
// attributes.
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
