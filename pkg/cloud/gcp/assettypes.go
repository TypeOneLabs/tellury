// Package gcp is the Google Cloud implementation of cloud.Provider: Cloud
// Asset Inventory ingestion, edge extraction and Cloud Monitoring enrichment.
//
// Everything provider-specific stops here. Rules see only pkg/graph.
package gcp

import (
	"sort"

	gcprules "github.com/TypeOneLabs/tellury/pkg/rules/gcp"
)

// CAI asset types the normalizers and linkers model. These are the forward
// contract the ingestion layer honours: an asset type NOT listed here is
// never normalized to a node, even if some rule mentions it. A rule that
// needs an asset type must declare it in Meta().RequiredAssetTypes; the scan
// planner (rules.Plan) unions those declarations into the exact server-side
// filter pushed to SearchAllResources. The constants here are the mapping
// from a declared type to a normalizer/edge extractor, NOT a hardcoded
// ingestion list — the scan filter is derived from the rules, so adding a
// rule for a new asset type never requires touching this file's filter.
const (
	TypeInstance     = gcprules.TypeInstance
	TypeDisk         = gcprules.TypeDisk
	TypeSnapshot     = gcprules.TypeSnapshot
	TypeAddress      = gcprules.TypeAddress
	TypeNetwork      = gcprules.TypeNetwork
	TypeBucket       = gcprules.TypeBucket
	TypeImage        = gcprules.TypeImage
	TypeMachineImage = gcprules.TypeMachineImage
)

// SupportedAssetTypes is the default server-side filter, used only when the
// rule plan is empty (a scan with zero data requirements). Normal scans derive
// the filter from the SHIPPED RULES via rules.Plan; this is a last-resort
// fallback so an empty plan still asks for the modelable set rather than every
// asset type in the scope.
var SupportedAssetTypes = []string{
	TypeInstance, TypeDisk, TypeSnapshot, TypeAddress, TypeNetwork, TypeBucket,
	TypeImage, TypeMachineImage,
}

// Attr keys written by normalize.go and read by rules. Single source of truth:
// the rule packages import the same constants from pkg/rules/gcp, so a typo
// cannot silently split the two halves of the contract.
const (
	AttrStatus                = gcprules.AttrStatus
	AttrResourceID            = gcprules.AttrResourceID
	AttrSizeGB                = gcprules.AttrSizeGB
	AttrStorageBytes          = gcprules.AttrStorageBytes
	AttrSourceDiskSizeGB      = gcprules.AttrSourceDiskSizeGB
	AttrDiskType              = gcprules.AttrDiskType
	AttrDiskSKU               = gcprules.AttrDiskSKU
	AttrUserCount             = gcprules.AttrUserCount
	AttrReplicaZoneCount      = gcprules.AttrReplicaZoneCount
	AttrProvisionedIOPS       = gcprules.AttrProvisionedIOPS
	AttrProvisionedThroughput = gcprules.AttrProvisionedThroughput
	AttrArchitecture          = gcprules.AttrArchitecture

	AttrInstanceID        = gcprules.AttrInstanceID
	AttrMachineType       = gcprules.AttrMachineType
	AttrMachineFamily     = gcprules.AttrMachineFamily
	AttrMachineSpecSource = gcprules.AttrMachineSpecSource
	AttrVCPUCount         = gcprules.AttrVCPUCount
	AttrMemoryGiB         = gcprules.AttrMemoryGiB
	AttrProvisioningModel = gcprules.AttrProvisioningModel
	AttrPreemptible       = gcprules.AttrPreemptible
	AttrAcceleratorCount  = gcprules.AttrAcceleratorCount
	AttrMinCPUPlatform    = gcprules.AttrMinCPUPlatform
	AttrCreatedBy         = gcprules.AttrCreatedBy
	AttrLastStartTime     = gcprules.AttrLastStartTime

	AttrBucketName        = gcprules.AttrBucketName
	AttrStorageClass      = gcprules.AttrStorageClass
	AttrLocationType      = gcprules.AttrLocationType
	AttrLifecycleRuleCnt  = gcprules.AttrLifecycleRuleCnt
	AttrLifecycleActions  = gcprules.AttrLifecycleActions
	AttrVersioning        = gcprules.AttrVersioning
	AttrAutoclass         = gcprules.AttrAutoclass
	AttrRetentionSeconds  = gcprules.AttrRetentionSeconds
	AttrRetentionLocked   = gcprules.AttrRetentionLocked
	AttrSoftDeleteSeconds = gcprules.AttrSoftDeleteSeconds

	AttrAddrType      = gcprules.AttrAddrType
	AttrAddrPurpose   = gcprules.AttrAddrPurpose
	AttrAddrIP        = gcprules.AttrAddrIP
	AttrAddrUserCount = gcprules.AttrAddrUserCount

	AttrCreationTime   = gcprules.AttrCreationTime
	AttrLastAttachTime = gcprules.AttrLastAttachTime
	AttrLastDetachTime = gcprules.AttrLastDetachTime

	AttrImageID            = gcprules.AttrImageID
	AttrMachineImageID     = gcprules.AttrMachineImageID
	AttrFamily             = gcprules.AttrFamily
	AttrStorageLocation    = gcprules.AttrStorageLocation
	AttrReferenceCount     = gcprules.AttrReferenceCount
	AttrReferenceSources   = gcprules.AttrReferenceSources
	AttrReferencesComplete = gcprules.AttrReferencesComplete
)

// Address types.
const (
	AddressTypeExternal = gcprules.AddressTypeExternal
	AddressTypeInternal = gcprules.AddressTypeInternal
)

// Service hostnames.
const (
	ServiceCompute = "compute.googleapis.com"
	ServiceStorage = "storage.googleapis.com"
)

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func sortStrings(s []string) { sort.Strings(s) }
