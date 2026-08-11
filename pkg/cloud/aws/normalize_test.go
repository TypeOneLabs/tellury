package aws

import (
	"testing"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

func strp(s string) *string     { return &s }
func i32p(v int32) *int32       { return &v }
func tp(t time.Time) *time.Time { return &t }

// TestNormalizeVolume_Attrs pin the exact attribute keys and values the
// normalizer writes, so a rule can rely on them. The keys are the EC2 SDK's
// own types.Volume field names (snake_cased): State -> state, Size -> size_gb,
// VolumeType -> volume_type, Iops -> iops, Throughput -> throughput,
// CreateTime -> create_time, Attachments -> attachments/attachment_count.
func TestNormalizeVolume_Attrs(t *testing.T) {
	created := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	v := &ec2types.Volume{
		VolumeId:         strp("vol-0abc"),
		AvailabilityZone: strp("us-east-1a"),
		Size:             i32p(100),
		VolumeType:       ec2types.VolumeTypeGp3,
		Iops:             i32p(3000),
		Throughput:       i32p(250),
		State:            ec2types.VolumeStateAvailable,
		CreateTime:       tp(created),
		Attachments: []ec2types.VolumeAttachment{{
			InstanceId: strp("i-0cafe"),
			Device:     strp("/dev/xvda"),
			State:      ec2types.VolumeAttachmentStateAttached,
			AttachTime: tp(created.Add(time.Hour)),
			VolumeId:   strp("vol-0abc"),
		}},
	}
	n := NormalizeVolume(v, "123456789012", "us-east-1")
	if n == nil {
		t.Fatal("NormalizeVolume returned nil")
	}
	if n.Kind != graph.KindDisk || n.Provider != "aws" || n.Service != "ec2" {
		t.Errorf("node kind/provider/service = %s/%s/%s, want disk/aws/ec2", n.Kind, n.Provider, n.Service)
	}
	if n.Name != "vol-0abc" {
		t.Errorf("Name = %q, want vol-0abc", n.Name)
	}
	if n.Project != "123456789012" {
		t.Errorf("Project = %q, want the account id", n.Project)
	}
	// The availability-zone form is canonicalised to the region via the single
	// pricing.CanonicalRegion wrapper — the same answer the pricer resolves.
	if n.Location != "us-east-1" {
		t.Errorf("Location = %q, want us-east-1 (canonicalised from us-east-1a)", n.Location)
	}
	if got, _ := n.Str(AttrState); got != "available" {
		t.Errorf("state = %q, want available (types.Volume.State)", got)
	}
	if got, _ := n.Num(AttrSizeGB); got != 100 {
		t.Errorf("size_gb = %v, want 100 (types.Volume.Size)", got)
	}
	if got, _ := n.Str(AttrVolumeType); got != "gp3" {
		t.Errorf("volume_type = %q, want gp3 (types.Volume.VolumeType)", got)
	}
	if got, _ := n.Num(AttrIops); got != 3000 {
		t.Errorf("iops = %v, want 3000 (types.Volume.Iops)", got)
	}
	if got, _ := n.Num(AttrThroughput); got != 250 {
		t.Errorf("throughput = %v, want 250 (types.Volume.Throughput)", got)
	}
	if got, _ := n.Str(AttrCreateTime); got != "2024-01-15T10:30:00Z" {
		t.Errorf("create_time = %q, want 2024-01-15T10:30:00Z (types.Volume.CreateTime)", got)
	}
	if got, _ := n.Str(AttrAvailabilityZone); got != "us-east-1a" {
		t.Errorf("availability_zone = %q, want us-east-1a (types.Volume.AvailabilityZone)", got)
	}
	if got, _ := n.Num(AttrAttachmentCount); got != 1 {
		t.Errorf("attachment_count = %v, want 1", got)
	}
	atts, ok := n.Attrs[AttrAttachments].([]map[string]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("attachments = %#v, want one attachment record", n.Attrs[AttrAttachments])
	}
	if atts[0]["instance_id"] != "i-0cafe" || atts[0]["device"] != "/dev/xvda" || atts[0]["state"] != "attached" {
		t.Errorf("attachment record = %#v, want instance_id i-0cafe / device /dev/xvda / state attached", atts[0])
	}
}

// TestNormalizeVolume_AttachmentCountAlwaysWritten: a volume with no
// attachments writes attachment_count 0 and an empty attachments list, so a
// rule can distinguish "no attachments" from "payload not parsed".
func TestNormalizeVolume_AttachmentCountAlwaysWritten(t *testing.T) {
	v := &ec2types.Volume{VolumeId: strp("vol-0detached"), Size: i32p(10), VolumeType: ec2types.VolumeTypeGp2, State: ec2types.VolumeStateAvailable}
	n := NormalizeVolume(v, "123456789012", "us-east-1")
	if n == nil {
		t.Fatal("NormalizeVolume returned nil")
	}
	if got, _ := n.Num(AttrAttachmentCount); got != 0 {
		t.Errorf("attachment_count = %v, want 0 for a detached volume", got)
	}
	if _, ok := n.Attrs[AttrAttachments]; !ok {
		t.Errorf("attachments key must always be written")
	}
	if got, _ := n.Num(AttrSizeGB); got != 10 {
		t.Errorf("size_gb = %v, want 10", got)
	}
}

// TestNormalizeVolume_NilAndUnidentifiedAreDropped: a nil volume or one with
// no VolumeId is dropped, never turned into an empty node.
func TestNormalizeVolume_NilAndUnidentifiedAreDropped(t *testing.T) {
	if n := NormalizeVolume(nil, "a", "r"); n != nil {
		t.Errorf("NormalizeVolume(nil) = %#v, want nil", n)
	}
	if n := NormalizeVolume(&ec2types.Volume{}, "a", "r"); n != nil {
		t.Errorf("NormalizeVolume(no VolumeId) = %#v, want nil", n)
	}
}

// TestNormalizeAddress_Attrs pin the address normalizer's attribute keys: the
// SDK's Domain field verbatim (domain), AllocationId (allocation_id), PublicIp
// (public_ip), AssociationId (association_id), InstanceId (instance_id), and
// the derived association_state.
func TestNormalizeAddress_Attrs(t *testing.T) {
	a := &ec2types.Address{
		AllocationId:  strp("eipalloc-0d2"),
		PublicIp:      strp("203.0.113.11"),
		Domain:        ec2types.DomainTypeVpc,
		AssociationId: strp("eipassoc-0e1"),
		InstanceId:    strp("i-0cafe"),
	}
	n := NormalizeAddress(a, "123456789012", "us-east-1")
	if n == nil {
		t.Fatal("NormalizeAddress returned nil")
	}
	if n.Kind != graph.KindAddress {
		t.Errorf("Kind = %s, want address", n.Kind)
	}
	if n.Name != "203.0.113.11" {
		t.Errorf("Name = %q, want the public IP", n.Name)
	}
	if got, _ := n.Str(AttrDomain); got != "vpc" {
		t.Errorf("domain = %q, want vpc (types.Address.Domain)", got)
	}
	if got, _ := n.Str(AttrAllocationID); got != "eipalloc-0d2" {
		t.Errorf("allocation_id = %q, want eipalloc-0d2 (types.Address.AllocationId)", got)
	}
	if got, _ := n.Str(AttrPublicIP); got != "203.0.113.11" {
		t.Errorf("public_ip = %q, want 203.0.113.11 (types.Address.PublicIp)", got)
	}
	if got, _ := n.Str(AttrAssociationState); got != "associated" {
		t.Errorf("association_state = %q, want associated (derived from types.Address.AssociationId)", got)
	}
	if got, _ := n.Str(AttrAssociationID); got != "eipassoc-0e1" {
		t.Errorf("association_id = %q, want eipassoc-0e1 (types.Address.AssociationId)", got)
	}
	if got, _ := n.Str(AttrInstanceID); got != "i-0cafe" {
		t.Errorf("instance_id = %q, want i-0cafe (types.Address.InstanceId)", got)
	}
}

// TestNormalizeAddress_Unassociated: an address with no AssociationId carries
// association_state "unassociated" and no association_id.
func TestNormalizeAddress_Unassociated(t *testing.T) {
	a := &ec2types.Address{
		AllocationId: strp("eipalloc-0d1"),
		PublicIp:     strp("203.0.113.10"),
		Domain:       ec2types.DomainTypeStandard,
	}
	n := NormalizeAddress(a, "123456789012", "us-east-1")
	if n == nil {
		t.Fatal("NormalizeAddress returned nil")
	}
	if got, _ := n.Str(AttrAssociationState); got != "unassociated" {
		t.Errorf("association_state = %q, want unassociated", got)
	}
	if _, ok := n.Str(AttrAssociationID); ok {
		t.Errorf("association_id must be absent for an unassociated address")
	}
	if got, _ := n.Str(AttrDomain); got != "standard" {
		t.Errorf("domain = %q, want standard", got)
	}
}

// TestNormalizeAddress_NilIsDropped: a nil address is dropped.
func TestNormalizeAddress_NilIsDropped(t *testing.T) {
	if n := NormalizeAddress(nil, "a", "r"); n != nil {
		t.Errorf("NormalizeAddress(nil) = %#v, want nil", n)
	}
}
