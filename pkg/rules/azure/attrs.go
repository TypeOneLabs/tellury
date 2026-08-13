// Package azurerules holds the attribute-key constants shared by the Azure
// rule packages. They are re-exported from pkg/cloud/azure so a rule never
// imports a cloud client (and therefore never pulls the Azure SDK into a unit
// test) while still agreeing with the normalizer on a single spelling — the
// same split pkg/rules/aws and pkg/rules/gcp use for the other providers.
package azurerules

// Attribute keys written by pkg/cloud/azure/normalize.go. Every name is the
// Azure Resource Graph row field path, snake_cased to the shared Attrs
// convention: id, resourceGroup, subscriptionId, sku.name, managedBy,
// properties.diskSizeGB, properties.diskState, properties.timeCreated,
// properties.publicIPAllocationMethod, properties.ipAddress and
// properties.ipConfiguration. A rule therefore reads exactly what Resource
// Graph returned and can trace every attribute back to the ARG row without a
// translation table.
const (
	AttrResourceGroup            = "resource_group"              // row.resourceGroup
	AttrSubscriptionID           = "subscription_id"             // row.subscriptionId
	AttrSKUName                  = "sku_name"                    // row.sku.name
	AttrDiskSizeGB               = "disk_size_gb"                // row.properties.diskSizeGB (GiB)
	AttrDiskState                = "disk_state"                  // row.properties.diskState
	AttrManagedBy                = "managed_by"                  // row.managedBy ("" when unattached)
	AttrTimeCreated              = "time_created"                // row.properties.timeCreated
	AttrPublicIPAllocationMethod = "public_ip_allocation_method" // row.properties.publicIPAllocationMethod
	AttrIPAddress                = "ip_address"                  // row.properties.ipAddress
	AttrIPConfiguration          = "ip_configuration"            // row.properties.ipConfiguration (present exactly when associated)
	AttrIPConfigurationCount     = "ip_configuration_count"      // derived: 0 or 1, always written
	AttrResourceID               = "resource_id"                 // row.id (ARM resource ID)
)

// Asset-type tokens the Azure rules declare in Meta.RequiredAssetTypes: the
// provider's own stable "azure.<service>.<resource>" spelling, mapped by the
// provider to Azure Resource Graph types.
const (
	TypeDisk     = "azure.compute.disk"
	TypePublicIP = "azure.network.publicipaddress"
)
