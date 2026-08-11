package aws

import awsrules "github.com/TypeOneLabs/tellury/pkg/rules/aws"

// Attribute keys written by the AWS normalizers (pkg/cloud/aws/normalize.go)
// and read by the AWS rules (pkg/rules/aws/ec2/*). Single source of truth:
// the constants are defined in pkg/rules/aws and re-exported here, so the
// rule packages import the lightweight constants package and never pull the
// AWS SDK into a unit test, exactly as GCP rules import their attribute keys
// from pkg/rules/gcp. A typo cannot silently split the two halves of the
// contract.
const (
	// Volume attributes (EC2 DescribeVolumes / types.Volume).
	AttrState            = awsrules.AttrState            // types.Volume.State
	AttrSizeGB           = awsrules.AttrSizeGB           // types.Volume.Size (GiB)
	AttrVolumeType       = awsrules.AttrVolumeType       // types.Volume.VolumeType
	AttrIops             = awsrules.AttrIops             // types.Volume.Iops (provisioned IOPS)
	AttrThroughput       = awsrules.AttrThroughput       // types.Volume.Throughput (MiB/s)
	AttrCreateTime       = awsrules.AttrCreateTime       // types.Volume.CreateTime (RFC3339)
	AttrAttachments      = awsrules.AttrAttachments      // types.Volume.Attachments (per-attachment records)
	AttrAttachmentCount  = awsrules.AttrAttachmentCount  // len(types.Volume.Attachments), always written
	AttrAvailabilityZone = awsrules.AttrAvailabilityZone // types.Volume.AvailabilityZone

	// Address attributes (EC2 DescribeAddresses / types.Address).
	AttrDomain           = awsrules.AttrDomain           // types.Address.Domain
	AttrAssociationState = awsrules.AttrAssociationState // derived: "associated" | "unassociated"
	AttrAssociationID    = awsrules.AttrAssociationID    // types.Address.AssociationId
	AttrAllocationID     = awsrules.AttrAllocationID     // types.Address.AllocationId
	AttrPublicIP         = awsrules.AttrPublicIP         // types.Address.PublicIp
	AttrInstanceID       = awsrules.AttrInstanceID       // types.Address.InstanceId
)

// Volume states (ec2types.VolumeState values verbatim) and address domains
// (ec2types.DomainType values verbatim), re-exported for the rules.
const (
	StateAvailable = awsrules.StateAvailable
	StateInUse     = awsrules.StateInUse
	StateCreating  = awsrules.StateCreating
	StateDeleting  = awsrules.StateDeleting
	StateDeleted   = awsrules.StateDeleted
	StateError     = awsrules.StateError

	DomainVpc      = awsrules.DomainVpc
	DomainStandard = awsrules.DomainStandard
)

// Asset-type tokens (provider's own "aws.<service>.<resource>" spelling).
const (
	TypeVolume   = awsrules.TypeVolume
	TypeAddress  = awsrules.TypeAddress
	TypeInstance = awsrules.TypeInstance
)
