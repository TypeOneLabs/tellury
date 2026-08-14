package unused_gallery_image_version

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	azurerules "github.com/TypeOneLabs/tellury/pkg/rules/azure"
)

var now = time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)

type fakePricer struct {
	unit float64
}

func (f fakePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if kind == pricing.KindGalleryImageStorage && sku == "Standard_LRS" {
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

func imageNode(assetType string, sizeGiB float64, creation string, provisioningState string, replicas []map[string]any, refsComplete bool, refCount float64) *graph.Node {
	n := &graph.Node{
		ID:        graph.Ref("/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/galleries/gal/images/img/versions/1.0.0"),
		Kind:      graph.KindImage,
		Name:      "1.0.0",
		Provider:  "azure",
		Service:   "compute",
		AssetType: assetType,
		Project:   "sub-1",
		Location:  "westeurope",
		Attrs:     map[string]any{},
	}
	n.SetAttr(azurerules.AttrResourceID, string(n.ID))
	n.SetAttr(azurerules.AttrGalleryImageID, "/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/galleries/gal/images/img")
	n.SetAttr(azurerules.AttrGallerySizeBytes, sizeGiB*(1<<30))
	n.SetAttr(azurerules.AttrCreationTimestamp, creation)
	n.SetAttr(azurerules.AttrProvisioningState, provisioningState)
	if replicas != nil {
		total := 0.0
		for _, r := range replicas {
			total += r["replica_count"].(float64)
		}
		n.SetAttr(azurerules.AttrGalleryReplicaRegions, replicas)
		n.SetAttr(azurerules.AttrGalleryReplicaCount, total)
	}
	n.SetAttr(azurerules.AttrReferencesComplete, refsComplete)
	n.SetAttr(azurerules.AttrReferenceCount, refCount)
	n.SetAttr(azurerules.AttrReferenceSources, []string{})
	return n
}

func replicaRegions(region string, count float64) []map[string]any {
	return []map[string]any{
		{
			"region":               region,
			"replica_count":        count,
			"storage_account_type": "Standard_LRS",
		},
	}
}

func runEval(t *testing.T, n *graph.Node, price interface {
	UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error)
	MonthlyCost(it pricing.Item) (float64, error)
}) ([]rules.Finding, map[rules.SkipCode]int) {
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

func TestEval_EveryGuardSkipPath(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	young := now.AddDate(0, 0, -10).Format(time.RFC3339)
	valid := func() *graph.Node {
		return imageNode(azurerules.TypeGalleryImageVersion, 100, old, "Succeeded", replicaRegions("westeurope", 1), true, 0)
	}

	tests := []struct {
		name string
		node *graph.Node
		code rules.SkipCode
	}{
		{"wrong asset type", imageNode(azurerules.TypeVM, 100, old, "Succeeded", replicaRegions("westeurope", 1), true, 0), rules.SkipNotTargetAssetType},
		{"missing size bytes", func() *graph.Node { n := valid(); delete(n.Attrs, azurerules.AttrGallerySizeBytes); return n }(), rules.SkipMissingAttr},
		{"unparseable creation time", func() *graph.Node { n := valid(); n.SetAttr(azurerules.AttrCreationTimestamp, "not-a-time"); return n }(), rules.SkipMissingAttr},
		{"non succeeded provisioning state", imageNode(azurerules.TypeGalleryImageVersion, 100, old, "Creating", replicaRegions("westeurope", 1), true, 0), rules.SkipNonBillingStatus},
		{"missing replica regions", imageNode(azurerules.TypeGalleryImageVersion, 100, old, "Succeeded", nil, true, 0), rules.SkipMissingAttr},
		{"references incomplete", imageNode(azurerules.TypeGalleryImageVersion, 100, old, "Succeeded", replicaRegions("westeurope", 1), false, 0), rules.SkipReferencesUnknown},
		{"referenced by VM", imageNode(azurerules.TypeGalleryImageVersion, 100, old, "Succeeded", replicaRegions("westeurope", 1), true, 1), rules.SkipAttached},
		{"too young", imageNode(azurerules.TypeGalleryImageVersion, 100, young, "Succeeded", replicaRegions("westeurope", 1), true, 0), rules.SkipTooYoung},
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

func TestEval_ScaleSetDefinitionReferenceNotReported(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	n := imageNode(azurerules.TypeGalleryImageVersion, 100, old, "Succeeded", replicaRegions("westeurope", 1), true, 1)
	n.SetAttr(azurerules.AttrReferenceSources, []string{"vmss"})

	findings, skips := runEval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 0 {
		t.Fatalf("scale-set-referenced version must not be reported, got %+v", findings)
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached recorded once, got %+v", skips)
	}
}

func TestEval_OneReplicaCostsExactlyOneHundredGiB(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	n := imageNode(azurerules.TypeGalleryImageVersion, 100, old, "Succeeded", replicaRegions("westeurope", 1), true, 0)

	findings, skips := runEval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	if got := findings[0].MonthlyWasteUSD; got != 5.00 {
		t.Errorf("MonthlyWasteUSD = %v, want 5.00 (100 GiB * 1 replica * $0.05/GiB-month)", got)
	}
	if findings[0].Confidence != 0.7 {
		t.Errorf("Confidence = %v, want 0.7", findings[0].Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing resource, got %+v", skips)
	}
}

func TestEval_ThreeReplicasCostThreeTimesOneReplica(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)

	one := imageNode(azurerules.TypeGalleryImageVersion, 100, old, "Succeeded", replicaRegions("westeurope", 1), true, 0)
	three := imageNode(azurerules.TypeGalleryImageVersion, 100, old, "Succeeded", replicaRegions("westeurope", 3), true, 0)

	oneFindings, oneSkips := runEval(t, one, fakePricer{unit: 0.05})
	threeFindings, threeSkips := runEval(t, three, fakePricer{unit: 0.05})

	if len(oneFindings) != 1 || len(oneSkips) != 0 {
		t.Fatalf("one-replica eval = findings %d skips %v, want 1/0", len(oneFindings), oneSkips)
	}
	if len(threeFindings) != 1 || len(threeSkips) != 0 {
		t.Fatalf("three-replica eval = findings %d skips %v, want 1/0", len(threeFindings), threeSkips)
	}

	oneWaste := oneFindings[0].MonthlyWasteUSD
	threeWaste := threeFindings[0].MonthlyWasteUSD
	if oneWaste != 5.00 {
		t.Fatalf("one-replica waste = %v, want 5.00", oneWaste)
	}
	if threeWaste != 15.00 {
		t.Fatalf("three-replica waste = %v, want 15.00 (three times one replica)", threeWaste)
	}
	if threeWaste != oneWaste*3 {
		t.Errorf("three-replica waste = %v, want exactly %v", threeWaste, oneWaste*3)
	}

	byKey := map[string]string{}
	for _, e := range threeFindings[0].Evidence {
		byKey[e.Key] = e.Value
	}
	if byKey["replica_count"] != "3" {
		t.Errorf("evidence replica_count = %q, want 3", byKey["replica_count"])
	}
	if byKey["unit_price_gib_month_westeurope"] != "$0.0500" {
		t.Errorf("evidence unit_price_gib_month_westeurope = %q, want $0.0500", byKey["unit_price_gib_month_westeurope"])
	}
	if !strings.Contains(byKey["price_source"], "sku=Standard_LRS") {
		t.Errorf("evidence price_source = %q, want it to name the ARM storage account type Standard_LRS", byKey["price_source"])
	}
}

func TestEval_MissingReferenceCountMeansZero(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	n := imageNode(azurerules.TypeGalleryImageVersion, 100, old, "Succeeded", replicaRegions("westeurope", 1), true, 0)
	delete(n.Attrs, azurerules.AttrReferenceCount)

	findings, skips := runEval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 1 {
		t.Fatalf("want 1 finding when reference_count is absent (absence means zero), got %+v", findings)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing resource, got %+v", skips)
	}
}

func TestEval_NoPriceSkips(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	n := imageNode(azurerules.TypeGalleryImageVersion, 100, old, "Succeeded", replicaRegions("westeurope", 1), true, 0)
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
	// 1 GiB * 1 replica * $0.05 = $0.05 < $0.10 floor.
	n := imageNode(azurerules.TypeGalleryImageVersion, 1, old, "Succeeded", replicaRegions("westeurope", 1), true, 0)
	findings, skips := runEval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 0 {
		t.Fatalf("want 0 findings below the noise floor, got %+v", findings)
	}
	if skips[rules.SkipBelowMinWaste] != 1 {
		t.Errorf("want SkipBelowMinWaste recorded once, got %+v", skips)
	}
}
