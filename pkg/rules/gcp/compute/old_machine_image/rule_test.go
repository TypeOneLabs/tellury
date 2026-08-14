package old_machine_image

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	gcprules "github.com/TypeOneLabs/tellury/pkg/rules/gcp"
)

var now = time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)

type fakePricer struct {
	unit float64
}

func (f fakePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if kind == pricing.KindMachineImageStorage && sku == MachineImageStorageSKU {
		return f.unit, region, nil
	}
	return 0, "", pricing.ErrNoPrice
}

func (f fakePricer) MonthlyCost(it pricing.Item) (float64, error) {
	unit, _, err := f.UnitPrice(it.Kind, it.Provider, it.SKU, it.Region)
	if err != nil {
		return 0, err
	}
	return unit * it.Quantity, nil
}

type noPricePricer struct{}

func (noPricePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	return 0, "", pricing.ErrNoPrice
}

func (noPricePricer) MonthlyCost(it pricing.Item) (float64, error) {
	return 0, pricing.ErrNoPrice
}

// machineImageNode builds a machine-image node. gib is the BILLABLE
// totalStorageBytes size in GiB.
func machineImageNode(assetType string, gib float64, creation string, status string) *graph.Node {
	n := &graph.Node{
		ID:        graph.Ref("//compute.googleapis.com/projects/p/global/machineImages/mi-1"),
		Kind:      graph.KindImage,
		Name:      "mi-1",
		Provider:  "gcp",
		Service:   "compute",
		AssetType: assetType,
		Project:   "p",
		Location:  "us-central1",
		Attrs:     map[string]any{},
	}
	n.SetAttr(gcprules.AttrMachineImageID, "999")
	n.SetAttr(gcprules.AttrStorageBytes, gib*(1<<30))
	n.SetAttr(gcprules.AttrCreationTime, creation)
	n.SetAttr(gcprules.AttrStatus, status)
	n.SetAttr(gcprules.AttrStorageLocation, "us-central1")
	return n
}

func runEval(t *testing.T, n *graph.Node, price rulesPricer) ([]rules.Finding, map[rules.SkipCode]int) {
	t.Helper()
	g := graph.New()
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()

	skipCounts := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: price,
		Now:   now,
		Skip: func(ruleID string, id graph.Ref, code rules.SkipCode) {
			skipCounts[code]++
		},
	}
	findings, err := rules.AdaptNodeRule(rule{}).Eval(context.Background(), p)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	return findings, skipCounts
}

type rulesPricer interface {
	UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error)
	MonthlyCost(it pricing.Item) (float64, error)
}

func TestEval_EveryGuardSkipPath(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	young := now.AddDate(0, 0, -10).Format(time.RFC3339)
	valid := func() *graph.Node {
		return machineImageNode(gcprules.TypeMachineImage, 100, old, gcprules.StatusReady)
	}

	tests := []struct {
		name string
		node *graph.Node
		code rules.SkipCode
	}{
		{"wrong asset type", machineImageNode(gcprules.TypeImage, 100, old, gcprules.StatusReady), rules.SkipNotTargetAssetType},
		{"missing storage bytes", func() *graph.Node { n := valid(); delete(n.Attrs, gcprules.AttrStorageBytes); return n }(), rules.SkipMissingAttr},
		{"unparseable creation time", func() *graph.Node { n := valid(); n.SetAttr(gcprules.AttrCreationTime, "not-a-time"); return n }(), rules.SkipMissingAttr},
		{"non ready status", machineImageNode(gcprules.TypeMachineImage, 100, old, "PENDING"), rules.SkipNonBillingStatus},
		{"missing storage location", func() *graph.Node { n := valid(); delete(n.Attrs, gcprules.AttrStorageLocation); return n }(), rules.SkipMissingAttr},
		{"too young", machineImageNode(gcprules.TypeMachineImage, 100, young, gcprules.StatusReady), rules.SkipTooYoung},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings, skips := runEval(t, tt.node, fakePricer{unit: 0.05})
			if len(findings) != 0 {
				t.Fatalf("want 0 findings, got %+v", findings)
			}
			if skips[tt.code] != 1 {
				t.Errorf("want skip code %q recorded once, got %+v", tt.code, skips)
			}
		})
	}
}

func TestEval_OldMachineImageFiresWithStoredBytesCost(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	n := machineImageNode(gcprules.TypeMachineImage, 250, old, gcprules.StatusReady)

	findings, skips := runEval(t, n, fakePricer{unit: 0.026})
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	if f.MonthlyWasteUSD != 6.50 {
		t.Errorf("MonthlyWasteUSD = %v, want 6.50 (250 GiB stored bytes * $0.026/GiB-month)", f.MonthlyWasteUSD)
	}
	if f.Confidence != 0.7 {
		t.Errorf("Confidence = %v, want 0.7", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing resource, got %+v", skips)
	}

	byKey := map[string]string{}
	for _, e := range f.Evidence {
		byKey[e.Key] = e.Value
	}
	if byKey["size_basis"] != "total_storage_bytes" {
		t.Errorf("evidence size_basis = %q, want total_storage_bytes", byKey["size_basis"])
	}
	if byKey["unit_price_gib_month"] != "$0.0260" {
		t.Errorf("evidence unit_price_gib_month = %q, want $0.0260", byKey["unit_price_gib_month"])
	}
	if !strings.Contains(byKey["price_source"], "sku=standard") {
		t.Errorf("evidence price_source = %q, want it to name sku=standard", byKey["price_source"])
	}
}

func TestEval_NoPriceSkips(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	n := machineImageNode(gcprules.TypeMachineImage, 100, old, gcprules.StatusReady)
	findings, skips := runEval(t, n, noPricePricer{})
	if len(findings) != 0 {
		t.Fatalf("want 0 findings when no price resolves, got %+v", findings)
	}
	if skips[rules.SkipNoPrice] != 1 {
		t.Errorf("want SkipNoPrice recorded once, got %+v", skips)
	}
}

func TestEval_BelowMinWasteSkips(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	// 1 GiB * $0.05 = $0.05 < $0.10 floor.
	n := machineImageNode(gcprules.TypeMachineImage, 1, old, gcprules.StatusReady)
	findings, skips := runEval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 0 {
		t.Fatalf("want 0 findings below the noise floor, got %+v", findings)
	}
	if skips[rules.SkipBelowMinWaste] != 1 {
		t.Errorf("want SkipBelowMinWaste recorded once, got %+v", skips)
	}
}
