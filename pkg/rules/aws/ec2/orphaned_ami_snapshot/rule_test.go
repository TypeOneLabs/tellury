package orphaned_ami_snapshot

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
type fakePricer struct{ unit float64 }

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

func (noPricePricer) UnitPrice(pricing.Kind, string, string, string) (float64, string, error) {
	return 0, "", pricing.ErrNoPrice
}
func (noPricePricer) MonthlyCost(pricing.Item) (float64, error) { return 0, pricing.ErrNoPrice }

var fixedNow = time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)

// snapshotNode builds a node exactly as pkg/cloud/aws NormalizeSnapshot does,
// writing every attribute unconditionally so absence is distinguishable from
// zero.
func snapshotNode(assetType, id, creation, state string, amiCreated, refsComplete bool, refCount, sizeGB float64) *graph.Node {
	n := &graph.Node{
		ID:        graph.Ref("accounts/123456789012/regions/us-east-1/snapshots/" + id),
		Kind:      graph.KindSnapshot,
		Name:      id,
		Provider:  "aws",
		Service:   "ec2",
		AssetType: assetType,
		Project:   "123456789012",
		Location:  "us-east-1",
		Attrs:     map[string]any{},
	}
	n.SetAttr(awsrules.AttrSnapshotID, id)
	n.SetAttr(awsrules.AttrVolumeSizeGB, sizeGB)
	n.SetAttr(awsrules.AttrCreationTimestamp, creation)
	n.SetAttr(awsrules.AttrState, state)
	n.SetAttr(awsrules.AttrAMICreated, amiCreated)
	n.SetAttr(awsrules.AttrAMIReferenceComplete, refsComplete)
	n.SetAttr(awsrules.AttrReferencedByAMICount, refCount)
	return n
}

func eval(t *testing.T, n *graph.Node, price pricing.Pricer) ([]rules.Finding, map[rules.SkipCode]int) {
	t.Helper()
	g := graph.New()
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.Freeze()
	skips := map[rules.SkipCode]int{}
	p := &rules.Pass{
		Graph: g,
		Price: price,
		Now:   fixedNow,
		Skip:  func(_ string, _ graph.Ref, code rules.SkipCode) { skips[code]++ },
	}
	findings, err := rules.AdaptNodeRule(rule{}).Eval(context.Background(), p)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	return findings, skips
}

const (
	oldEnough = "2025-01-01T00:00:00Z" // ~590 days before fixedNow
	tooYoung  = "2026-08-01T00:00:00Z" // 13 days
)

func TestEval_ReportsOrphanedAMISnapshot(t *testing.T) {
	n := snapshotNode(awsrules.TypeSnapshot, "snap-1", oldEnough, awsrules.SnapshotStateCompleted, true, true, 0, 100)
	findings, _ := eval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	if got := findings[0].MonthlyWasteUSD; got != 5.00 {
		t.Errorf("waste = %v, want 5.00 (100 GiB x $0.05/GiB-month)", got)
	}
	if got := findings[0].RuleID; got != ID {
		t.Errorf("rule id = %q, want %q", got, ID)
	}
}

// TestEval_SkipPaths pins every guard. The two that matter most are
// ami_reference_complete and not_referenced_by_ami: between them they are the
// difference between reporting a genuinely orphaned snapshot and recommending
// the deletion of a snapshot that still backs a live AMI, which destroys it.
func TestEval_SkipPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		node *graph.Node
		want rules.SkipCode
	}{
		{
			"not a snapshot asset type",
			snapshotNode("aws.ec2.volume", "snap-1", oldEnough, awsrules.SnapshotStateCompleted, true, true, 0, 100),
			rules.SkipNotTargetAssetType,
		},
		{
			"still pending, size not final",
			snapshotNode(awsrules.TypeSnapshot, "snap-1", oldEnough, awsrules.SnapshotStatePending, true, true, 0, 100),
			rules.SkipNonBillingStatus,
		},
		{
			// A hand-made or backup-tool snapshot. Deleting it is a different
			// decision with different risk, and this rule declines to make it.
			"not AMI-created",
			snapshotNode(awsrules.TypeSnapshot, "snap-1", oldEnough, awsrules.SnapshotStateCompleted, false, true, 0, 100),
			rules.SkipNotAMISnapshot,
		},
		{
			// DescribeImages could not be read: a zero reference count means
			// nothing, so concluding "orphaned" would be a guess with an
			// irreversible consequence.
			"AMI inventory unreadable",
			snapshotNode(awsrules.TypeSnapshot, "snap-1", oldEnough, awsrules.SnapshotStateCompleted, true, false, 0, 100),
			rules.SkipReferencesUnknown,
		},
		{
			"still referenced by a live AMI",
			snapshotNode(awsrules.TypeSnapshot, "snap-1", oldEnough, awsrules.SnapshotStateCompleted, true, true, 1, 100),
			rules.SkipAttached,
		},
		{
			"too young — an AMI rebuild may re-register it",
			snapshotNode(awsrules.TypeSnapshot, "snap-1", tooYoung, awsrules.SnapshotStateCompleted, true, true, 0, 100),
			rules.SkipTooYoung,
		},
		{
			"size absent",
			snapshotNode(awsrules.TypeSnapshot, "snap-1", oldEnough, awsrules.SnapshotStateCompleted, true, true, 0, 0),
			rules.SkipMissingAttr,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings, skips := eval(t, tc.node, fakePricer{unit: 0.05})
			if len(findings) != 0 {
				t.Fatalf("findings = %d, want 0", len(findings))
			}
			if skips[tc.want] != 1 {
				t.Errorf("skip %q = %d, want 1 (got %v)", tc.want, skips[tc.want], skips)
			}
		})
	}
}

// TestEval_MissingReferenceCountIsNotZero pins the absence-versus-zero
// discipline. A node whose reference count was never written must skip, not be
// read as "nothing references it" — the normalizer writes it unconditionally,
// so its absence means the payload was not parsed.
func TestEval_MissingReferenceCountIsNotZero(t *testing.T) {
	n := snapshotNode(awsrules.TypeSnapshot, "snap-1", oldEnough, awsrules.SnapshotStateCompleted, true, true, 0, 100)
	delete(n.Attrs, awsrules.AttrReferencedByAMICount)
	findings, skips := eval(t, n, fakePricer{unit: 0.05})
	if len(findings) != 0 {
		t.Fatalf("findings = %d, want 0: an unwritten reference count was read as zero", len(findings))
	}
	if skips[rules.SkipAttached] != 1 {
		t.Errorf("want SkipAttached for an absent count, got %v", skips)
	}
}

// TestEval_NoPriceSkipsRatherThanReportingZero pins that an unresolvable price
// never becomes a $0 finding, which would read as free.
func TestEval_NoPriceSkipsRatherThanReportingZero(t *testing.T) {
	n := snapshotNode(awsrules.TypeSnapshot, "snap-1", oldEnough, awsrules.SnapshotStateCompleted, true, true, 0, 100)
	findings, skips := eval(t, n, noPricePricer{})
	if len(findings) != 0 {
		t.Fatalf("findings = %d, want 0", len(findings))
	}
	if skips[rules.SkipNoPrice] != 1 {
		t.Errorf("want SkipNoPrice, got %v", skips)
	}
}
