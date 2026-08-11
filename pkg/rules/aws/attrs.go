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

// Address domains: the ec2types.DomainType values verbatim. Every Elastic IP
// is one of these; anything else DescribeAddresses could ever return is not
// an address this rule prices.
const (
	DomainVpc      = "vpc"
	DomainStandard = "standard"
)

// Asset-type tokens the AWS rules declare in Meta.RequiredAssetTypes: the
// provider's own stable "aws.<service>.<resource>" spelling (AWS has no Cloud
// Asset Inventory, so there is no CAI type to reuse).
const (
	TypeVolume   = "aws.ec2.volume"
	TypeAddress  = "aws.ec2.address"
	TypeInstance = "aws.ec2.instance"
)
