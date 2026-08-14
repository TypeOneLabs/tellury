package unused_custom_image

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
	if kind == pricing.KindImageStorage && sku == ImageStorageSKU {
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

// imageNode builds a custom-image node. storedGiB is the BILLABLE archive
// size in GiB; the helper writes a deliberately much larger source disk size
// so any test that accidentally prices diskSizeGb instead of archiveSizeBytes
// produces an obviously wrong number.
func imageNode(assetType string, storedGiB float64, creation string, status string, refsComplete bool, refCount float64) *graph.Node {
	n := &graph.Node{
		ID:        graph.Ref("//compute.googleapis.com/projects/p/global/images/img-1"),
		Kind:      graph.KindImage,
		Name:      "img-1",
		Provider:  "gcp",
		Service:   "compute",
		AssetType: assetType,
		Project:   "p",
		Location:  "us-central1",
		Attrs:     map[string]any{},
	}
	n.SetAttr(gcprules.AttrImageID, "123")
	n.SetAttr(gcprules.AttrStorageBytes, storedGiB*(1<<30))
	n.SetAttr(gcprules.AttrSourceDiskSizeGB, storedGiB*10)
	n.SetAttr(gcprules.AttrCreationTime, creation)
	n.SetAttr(gcprules.AttrStatus, status)
	n.SetAttr(gcprules.AttrStorageLocation, "us-central1")
	n.SetAttr(gcprules.AttrReferencesComplete, refsComplete)
	n.SetAttr(gcprules.AttrReferenceCount, refCount)
	n.SetAttr(gcprules.AttrReferenceSources, []string{})
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

// TestEval_SourceDiskSizeFallback pins the behaviour that makes this rule fire
// at all on real images.
//
// Measured against the live API on 2026-08-14: GCP populates archiveSizeBytes
// only for images imported from a tar.gz (they carry a rawDisk block). An image
// created with --source-disk has neither rawDisk nor archiveSizeBytes, in
// images.get and in the Cloud Asset Inventory payload alike — so requiring the
// archive size meant the rule silently skipped the images operators actually
// have.
//
// The fallback prices the source disk size instead, which is an UPPER BOUND
// because GCP bills the compressed archive. The two things that must hold are
// that the bound is labelled as such in size_basis, and that it carries a lower
// confidence than the billed figure.
func TestEval_SourceDiskSizeFallback(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)

	t.Run("archive size present wins", func(t *testing.T) {
		// imageNode writes a source disk 10x the archive, so pricing the wrong
		// one is off by an order of magnitude rather than subtly wrong.
		n := imageNode(gcprules.TypeImage, 100, old, gcprules.StatusReady, true, 0)
		findings, _ := runEval(t, n, fakePricer{unit: 0.05})
		if len(findings) != 1 {
			t.Fatalf("want 1 finding, got %d", len(findings))
		}
		if got := findings[0].MonthlyWasteUSD; got != 5.00 {
			t.Errorf("waste = %v, want 5.00 (100 GiB archive x $0.05); "+
				"50.00 would mean the 1000 GiB source disk was priced", got)
		}
		// Literal, not the constant: asserting against the same constant the
		// rule reads makes the check move with the code and pin nothing.
		if got := findings[0].Confidence; got != 0.7 {
			t.Errorf("confidence = %v, want 0.7 for a billed figure", got)
		}
		if got := evidenceValue(findings[0], "size_basis"); got != basisArchive {
			t.Errorf("size_basis = %q, want %q", got, basisArchive)
		}
	})

	t.Run("falls back to the source disk when the archive size is absent", func(t *testing.T) {
		n := imageNode(gcprules.TypeImage, 100, old, gcprules.StatusReady, true, 0)
		delete(n.Attrs, gcprules.AttrStorageBytes) // exactly what CAI returns
		findings, skips := runEval(t, n, fakePricer{unit: 0.05})
		if len(findings) != 1 {
			t.Fatalf("want 1 finding, got %d (skips %+v): a disk-sourced image "+
				"must not be silently skipped", len(findings), skips)
		}
		if got := findings[0].MonthlyWasteUSD; got != 50.00 {
			t.Errorf("waste = %v, want 50.00 (1000 GiB source disk x $0.05)", got)
		}
		if got := findings[0].Confidence; got != 0.6 {
			t.Errorf("confidence = %v, want 0.6 — a bound must not claim the "+
				"confidence of a billed figure", got)
		}
		// The invariant that actually matters, stated independently of both
		// literals: a bounded figure is always less certain than a billed one.
		if confidenceSourceDisk >= confidenceArchive {
			t.Errorf("confidenceSourceDisk (%v) must be strictly below confidenceArchive (%v)",
				confidenceSourceDisk, confidenceArchive)
		}
		if got := evidenceValue(findings[0], "size_basis"); got != basisSourceDisk {
			t.Errorf("size_basis = %q, want %q: a reader cannot tell a bound "+
				"from a bill without it", got, basisSourceDisk)
		}
	})
}

func evidenceValue(f rules.Finding, key string) string {
	for _, e := range f.Evidence {
		if e.Key == key {
			return e.Value
		}
	}
	return ""
}

func TestEval_EveryGuardSkipPath(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	young := now.AddDate(0, 0, -10).Format(time.RFC3339)
	valid := func() *graph.Node {
		return imageNode(gcprules.TypeImage, 100, old, gcprules.StatusReady, true, 0)
	}

	tests := []struct {
		name string
		node *graph.Node
		code rules.SkipCode
	}{
		{"wrong asset type", imageNode(gcprules.TypeMachineImage, 100, old, gcprules.StatusReady, true, 0), rules.SkipNotTargetAssetType},
		// Neither size is available: the payload is genuinely unreadable.
		// Dropping ONLY archiveSizeBytes is the common case and now falls back
		// to the source disk size — see TestEval_SourceDiskSizeFallback.
		{"no size at all", func() *graph.Node {
			n := valid()
			delete(n.Attrs, gcprules.AttrStorageBytes)
			delete(n.Attrs, gcprules.AttrSourceDiskSizeGB)
			return n
		}(), rules.SkipMissingAttr},
		{"no archive size and a zero source disk", func() *graph.Node {
			n := valid()
			delete(n.Attrs, gcprules.AttrStorageBytes)
			n.SetAttr(gcprules.AttrSourceDiskSizeGB, 0.0)
			return n
		}(), rules.SkipMissingAttr},
		{"unparseable creation time", func() *graph.Node { n := valid(); n.SetAttr(gcprules.AttrCreationTime, "not-a-time"); return n }(), rules.SkipMissingAttr},
		{"non ready status", imageNode(gcprules.TypeImage, 100, old, "PENDING", true, 0), rules.SkipNonBillingStatus},
		{"missing storage location", func() *graph.Node { n := valid(); delete(n.Attrs, gcprules.AttrStorageLocation); return n }(), rules.SkipMissingAttr},
		{"references incomplete", imageNode(gcprules.TypeImage, 100, old, gcprules.StatusReady, false, 0), rules.SkipReferencesUnknown},
		{"referenced by instance template", imageNode(gcprules.TypeImage, 100, old, gcprules.StatusReady, true, 1), rules.SkipAttached},
		{"too young", imageNode(gcprules.TypeImage, 100, young, gcprules.StatusReady, true, 0), rules.SkipTooYoung},
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

func TestEval_InstanceTemplateOnlyReferenceNotReported(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	n := imageNode(gcprules.TypeImage, 100, old, gcprules.StatusReady, true, 1)
	n.SetAttr(gcprules.AttrReferenceSources, []string{"instance_template"})

	findings, skips := runEval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 0 {
		t.Fatalf("instance-template-referenced image must not be reported, got %+v", findings)
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached recorded once, got %+v", skips)
	}
}

func TestEval_UnreferencedOldImageFiresWithStoredBytesCost(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	// 100 GiB stored bytes, but source disk is 1000 GiB. A defect that prices
	// diskSizeGb would report $50.00 instead of the correct $5.00.
	n := imageNode(gcprules.TypeImage, 100, old, gcprules.StatusReady, true, 0)
	n.SetAttr(gcprules.AttrReferenceSources, []string{})

	findings, skips := runEval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d (%+v)", len(findings), findings)
	}
	f := findings[0]
	if f.ResourceID != n.ID {
		t.Fatalf("finding for wrong resource: %s", f.ResourceID)
	}
	if f.MonthlyWasteUSD != 5.00 {
		t.Errorf("MonthlyWasteUSD = %v, want 5.00 (100 GiB stored bytes * $0.05/GiB-month)", f.MonthlyWasteUSD)
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
	if byKey["source_disk_size_gb"] != "1000" {
		t.Errorf("evidence source_disk_size_gb = %q, want 1000 (kept as evidence, never priced)", byKey["source_disk_size_gb"])
	}
	if byKey["size_basis"] != "archive_size_bytes" {
		t.Errorf("evidence size_basis = %q, want archive_size_bytes", byKey["size_basis"])
	}
	if byKey["unit_price_gib_month"] != "$0.0500" {
		t.Errorf("evidence unit_price_gib_month = %q, want $0.0500", byKey["unit_price_gib_month"])
	}
	if !strings.Contains(byKey["price_source"], "sku=standard") {
		t.Errorf("evidence price_source = %q, want it to name sku=standard", byKey["price_source"])
	}
}

func TestEval_MissingReferenceCountMeansZero(t *testing.T) {
	old := now.AddDate(0, 0, -120).Format(time.RFC3339)
	n := imageNode(gcprules.TypeImage, 100, old, gcprules.StatusReady, true, 0)
	delete(n.Attrs, gcprules.AttrReferenceCount)

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
	n := imageNode(gcprules.TypeImage, 100, old, gcprules.StatusReady, true, 0)
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
	n := imageNode(gcprules.TypeImage, 1, old, gcprules.StatusReady, true, 0)
	findings, skips := runEval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 0 {
		t.Fatalf("want 0 findings below the noise floor, got %+v", findings)
	}
	if skips[rules.SkipBelowMinWaste] != 1 {
		t.Errorf("want SkipBelowMinWaste recorded once, got %+v", skips)
	}
}
