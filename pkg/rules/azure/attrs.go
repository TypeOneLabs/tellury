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

	// Virtual machine attributes, named from the ARG row fields they came
	// from. vm_size, power_state_code, priority, os_type and
	// virtual_machine_scale_set_id are written unconditionally so a rule can
	// tell an unparsed payload (attribute absent) from a real VM whose ARM
	// payload simply omitted the field.
	AttrVMSize     = "vm_size"                      // row.properties.hardwareProfile.vmSize
	AttrPowerState = "power_state_code"             // row.properties.extended.instanceView.powerState.code
	AttrPriority   = "priority"                     // row.properties.priority ("" for a regular VM; "Spot" for spot)
	AttrOSType     = "os_type"                      // row.properties.storageProfile.osDisk.osType
	AttrVMSSID     = "virtual_machine_scale_set_id" // row.properties.virtualMachineScaleSet.id ("" when standalone)

	// Shape attributes hydrated during ingest from the Resource SKUs API.
	// They are absent when the size was not loaded, never zero.
	AttrVCpuCount     = "vcpu_count"     // Resource SKU vCPUs capability
	AttrMemoryGiB     = "memory_gib"     // Resource SKU MemoryGB capability
	AttrMachineFamily = "machine_family" // Resource SKU family field, e.g. standardDasv5Family
)

// Asset-type tokens the Azure rules declare in Meta.RequiredAssetTypes: the
// provider's own stable "azure.<service>.<resource>" spelling, mapped by the
// provider to Azure Resource Graph types.
const (
	TypeDisk     = "azure.compute.disk"
	TypePublicIP = "azure.network.publicipaddress"
	TypeVM       = "azure.compute.vm"
)
