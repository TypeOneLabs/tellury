package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Fixture is the offline data source for an AWS scan. It replays captured EC2
// volumes and addresses per region from local JSON, so ingestion is testable
// and `tellury scan --fixture ... --provider aws --aws-account <id>` runs with
// no AWS credentials and no network — the AWS analog of GCP's FakeLister.
//
// Each volume/address element in a fixture file is the JSON form of the EC2
// SDK's own types.Volume / types.Address, so a capture produced by the AWS
// CLI or SDK feeds the fixture unedited and the normalizers read exactly the
// fields the live Describe calls would have returned.
type Fixture struct {
	// Regions maps a region name to that region's captured resources. Keys
	// may use the availability-zone form ("us-east-1a"); the provider
	// canonicalises them to the region ("us-east-1") when it resolves which
	// regions to scan.
	Regions map[string]*RegionFixture `json:"regions"`
}

// RegionFixture holds one region's captured EC2 resources.
type RegionFixture struct {
	Volumes   []ec2types.Volume  `json:"volumes"`
	Addresses []ec2types.Address `json:"addresses"`
}

// RegionNames returns the fixture's region keys, canonicalised through
// pricing.CanonicalRegion (an "us-east-1a" key becomes "us-east-1") and
// sorted for determinism. It is what an offline provider scans when no
// --aws-regions list narrows it.
func (f *Fixture) RegionNames() []string {
	names := make([]string, 0, len(f.Regions))
	for r := range f.Regions {
		names = append(names, r)
	}
	return canonicaliseRegions(names)
}

// LoadFixture reads one or more fixture files. Each file may be one of two
// shapes, both accepted without hand-editing:
//
//  1. The canonical envelope:
//     {"regions": {"us-east-1": {"volumes": [ ...types.Volume... ],
//     "addresses": [ ...types.Address... ]}}}
//  2. A bare JSON object mapping region name to the same per-region shape,
//     which is what a hand-rolled test fixture or a small capture looks like.
//
// Later files merge on top of earlier ones (last region wins), so a multi-file
// fixture set can layer resources the way GCP's LoadFakeLister appends assets.
func LoadFixture(paths ...string) (*Fixture, error) {
	f := &Fixture{Regions: map[string]*RegionFixture{}}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("aws: read fixture %s: %w", p, err)
		}
		var envelope struct {
			Regions map[string]*RegionFixture `json:"regions"`
		}
		if err := json.Unmarshal(b, &envelope); err == nil && envelope.Regions != nil {
			mergeRegionFixtures(f, envelope.Regions)
			continue
		}
		var bare map[string]*RegionFixture
		if err := json.Unmarshal(b, &bare); err != nil {
			return nil, fmt.Errorf(
				"aws: fixture %s: expected {\"regions\":{...}} or a bare {region: {...}} map: %w", p, err)
		}
		mergeRegionFixtures(f, bare)
	}
	return f, nil
}

// mergeRegionFixtures merges src into dst per region, replacing (not appending
// to) a region's resource lists so a later file overrides an earlier one
// cleanly.
func mergeRegionFixtures(dst *Fixture, src map[string]*RegionFixture) {
	for region, data := range src {
		if data == nil {
			continue
		}
		dst.Regions[region] = data
	}
}

// fakeEC2 is the fixture-backed EC2 API. It stands in for the live SDK client
// on the offline path so ingestion exercises the same Describe calls — the
// DescribeVolumes paginator included — with no network and no credentials.
type fakeEC2 struct {
	region string // the region this client is scoped to
	f      *Fixture
}

var _ ec2API = (*fakeEC2)(nil)

// DescribeRegions returns the fixture's regions as enabled regions, so the
// offline default (no --aws-regions) sweeps exactly what the fixture carries.
func (c *fakeEC2) DescribeRegions(_ context.Context, _ *ec2.DescribeRegionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	out := &ec2.DescribeRegionsOutput{}
	for _, name := range c.f.RegionNames() {
		n := name
		out.Regions = append(out.Regions, ec2types.Region{RegionName: &n})
	}
	return out, nil
}

// DescribeVolumes returns the region's volumes paginated by MaxResults/
// NextToken, exactly like the live API, so the provider's paginator loop is
// exercised by fixture tests.
func (c *fakeEC2) DescribeVolumes(_ context.Context, in *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	vols := c.regionVolumes()
	pageSize := 1000
	if in != nil && in.MaxResults != nil && *in.MaxResults > 0 {
		pageSize = int(*in.MaxResults)
	}
	start := 0
	if in != nil && in.NextToken != nil && *in.NextToken != "" {
		n, err := strconv.Atoi(*in.NextToken)
		if err != nil || n < 0 || n >= len(vols) {
			return &ec2.DescribeVolumesOutput{}, nil
		}
		start = n
	}
	end := start + pageSize
	if end > len(vols) {
		end = len(vols)
	}
	out := &ec2.DescribeVolumesOutput{Volumes: vols[start:end]}
	if end < len(vols) {
		tok := strconv.Itoa(end)
		out.NextToken = &tok
	}
	return out, nil
}

// DescribeAddresses returns every address in the region in one call. The EC2
// API's DescribeAddresses has no paginator (no MaxResults/NextToken fields),
// so one call is the complete, correct fetch — this mirrors the live path
// exactly.
func (c *fakeEC2) DescribeAddresses(_ context.Context, _ *ec2.DescribeAddressesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	return &ec2.DescribeAddressesOutput{Addresses: c.regionAddresses()}, nil
}

func (c *fakeEC2) regionVolumes() []ec2types.Volume {
	if c.f == nil {
		return nil
	}
	rf, ok := c.f.Regions[c.region]
	if !ok || rf == nil {
		return nil
	}
	return rf.Volumes
}

func (c *fakeEC2) regionAddresses() []ec2types.Address {
	if c.f == nil {
		return nil
	}
	rf, ok := c.f.Regions[c.region]
	if !ok || rf == nil {
		return nil
	}
	return rf.Addresses
}
