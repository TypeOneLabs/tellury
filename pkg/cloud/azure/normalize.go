package azure

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// Azure Resource Graph type and service tokens. The service token is the ARG
// type's provider namespace ("microsoft.compute" / "microsoft.network") and
// the asset-type token is tellury's own stable "azure.<service>.<resource>"
// spelling, matching the rule registry.
const (
	argTypeDisk                = "microsoft.compute/disks"
	argTypePublicIP            = "microsoft.network/publicipaddresses"
	argTypeVM                  = "microsoft.compute/virtualmachines"
	argTypeVMSS                = "microsoft.compute/virtualmachinescalesets"
	argTypeGalleryImageVersion = "microsoft.compute/galleries/images/versions"

	serviceCompute = "microsoft.compute"
	serviceNetwork = "microsoft.network"
)

// NormalizeResource dispatches one Azure Resource Graph row to the matching
// normalizer based on the row's `type` column. A row whose type is not one of
// the modelled ARG types returns nil.
func NormalizeResource(row map[string]any) *graph.Node {
	switch strings.ToLower(stringOf(row["type"])) {
	case argTypeDisk:
		return NormalizeDisk(row)
	case argTypePublicIP:
		return NormalizePublicIP(row)
	case argTypeVM:
		return NormalizeVM(row)
	case argTypeGalleryImageVersion:
		return NormalizeGalleryImageVersion(row)
	default:
		return nil
	}
}

// NormalizeDisk converts one Microsoft.Compute/disks Resource Graph row into a
// graph node. A row without an ARM resource `id` is dropped.
//
// Attrs, named from the ARG row fields they came from:
//
//	resource_id      — row.id (the ARM resource ID; also the node ID)
//	resource_group   — row.resourceGroup
//	subscription_id  — row.subscriptionId
//	sku_name         — row.sku.name, e.g. "Premium_LRS"
//	disk_size_gb     — row.properties.diskSizeGB, in GiB
//	disk_state       — row.properties.diskState, e.g. "Unattached"/"Attached"
//	managed_by       — row.managedBy; written unconditionally, "" means unattached
//	time_created     — row.properties.timeCreated
//
// disk_size_gb is written ONLY when Resource Graph returned a positive value.
// An absent key therefore means "the payload did not carry a billable size",
// never "a zero-size disk" — the same distinction the GCP snapshot normalizer
// preserves for storage_bytes. managed_by is written unconditionally so a rule
// can tell the unattached state ("managed_by" present, empty) from an
// unparsed payload (the key would be absent).
func NormalizeDisk(row map[string]any) *graph.Node {
	id := stringOf(row["id"])
	if id == "" {
		return nil
	}

	subscriptionID := stringOf(row["subscriptionId"])
	if subscriptionID == "" {
		subscriptionID = subscriptionFromID(id)
	}
	location := locationRegion(stringOf(row["location"]))

	n := &graph.Node{
		ID:        graph.Ref(id),
		Kind:      graph.KindDisk,
		Name:      resourceName(row, id),
		Provider:  "azure",
		Service:   serviceCompute,
		AssetType: TypeDisk,
		Project:   subscriptionID,
		Location:  location,
		Labels:    labelsOf(row["tags"]),
		Attrs:     map[string]any{},
	}

	n.SetAttr(AttrResourceID, id)
	n.SetAttr(AttrResourceGroup, stringOf(row["resourceGroup"]))
	n.SetAttr(AttrSubscriptionID, subscriptionID)

	sku := mapOf(row["sku"])
	n.SetAttr(AttrSKUName, stringOf(sku["name"]))

	properties := mapOf(row["properties"])
	n.SetAttr(AttrDiskState, stringOf(properties["diskState"]))
	n.SetAttr(AttrManagedBy, stringOf(row["managedBy"]))

	if size, ok := numOf(properties["diskSizeGB"]); ok && size > 0 {
		n.SetAttr(AttrDiskSizeGB, size)
	}
	if created := stringOf(properties["timeCreated"]); created != "" {
		n.SetAttr(AttrTimeCreated, created)
	}

	return n
}

// NormalizePublicIP converts one Microsoft.Network/publicIPAddresses Resource
// Graph row into a graph node. A row without an ARM resource `id` is dropped.
//
// Attrs, named from the ARG row fields they came from:
//
//	resource_id                  — row.id (the ARM resource ID; also the node ID)
//	resource_group               — row.resourceGroup
//	subscription_id              — row.subscriptionId
//	sku_name                     — row.sku.name, "Standard" or "Basic"
//	public_ip_allocation_method  — row.properties.publicIPAllocationMethod
//	ip_address                   — row.properties.ipAddress, when allocated
//	ip_configuration             — row.properties.ipConfiguration, present
//	                               exactly when associated
//	ip_configuration_count       — derived: 0 or 1, ALWAYS written
//
// ip_configuration_count is the countable attribute written unconditionally:
// zero means "known unassociated", while an absent attribute would mean the
// payload was not parsed. ip_configuration itself keeps the raw ARM object so
// the rule can also test association exactly as ARG expressed it.
func NormalizePublicIP(row map[string]any) *graph.Node {
	id := stringOf(row["id"])
	if id == "" {
		return nil
	}

	subscriptionID := stringOf(row["subscriptionId"])
	if subscriptionID == "" {
		subscriptionID = subscriptionFromID(id)
	}
	location := locationRegion(stringOf(row["location"]))

	n := &graph.Node{
		ID:        graph.Ref(id),
		Kind:      graph.KindAddress,
		Name:      resourceName(row, id),
		Provider:  "azure",
		Service:   serviceNetwork,
		AssetType: TypePublicIP,
		Project:   subscriptionID,
		Location:  location,
		Labels:    labelsOf(row["tags"]),
		Attrs:     map[string]any{},
	}

	n.SetAttr(AttrResourceID, id)
	n.SetAttr(AttrResourceGroup, stringOf(row["resourceGroup"]))
	n.SetAttr(AttrSubscriptionID, subscriptionID)

	sku := mapOf(row["sku"])
	n.SetAttr(AttrSKUName, stringOf(sku["name"]))

	properties := mapOf(row["properties"])
	n.SetAttr(AttrPublicIPAllocationMethod, stringOf(properties["publicIPAllocationMethod"]))

	if ip := stringOf(properties["ipAddress"]); ip != "" {
		n.SetAttr(AttrIPAddress, ip)
	}

	// The count is written first, then overwritten when the ARM configuration
	// object is present. The attribute is therefore never absent for a parsed
	// row, exactly like attachment_count / user_count on the other providers.
	n.SetAttr(AttrIPConfigurationCount, 0.0)
	if cfg := mapOf(properties["ipConfiguration"]); cfg != nil {
		n.SetAttr(AttrIPConfigurationCount, 1.0)
		n.SetAttr(AttrIPConfiguration, cfg)
	}

	return n
}

// NormalizeVM converts one Microsoft.Compute/virtualMachines Resource Graph
// row into an instance node. A row without an ARM resource `id` is dropped.
// No VM hydration call is needed: the ARG `properties` object already carries
// the hardware profile, power state, priority, OS type, creation time and
// VMSS membership fields this normalizer reads.
//
// Attrs, named from the ARG row fields they came from:
//
//	resource_id                  — row.id (the ARM resource ID; also the node ID)
//	resource_group               — row.resourceGroup
//	subscription_id              — row.subscriptionId
//	vm_size                      — row.properties.hardwareProfile.vmSize
//	power_state_code             — row.properties.extended.instanceView.powerState.code
//	priority                     — row.properties.priority; ALWAYS written,
//	                               "" means regular/on-demand, "Spot" means spot
//	os_type                      — row.properties.storageProfile.osDisk.osType
//	time_created                 — row.properties.timeCreated
//	virtual_machine_scale_set_id — row.properties.virtualMachineScaleSet.id;
//	                               ALWAYS written, "" means standalone
//
// priority and virtual_machine_scale_set_id are load-bearing unconditional
// writes: a regular standalone VM omits both ARM fields, and a missing attr
// would be indistinguishable from "payload not parsed". Writing "" preserves
// the distinction the existing normalizers apply to countable fields.
func NormalizeVM(row map[string]any) *graph.Node {
	id := stringOf(row["id"])
	if id == "" {
		return nil
	}

	subscriptionID := stringOf(row["subscriptionId"])
	if subscriptionID == "" {
		subscriptionID = subscriptionFromID(id)
	}
	location := locationRegion(stringOf(row["location"]))

	n := &graph.Node{
		ID:        graph.Ref(id),
		Kind:      graph.KindInstance,
		Name:      resourceName(row, id),
		Provider:  "azure",
		Service:   serviceCompute,
		AssetType: TypeVM,
		Project:   subscriptionID,
		Location:  location,
		Labels:    labelsOf(row["tags"]),
		Attrs:     map[string]any{},
	}

	n.SetAttr(AttrResourceID, id)
	n.SetAttr(AttrResourceGroup, stringOf(row["resourceGroup"]))
	n.SetAttr(AttrSubscriptionID, subscriptionID)

	properties := mapOf(row["properties"])

	hardwareProfile := mapOf(properties["hardwareProfile"])
	n.SetAttr(AttrVMSize, stringOf(hardwareProfile["vmSize"]))

	extended := mapOf(properties["extended"])
	instanceView := mapOf(extended["instanceView"])
	powerState := mapOf(instanceView["powerState"])
	n.SetAttr(AttrPowerState, stringOf(powerState["code"]))

	// properties.priority is absent for a regular VM and "Spot" for a spot
	// VM. Write it unconditionally so a not-spot guard reads a present value
	// rather than inferring regular from a missing attribute.
	n.SetAttr(AttrPriority, stringOf(properties["priority"]))

	storageProfile := mapOf(properties["storageProfile"])
	osDisk := mapOf(storageProfile["osDisk"])
	n.SetAttr(AttrOSType, stringOf(osDisk["osType"]))

	if created := stringOf(properties["timeCreated"]); created != "" {
		n.SetAttr(AttrTimeCreated, created)
	}

	// A standalone VM has no properties.virtualMachineScaleSet object. Write
	// the empty string for the same reason as priority: absence means
	// "payload not parsed", while "" means "known standalone".
	vmss := mapOf(properties["virtualMachineScaleSet"])
	n.SetAttr(AttrVMSSID, stringOf(vmss["id"]))

	return n
}

// NormalizeGalleryImageVersion converts one
// Microsoft.Compute/galleries/images/versions Resource Graph row into an image
// node. A row without an ARM resource `id` is dropped.
//
// No hydration call follows this normalizer: ARG returns the image version's
// `properties` object directly, including publishingProfile.publishedDate,
// publishingProfile.targetRegions and storageProfile. The reference pass then
// reads the VM and VMSS rows from the SAME ARG page and stamps reference_count,
// reference_sources and references_complete on every gallery image node.
//
// Attrs:
//
//	resource_id          — row.id (ARM version ID; also node ID)
//	gallery_image_id     — parent gallery image definition ARM ID, derived by
//	                       dropping the trailing /versions/<version>
//	resource_group       — row.resourceGroup
//	subscription_id      — row.subscriptionId
//	creation_timestamp   — row.properties.publishingProfile.publishedDate
//	provisioning_state   — row.properties.provisioningState
//	size_bytes           — osDiskImage.sizeInBytes + sum(dataDiskImages[].sizeInBytes)
//	replica_regions      — []map{region, replica_count, storage_account_type}
//	replica_count        — sum of regionalReplicaCount across targetRegions
func NormalizeGalleryImageVersion(row map[string]any) *graph.Node {
	id := stringOf(row["id"])
	if id == "" {
		return nil
	}

	subscriptionID := stringOf(row["subscriptionId"])
	if subscriptionID == "" {
		subscriptionID = subscriptionFromID(id)
	}
	location := locationRegion(stringOf(row["location"]))

	n := &graph.Node{
		ID:        graph.Ref(id),
		Kind:      graph.KindImage,
		Name:      resourceName(row, id),
		Provider:  "azure",
		Service:   serviceCompute,
		AssetType: TypeGalleryImageVersion,
		Project:   subscriptionID,
		Location:  location,
		Labels:    labelsOf(row["tags"]),
		Attrs:     map[string]any{},
	}

	n.SetAttr(AttrResourceID, id)
	n.SetAttr(AttrGalleryImageID, galleryImageDefinitionID(id))
	n.SetAttr(AttrResourceGroup, stringOf(row["resourceGroup"]))
	n.SetAttr(AttrSubscriptionID, subscriptionID)

	properties := mapOf(row["properties"])
	n.SetAttr(AttrProvisioningState, stringOf(properties["provisioningState"]))

	publishingProfile := mapOf(properties["publishingProfile"])
	if published := stringOf(publishingProfile["publishedDate"]); published != "" {
		n.SetAttr(AttrCreationTimestamp, published)
	}

	storageProfile := mapOf(properties["storageProfile"])
	osDiskImage := mapOf(storageProfile["osDiskImage"])
	if osSize, ok := numOf(osDiskImage["sizeInBytes"]); ok && osSize > 0 {
		sizeBytes := osSize
		for _, dataDisk := range mapsOf(storageProfile["dataDiskImages"]) {
			if diskSize, ok := numOf(dataDisk["sizeInBytes"]); ok && diskSize > 0 {
				sizeBytes += diskSize
			}
		}
		n.SetAttr(AttrGallerySizeBytes, sizeBytes)
	}

	replicaRegions, totalReplicas := replicaRegionsFromTargets(mapsOf(publishingProfile["targetRegions"]))
	if len(replicaRegions) > 0 {
		n.SetAttr(AttrGalleryReplicaRegions, replicaRegions)
		n.SetAttr(AttrGalleryReplicaCount, totalReplicas)
	}

	return n
}

// galleryImageDefinitionID derives the parent gallery image definition ARM ID
// from a gallery image version ARM ID:
//
//	/subscriptions/s/resourceGroups/r/providers/Microsoft.Compute/galleries/g/images/i/versions/1.0.0
//	-> /subscriptions/s/resourceGroups/r/providers/Microsoft.Compute/galleries/g/images/i
func galleryImageDefinitionID(versionID string) string {
	versionID = strings.TrimSuffix(versionID, "/")
	parts := strings.Split(versionID, "/")
	if len(parts) >= 2 && strings.EqualFold(parts[len(parts)-2], "versions") {
		return strings.Join(parts[:len(parts)-2], "/")
	}
	return versionID
}

// replicaRegionsFromTargets converts an ARG publishingProfile.targetRegions
// value into the normalizer's replica_regions contract:
//
//	region                = locationRegion(targetRegion.name)
//	replica_count         = targetRegion.regionalReplicaCount, default 1
//	storage_account_type  = targetRegion.storageAccountType, default Standard_LRS
//
// It returns the list and the total replica count. An absent or empty
// targetRegions list returns an empty list and zero, so replica_regions stays
// absent and the rule skips SkipMissingAttr rather than pricing a zero-replica
// image as free.
func replicaRegionsFromTargets(targetRegions []map[string]any) ([]map[string]any, float64) {
	if len(targetRegions) == 0 {
		return nil, 0
	}
	out := make([]map[string]any, 0, len(targetRegions))
	var total float64
	for _, target := range targetRegions {
		region := locationRegion(stringOf(target["name"]))
		if region == "" {
			continue
		}
		replicaCount := 1.0
		if v, ok := numOf(target["regionalReplicaCount"]); ok && v > 0 {
			replicaCount = v
		}
		storageAccountType := stringOf(target["storageAccountType"])
		if storageAccountType == "" {
			storageAccountType = "Standard_LRS"
		}
		out = append(out, map[string]any{
			"region":               region,
			"replica_count":        replicaCount,
			"storage_account_type": storageAccountType,
		})
		total += replicaCount
	}
	return out, total
}

// ─────────────────────────────────────────────────────────────────────────────
// Row field helpers. Resource Graph returns JSON decoded into any; every
// normalizer reads through these so a malformed field degrades to absent
// rather than panicking.
// ─────────────────────────────────────────────────────────────────────────────

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func numOf(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// mapsOf accepts the two JSON shapes ARG can produce for an array of objects:
// []map[string]any (fixtures) and []any of map[string]any (some live decoder
// paths). Any other shape returns nil so callers treat it as absent.
func mapsOf(v any) []map[string]any {
	switch t := v.(type) {
	case []map[string]any:
		return t
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, raw := range t {
			if m, ok := raw.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

func resourceName(row map[string]any, id string) string {
	if name := stringOf(row["name"]); name != "" {
		return name
	}
	return lastPathSegment(id)
}

func lastPathSegment(s string) string {
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// subscriptionFromID extracts the subscription ID from an ARM resource ID:
// /subscriptions/<sub>/resourceGroups/... -> <sub>.
func subscriptionFromID(id string) string {
	parts := strings.Split(id, "/")
	for i := 0; i < len(parts)-1; i++ {
		if strings.EqualFold(parts[i], "subscriptions") {
			return parts[i+1]
		}
	}
	return ""
}

func labelsOf(v any) map[string]string {
	switch t := v.(type) {
	case map[string]string:
		if len(t) == 0 {
			return nil
		}
		return t
	case map[string]any:
		out := make(map[string]string, len(t))
		for k, raw := range t {
			if s, ok := raw.(string); ok && s != "" {
				out[k] = s
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}
