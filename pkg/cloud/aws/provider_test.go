package aws

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// newTestLogger returns a discard logger for tests that drive code paths that
// write log lines (Ingest's completion line).
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// loadTestFixture loads the shipped testdata fixture.
func loadTestFixture(t *testing.T) *Fixture {
	t.Helper()
	f, err := LoadFixture(filepath.Join("testdata", "aws-ec2-fixture.json"))
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	return f
}

// TestNew_OfflineConstructsZeroSDKClients is the AWS analog of the GCP
// regression: an offline AWS provider must construct zero SDK clients, so
// `tellury scan --fixture aws.json --provider aws --aws-account <id>` runs on
// a host with no AWS credentials at all.
func TestNew_OfflineConstructsZeroSDKClients(t *testing.T) {
	p, err := New(context.Background(), WithOffline(), WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("offline aws.New must succeed with no credentials: %v", err)
	}
	defer p.Close()
	if p.awsCfg.Region != "" {
		t.Errorf("offline provider must not resolve an AWS region (no SDK config load)")
	}
}

// TestIngest_FixtureRegionsAndNodes ingests the shipped fixture end to end and
// pins the graph shape: account container -> region containers -> volumes and
// addresses, with instance placeholder nodes for attachments. This is the
// "something works end to end first" acceptance test — no credentials, no
// network, just the Describe calls driven through the fixture fake and the
// normalizers.
func TestIngest_FixtureRegionsAndNodes(t *testing.T) {
	p, err := New(context.Background(),
		WithOffline(),
		WithFixture(loadTestFixture(t)),
		WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	sc := cloud.Scope{Provider: "aws", AWS: &cloud.AWSScope{Account: "123456789012"}}
	gr, err := p.Ingest(context.Background(), sc, nil)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Region coverage and source from the fixture (canonicalised and sorted).
	regions, source := p.Regions()
	if source != "fixture" {
		t.Errorf("region source = %q, want fixture", source)
	}
	wantRegions := []string{"eu-west-1", "us-east-1"}
	if len(regions) != len(wantRegions) {
		t.Fatalf("regions = %v, want %v", regions, wantRegions)
	}
	for i := range wantRegions {
		if regions[i] != wantRegions[i] {
			t.Fatalf("regions = %v, want %v", regions, wantRegions)
		}
	}

	// Hierarchy: one account container, one region per covered region.
	if got := gr.CountByKind(graph.KindAccount); got != 1 {
		t.Errorf("account containers = %d, want 1", got)
	}
	if got := gr.CountByKind(graph.KindRegion); got != 2 {
		t.Errorf("region containers = %d, want 2", got)
	}
	if got := gr.CountByKind(graph.KindDisk); got != 3 {
		t.Errorf("disk nodes = %d, want 3 (two in us-east-1, one in eu-west-1)", got)
	}
	if got := gr.CountByKind(graph.KindAddress); got != 2 {
		t.Errorf("address nodes = %d, want 2", got)
	}
	if got := gr.CountByKind(graph.KindInstance); got != 2 {
		t.Errorf("instance nodes = %d, want 2 (one per attached volume)", got)
	}
	if got := gr.ResourceNodeCount(); got != 7 {
		t.Errorf("ResourceNodeCount = %d, want 7 (3 disks + 2 addresses + 2 instances)", got)
	}

	// Edge topology: 5 resource->region contains, 2 region->account contains,
	// 2 instance->volume attached_to.
	if got := gr.EdgeCount(); got != 9 {
		t.Errorf("EdgeCount = %d, want 9", got)
	}

	// The named nodes exist with the right containment path.
	accountID := graph.Ref("accounts/123456789012")
	if _, ok := gr.Node(accountID); !ok {
		t.Fatalf("account container node %s missing", accountID)
	}
	regionID := graph.Ref("accounts/123456789012/regions/us-east-1")
	if n, ok := gr.Node(regionID); !ok || n.Kind != graph.KindRegion || n.Name != "us-east-1" {
		t.Errorf("us-east-1 region node = %#v, want Kind=region Name=us-east-1", n)
	}
	volID := graph.Ref("accounts/123456789012/regions/us-east-1/volumes/vol-0aaa")
	if n, ok := gr.Node(volID); !ok || n.Kind != graph.KindDisk {
		t.Errorf("volume node %s missing or wrong kind", volID)
	} else if got, _ := n.Str(AttrState); got != "available" {
		t.Errorf("vol-0aaa state = %q, want available", got)
	}
}

// TestIngest_ExplicitRegionsOverrideFixture: --aws-regions narrows the scan
// and reports source "explicit", even on the offline path. An
// availability-zone form is canonicalised to its region.
func TestIngest_ExplicitRegionsOverrideFixture(t *testing.T) {
	p, err := New(context.Background(),
		WithOffline(),
		WithFixture(loadTestFixture(t)),
		WithExplicitRegions([]string{"us-east-1a"}),
		WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	sc := cloud.Scope{Provider: "aws", AWS: &cloud.AWSScope{Account: "123456789012"}}
	gr, err := p.Ingest(context.Background(), sc, nil)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	regions, source := p.Regions()
	if source != "explicit" {
		t.Errorf("region source = %q, want explicit", source)
	}
	if len(regions) != 1 || regions[0] != "us-east-1" {
		t.Errorf("regions = %v, want [us-east-1] (canonicalised from us-east-1a)", regions)
	}
	if got := gr.CountByKind(graph.KindRegion); got != 1 {
		t.Errorf("region containers = %d, want 1", got)
	}
	if got := gr.CountByKind(graph.KindDisk); got != 2 {
		t.Errorf("disk nodes = %d, want 2 (us-east-1 only)", got)
	}
	if got := gr.CountByKind(graph.KindAddress); got != 2 {
		t.Errorf("address nodes = %d, want 2 (us-east-1 only)", got)
	}
}

// TestIngest_RequiresAccount: an AWS Ingest with no account fails even on the
// offline path — the provider has no way to construct a meaningful node or
// reconcile a fixture's data to an account without one.
func TestIngest_RequiresAccount(t *testing.T) {
	p, err := New(context.Background(),
		WithOffline(),
		WithFixture(loadTestFixture(t)),
		WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	if _, err := p.Ingest(context.Background(), cloud.Scope{Provider: "aws", AWS: &cloud.AWSScope{}}, nil); err == nil {
		t.Fatal("Ingest with no account must fail")
	}
}

// TestIngest_RejectsNilAWSScope: a non-AWS (or nil) scope block is rejected.
func TestIngest_RejectsNilAWSScope(t *testing.T) {
	p, err := New(context.Background(), WithOffline(), WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	if _, err := p.Ingest(context.Background(), cloud.Scope{Provider: "aws"}, nil); err == nil {
		t.Fatal("Ingest with no AWS scope block must fail")
	}
}
