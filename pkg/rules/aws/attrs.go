// Package awsrules holds the attribute-key constants shared by the AWS rule
// packages. They are re-exported from pkg/cloud/aws so that a rule never
// imports a cloud client (and therefore never pulls the AWS SDK into a unit
// test) while still agreeing with the normalizer on a single spelling — the
// same split pkg/rules/gcp gives the GCP side.
package awsrules

// Volume attributes written by pkg/cloud/aws/normalize.go. Every name is the
// EC2 SDK's own field name (types.Volume), snake_cased to the shared Attrs
// convention — never invented: State, Size, VolumeType, Iops, Throughput,
// CreateTime and Attachments. A rule therefore reads exactly what
// DescribeVolumes documented, and can trace every attribute back to the EC2
// API reference without a translation table.
const (
	AttrState            = "state"             // types.Volume.State
	AttrSizeGB           = "size_gb"           // types.Volume.Size (GiB)
	AttrVolumeType       = "volume_type"       // types.Volume.VolumeType
	AttrIops             = "iops"              // types.Volume.Iops (provisioned IOPS)
	AttrThroughput       = "throughput"        // types.Volume.Throughput (MiB/s)
	AttrCreateTime       = "create_time"       // types.Volume.CreateTime (RFC3339)
	AttrAttachments      = "attachments"       // types.Volume.Attachments (per-attachment records)
	AttrAttachmentCount  = "attachment_count"  // len(types.Volume.Attachments), always written
	AttrAvailabilityZone = "availability_zone" // types.Volume.AvailabilityZone
)

// Address attributes written by pkg/cloud/aws/normalize.go, named from the
// EC2 SDK's own types.Address fields.
const (
	AttrDomain           = "domain"            // types.Address.Domain ("vpc" | "standard")
	AttrAssociationState = "association_state" // derived: "associated" | "unassociated"
	AttrAssociationID    = "association_id"    // types.Address.AssociationId, present exactly when associated
	AttrAllocationID     = "allocation_id"     // types.Address.AllocationId
	AttrPublicIP         = "public_ip"         // types.Address.PublicIp
	AttrInstanceID       = "instance_id"       // types.Address.InstanceId
)

// Instance attributes written by pkg/cloud/aws/normalize.go
// (NormalizeInstance), named from the EC2 SDK's own types.Instance fields
// and types.InstanceTypeInfo. A rule reads exactly what DescribeInstances
// and DescribeInstanceTypes documented.
const (
	AttrInstanceType      = "instance_type"      // types.Instance.InstanceType
	AttrLaunchTime        = "launch_time"        // types.Instance.LaunchTime (RFC3339)
	AttrPlatform          = "platform"           // types.Instance.Platform
	AttrArchitecture      = "architecture"       // types.Instance.Architecture
	AttrTenancy           = "tenancy"            // types.Instance.Placement.Tenancy
	AttrLifecycle         = "lifecycle"          // types.Instance.InstanceLifecycle
	AttrVCpuCount         = "vcpu_count"         // types.InstanceTypeInfo.VCpuInfo.DefaultVCpus
	AttrMemoryGiB         = "memory_gib"         // types.InstanceTypeInfo.MemoryInfo.SizeInMiB / 1024
	AttrMachineFamily     = "machine_family"     // derived from instance_type (prefix before ".")
	AttrProvisioningModel = "provisioning_model" // derived from lifecycle: "SPOT" | "STANDARD"
)

// Image attributes written by pkg/cloud/aws/normalize.go (NormalizeImage),
// named from the EC2 SDK's own types.Image fields plus the backing-snapshot
// and reference derivations the unused_ami rule prices from.
const (
	AttrImageID                = "image_id"                  // types.Image.ImageId
	AttrImageName              = "image_name"                // types.Image.Name
	AttrCreationTimestamp      = "creation_timestamp"        // types.Image.CreationDate (RFC3339)
	AttrRootDeviceType         = "root_device_type"          // types.Image.RootDeviceType
	AttrBlockDeviceMappings    = "block_device_mappings"     // types.Image.BlockDeviceMappings
	AttrBackingSnapshotIDs     = "backing_snapshot_ids"      // derived: Ebs.SnapshotId for every EBS mapping
	AttrBackingSnapshotCount   = "backing_snapshot_count"    // len(backing_snapshot_ids), always written
	AttrBackingSizeGB          = "backing_size_gb"           // sum of backing snapshot VolumeSize, always written
	AttrBackingExclusiveSizeGB = "backing_exclusive_size_gb" // sum of snapshots referenced only by this AMI, always written
	AttrBackingComplete        = "backing_complete"          // false when a backing snapshot is absent from DescribeSnapshots
	AttrReferenceCount         = "reference_count"           // total AMI references, always written (0 means none)
	AttrReferenceSources       = "reference_sources"         // distinct reference source labels, always written
	AttrReferencesComplete     = "references_complete"       // false when any reference API could not be read
)

// Snapshot attributes written by pkg/cloud/aws/normalize.go
// (NormalizeSnapshot), named from the EC2 SDK's own types.Snapshot fields.
const (
	AttrSnapshotID           = "snapshot_id"             // types.Snapshot.SnapshotId
	AttrVolumeSizeGB         = "volume_size_gb"          // types.Snapshot.VolumeSize (GiB)
	AttrDescription          = "description"             // types.Snapshot.Description
	AttrAMICreated           = "ami_created"             // derived: Description starts with "Created by CreateImage("
	AttrReferencedByAMICount = "referenced_by_ami_count" // count of current AMI block-device mappings naming this snapshot
	AttrAMIReferenceComplete = "ami_reference_complete"  // false when DescribeImages could not be read
)

// Volume states: the ec2types.VolumeState values verbatim. DescribeVolumes
// returns exactly these strings; the unattached_ebs_volume rule compares
// against them.
const (
	StateAvailable = "available"
	StateInUse     = "in-use"
	StateCreating  = "creating"
	StateDeleting  = "deleting"
	StateDeleted   = "deleted"
	StateError     = "error"
)

// Image and snapshot states. They intentionally do not reuse the volume-state
// constants above: even where the literal string happens to match, the type
// is named after the resource it came from so a reader can trace it back to
// DescribeImages / DescribeSnapshots without a translation table.
const (
	ImageStateAvailable    = "available"
	ImageStatePending      = "pending"
	ImageStateFailed       = "failed"
	SnapshotStateCompleted = "completed"
	SnapshotStatePending   = "pending"
	SnapshotStateError     = "error"
)

// Address domains: the ec2types.DomainType values verbatim. Every Elastic IP
// is one of these; anything else DescribeAddresses could ever return is not
// an address this rule prices.
const (
	DomainVpc      = "vpc"
	DomainStandard = "standard"
)

// Instance lifecycle values from types.InstanceLifecycleType.
const (
	LifecycleSpot      = "spot"
	LifecycleScheduled = "scheduled"
)

// Provisioning model values derived from lifecycle.
const (
	ProvisioningStandard = "STANDARD"
	ProvisioningSpot     = "SPOT"
)

// Asset-type tokens the AWS rules declare in Meta.RequiredAssetTypes: the
// provider's own stable "aws.<service>.<resource>" spelling (AWS has no Cloud
// Asset Inventory, so there is no CAI type to reuse).
const (
	TypeVolume   = "aws.ec2.volume"
	TypeAddress  = "aws.ec2.address"
	TypeInstance = "aws.ec2.instance"
	TypeImage    = "aws.ec2.image"
	TypeSnapshot = "aws.ec2.snapshot"
)
