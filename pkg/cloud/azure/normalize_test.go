package azure

import (
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

func TestNormalizeDisk_MapsARGFieldsAndDistinguishesUnattached(t *testing.T) {
	row := map[string]any{
		"id":             "/subscriptions/sub-1/resourceGroups/rg-a/providers/Microsoft.Compute/disks/disk-a",
		"name":           "disk-a",
		"type":           "microsoft.compute/disks",
		"location":       "West Europe",
		"resourceGroup":  "rg-a",
		"subscriptionId": "sub-1",
		"sku":            map[string]any{"name": "Premium_LRS"},
		"managedBy":      "",
		"properties": map[string]any{
			"diskSizeGB":  128.0,
			"diskState":   "Unattached",
			"timeCreated": "2024-01-02T03:04:05Z",
		},
	}

	n := NormalizeDisk(row)
	if n == nil {
		t.Fatal("NormalizeDisk returned nil")
	}
	if n.Kind != graph.KindDisk {
		t.Errorf("Kind = %s, want disk", n.Kind)
	}
	if n.Location != "westeurope" {
		t.Errorf("Location = %q, want westeurope", n.Location)
	}
	if n.Project != "sub-1" {
		t.Errorf("Project = %q, want sub-1", n.Project)
	}
	if got, ok := n.Str(AttrResourceGroup); !ok || got != "rg-a" {
		t.Errorf("resource_group = %q, %v; want rg-a, true", got, ok)
	}
	if got, ok := n.Str(AttrSKUName); !ok || got != "Premium_LRS" {
		t.Errorf("sku_name = %q, %v; want Premium_LRS, true", got, ok)
	}
	if got, ok := n.Str(AttrManagedBy); !ok || got != "" {
		t.Errorf("managed_by = %q, %v; want empty-but-present", got, ok)
	}
	if got, ok := n.Num(AttrDiskSizeGB); !ok || got != 128 {
		t.Errorf("disk_size_gb = %v, %v; want 128, true", got, ok)
	}
	if got, ok := n.Str(AttrTimeCreated); !ok || got != "2024-01-02T03:04:05Z" {
		t.Errorf("time_created = %q, %v; want 2024-01-02T03:04:05Z, true", got, ok)
	}
}

func TestNormalizeDisk_MissingSizeIsAbsentNotZero(t *testing.T) {
	row := map[string]any{
		"id":             "/subscriptions/sub-1/resourceGroups/rg-a/providers/Microsoft.Compute/disks/disk-a",
		"name":           "disk-a",
		"type":           "microsoft.compute/disks",
		"location":       "westeurope",
		"resourceGroup":  "rg-a",
		"subscriptionId": "sub-1",
		"sku":            map[string]any{"name": "Premium_LRS"},
		"managedBy":      "",
		"properties":     map[string]any{},
	}

	n := NormalizeDisk(row)
	if n == nil {
		t.Fatal("NormalizeDisk returned nil")
	}
	if got, ok := n.Num(AttrDiskSizeGB); ok {
		t.Errorf("disk_size_gb = %v, %v; want absent so a rule can distinguish missing from zero", got, ok)
	}
}

func TestNormalizePublicIP_MapsARGFieldsAndCountsConfiguration(t *testing.T) {
	row := map[string]any{
		"id":             "/subscriptions/sub-1/resourceGroups/rg-b/providers/Microsoft.Network/publicIPAddresses/pip-a",
		"name":           "pip-a",
		"type":           "microsoft.network/publicipaddresses",
		"location":       "East US",
		"resourceGroup":  "rg-b",
		"subscriptionId": "sub-1",
		"sku":            map[string]any{"name": "Standard"},
		"properties": map[string]any{
			"publicIPAllocationMethod": "Static",
			"ipAddress":                "203.0.113.10",
			"ipConfiguration": map[string]any{
				"id": "/subscriptions/sub-1/resourceGroups/rg-b/providers/Microsoft.Network/networkInterfaces/nic-a/ipConfigurations/ipconfig1",
			},
		},
	}

	n := NormalizePublicIP(row)
	if n == nil {
		t.Fatal("NormalizePublicIP returned nil")
	}
	if n.Kind != graph.KindAddress {
		t.Errorf("Kind = %s, want address", n.Kind)
	}
	if n.Location != "eastus" {
		t.Errorf("Location = %q, want eastus", n.Location)
	}
	if got, ok := n.Str(AttrPublicIPAllocationMethod); !ok || got != "Static" {
		t.Errorf("public_ip_allocation_method = %q, %v; want Static, true", got, ok)
	}
	if got, ok := n.Num(AttrIPConfigurationCount); !ok || got != 1 {
		t.Errorf("ip_configuration_count = %v, %v; want 1, true", got, ok)
	}
	if _, ok := n.Attrs[AttrIPConfiguration]; !ok {
		t.Error("ip_configuration should be present exactly when associated")
	}
}

func TestNormalizePublicIP_UnassociatedWritesZeroCount(t *testing.T) {
	row := map[string]any{
		"id":             "/subscriptions/sub-1/resourceGroups/rg-b/providers/Microsoft.Network/publicIPAddresses/pip-a",
		"name":           "pip-a",
		"type":           "microsoft.network/publicipaddresses",
		"location":       "eastus",
		"resourceGroup":  "rg-b",
		"subscriptionId": "sub-1",
		"sku":            map[string]any{"name": "Standard"},
		"properties": map[string]any{
			"publicIPAllocationMethod": "Static",
			"ipAddress":                "203.0.113.10",
		},
	}

	n := NormalizePublicIP(row)
	if n == nil {
		t.Fatal("NormalizePublicIP returned nil")
	}
	if got, ok := n.Num(AttrIPConfigurationCount); !ok || got != 0 {
		t.Errorf("ip_configuration_count = %v, %v; want 0, true (known unassociated)", got, ok)
	}
	if _, ok := n.Attrs[AttrIPConfiguration]; ok {
		t.Error("ip_configuration should be absent when unassociated")
	}
}

func TestNormalizeVM_MapsARGFieldsAndWritesAbsentPriorityAndVMSS(t *testing.T) {
	row := map[string]any{
		"id":             "/subscriptions/sub-1/resourceGroups/rg-vm/providers/Microsoft.Compute/virtualMachines/vm-a",
		"name":           "vm-a",
		"type":           "microsoft.compute/virtualmachines",
		"location":       "West Europe",
		"resourceGroup":  "rg-vm",
		"subscriptionId": "sub-1",
		"tags":           map[string]any{"env": "test"},
		"properties": map[string]any{
			"hardwareProfile": map[string]any{
				"vmSize": "Standard_D2as_v5",
			},
			"extended": map[string]any{
				"instanceView": map[string]any{
					"powerState": map[string]any{
						"code": "PowerState/running",
					},
				},
			},
			"storageProfile": map[string]any{
				"osDisk": map[string]any{
					"osType": "Linux",
				},
			},
			"timeCreated": "2024-01-02T03:04:05Z",
		},
	}

	n := NormalizeVM(row)
	if n == nil {
		t.Fatal("NormalizeVM returned nil")
	}
	if n.Kind != graph.KindInstance {
		t.Errorf("Kind = %s, want instance", n.Kind)
	}
	if n.AssetType != TypeVM {
		t.Errorf("AssetType = %q, want %q", n.AssetType, TypeVM)
	}
	if n.Location != "westeurope" {
		t.Errorf("Location = %q, want westeurope", n.Location)
	}
	if n.Project != "sub-1" {
		t.Errorf("Project = %q, want sub-1", n.Project)
	}
	if got, ok := n.Str(AttrVMSize); !ok || got != "Standard_D2as_v5" {
		t.Errorf("vm_size = %q, %v; want Standard_D2as_v5, true", got, ok)
	}
	if got, ok := n.Str(AttrPowerState); !ok || got != "PowerState/running" {
		t.Errorf("power_state_code = %q, %v; want PowerState/running, true", got, ok)
	}
	if got, ok := n.Str(AttrPriority); !ok || got != "" {
		t.Errorf("priority = %q, %v; want empty-but-present for a regular VM", got, ok)
	}
	if got, ok := n.Str(AttrOSType); !ok || got != "Linux" {
		t.Errorf("os_type = %q, %v; want Linux, true", got, ok)
	}
	if got, ok := n.Str(AttrTimeCreated); !ok || got != "2024-01-02T03:04:05Z" {
		t.Errorf("time_created = %q, %v; want 2024-01-02T03:04:05Z, true", got, ok)
	}
	if got, ok := n.Str(AttrVMSSID); !ok || got != "" {
		t.Errorf("virtual_machine_scale_set_id = %q, %v; want empty-but-present for a standalone VM", got, ok)
	}
}

func TestNormalizeVM_SpotVMSSWritesBothValues(t *testing.T) {
	row := map[string]any{
		"id":             "/subscriptions/sub-1/resourceGroups/rg-vm/providers/Microsoft.Compute/virtualMachines/vm-spot",
		"name":           "vm-spot",
		"type":           "microsoft.compute/virtualmachines",
		"location":       "westeurope",
		"resourceGroup":  "rg-vm",
		"subscriptionId": "sub-1",
		"properties": map[string]any{
			"hardwareProfile": map[string]any{"vmSize": "Standard_D2as_v5"},
			"priority":        "Spot",
			"virtualMachineScaleSet": map[string]any{
				"id": "/subscriptions/sub-1/resourceGroups/rg-vm/providers/Microsoft.Compute/virtualMachineScaleSets/vmss-a",
			},
		},
	}

	n := NormalizeVM(row)
	if n == nil {
		t.Fatal("NormalizeVM returned nil")
	}
	if got, ok := n.Str(AttrPriority); !ok || got != "Spot" {
		t.Errorf("priority = %q, %v; want Spot, true", got, ok)
	}
	if got, ok := n.Str(AttrVMSSID); !ok || got != "/subscriptions/sub-1/resourceGroups/rg-vm/providers/Microsoft.Compute/virtualMachineScaleSets/vmss-a" {
		t.Errorf("virtual_machine_scale_set_id = %q, %v; want the VMSS ARM id, true", got, ok)
	}
}
