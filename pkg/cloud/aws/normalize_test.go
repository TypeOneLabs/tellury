package aws

import (
	"testing"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

func strp(s string) *string     { return &s }
func i32p(v int32) *int32       { return &v }
func i64p(v int64) *int64       { return &v }
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

// ---------------------------------------------------------------------------
// NormalizeInstance tests
// ---------------------------------------------------------------------------

// TestNormalizeInstance_Attrs pin the exact attribute keys and values
// NormalizeInstance writes. An on-demand Linux instance with a resolved
// shape carries every attribute the rule's guards read.
func TestNormalizeInstance_Attrs(t *testing.T) {
	launched := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	inst := &ec2types.Instance{
		InstanceId:   strp("i-0cafe"),
		InstanceType: ec2types.InstanceTypeT3Medium,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		LaunchTime:   tp(launched),
		Platform:     "", // Linux
		Architecture: ec2types.ArchitectureValuesX8664,
		Placement: &ec2types.Placement{
			AvailabilityZone: strp("us-east-1b"),
			Tenancy:          ec2types.TenancyDefault,
		},
		InstanceLifecycle: "", // on-demand
	}
	shape := &InstanceTypeInfo{VCPU: 2, MemoryGiB: 4}
	n := NormalizeInstance(inst, shape, "123456789012", "us-east-1")
	if n == nil {
		t.Fatal("NormalizeInstance returned nil")
	}

	if n.Kind != graph.KindInstance || n.Provider != "aws" || n.Service != "ec2" {
		t.Errorf("kind/provider/service = %s/%s/%s, want instance/aws/ec2", n.Kind, n.Provider, n.Service)
	}
	if n.Name != "i-0cafe" {
		t.Errorf("Name = %q, want i-0cafe", n.Name)
	}
	if n.Project != "123456789012" {
		t.Errorf("Project = %q, want account id", n.Project)
	}
	if n.Location != "us-east-1" {
		t.Errorf("Location = %q, want us-east-1 (canonicalised from us-east-1b)", n.Location)
	}
	if n.AssetType != "aws.ec2.instance" {
		t.Errorf("AssetType = %q, want aws.ec2.instance", n.AssetType)
	}

	// instance_id — always written.
	if got, _ := n.Str(AttrInstanceID); got != "i-0cafe" {
		t.Errorf("instance_id = %q, want i-0cafe", got)
	}
	// state — from State.Name.
	if got, _ := n.Str(AttrState); got != "running" {
		t.Errorf("state = %q, want running", got)
	}
	// instance_type.
	if got, _ := n.Str(AttrInstanceType); got != "t3.medium" {
		t.Errorf("instance_type = %q, want t3.medium", got)
	}
	// launch_time — RFC3339.
	if got, _ := n.Str(AttrLaunchTime); got != "2024-01-15T10:30:00Z" {
		t.Errorf("launch_time = %q, want 2024-01-15T10:30:00Z", got)
	}
	// platform — always written, empty means Linux.
	if got, _ := n.Str(AttrPlatform); got != "" {
		t.Errorf("platform = %q, want empty for Linux", got)
	}
	// architecture.
	if got, _ := n.Str(AttrArchitecture); got != "x86_64" {
		t.Errorf("architecture = %q, want x86_64", got)
	}
	// tenancy.
	if got, _ := n.Str(AttrTenancy); got != "default" {
		t.Errorf("tenancy = %q, want default", got)
	}
	// availability_zone.
	if got, _ := n.Str(AttrAvailabilityZone); got != "us-east-1b" {
		t.Errorf("availability_zone = %q, want us-east-1b", got)
	}
	// lifecycle — always written, empty means on-demand.
	if got, _ := n.Str(AttrLifecycle); got != "" {
		t.Errorf("lifecycle = %q, want empty for on-demand", got)
	}
	// provisioning_model — derived.
	if got, _ := n.Str(AttrProvisioningModel); got != "STANDARD" {
		t.Errorf("provisioning_model = %q, want STANDARD", got)
	}
	// machine_family — derived.
	if got, _ := n.Str(AttrMachineFamily); got != "t3" {
		t.Errorf("machine_family = %q, want t3", got)
	}
	// vcpu_count — from shape.
	if got, _ := n.Num(AttrVCpuCount); got != 2 {
		t.Errorf("vcpu_count = %v, want 2", got)
	}
	// memory_gib — from shape.
	if got, _ := n.Num(AttrMemoryGiB); got != 4 {
		t.Errorf("memory_gib = %v, want 4", got)
	}

	// The ID format matches the stub.
	wantID := graph.Ref("accounts/123456789012/regions/us-east-1/instances/i-0cafe")
	if n.ID != wantID {
		t.Errorf("ID = %q, want %q", n.ID, wantID)
	}
}

// TestNormalizeInstance_Spot carries lifecycle "spot" and provisioning_model
// "SPOT", so a rule's not-spot guard can read the present value.
func TestNormalizeInstance_Spot(t *testing.T) {
	inst := &ec2types.Instance{
		InstanceId:        strp("i-0spot"),
		InstanceType:      ec2types.InstanceTypeT3Medium,
		State:             &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		InstanceLifecycle: ec2types.InstanceLifecycleTypeSpot,
	}
	n := NormalizeInstance(inst, nil, "123456789012", "us-east-1")
	if n == nil {
		t.Fatal("NormalizeInstance returned nil")
	}
	// lifecycle is written unconditionally.
	if got, _ := n.Str(AttrLifecycle); got != "spot" {
		t.Errorf("lifecycle = %q, want spot", got)
	}
	// provisioning_model is derived.
	if got, _ := n.Str(AttrProvisioningModel); got != "SPOT" {
		t.Errorf("provisioning_model = %q, want SPOT", got)
	}
	// Shape is nil, so vcpu_count and memory_gib are absent.
	if _, ok := n.Num(AttrVCpuCount); ok {
		t.Errorf("vcpu_count must be absent when shape is nil")
	}
	if _, ok := n.Num(AttrMemoryGiB); ok {
		t.Errorf("memory_gib must be absent when shape is nil")
	}
}

// TestNormalizeInstance_NoShape: when the shape is nil, vcpu_count and
// memory_gib are absent — never zero and never a guess.
func TestNormalizeInstance_NoShape(t *testing.T) {
	inst := &ec2types.Instance{
		InstanceId:   strp("i-0noshape"),
		InstanceType: ec2types.InstanceTypeT3Medium,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
	}
	n := NormalizeInstance(inst, nil, "123456789012", "us-east-1")
	if n == nil {
		t.Fatal("NormalizeInstance returned nil")
	}
	if _, ok := n.Num(AttrVCpuCount); ok {
		t.Errorf("vcpu_count must be absent when shape is nil")
	}
	if _, ok := n.Num(AttrMemoryGiB); ok {
		t.Errorf("memory_gib must be absent when shape is nil")
	}
	// But other attributes are still present.
	if got, _ := n.Str(AttrInstanceType); got != "t3.medium" {
		t.Errorf("instance_type = %q, want t3.medium", got)
	}
	if got, _ := n.Str(AttrInstanceID); got != "i-0noshape" {
		t.Errorf("instance_id = %q, want i-0noshape", got)
	}
}

// TestNormalizeInstance_ShapeZeroVCPU: a shape whose DefaultVCpus is nil or 0
// still writes the attribute (0.0), so a rule can tell the shape was
// resolved but reports zero — distinct from "absent".
func TestNormalizeInstance_ShapeZeroVCPU(t *testing.T) {
	inst := &ec2types.Instance{
		InstanceId:   strp("i-0zero"),
		InstanceType: ec2types.InstanceTypeT3Medium,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
	}
	// Shape with zero VCPU and zero memory — degenerate but the attribute is
	// present, so a rule knows the shape was resolved.
	shape := &InstanceTypeInfo{VCPU: 0, MemoryGiB: 0}
	n := NormalizeInstance(inst, shape, "123456789012", "us-east-1")
	if n == nil {
		t.Fatal("NormalizeInstance returned nil")
	}
	if got, _ := n.Num(AttrVCpuCount); got != 0 {
		t.Errorf("vcpu_count = %v, want 0 (shape resolved, value is zero)", got)
	}
	if got, _ := n.Num(AttrMemoryGiB); got != 0 {
		t.Errorf("memory_gib = %v, want 0 (shape resolved, value is zero)", got)
	}
}

// TestNormalizeInstance_NilAndUnidentifiedAreDropped: a nil instance or one
// with no InstanceId is dropped.
func TestNormalizeInstance_NilAndUnidentifiedAreDropped(t *testing.T) {
	if n := NormalizeInstance(nil, nil, "a", "r"); n != nil {
		t.Errorf("NormalizeInstance(nil) = %#v, want nil", n)
	}
	if n := NormalizeInstance(&ec2types.Instance{}, nil, "a", "r"); n != nil {
		t.Errorf("NormalizeInstance(no InstanceId) = %#v, want nil", n)
	}
}

// TestNormalizeInstance_NoPlacement: an instance with no Placement still gets
// tenancy "" written, and no availability_zone. The node's Location falls
// back to the region argument.
func TestNormalizeInstance_NoPlacement(t *testing.T) {
	inst := &ec2types.Instance{
		InstanceId:   strp("i-0noplace"),
		InstanceType: ec2types.InstanceTypeT3Medium,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
	}
	n := NormalizeInstance(inst, nil, "123456789012", "us-east-1")
	if n == nil {
		t.Fatal("NormalizeInstance returned nil")
	}
	if got, _ := n.Str(AttrTenancy); got != "" {
		t.Errorf("tenancy = %q, want empty for no placement", got)
	}
	if _, ok := n.Str(AttrAvailabilityZone); ok {
		t.Errorf("availability_zone must be absent when placement is nil")
	}
	if n.Location != "us-east-1" {
		t.Errorf("Location = %q, want region fallback us-east-1", n.Location)
	}
}

// TestNormalizeInstance_NoState: when State is nil, state is written as "".
func TestNormalizeInstance_NoState(t *testing.T) {
	inst := &ec2types.Instance{
		InstanceId:   strp("i-0nostate"),
		InstanceType: ec2types.InstanceTypeT3Medium,
	}
	n := NormalizeInstance(inst, nil, "123456789012", "us-east-1")
	if n == nil {
		t.Fatal("NormalizeInstance returned nil")
	}
	if got, _ := n.Str(AttrState); got != "" {
		t.Errorf("state = %q, want empty when State is nil", got)
	}
}

// TestNormalizeInstance_LifecycleAlwaysWritten: even for on-demand instances
// (empty lifecycle), the attribute is present so a rule can distinguish "not
// spot" from "payload not parsed".
func TestNormalizeInstance_LifecycleAlwaysWritten(t *testing.T) {
	inst := &ec2types.Instance{
		InstanceId:   strp("i-0ondemand"),
		InstanceType: ec2types.InstanceTypeT3Medium,
	}
	// Intentionally not setting InstanceLifecycle — on-demand.
	n := NormalizeInstance(inst, nil, "123456789012", "us-east-1")
	if n == nil {
		t.Fatal("NormalizeInstance returned nil")
	}
	// lifecycle must be present even for on-demand.
	if _, ok := n.Str(AttrLifecycle); !ok {
		t.Errorf("lifecycle must always be written, even for on-demand (empty)")
	}
	if got, _ := n.Str(AttrLifecycle); got != "" {
		t.Errorf("lifecycle = %q, want empty for on-demand", got)
	}
	if got, _ := n.Str(AttrProvisioningModel); got != "STANDARD" {
		t.Errorf("provisioning_model = %q, want STANDARD for empty lifecycle", got)
	}
}

// TestNormalizeInstance_MachineFamily extracts the prefix before the dot.
func TestNormalizeInstance_MachineFamily(t *testing.T) {
	tests := []struct {
		instanceType string
		want         string
	}{
		{"t3.medium", "t3"},
		{"m6i.xlarge", "m6i"},
		{"c7g.large", "c7g"},
		{"r6gd.16xlarge", "r6gd"},
		{"p4d.24xlarge", "p4d"},
		{"", ""},
		{"nodot", "nodot"},
	}
	for _, tt := range tests {
		got := machineFamily(tt.instanceType)
		if got != tt.want {
			t.Errorf("machineFamily(%q) = %q, want %q", tt.instanceType, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Instance stub + enriched node reconciliation
// ---------------------------------------------------------------------------

// TestInstanceNode_StubThenEnriched: when a stub is added to the graph first
// (as happens when an EBS volume references the instance) and then the
// enriched instance replaces it, the final node carries every attribute. This
// proves that "last write wins" preserves all attributes when instances are
// processed after volumes.
func TestInstanceNode_StubThenEnriched(t *testing.T) {
	g := graph.New()
	account := "123456789012"
	region := "us-east-1"

	// Step 1: add the stub (as the EBS attachment path does).
	stub := instanceNode("i-0cafe", account, region)
	if err := g.AddNode(stub); err != nil {
		t.Fatalf("add stub: %v", err)
	}

	// Step 2: add the enriched instance (as DescribeInstances does).
	launched := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	inst := &ec2types.Instance{
		InstanceId:   strp("i-0cafe"),
		InstanceType: ec2types.InstanceTypeT3Medium,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		LaunchTime:   tp(launched),
		Architecture: ec2types.ArchitectureValuesX8664,
		Placement: &ec2types.Placement{
			AvailabilityZone: strp("us-east-1b"),
			Tenancy:          ec2types.TenancyDefault,
		},
	}
	shape := &InstanceTypeInfo{VCPU: 2, MemoryGiB: 4}
	enriched := NormalizeInstance(inst, shape, account, region)
	if err := g.AddNode(enriched); err != nil {
		t.Fatalf("add enriched: %v", err)
	}

	// Read the node back and verify all attributes are present.
	n, ok := g.Node(graph.Ref("accounts/123456789012/regions/us-east-1/instances/i-0cafe"))
	if !ok {
		t.Fatal("node not found in graph")
	}
	if n.Kind != graph.KindInstance {
		t.Errorf("Kind = %s, want instance", n.Kind)
	}
	if got, _ := n.Str(AttrInstanceID); got != "i-0cafe" {
		t.Errorf("instance_id = %q, want i-0cafe", got)
	}
	if got, _ := n.Str(AttrInstanceType); got != "t3.medium" {
		t.Errorf("instance_type = %q, want t3.medium", got)
	}
	if got, _ := n.Str(AttrState); got != "running" {
		t.Errorf("state = %q, want running", got)
	}
	if got, _ := n.Num(AttrVCpuCount); got != 2 {
		t.Errorf("vcpu_count = %v, want 2", got)
	}
	if got, _ := n.Num(AttrMemoryGiB); got != 4 {
		t.Errorf("memory_gib = %v, want 4", got)
	}
	if got, _ := n.Str(AttrLifecycle); got != "" {
		t.Errorf("lifecycle = %q, want empty (on-demand)", got)
	}
	if got, _ := n.Str(AttrProvisioningModel); got != "STANDARD" {
		t.Errorf("provisioning_model = %q, want STANDARD", got)
	}
}

// TestInstanceNode_EnrichedThenStub: if the enriched instance arrives first and
// a stub arrives second (the reverse of the guaranteed ingestion order), the
// stub would overwrite and drop all attributes. This test documents the
// requirement that instances MUST be processed after volumes, and proves the
// problem exists if the order is reversed.
func TestInstanceNode_EnrichedThenStub(t *testing.T) {
	g := graph.New()
	account := "123456789012"
	region := "us-east-1"

	// Step 1: add the enriched instance first (wrong order).
	launched := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	inst := &ec2types.Instance{
		InstanceId:   strp("i-0cafe"),
		InstanceType: ec2types.InstanceTypeT3Medium,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		LaunchTime:   tp(launched),
		Architecture: ec2types.ArchitectureValuesX8664,
		Placement: &ec2types.Placement{
			AvailabilityZone: strp("us-east-1b"),
			Tenancy:          ec2types.TenancyDefault,
		},
	}
	shape := &InstanceTypeInfo{VCPU: 2, MemoryGiB: 4}
	enriched := NormalizeInstance(inst, shape, account, region)
	if err := g.AddNode(enriched); err != nil {
		t.Fatalf("add enriched: %v", err)
	}

	// Step 2: add the stub (wrong order — this would overwrite the enriched node).
	stub := instanceNode("i-0cafe", account, region)
	if err := g.AddNode(stub); err != nil {
		t.Fatalf("add stub: %v", err)
	}

	n, ok := g.Node(graph.Ref("accounts/123456789012/regions/us-east-1/instances/i-0cafe"))
	if !ok {
		t.Fatal("node not found in graph")
	}

	// The stub has no attributes — prove that the enriched attributes are LOST
	// when the stub is the last write. This test exists to document why the
	// ingestion loop must always process instances AFTER volumes.
	if _, ok := n.Str(AttrInstanceType); ok {
		t.Errorf("instance_type should be absent after stub overwrites enriched node; got %q", n.Attrs[AttrInstanceType])
	}
	if _, ok := n.Num(AttrVCpuCount); ok {
		t.Errorf("vcpu_count should be absent after stub overwrites enriched node")
	}

	// Re-apply enriched to recover — proves the fix is to process instances last.
	if err := g.AddNode(NormalizeInstance(inst, shape, account, region)); err != nil {
		t.Fatalf("re-add enriched: %v", err)
	}
	n2, _ := g.Node(graph.Ref("accounts/123456789012/regions/us-east-1/instances/i-0cafe"))
	if got, _ := n2.Str(AttrInstanceType); got != "t3.medium" {
		t.Errorf("after re-adding enriched: instance_type = %q, want t3.medium", got)
	}
}

// TestNormalizeInstance_TagsBecomeLabels pins the wiring the Auto Scaling
// guard depends on. The guard reads Node.Label("aws:autoscaling:groupName");
// before this, NormalizeInstance never wrote Tags to Labels at all, so the
// label was always absent, the guard always passed, and every ASG member was
// recommended for rightsizing an operator cannot act on. The rule's own test
// passed only because it set Labels by hand — something the pipeline never did.
func TestNormalizeInstance_TagsBecomeLabels(t *testing.T) {
	inst := &ec2types.Instance{
		InstanceId:   strp("i-abc"),
		InstanceType: ec2types.InstanceTypeT3Micro,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Tags: []ec2types.Tag{
			{Key: strp("aws:autoscaling:groupName"), Value: strp("web-asg")},
			{Key: strp("Name"), Value: strp("web-1")},
			{Key: nil, Value: strp("dropped")},
		},
	}
	n := NormalizeInstance(inst, nil, "111122223333", "eu-west-1")
	if n == nil {
		t.Fatal("NormalizeInstance returned nil")
	}
	if got := n.Labels["aws:autoscaling:groupName"]; got != "web-asg" {
		t.Errorf("ASG label = %q, want %q — the Auto Scaling guard is dead without it", got, "web-asg")
	}
	if got := n.Labels["Name"]; got != "web-1" {
		t.Errorf("Name label = %q, want %q", got, "web-1")
	}
	if len(n.Labels) != 2 {
		t.Errorf("labels = %v, want exactly the two well-formed tags", n.Labels)
	}
}
