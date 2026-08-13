package azure

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

func strPtr(s string) *string { return &s }

type stubResourceSKUs struct {
	calls int
	skus  []*armcompute.ResourceSKU
	err   error
}

func (f *stubResourceSKUs) List(context.Context, string, []string) ([]*armcompute.ResourceSKU, error) {
	f.calls++
	return f.skus, f.err
}

func vmSKU(name, family string, locations []string, vcpu, memoryGB string) *armcompute.ResourceSKU {
	return &armcompute.ResourceSKU{
		ResourceType: strPtr("virtualMachines"),
		Name:         strPtr(name),
		Family:       strPtr(family),
		Locations:    stringSlicePtrs(locations),
		Capabilities: []*armcompute.ResourceSKUCapabilities{
			{Name: strPtr("vCPUs"), Value: strPtr(vcpu)},
			{Name: strPtr("MemoryGB"), Value: strPtr(memoryGB)},
		},
	}
}

func stringSlicePtrs(in []string) []*string {
	out := make([]*string, len(in))
	for i := range in {
		out[i] = strPtr(in[i])
	}
	return out
}

func TestSizer_LoadSubscriptionBuildsRegionLadderAndDropsMissingCapabilities(t *testing.T) {
	f := &stubResourceSKUs{skus: []*armcompute.ResourceSKU{
		vmSKU("Standard_D2as_v5", "standardDasv5Family", []string{"westeurope", "eastus"}, "2", "8"),
		vmSKU("Standard_D4as_v5", "standardDasv5Family", []string{"westeurope"}, "4", "16"),
		vmSKU("Standard_D8as_v5", "standardDasv5Family", []string{"eastus"}, "8", "32"),
		// Missing MemoryGB: must be DROPPED, not recorded as zero.
		{ResourceType: strPtr("virtualMachines"), Name: strPtr("Standard_D2as_v5_broken"), Family: strPtr("standardDasv5Family"), Locations: stringSlicePtrs([]string{"westeurope"}), Capabilities: []*armcompute.ResourceSKUCapabilities{{Name: strPtr("vCPUs"), Value: strPtr("2")}}},
		// Zero vCPU: must be DROPPED for the same reason.
		vmSKU("Standard_D0as_v5", "standardDasv5Family", []string{"westeurope"}, "0", "4"),
	}}

	s := NewSizer(f)
	if err := s.LoadSubscription(context.Background(), "sub-1", nil); err != nil {
		t.Fatalf("LoadSubscription: %v", err)
	}

	spec, ok := s.Spec("Standard_D2as_v5")
	if !ok {
		t.Fatal("Spec(Standard_D2as_v5) not found")
	}
	if spec.VCPU != 2 || spec.MemoryGiB != 8 || spec.Family != "standardDasv5Family" {
		t.Errorf("D2as_v5 spec = %#v, want 2 vCPU / 8 GiB / standardDasv5Family", spec)
	}
	if got := s.Family("Standard_D2as_v5"); got != "standardDasv5Family" {
		t.Errorf("Family = %q, want the Resource SKU's authoritative family field", got)
	}

	if _, ok := s.Spec("Standard_D2as_v5_broken"); ok {
		t.Error("size missing MemoryGB must be dropped, not recorded as zero")
	}
	if _, ok := s.Spec("Standard_D0as_v5"); ok {
		t.Error("zero-vCPU size must be dropped, not recorded as zero")
	}

	west := s.LadderInRegion("standardDasv5Family", "westeurope")
	if len(west) != 2 || west[0].Name != "Standard_D2as_v5" || west[1].Name != "Standard_D4as_v5" {
		t.Errorf("westeurope ladder = %v, want [Standard_D2as_v5 Standard_D4as_v5]", machineNames(west))
	}

	east := s.LadderInRegion("standardDasv5Family", "eastus")
	if len(east) != 2 || east[0].Name != "Standard_D2as_v5" || east[1].Name != "Standard_D8as_v5" {
		t.Errorf("eastus ladder = %v, want [Standard_D2as_v5 Standard_D8as_v5]", machineNames(east))
	}

	all := s.Ladder("standardDasv5Family")
	if len(all) != 3 || all[0].Name != "Standard_D2as_v5" || all[1].Name != "Standard_D4as_v5" || all[2].Name != "Standard_D8as_v5" {
		t.Errorf("cross-region ladder = %v, want [Standard_D2as_v5 Standard_D4as_v5 Standard_D8as_v5]", machineNames(all))
	}

	if _, ok := s.SpecInRegion("Standard_D4as_v5", "eastus"); ok {
		t.Error("SpecInRegion should not report a size whose Locations do not include the region")
	}
	if got, ok := s.SpecInRegion("Standard_D4as_v5", "West Europe"); !ok || got.Name != "Standard_D4as_v5" {
		t.Errorf("SpecInRegion with display-name region = %v, %v; want the canonicalised lookup", got, ok)
	}

	// The Azure sizer must implement both contracts.
	var _ pricing.Sizer = s
	var _ pricing.RegionalSizer = s
}

func TestSizer_SecondLoadOfSameSubscriptionCostsNoCalls(t *testing.T) {
	f := &stubResourceSKUs{skus: []*armcompute.ResourceSKU{
		vmSKU("Standard_D2as_v5", "standardDasv5Family", []string{"westeurope"}, "2", "8"),
	}}
	s := NewSizer(f)
	ctx := context.Background()
	if err := s.LoadSubscription(ctx, "sub-1", nil); err != nil {
		t.Fatalf("LoadSubscription: %v", err)
	}
	before := f.calls
	if err := s.LoadSubscription(ctx, "sub-1", nil); err != nil {
		t.Fatalf("second LoadSubscription: %v", err)
	}
	if f.calls != before {
		t.Errorf("subscription loaded twice: %d -> %d calls", before, f.calls)
	}
}

func TestSizer_FailedLoadIsNotHalfCommitted(t *testing.T) {
	f := &stubResourceSKUs{err: errors.New("boom")}
	s := NewSizer(f)
	if err := s.LoadSubscription(context.Background(), "sub-1", nil); err == nil {
		t.Fatal("LoadSubscription should return the client error")
	}
	if _, ok := s.Spec("Standard_D2as_v5"); ok {
		t.Error("failed load must not half-commit any size")
	}
}

func machineNames(specs []pricing.MachineSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// TestSizer_ListIsFilteredToRegionsInUse pins the region filter on the
// Resource SKUs call.
//
// Unfiltered, that API returns every SKU the subscription can see — every VM
// size, disk type and storage tier in every region. Measured against a real
// subscription holding ONE VM, the unfiltered walk took 52 seconds and made
// asset discovery slower than everything else in the scan combined; filtered
// to the one region in use it took 7. The filter is not an optimisation
// detail, it is the difference between a usable scan and an unusable one.
func TestSizer_ListIsFilteredToRegionsInUse(t *testing.T) {
	f := &recordingSKUs{}
	s := NewSizer(f)
	if err := s.LoadSubscription(context.Background(), "sub-1", []string{"swedencentral"}); err != nil {
		t.Fatalf("LoadSubscription: %v", err)
	}
	if len(f.gotRegions) != 1 || f.gotRegions[0] != "swedencentral" {
		t.Errorf("regions passed to List = %v, want [swedencentral]: an empty list falls back to "+
			"an unfiltered API call that returns every SKU in every region", f.gotRegions)
	}
}

type recordingSKUs struct{ gotRegions []string }

func (r *recordingSKUs) List(_ context.Context, _ string, regions []string) ([]*armcompute.ResourceSKU, error) {
	r.gotRegions = append(r.gotRegions, regions...)
	return nil, nil
}
