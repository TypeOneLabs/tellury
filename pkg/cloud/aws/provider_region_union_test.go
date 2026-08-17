package aws

import (
	"context"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2/types"
)

// stubRegionEC2 is an ec2API stand-in that reports a fixed enabled-region
// list and returns no resources for every other call. It lets a test drive
// multiple per-account ingests without a network or fixture.
type stubRegionEC2 struct {
	*fakeEC2
	regions []string
}

func (c *stubRegionEC2) DescribeRegions(_ context.Context, _ *ec2.DescribeRegionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	out := &ec2.DescribeRegionsOutput{}
	for _, r := range c.regions {
		r := r
		out.Regions = append(out.Regions, ec2types.Region{RegionName: &r})
	}
	return out, nil
}

// TestIngestAccountWithClient_RegionUnionAcrossAccounts pins defect A: a
// per-account ingest must UNION into the provider's region coverage, not
// overwrite it. Two accounts with different enabled-region sets must report
// the union, not the second account's list.
func TestIngestAccountWithClient_RegionUnionAcrossAccounts(t *testing.T) {
	p := &Provider{log: newTestLogger()}
	ctx := context.Background()

	_, err := p.ingestAccountWithClient(ctx, "111122223333", func(region string) ec2API {
		return &stubRegionEC2{fakeEC2: &fakeEC2{}, regions: []string{"us-east-1", "us-west-1"}}
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("first account ingest: %v", err)
	}

	_, err = p.ingestAccountWithClient(ctx, "444455556666", func(region string) ec2API {
		return &stubRegionEC2{fakeEC2: &fakeEC2{}, regions: []string{"us-west-1", "eu-west-1"}}
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("second account ingest: %v", err)
	}

	regions, source := p.Regions()
	want := []string{"eu-west-1", "us-east-1", "us-west-1"}
	if !reflect.DeepEqual(regions, want) {
		t.Errorf("regions = %v, want union %v", regions, want)
	}
	if source != "describe_regions" {
		t.Errorf("region source = %q, want describe_regions", source)
	}
}

// TestMergeRegionSource_Mixed pins that different account-level sources are
// reported as "mixed", never as whichever account happened to finish last.
func TestMergeRegionSource_Mixed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing string
		incoming string
		want     string
	}{
		{"empty adopts first", "", "describe_regions", "describe_regions"},
		{"same collapses", "resource_explorer", "resource_explorer", "resource_explorer"},
		{"different becomes mixed", "describe_regions", "resource_explorer", "mixed"},
		{"mixed stays mixed", "mixed", "explicit", "mixed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeRegionSource(tc.existing, tc.incoming); got != tc.want {
				t.Errorf("mergeRegionSource(%q, %q) = %q, want %q",
					tc.existing, tc.incoming, got, tc.want)
			}
		})
	}
}

// TestResolveRegions_NoAggregatorRecordsSearchableCoverage pins the coverage
// half of defect B at the source: the Resource Explorer per-region sweep knows
// both the enabled-region count and how many of those regions are indexed.
func TestResolveRegions_NoAggregatorRecordsSearchableCoverage(t *testing.T) {
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
		t.Fatalf("resolveRegionsWithClient: %v", err)
	}
	if source != "resource_explorer" {
		t.Errorf("source = %q, want resource_explorer", source)
	}
	if len(regions) != 1 || regions[0] != "eu-west-1" {
		t.Errorf("regions = %v, want [eu-west-1]", regions)
	}

	if p.lastRegionEnabledCount != len(fix.RegionNames()) {
		t.Errorf("enabled count = %d, want %d", p.lastRegionEnabledCount, len(fix.RegionNames()))
	}
	if p.lastRegionSearchableCount != 0 {
		t.Errorf("searchable count = %d, want 0 (the fake has local indexes in no regions)", p.lastRegionSearchableCount)
	}
}
