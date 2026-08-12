package aws

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2"

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
// pins the graph shape: account container -> region containers -> volumes,
// addresses, and instances. Instance nodes come from DescribeInstances (not
// just attachment stubs), enriched with InstanceTypeInfo from
// DescribeInstanceTypes. This is the "something works end to end first"
// acceptance test — no credentials, no network, just the Describe calls
// driven through the fixture fake and the normalizers.
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
	// Three instances from the fixture: i-0cafe, i-0dead, i-0d00d.
	if got := gr.CountByKind(graph.KindInstance); got != 3 {
		t.Errorf("instance nodes = %d, want 3 (fixture has i-0cafe, i-0dead, i-0d00d)", got)
	}
	if got := gr.ResourceNodeCount(); got != 8 {
		t.Errorf("ResourceNodeCount = %d, want 8 (3 disks + 2 addresses + 3 instances)", got)
	}

	// Edge topology:
	//   3 disks     -> region  (contains)
	//   2 addresses -> region  (contains)
	//   3 instances -> region  (contains)
	//   2 region    -> account (contains)
	//   2 instance  -> volume  (attached_to)
	if got := gr.EdgeCount(); got != 12 {
		t.Errorf("EdgeCount = %d, want 12", got)
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

	// Instance i-0cafe: enriched from DescribeInstances, shape resolved from
	// DescribeInstanceTypes, and the stub from the EBS attachment was
	// overwritten (instances processed last).
	instID := graph.Ref("accounts/123456789012/regions/us-east-1/instances/i-0cafe")
	n, ok := gr.Node(instID)
	if !ok {
		t.Fatalf("instance node %s missing", instID)
	}
	if n.Kind != graph.KindInstance {
		t.Errorf("i-0cafe Kind = %s, want instance", n.Kind)
	}
	if got, _ := n.Str(AttrInstanceType); got != "t3.medium" {
		t.Errorf("i-0cafe instance_type = %q, want t3.medium", got)
	}
	if got, _ := n.Str(AttrState); got != "running" {
		t.Errorf("i-0cafe state = %q, want running", got)
	}
	if got, _ := n.Num(AttrVCpuCount); got != 2 {
		t.Errorf("i-0cafe vcpu_count = %v, want 2", got)
	}
	if got, _ := n.Num(AttrMemoryGiB); got != 4 {
		t.Errorf("i-0cafe memory_gib = %v, want 4", got)
	}
	if got, _ := n.Str(AttrLifecycle); got != "" {
		t.Errorf("i-0cafe lifecycle = %q, want empty (on-demand)", got)
	}

	// Instance i-0dead: spot instance with no EBS attachment.
	instDeadID := graph.Ref("accounts/123456789012/regions/us-east-1/instances/i-0dead")
	nd, ok := gr.Node(instDeadID)
	if !ok {
		t.Fatalf("instance node %s missing", instDeadID)
	}
	if got, _ := nd.Str(AttrLifecycle); got != "spot" {
		t.Errorf("i-0dead lifecycle = %q, want spot", got)
	}
	if got, _ := nd.Str(AttrProvisioningModel); got != "SPOT" {
		t.Errorf("i-0dead provisioning_model = %q, want SPOT", got)
	}
	if got, _ := nd.Str(AttrInstanceType); got != "c6i.xlarge" {
		t.Errorf("i-0dead instance_type = %q, want c6i.xlarge", got)
	}
	if got, _ := nd.Num(AttrVCpuCount); got != 4 {
		t.Errorf("i-0dead vcpu_count = %v, want 4", got)
	}
	if got, _ := nd.Num(AttrMemoryGiB); got != 8 {
		t.Errorf("i-0dead memory_gib = %v, want 8 (8192 MiB / 1024)", got)
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
	if got := gr.CountByKind(graph.KindInstance); got != 2 {
		t.Errorf("instance nodes = %d, want 2 (i-0cafe, i-0dead in us-east-1)", got)
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

// TestAssetTypesToCloudFormation_MapsKnownTypes verifies the mapping from the
// provider's own asset-type tokens ("aws.ec2.volume", "aws.ec2.address") to
// CloudFormation resource type identifiers ("AWS::EC2::Volume",
// "AWS::EC2::EIP") that Resource Explorer Search expects. The mapping table
// must stay exhaustive for the types the current rules declare, so a missing
// mapping is a test failure — the scan silently falls back to DescribeRegions
// instead of narrowing.
func TestAssetTypesToCloudFormation_MapsKnownTypes(t *testing.T) {
	tests := []struct {
		hint string
		want string
	}{
		{"aws.ec2.volume", "AWS::EC2::Volume"},
		{"aws.ec2.address", "AWS::EC2::EIP"},
	}
	for _, tt := range tests {
		got := assetTypesToCloudFormation([]string{tt.hint})
		if len(got) != 1 || got[0] != tt.want {
			t.Errorf("assetTypesToCloudFormation(%q) = %v, want [%s]", tt.hint, got, tt.want)
		}
	}
}

// TestAssetTypesToCloudFormation_UnknownHintIsDropped: an asset-type hint
// that has no mapping is dropped, not passed through to Resource Explorer as
// a bogus type string that would return zero results.
func TestAssetTypesToCloudFormation_UnknownHintIsDropped(t *testing.T) {
	got := assetTypesToCloudFormation([]string{"aws.ec2.volume", "aws.s3.bucket", "aws.ec2.address"})
	want := []string{"AWS::EC2::Volume", "AWS::EC2::EIP"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("assetTypesToCloudFormation = %v, want %v (unknown hints dropped)", got, want)
	}
}

// TestAssetTypesToCloudFormation_EmptyInput: nil or empty slice returns nil.
func TestAssetTypesToCloudFormation_EmptyInput(t *testing.T) {
	if got := assetTypesToCloudFormation(nil); got != nil {
		t.Errorf("assetTypesToCloudFormation(nil) = %v, want nil", got)
	}
	if got := assetTypesToCloudFormation([]string{}); got != nil {
		t.Errorf("assetTypesToCloudFormation([]) = %v, want nil", got)
	}
}

// TestAssetTypesToCloudFormation_AllUnknown: when none of the hints map, the
// result is empty — the caller falls through to DescribeRegions.
func TestAssetTypesToCloudFormation_AllUnknown(t *testing.T) {
	got := assetTypesToCloudFormation([]string{"aws.s3.bucket", "aws.rds.instance"})
	if len(got) != 0 {
		t.Errorf("assetTypesToCloudFormation(all unknown) = %v, want empty (len 0)", got)
	}
}

// TestResolveRegions_ExplicitOverridesDiscovery: even with asset type hints,
// explicit --aws-regions wins and the source is "explicit". The discovery
// path is never consulted.
func TestResolveRegions_ExplicitOverridesDiscovery(t *testing.T) {
	p := &Provider{
		log:      newTestLogger(),
		offline:  false,
		explicit: []string{"us-east-1"},
	}
	regions, source, err := p.resolveRegions(context.Background(), []string{"aws.ec2.volume"})
	if err != nil {
		t.Fatalf("resolveRegions: %v", err)
	}
	if source != "explicit" {
		t.Errorf("source = %q, want explicit", source)
	}
	if len(regions) != 1 || regions[0] != "us-east-1" {
		t.Errorf("regions = %v, want [us-east-1]", regions)
	}
}

// TestResolveRegions_OfflineReturnsFixture: the offline path returns the
// fixture's regions with source "fixture", ignoring any asset type hints.
func TestResolveRegions_OfflineReturnsFixture(t *testing.T) {
	f := loadTestFixture(t)
	p := &Provider{
		log:     newTestLogger(),
		offline: true,
		fixture: f,
	}
	regions, source, err := p.resolveRegions(context.Background(), []string{"aws.ec2.volume"})
	if err != nil {
		t.Fatalf("resolveRegions: %v", err)
	}
	if source != "fixture" {
		t.Errorf("source = %q, want fixture", source)
	}
	want := canonicaliseRegions(f.RegionNames())
	if !reflect.DeepEqual(regions, want) {
		t.Errorf("regions = %v, want %v", regions, want)
	}
}

// TestResolveRegions_DiscoveryNarrowsRegions: when the discoverer returns
// regions, those regions are used and the source is "resource_explorer".
func TestResolveRegions_DiscoveryNarrowsRegions(t *testing.T) {
	fake := newDiscovererWithClient(loadSearchFixture(t))
	p := &Provider{
		log:        newTestLogger(),
		offline:    false,
		discoverer: fake,
	}
	regions, source, err := p.resolveRegions(context.Background(), []string{"aws.ec2.volume", "aws.ec2.address"})
	if err != nil {
		t.Fatalf("resolveRegions: %v", err)
	}
	if source != "resource_explorer" {
		t.Errorf("source = %q, want resource_explorer", source)
	}
	want := []string{"eu-west-1", "us-east-1"}
	sort.Strings(regions)
	if !reflect.DeepEqual(regions, want) {
		t.Errorf("regions = %v, want %v", regions, want)
	}
}

// TestResolveRegions_DiscoveryEmptyResultFallsBack: when discovery returns
// zero regions (valid index, no matching resources), the provider falls back
// to DescribeRegions.
func TestResolveRegions_DiscoveryEmptyResultFallsBack(t *testing.T) {
	emptyDiscoverer := newDiscovererWithClient(&fakeResourceExplorer{
		pages: []*resourceexplorer2.SearchOutput{
			{Resources: nil},
		},
	})

	p := &Provider{
		log:        newTestLogger(),
		offline:    true,
		discoverer: emptyDiscoverer,
		fixture:    loadTestFixture(t),
	}

	regions, source, err := p.resolveRegions(context.Background(), []string{"aws.ec2.volume"})
	if err != nil {
		t.Fatalf("resolveRegions: %v", err)
	}
	if source != "fixture" {
		t.Errorf("source = %q, want fixture (offline path after discovery returned empty)", source)
	}
	if len(regions) != 2 {
		t.Errorf("regions = %v, want 2 regions from fixture fallback", regions)
	}
}

// TestResolveRegions_NoHintsUsesDescribeRegions: when asset type hints are
// empty or nil, the discovery path is skipped entirely and the provider falls
// through to DescribeRegions.
func TestResolveRegions_NoHintsUsesDescribeRegions(t *testing.T) {
	p := &Provider{
		log:     newTestLogger(),
		offline: true,
		fixture: loadTestFixture(t),
	}
	regions, source, err := p.resolveRegions(context.Background(), nil)
	if err != nil {
		t.Fatalf("resolveRegions: %v", err)
	}
	if source != "fixture" {
		t.Errorf("source = %q, want fixture (offline, no hints)", source)
	}
	if len(regions) != 2 {
		t.Errorf("regions = %v, want 2 regions from fixture", regions)
	}
}
