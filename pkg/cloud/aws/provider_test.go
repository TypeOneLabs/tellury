package aws

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2/types"

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
	if p.discoverer != nil {
		t.Errorf("offline provider must not construct a Resource Explorer discoverer")
	}
}

// TestNew_DefaultDoesNotConstructDiscoverer is the read-only guarantee made
// mechanical: the default AWS provider (no --aws-use-resource-explorer) must
// not construct a Discoverer at all, so a default scan can never call
// Resource Explorer Search or ListIndexes.
func TestNew_DefaultDoesNotConstructDiscoverer(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-east-1")

	p, err := New(context.Background(), WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("New with static env credentials failed: %v", err)
	}
	defer p.Close()

	if p.useResourceExplorer {
		t.Error("default provider must have useResourceExplorer=false")
	}
	if p.discoverer != nil {
		t.Error("default provider must not construct a Resource Explorer Discoverer")
	}
}

// TestNew_UseResourceExplorerConstructsDiscoverer pins the opt-in side: when
// --aws-use-resource-explorer is set, the provider does construct a Discoverer.
func TestNew_UseResourceExplorerConstructsDiscoverer(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-east-1")

	p, err := New(context.Background(), WithLogger(newTestLogger()), WithUseResourceExplorer(true))
	if err != nil {
		t.Fatalf("New with Resource Explorer enabled failed: %v", err)
	}
	defer p.Close()

	if !p.useResourceExplorer {
		t.Error("provider must have useResourceExplorer=true")
	}
	if p.discoverer == nil {
		t.Error("provider with Resource Explorer enabled must construct a Discoverer")
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

// The mapping must cover EVERY asset type an AWS rule can declare.
//
// There is no DescribeRegions sweep behind Resource Explorer, but the default
// mode does use DescribeRegions. For the opt-in Resource Explorer path, an
// unmapped type means resolveRegions refuses to narrow at all, because a
// region holding only that type would otherwise be dropped silently. The old
// table mapped three of the five types the rules declare — instance and
// snapshot were missing — which under the current design would refuse every
// Resource Explorer scan.
func TestAssetTypesToResourceExplorer_CoversEveryRuleAssetType(t *testing.T) {
	// The asset types the AWS rules declare today.
	for _, hint := range []string{
		"aws.ec2.volume",
		"aws.ec2.address",
		"aws.ec2.image",
		"aws.ec2.instance",
		"aws.ec2.snapshot",
	} {
		mapped, unmapped := assetTypesToResourceExplorer([]string{hint})
		if len(unmapped) != 0 {
			t.Errorf("asset type %q has no Resource Explorer mapping; scans requesting it "+
				"cannot resolve regions through Resource Explorer", hint)
			continue
		}
		if len(mapped) != 1 {
			t.Errorf("assetTypesToResourceExplorer(%q) = %v, want one type", hint, mapped)
		}
	}
}

// The type strings are the ones Search RETURNS. CloudFormation-style aliases
// are not reliably accepted: measured 2026-08-14, "AWS::EC2::Volume" resolved
// 3 hits while "AWS::EC2::Image" resolved 0 where "ec2:image" resolved 1 — a
// wrong alias returns no error, just nothing.
func TestAssetTypesToResourceExplorer_UsesNativeTypeStrings(t *testing.T) {
	for hint, want := range map[string]string{
		"aws.ec2.volume":   "ec2:volume",
		"aws.ec2.address":  "ec2:elastic-ip",
		"aws.ec2.image":    "ec2:image",
		"aws.ec2.instance": "ec2:instance",
		"aws.ec2.snapshot": "ec2:snapshot",
	} {
		mapped, _ := assetTypesToResourceExplorer([]string{hint})
		if len(mapped) != 1 || mapped[0] != want {
			t.Errorf("assetTypesToResourceExplorer(%q) = %v, want [%s]", hint, mapped, want)
		}
		if strings.HasPrefix(want, "AWS::") {
			t.Errorf("mapping for %q uses a CloudFormation alias", hint)
		}
	}
}

// An unmapped hint is REPORTED, not dropped. Dropping it silently is what let
// a scan narrow to regions chosen by a subset of the types it needed.
func TestAssetTypesToResourceExplorer_ReportsUnmapped(t *testing.T) {
	mapped, unmapped := assetTypesToResourceExplorer([]string{"aws.ec2.volume", "aws.rds.instance"})
	if !reflect.DeepEqual(mapped, []string{"ec2:volume"}) {
		t.Errorf("mapped = %v, want [ec2:volume]", mapped)
	}
	if !reflect.DeepEqual(unmapped, []string{"aws.rds.instance"}) {
		t.Errorf("unmapped = %v, want [aws.rds.instance] — an unmappable type must be "+
			"named so the caller can refuse to narrow", unmapped)
	}
}

func TestAssetTypesToResourceExplorer_EmptyInput(t *testing.T) {
	mapped, unmapped := assetTypesToResourceExplorer(nil)
	if mapped != nil || unmapped != nil {
		t.Errorf("assetTypesToResourceExplorer(nil) = %v, %v, want nil, nil", mapped, unmapped)
	}
}

// TestResolveRegions_ExplicitOverridesDiscovery: even with asset type hints
// and the Resource Explorer flag enabled, explicit --aws-regions wins and the
// source is "explicit". The discovery path is never consulted.
func TestResolveRegions_ExplicitOverridesDiscovery(t *testing.T) {
	p := &Provider{
		log:                 newTestLogger(),
		offline:             false,
		useResourceExplorer: true,
		explicit:            []string{"us-east-1"},
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

// TestResolveRegions_DiscoveryNarrowsRegions: when the Resource Explorer flag
// is on and the discoverer returns regions, those regions are used and the
// source is "resource_explorer".
func TestResolveRegions_DiscoveryNarrowsRegions(t *testing.T) {
	fake := newDiscovererWithClient(loadSearchFixture(t))
	p := &Provider{
		log:                 newTestLogger(),
		offline:             false,
		useResourceExplorer: true,
		discoverer:          fake,
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

// TestResolveRegions_OfflineWinsOverDiscovery: the offline path precedes
// Resource Explorer, even when the flag is on and a discoverer is present.
func TestResolveRegions_OfflineWinsOverDiscovery(t *testing.T) {
	emptyDiscoverer := newDiscovererWithClient(&fakeResourceExplorer{
		pages: []*resourceexplorer2.SearchOutput{
			{Resources: nil},
		},
	})

	p := &Provider{
		log:                 newTestLogger(),
		offline:             true,
		useResourceExplorer: true,
		discoverer:          emptyDiscoverer,
		fixture:             loadTestFixture(t),
	}

	regions, source, err := p.resolveRegions(context.Background(), []string{"aws.ec2.volume"})
	if err != nil {
		t.Fatalf("resolveRegions: %v", err)
	}
	if source != "fixture" {
		t.Errorf("source = %q, want fixture (the offline path precedes discovery)", source)
	}
	if len(regions) != 2 {
		t.Errorf("regions = %v, want 2 regions from fixture fallback", regions)
	}
}

// TestResolveRegions_OfflineNoHintsUsesFixture: when asset type hints are
// empty or nil, the discovery path is skipped entirely and the provider
// returns the fixture regions.
func TestResolveRegions_OfflineNoHintsUsesFixture(t *testing.T) {
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

// TestResolveRegions_DefaultDescribeRegionsSweep pins the restored default:
// with neither --aws-regions nor --aws-use-resource-explorer, the provider
// enumerates enabled regions through ec2:DescribeRegions and reports source
// "describe_regions".
func TestResolveRegions_DefaultDescribeRegionsSweep(t *testing.T) {
	fix := loadTestFixture(t)
	p := &Provider{log: newTestLogger()}
	factory := func(region string) ec2API { return &fakeEC2{region: region, f: fix} }

	regions, source, err := p.resolveRegionsWithClient(
		context.Background(), factory, nil, []string{"aws.ec2.volume"})
	if err != nil {
		t.Fatalf("resolveRegionsWithClient: %v", err)
	}
	if source != "describe_regions" {
		t.Errorf("source = %q, want describe_regions", source)
	}
	want := canonicaliseRegions(fix.RegionNames())
	if !reflect.DeepEqual(regions, want) {
		t.Errorf("regions = %v, want %v", regions, want)
	}
}

// TestResolveRegions_DefaultMakesNoResourceExplorerCalls proves mechanically
// that the default path never calls Resource Explorer. The discoverer passed
// here would error on any Search or ListIndexes call; the fact that the
// resolution succeeds with a describe_regions source is the assertion.
func TestResolveRegions_DefaultMakesNoResourceExplorerCalls(t *testing.T) {
	fix := loadTestFixture(t)
	f := &fakeResourceExplorer{
		err:            errors.New("Search was called"),
		listIndexesErr: errors.New("ListIndexes was called"),
		aggregator:     true,
	}
	p := &Provider{log: newTestLogger()}
	factory := func(region string) ec2API { return &fakeEC2{region: region, f: fix} }

	regions, source, err := p.resolveRegionsWithClient(
		context.Background(), factory, newDiscovererWithClient(f), []string{"aws.ec2.volume"})
	if err != nil {
		t.Fatalf("resolveRegionsWithClient default path failed: %v", err)
	}
	if source != "describe_regions" {
		t.Errorf("source = %q, want describe_regions", source)
	}
	if len(f.queries) != 0 {
		t.Errorf("Resource Explorer Search was called %d times, want 0", len(f.queries))
	}
	if len(regions) == 0 {
		t.Error("default path returned no regions")
	}
}

// TestProvider_SizerIsWired pins that a provider hands rules a usable Sizer.
//
// This is deliberately a test of the WIRING, not of the ladder logic. The
// underutilized rule shipped with Provider.Sizer() returning nil, which made
// its rightsize branch unreachable in production: every overprovisioned
// instance was reported as stop/delete with the full cost as waste, even where
// a smaller sibling existed. The rule's own tests passed throughout, because
// they injected a fake Sizer — nothing asserted the real provider supplied one.
func TestProvider_SizerIsWired(t *testing.T) {
	p, err := New(context.Background(), WithOffline(), WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := p.Sizer()
	if s == nil {
		t.Fatal("Provider.Sizer() is nil: the rule's rightsize branch is unreachable and every finding degrades to stop/delete")
	}
	// And it must be usable: a swept family produces an ordered ladder.
	if err := p.sizer.LoadFamilies(context.Background(), &sizerFakeEC2{}, []string{"t3"}); err != nil {
		t.Fatalf("LoadFamilies: %v", err)
	}
	if got := len(s.Ladder("t3")); got == 0 {
		t.Error("Ladder is empty after a successful sweep")
	}
	if fam := s.Family("t3.micro"); fam != "t3" {
		t.Errorf("Family(t3.micro) = %q, want t3", fam)
	}
}

// The refusal paths for the opt-in Resource Explorer mode. Without a
// DescribeRegions sweep behind it, Resource Explorer must answer "which
// regions" correctly or not at all — every failure below returns an error
// naming its remedy, because the alternative is a short region list that
// looks exactly like a clean scan.

// No aggregator index is the COMMON case, and must not stop a scan: the
// discoverer sweeps the account's enabled regions, asking each one directly.
//
// The sweep needs a region list to sweep, which is the one DescribeRegions call
// that remains in Resource Explorer mode — it enumerates regions to ASK, and
// hydration still happens only where Resource Explorer found something.
func TestResolveRegions_NoAggregatorSweepsRegions(t *testing.T) {
	fix := loadTestFixture(t)
	f := &fakeResourceExplorer{
		aggregator: false,
		pages: []*resourceexplorer2.SearchOutput{{
			Resources: []types.Resource{
				{Region: aws.String("eu-west-1"), ResourceType: aws.String("ec2:volume")},
			},
		}},
	}
	p := &Provider{log: newTestLogger(), useResourceExplorer: true}
	factory := func(region string) ec2API { return &fakeEC2{region: region, f: fix} }

	regions, source, err := p.resolveRegionsWithClient(
		context.Background(), factory, newDiscovererWithClient(f), []string{"aws.ec2.volume"})
	if err != nil {
		t.Fatalf("resolveRegions: %v — a missing aggregator index must not stop the scan", err)
	}
	if source != "resource_explorer" {
		t.Errorf("source = %q, want resource_explorer", source)
	}
	if len(regions) != 1 || regions[0] != "eu-west-1" {
		t.Errorf("regions = %v, want [eu-west-1]", regions)
	}
	// One Search per region, so each region's own LOCAL index is consulted.
	if len(f.queries) != len(fix.RegionNames()) {
		t.Errorf("issued %d searches for %d enabled regions; the sweep must ask each one",
			len(f.queries), len(fix.RegionNames()))
	}
}

// An asset type with no Resource Explorer mapping means the regions holding it
// are unknowable, so narrowing on the remaining types could drop a region.
func TestResolveRegions_RefusesUnmappedAssetType(t *testing.T) {
	p := &Provider{
		log:                 newTestLogger(),
		useResourceExplorer: true,
		discoverer:          newDiscovererWithClient(&fakeResourceExplorer{aggregator: true}),
	}
	_, _, err := p.resolveRegions(context.Background(), []string{"aws.ec2.volume", "aws.rds.instance"})
	if err == nil {
		t.Fatal("an unmapped asset type must not be silently dropped from discovery")
	}
	if !strings.Contains(err.Error(), "aws.rds.instance") {
		t.Errorf("error %q does not name the unmappable type", err)
	}
}

// A working aggregator index returning nothing is a REAL answer: the account
// holds none of those types anywhere. That must scan zero regions rather than
// erroring, or an empty account could never be scanned.
func TestResolveRegions_EmptyAggregatorResultIsAnAnswer(t *testing.T) {
	p := &Provider{
		log:                 newTestLogger(),
		useResourceExplorer: true,
		discoverer: newDiscovererWithClient(&fakeResourceExplorer{
			aggregator: true,
			pages:      []*resourceexplorer2.SearchOutput{{Resources: nil}},
		}),
	}
	regions, source, err := p.resolveRegions(context.Background(), []string{"aws.ec2.volume"})
	if err != nil {
		t.Fatalf("resolveRegions: %v — an empty index result is not a failure", err)
	}
	if source != "resource_explorer" {
		t.Errorf("source = %q, want resource_explorer", source)
	}
	if len(regions) != 0 {
		t.Errorf("regions = %v, want none", regions)
	}
}

// No discoverer at all (a provider built without AWS config) must say so when
// Resource Explorer is requested.
func TestResolveRegions_RefusesWithoutDiscoverer(t *testing.T) {
	p := &Provider{log: newTestLogger(), useResourceExplorer: true}
	_, _, err := p.resolveRegions(context.Background(), []string{"aws.ec2.volume"})
	if err == nil {
		t.Fatal("no discoverer must be an error, not an empty region list")
	}
	if !strings.Contains(err.Error(), "--aws-regions") {
		t.Errorf("error %q does not offer the escape hatch", err)
	}
}

// regionsWithoutIndex names the regions where a search answers a confident
// empty. It is the difference between a first-run gap that is reported and one
// that is silent.
func TestRegionsWithoutIndex(t *testing.T) {
	enabled := []string{"eu-west-1", "us-east-1", "us-west-1", "ap-south-1"}
	indexed := map[string]bool{"eu-west-1": true, "us-east-1": true}

	got := regionsWithoutIndex(enabled, indexed)
	want := []string{"ap-south-1", "us-west-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("regionsWithoutIndex = %v, want %v", got, want)
	}

	if got := regionsWithoutIndex(enabled, map[string]bool{
		"eu-west-1": true, "us-east-1": true, "us-west-1": true, "ap-south-1": true,
	}); len(got) != 0 {
		t.Errorf("fully indexed account reported %v as un-indexed", got)
	}
}
