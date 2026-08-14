package unused_ami

import (
	"context"
	"testing"
	"time"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules"
	awsrules "github.com/TypeOneLabs/tellury/pkg/rules/aws"
)

// fakePricer prices exactly the EBS snapshot-storage SKU this rule looks up;
// every other lookup misses, matching ErrNoPrice semantics.
type fakePricer struct {
	unit float64
}

func (f fakePricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	if kind == pricing.KindSnapshotStorage && sku == SnapshotStorageSKU {
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

func imageNode(assetType, imageID, creation, state string, backingComplete, refsComplete bool, refCount, exclusiveSizeGB float64) *graph.Node {
	n := &graph.Node{
		ID:        graph.Ref("accounts/123456789012/regions/us-east-1/images/" + imageID),
		Kind:      graph.KindImage,
		Name:      imageID,
		Provider:  "aws",
		Service:   "ec2",
		AssetType: assetType,
		Project:   "123456789012",
		Location:  "us-east-1",
		Attrs:     map[string]any{},
	}
	n.SetAttr(awsrules.AttrImageID, imageID)
	n.SetAttr(awsrules.AttrImageName, imageID+"-name")
	n.SetAttr(awsrules.AttrCreationTimestamp, creation)
	n.SetAttr(awsrules.AttrState, state)
	n.SetAttr(awsrules.AttrBackingComplete, backingComplete)
	n.SetAttr(awsrules.AttrReferencesComplete, refsComplete)
	n.SetAttr(awsrules.AttrReferenceCount, refCount)
	n.SetAttr(awsrules.AttrBackingExclusiveSizeGB, exclusiveSizeGB)
	n.SetAttr(awsrules.AttrBackingSnapshotCount, float64(1))
	n.SetAttr(awsrules.AttrReferenceSources, []string{})
	return n
}

var fixedNow = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
var oldCreation = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
var youngCreation = time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

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
		Now:   fixedNow,
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

// rulesPricer is the subset of pricing.Pricer both fake pricers implement.
type rulesPricer interface {
	UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error)
	MonthlyCost(it pricing.Item) (float64, error)
}

func TestEval_EveryGuardSkipPath(t *testing.T) {
	tests := []struct {
		name string
		node *graph.Node
		code rules.SkipCode
	}{
		{
			name: "wrong asset type",
			node: imageNode("azure.compute.galleryimageversion", "ami-1", oldCreation, awsrules.ImageStateAvailable, true, true, 0, 100),
			code: rules.SkipNotTargetAssetType,
		},
		{
			name: "missing image id",
			node: imageNode(awsrules.TypeImage, "", oldCreation, awsrules.ImageStateAvailable, true, true, 0, 100),
			code: rules.SkipMissingAttr,
		},
		{
			name: "unparseable creation time",
			node: imageNode(awsrules.TypeImage, "ami-1", "not-a-timestamp", awsrules.ImageStateAvailable, true, true, 0, 100),
			code: rules.SkipMissingAttr,
		},
		{
			name: "non available state",
			node: imageNode(awsrules.TypeImage, "ami-1", oldCreation, awsrules.ImageStatePending, true, true, 0, 100),
			code: rules.SkipNonBillingStatus,
		},
		{
			name: "backing incomplete",
			node: imageNode(awsrules.TypeImage, "ami-1", oldCreation, awsrules.ImageStateAvailable, false, true, 0, 100),
			code: rules.SkipMissingAttr,
		},
		{
			name: "references incomplete",
			node: imageNode(awsrules.TypeImage, "ami-1", oldCreation, awsrules.ImageStateAvailable, true, false, 0, 100),
			code: rules.SkipReferencesUnknown,
		},
		{
			name: "referenced by launch template",
			node: imageNode(awsrules.TypeImage, "ami-1", oldCreation, awsrules.ImageStateAvailable, true, true, 1, 100),
			code: rules.SkipAttached,
		},
		{
			name: "too young",
			node: imageNode(awsrules.TypeImage, "ami-1", youngCreation, awsrules.ImageStateAvailable, true, true, 0, 100),
			code: rules.SkipTooYoung,
		},
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

func TestEval_LaunchTemplateOnlyReferenceNotReported(t *testing.T) {
	n := imageNode(awsrules.TypeImage, "ami-0used", oldCreation, awsrules.ImageStateAvailable, true, true, 1, 100)
	n.SetAttr(awsrules.AttrReferenceSources, []string{"launch_template:lt-0a:1"})

	findings, skips := runEval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 0 {
		t.Fatalf("launch-template-referenced AMI must not be reported, got %+v", findings)
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached recorded once, got %+v", skips)
	}
}

func TestEval_UnreferencedOldAMIFires(t *testing.T) {
	n := imageNode(awsrules.TypeImage, "ami-0unused", oldCreation, awsrules.ImageStateAvailable, true, true, 0, 100)
	n.SetAttr(awsrules.AttrReferenceSources, []string{})

	findings, skips := runEval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	wantWaste := 100 * 0.05
	if f.MonthlyWasteUSD != wantWaste {
		t.Errorf("MonthlyWasteUSD = %v, want %v (100 GiB * $0.05/GiB-month)", f.MonthlyWasteUSD, wantWaste)
	}
	if f.Confidence != 0.6 {
		t.Errorf("Confidence = %v, want 0.6 (source_volume_size basis)", f.Confidence)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing resource, got %+v", skips)
	}
}

func TestEval_MissingReferenceCountMeansZero(t *testing.T) {
	n := imageNode(awsrules.TypeImage, "ami-0unused", oldCreation, awsrules.ImageStateAvailable, true, true, 0, 100)
	delete(n.Attrs, awsrules.AttrReferenceCount)

	findings, skips := runEval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 1 {
		t.Fatalf("want 1 finding when reference_count is absent (absence means zero), got %+v", findings)
	}
	if len(skips) != 0 {
		t.Errorf("expected zero skips for a firing resource, got %+v", skips)
	}
}

func TestEval_NoPriceSkips(t *testing.T) {
	n := imageNode(awsrules.TypeImage, "ami-0unused", oldCreation, awsrules.ImageStateAvailable, true, true, 0, 100)
	findings, skips := runEval(t, n, noPricePricer{})
	if len(findings) != 0 {
		t.Fatalf("want 0 findings when no price resolves, got %+v", findings)
	}
	if skips[rules.SkipNoPrice] != 1 {
		t.Errorf("want SkipNoPrice recorded once, got %+v", skips)
	}
}
