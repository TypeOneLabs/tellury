package azure

import azurerules "github.com/TypeOneLabs/tellury/pkg/rules/azure"

// Attribute keys written by the Azure normalizers (pkg/cloud/azure/normalize.go)
// and read by the Azure rules (pkg/rules/azure/...). Single source of truth:
// the constants are defined in pkg/rules/azure and re-exported here, exactly
// as the AWS provider re-exports pkg/rules/aws and GCP re-exports
// pkg/rules/gcp, so a typo cannot silently split the two halves of the
// contract.
const (
	AttrResourceGroup            = azurerules.AttrResourceGroup
	AttrSubscriptionID           = azurerules.AttrSubscriptionID
	AttrSKUName                  = azurerules.AttrSKUName
	AttrDiskSizeGB               = azurerules.AttrDiskSizeGB
	AttrDiskState                = azurerules.AttrDiskState
	AttrManagedBy                = azurerules.AttrManagedBy
	AttrTimeCreated              = azurerules.AttrTimeCreated
	AttrPublicIPAllocationMethod = azurerules.AttrPublicIPAllocationMethod
	AttrIPAddress                = azurerules.AttrIPAddress
	AttrIPConfiguration          = azurerules.AttrIPConfiguration
	AttrIPConfigurationCount     = azurerules.AttrIPConfigurationCount
	AttrResourceID               = azurerules.AttrResourceID
)

// Asset-type tokens (provider's own "azure.<service>.<resource>" spelling).
const (
	TypeDisk     = azurerules.TypeDisk
	TypePublicIP = azurerules.TypePublicIP
)
