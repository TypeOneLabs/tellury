package azure

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
	azurepricing "github.com/TypeOneLabs/tellury/pkg/pricing/azure"
)

// resourceSKUsAPI is the subset of the Azure Resource SKUs API the sizer
// calls. It exists so the live SDK pager and the offline fixture-backed fake
// share one method, exactly like resourceGraphAPI above.
type resourceSKUsAPI interface {
	List(ctx context.Context, subscriptionID string, regions []string) ([]*armcompute.ResourceSKU, error)
}

// resourceSKUsClient adapts the Azure Resource SKUs SDK to resourceSKUsAPI.
// The SDK client is subscription-scoped, so one is constructed per
// subscription on first use; the Sizer's cache makes that at most once per
// subscription for the scan lifetime.
type resourceSKUsClient struct {
	credential azcore.TokenCredential
}

func (c *resourceSKUsClient) List(ctx context.Context, subscriptionID string, regions []string) ([]*armcompute.ResourceSKU, error) {
	client, err := armcompute.NewResourceSKUsClient(subscriptionID, c.credential, nil)
	if err != nil {
		return nil, fmt.Errorf("azure: resource skus client: %w", err)
	}

	// FILTER SERVER-SIDE, PER REGION. Unfiltered, this API returns every SKU
	// the subscription can see — every VM size, disk type and storage tier in
	// every region on earth. Measured against a real subscription holding one
	// VM, the unfiltered walk took 55 seconds and made asset discovery 18x
	// slower than the entire rest of the scan. The API accepts an OData
	// location filter, and a scan only ever needs the regions its resources
	// are actually in.
	// One filtered request per region a resource actually lives in. With no
	// regions the call falls back to unfiltered, which is correct but slow.
	filters := make([]*string, 0, len(regions))
	for _, r := range regions {
		if r != "" {
			filters = append(filters, to.Ptr("location eq '"+r+"'"))
		}
	}
	if len(filters) == 0 {
		filters = append(filters, nil)
	}

	var out []*armcompute.ResourceSKU
	for _, f := range filters {
		pager := client.NewListPager(&armcompute.ResourceSKUsClientListOptions{Filter: f})
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("azure: resource skus: %w", err)
			}
			out = append(out, page.Value...)
		}
	}
	return out, nil
}

// Sizer is the Azure implementation of pricing.Sizer and
// pricing.RegionalSizer: it answers "what shape is this VM size" and "what
// else exists in this VM's family, in this region".
//
// The shapes come from the Resource SKUs API, live, with no embedded table:
// the API's vCPUs and MemoryGB capabilities become MachineSpec.VCPU and
// MachineSpec.MemoryGiB, and the API's authoritative `family` field becomes
// MachineSpec.Family. A size name is therefore a key, never something the
// code hand-parses.
//
// Caching is per subscription, in memory, for the scan lifetime. A
// subscription is loaded at most once, and a failed page load is not
// half-committed: rows accumulate into a local map and commit only after the
// full pager walk succeeds.
type Sizer struct {
	mu sync.RWMutex

	client resourceSKUsAPI

	loaded map[string]bool
	// byRegion is subscriptionID -> region -> sizeName -> spec. The
	// subscription dimension mirrors the Resource SKUs API: the list is
	// subscription-scoped and availability can differ between subscriptions.
	byRegion map[string]map[string]map[string]pricing.MachineSpec
	// byName is the cross-region shape cache for pricing.Sizer.Spec and
	// pricing.Sizer.Family. Shape (vCPU/RAM/family) is constant for a size
	// name; availability is what varies by region.
	byName map[string]pricing.MachineSpec
}

// NewSizer returns an empty Azure Sizer backed by client. It is populated by
// LoadSubscription during ingest and read by rules afterwards.
func NewSizer(client resourceSKUsAPI) *Sizer {
	return &Sizer{
		client:   client,
		loaded:   map[string]bool{},
		byRegion: map[string]map[string]map[string]pricing.MachineSpec{},
		byName:   map[string]pricing.MachineSpec{},
	}
}

// Spec implements pricing.Sizer.
func (s *Sizer) Spec(machineType string) (pricing.MachineSpec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	spec, ok := s.byName[machineType]
	return spec, ok
}

// Family implements pricing.Sizer. It returns the Resource SKU row's
// authoritative `family` field, e.g. "standardDasv5Family" for
// "Standard_D2as_v5". If the size has not been loaded it returns "", and the
// rule skips the shape as unknown.
func (s *Sizer) Family(machineType string) string {
	spec, ok := s.Spec(machineType)
	if !ok {
		return ""
	}
	return spec.Family
}

// Ladder implements pricing.Sizer: every known member of a family across all
// loaded subscriptions, sorted ascending by (VCPU, MemoryGiB, Name) so a
// caller walking it from the start meets the smallest candidate first.
func (s *Sizer) Ladder(family string) []pricing.MachineSpec {
	s.mu.RLock()
	seen := map[string]pricing.MachineSpec{}
	for _, spec := range s.byName {
		if spec.Family == family {
			seen[spec.Name] = spec
		}
	}
	s.mu.RUnlock()

	return sortSpecs(seen)
}

// SpecInRegion implements pricing.RegionalSizer.
func (s *Sizer) SpecInRegion(machineType, region string) (pricing.MachineSpec, bool) {
	region = azurepricing.LocationRegion(region)
	if region == "" {
		return pricing.MachineSpec{}, false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, byRegion := range s.byRegion {
		bySize := byRegion[region]
		if spec, ok := bySize[machineType]; ok {
			return spec, true
		}
	}
	return pricing.MachineSpec{}, false
}

// LadderInRegion implements pricing.RegionalSizer: every loaded Resource SKU
// row with the given authoritative family whose Locations include region,
// sorted ascending by (VCPU, MemoryGiB, Name). It is the region-specific
// counterpart of Ladder, and the method a rule must use so a VM in a region
// that does not offer the family's smallest sibling is never recommended a
// size it cannot provision there.
func (s *Sizer) LadderInRegion(family, region string) []pricing.MachineSpec {
	region = azurepricing.LocationRegion(region)
	if region == "" {
		return nil
	}

	s.mu.RLock()
	seen := map[string]pricing.MachineSpec{}
	for _, byRegion := range s.byRegion {
		bySize := byRegion[region]
		for name, spec := range bySize {
			if spec.Family == family {
				seen[name] = spec
			}
		}
	}
	s.mu.RUnlock()

	return sortSpecs(seen)
}

// LoadSubscription loads every virtualMachines Resource SKU for subscriptionID
// once. It returns nil when the subscription has already been loaded.
//
// Rows missing vCPUs or MemoryGB — or carrying non-positive values — are
// DROPPED rather than recorded as zero: a zero-vCPU shape would sort to the
// front of a ladder and be recommended to everyone.
func (s *Sizer) LoadSubscription(ctx context.Context, subscriptionID string, regions []string) error {
	if subscriptionID == "" {
		return fmt.Errorf("azure: resource skus subscription is empty")
	}

	s.mu.RLock()
	done := s.loaded[subscriptionID]
	s.mu.RUnlock()
	if done {
		return nil
	}

	if s.client == nil {
		return fmt.Errorf("azure: resource skus client is unavailable")
	}

	skus, err := s.client.List(ctx, subscriptionID, regions)
	if err != nil {
		return fmt.Errorf("azure: load resource skus for subscription %s: %w", subscriptionID, err)
	}

	byRegion := map[string]map[string]pricing.MachineSpec{}
	byName := map[string]pricing.MachineSpec{}

	for _, sku := range skus {
		spec, regions, ok := machineSpecFromSKU(sku)
		if !ok {
			continue
		}

		byName[spec.Name] = spec
		for _, region := range regions {
			if byRegion[region] == nil {
				byRegion[region] = map[string]pricing.MachineSpec{}
			}
			byRegion[region][spec.Name] = spec
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// A duplicate concurrent load may have committed while the pager walked.
	// Keep the first committed result; both came from the same API shape.
	if s.loaded[subscriptionID] {
		return nil
	}
	s.loaded[subscriptionID] = true
	s.byRegion[subscriptionID] = byRegion
	for name, spec := range byName {
		s.byName[name] = spec
	}
	return nil
}

// machineSpecFromSKU converts one Resource SKU row. It returns false — and the
// row is dropped — when the row is not a virtualMachines size, has no usable
// size name, or is missing either vCPUs or MemoryGB (including non-positive
// values). A row with no Locations is still kept in byName so its shape can
// describe a running VM, but it never appears in a region-specific ladder.
func machineSpecFromSKU(sku *armcompute.ResourceSKU) (pricing.MachineSpec, []string, bool) {
	if sku == nil {
		return pricing.MachineSpec{}, nil, false
	}
	if sku.ResourceType == nil || !strings.EqualFold(*sku.ResourceType, "virtualMachines") {
		return pricing.MachineSpec{}, nil, false
	}
	if sku.Name == nil || *sku.Name == "" {
		return pricing.MachineSpec{}, nil, false
	}
	name := *sku.Name
	if !strings.HasPrefix(name, "Standard_") && !strings.HasPrefix(name, "Basic_") {
		return pricing.MachineSpec{}, nil, false
	}

	capabilities := resourceSKUCapabilities(sku.Capabilities)
	vcpu, ok := parseCapability(capabilities, "vcpus")
	if !ok || vcpu <= 0 {
		return pricing.MachineSpec{}, nil, false
	}
	memoryGiB, ok := parseCapability(capabilities, "memorygb")
	if !ok || memoryGiB <= 0 {
		return pricing.MachineSpec{}, nil, false
	}

	family := ""
	if sku.Family != nil {
		family = *sku.Family
	}

	return pricing.MachineSpec{
		Name:      name,
		Family:    family,
		VCPU:      vcpu,
		MemoryGiB: memoryGiB,
	}, resourceSKULocations(sku), true
}

func resourceSKUCapabilities(caps []*armcompute.ResourceSKUCapabilities) map[string]string {
	out := map[string]string{}
	for _, c := range caps {
		if c == nil || c.Name == nil {
			continue
		}
		value := ""
		if c.Value != nil {
			value = *c.Value
		}
		out[strings.ToLower(*c.Name)] = value
	}
	return out
}

func parseCapability(caps map[string]string, name string) (float64, bool) {
	raw, ok := caps[name]
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// resourceSKULocations canonicalises a Resource SKU's Locations, falling back
// to LocationInfo[*].Location when Locations is empty. Every location is
// canonicalised with the same azurepricing.LocationRegion wrapper the graph
// and price catalogue use, so "West Europe" and "westeurope" cannot disagree.
func resourceSKULocations(sku *armcompute.ResourceSKU) []string {
	seen := map[string]bool{}
	var out []string
	add := func(location string) {
		region := azurepricing.LocationRegion(location)
		if region == "" || seen[region] {
			return
		}
		seen[region] = true
		out = append(out, region)
	}

	if len(sku.Locations) > 0 {
		for _, loc := range sku.Locations {
			if loc != nil {
				add(*loc)
			}
		}
		return out
	}

	for _, info := range sku.LocationInfo {
		if info != nil && info.Location != nil {
			add(*info.Location)
		}
	}
	return out
}

func sortSpecs(specs map[string]pricing.MachineSpec) []pricing.MachineSpec {
	out := make([]pricing.MachineSpec, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].VCPU != out[j].VCPU {
			return out[i].VCPU < out[j].VCPU
		}
		if out[i].MemoryGiB != out[j].MemoryGiB {
			return out[i].MemoryGiB < out[j].MemoryGiB
		}
		return out[i].Name < out[j].Name
	})
	return out
}
