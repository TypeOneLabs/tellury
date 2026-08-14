// Package gcprules holds the attribute-key constants shared by the GCP rule
// packages. They are re-exported from pkg/cloud/gcp so that a rule never
// imports a cloud client (and therefore never pulls the GCP SDK into a unit
// test) while still agreeing with the normalizer on a single spelling.
package gcprules

// Attribute keys written by pkg/cloud/gcp/normalize.go.
const (
	AttrStatus                = "status"
	AttrResourceID            = "resource_id"
	AttrSizeGB                = "size_gb"
	AttrDiskType              = "disk_type"
	AttrDiskSKU               = "disk_sku"
	AttrUserCount             = "user_count"
	AttrReplicaZoneCount      = "replica_zone_count"
	AttrProvisionedIOPS       = "provisioned_iops"
	AttrProvisionedThroughput = "provisioned_throughput_mbps"
	AttrArchitecture          = "architecture"

	// Snapshot sizes. storage_bytes is the incremental, deduplicated size
	// Google bills against; source_disk_size_gb is the source disk's size at
	// snapshot time, which the console displays but nothing should price.
	AttrStorageBytes     = "storage_bytes"
	AttrSourceDiskSizeGB = "source_disk_size_gb"

	AttrInstanceID        = "instance_id"
	AttrMachineType       = "machine_type"
	AttrMachineFamily     = "machine_family"
	AttrMachineSpecSource = "machine_spec_source"
	AttrVCPUCount         = "vcpu_count"
	AttrMemoryGiB         = "memory_gib"
	AttrProvisioningModel = "provisioning_model"
	AttrPreemptible       = "preemptible"
	AttrAcceleratorCount  = "accelerator_count"
	AttrMinCPUPlatform    = "min_cpu_platform"
	AttrCreatedBy         = "created_by"
	AttrLastStartTime     = "last_start_time"

	AttrBucketName        = "bucket_name"
	AttrStorageClass      = "storage_class"
	AttrLocationType      = "location_type"
	AttrLifecycleRuleCnt  = "lifecycle_rule_count"
	AttrLifecycleActions  = "lifecycle_actions"
	AttrVersioning        = "versioning_enabled"
	AttrAutoclass         = "autoclass_enabled"
	AttrRetentionSeconds  = "retention_seconds"
	AttrRetentionLocked   = "retention_locked"
	AttrSoftDeleteSeconds = "soft_delete_seconds"

	AttrAddrType      = "address_type"
	AttrAddrPurpose   = "address_purpose"
	AttrAddrIP        = "address_ip"
	AttrAddrUserCount = "address_user_count"

	AttrCreationTime   = "creation_timestamp"
	AttrLastAttachTime = "last_attach_time"
	AttrLastDetachTime = "last_detach_time"

	// Custom image and machine image attributes. The sizes follow the same
	// stored-bytes contract as snapshots: storage_bytes is the billable size
	// and source_disk_size_gb (custom images only) is evidence, never a price.
	AttrImageID            = "image_id"
	AttrMachineImageID     = "machine_image_id"
	AttrFamily             = "family"
	AttrStorageLocation    = "storage_location"
	AttrReferenceCount     = "reference_count"
	AttrReferenceSources   = "reference_sources"
	AttrReferencesComplete = "references_complete"
)

// Provisioning models.
const (
	ModelStandard = "STANDARD"
	ModelSpot     = "SPOT"
)

// Instance statuses.
const (
	StatusRunning = "RUNNING"
)

// Address types.
const (
	AddressTypeExternal = "EXTERNAL"
	AddressTypeInternal = "INTERNAL"
)

// StatusReady is the one billable status for custom images and machine
// images: READY. Other statuses are transitional or failed and accrue no
// stable storage charge worth reporting.
const StatusReady = "READY"

// CAI asset types required by the GCP rules.
const (
	TypeInstance     = "compute.googleapis.com/Instance"
	TypeDisk         = "compute.googleapis.com/Disk"
	TypeSnapshot     = "compute.googleapis.com/Snapshot"
	TypeAddress      = "compute.googleapis.com/Address"
	TypeNetwork      = "compute.googleapis.com/Network"
	TypeBucket       = "storage.googleapis.com/Bucket"
	TypeImage        = "compute.googleapis.com/Image"
	TypeMachineImage = "compute.googleapis.com/MachineImage"
)
