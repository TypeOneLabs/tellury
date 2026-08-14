package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Fixture is the offline data source for an AWS scan. It replays captured EC2
// volumes, addresses, instances, instance types, AMIs, snapshots and AMI
// reference sources per region from local JSON, so ingestion is testable and
// `tellury scan --fixture ... --provider aws --aws-account <id>` runs with no
// AWS credentials and no network — the AWS analog of GCP's FakeLister.
//
// Each element in a fixture file is the JSON form of the AWS SDK's own types,
// so a capture produced by the AWS CLI or SDK feeds the fixture unedited and
// the normalizers read exactly the fields the live Describe calls would have
// returned.
type Fixture struct {
	// Regions maps a region name to that region's captured resources. Keys
	// may use the availability-zone form ("us-east-1a"); the provider
	// canonicalises them to the region ("us-east-1") when it resolves which
	// regions to scan.
	Regions map[string]*RegionFixture `json:"regions"`
}

// RegionFixture holds one region's captured EC2 and Auto Scaling resources.
type RegionFixture struct {
	Volumes       []ec2types.Volume           `json:"volumes"`
	Addresses     []ec2types.Address          `json:"addresses"`
	Instances     []ec2types.Instance         `json:"instances"`
	InstanceTypes []ec2types.InstanceTypeInfo `json:"instance_types"`

	// AMI discovery and reference enumeration. The launch-template version
	// slice is flat; fakeEC2 filters it by LaunchTemplateId, exactly like the
	// live per-template DescribeLaunchTemplateVersions call.
	Images                 []ec2types.Image                  `json:"images"`
	Snapshots              []ec2types.Snapshot               `json:"snapshots"`
	LaunchTemplates        []ec2types.LaunchTemplate         `json:"launch_templates"`
	LaunchTemplateVersions []ec2types.LaunchTemplateVersion  `json:"launch_template_versions"`
	LaunchConfigurations   []asgtypes.LaunchConfiguration    `json:"launch_configurations"`
	Fleets                 []ec2types.FleetData              `json:"fleets"`
	SpotFleetRequests      []ec2types.SpotFleetRequestConfig `json:"spot_fleet_requests"`
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
//     "addresses": [ ...types.Address... ],
//     "instances": [ ...types.Instance... ],
//     "instance_types": [ ...types.InstanceTypeInfo... ],
//     "images": [ ...types.Image... ], ...}}}
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
// DescribeVolumes paginator, DescribeAddresses, DescribeInstances paginator,
// DescribeInstanceTypes, and the AMI discovery/reference calls included — with
// no network and no credentials.
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
	start, pageSize := paginationRange(in.MaxResults, in.NextToken, len(vols))
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

// DescribeInstances returns the region's instances paginated by MaxResults/
// NextToken, matching the live API. Each page is a single Reservation
// containing up to pageSize instances, so the provider's paginator loop is
// exercised.
func (c *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	insts := c.regionInstances()
	start, pageSize := paginationRange(in.MaxResults, in.NextToken, len(insts))
	end := start + pageSize
	if end > len(insts) {
		end = len(insts)
	}
	page := insts[start:end]
	out := &ec2.DescribeInstancesOutput{}
	if len(page) > 0 {
		out.Reservations = []ec2types.Reservation{{Instances: page}}
	}
	if end < len(insts) {
		tok := strconv.Itoa(end)
		out.NextToken = &tok
	}
	return out, nil
}

// DescribeInstanceTypes returns the fixture's instance types for the region,
// optionally filtered by the InstanceTypes list in the input. This mirrors
// the live API's targeted lookup: pass InstanceTypes to get specific shapes,
// or omit it to get all shapes in the fixture.
func (c *fakeEC2) DescribeInstanceTypes(_ context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	all := c.regionInstanceTypes()
	if in == nil || len(in.InstanceTypes) == 0 {
		return &ec2.DescribeInstanceTypesOutput{InstanceTypes: all}, nil
	}
	want := make(map[ec2types.InstanceType]bool, len(in.InstanceTypes))
	for _, t := range in.InstanceTypes {
		want[t] = true
	}
	var filtered []ec2types.InstanceTypeInfo
	for _, it := range all {
		if want[it.InstanceType] {
			filtered = append(filtered, it)
		}
	}
	start, pageSize := paginationRange(in.MaxResults, in.NextToken, len(filtered))
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	out := &ec2.DescribeInstanceTypesOutput{InstanceTypes: filtered[start:end]}
	if end < len(filtered) {
		tok := strconv.Itoa(end)
		out.NextToken = &tok
	}
	return out, nil
}

// DescribeImages returns the region's self-owned images paginated by
// MaxResults/NextToken. The live caller sets Owners:["self"]; the fixture fake
// trusts the fixture to already contain only self-owned images.
func (c *fakeEC2) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	images := c.regionImages()
	start, pageSize := paginationRange(in.MaxResults, in.NextToken, len(images))
	end := start + pageSize
	if end > len(images) {
		end = len(images)
	}
	out := &ec2.DescribeImagesOutput{Images: images[start:end]}
	if end < len(images) {
		tok := strconv.Itoa(end)
		out.NextToken = &tok
	}
	return out, nil
}

// DescribeSnapshots returns the region's self-owned snapshots paginated by
// MaxResults/NextToken. The live caller sets OwnerIds:["self"].
func (c *fakeEC2) DescribeSnapshots(_ context.Context, in *ec2.DescribeSnapshotsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	snapshots := c.regionSnapshots()
	start, pageSize := paginationRange(in.MaxResults, in.NextToken, len(snapshots))
	end := start + pageSize
	if end > len(snapshots) {
		end = len(snapshots)
	}
	out := &ec2.DescribeSnapshotsOutput{Snapshots: snapshots[start:end]}
	if end < len(snapshots) {
		tok := strconv.Itoa(end)
		out.NextToken = &tok
	}
	return out, nil
}

// DescribeLaunchTemplates returns the region's launch templates paginated by
// MaxResults/NextToken.
func (c *fakeEC2) DescribeLaunchTemplates(_ context.Context, in *ec2.DescribeLaunchTemplatesInput, _ ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error) {
	templates := c.regionLaunchTemplates()
	start, pageSize := paginationRange(in.MaxResults, in.NextToken, len(templates))
	end := start + pageSize
	if end > len(templates) {
		end = len(templates)
	}
	out := &ec2.DescribeLaunchTemplatesOutput{LaunchTemplates: templates[start:end]}
	if end < len(templates) {
		tok := strconv.Itoa(end)
		out.NextToken = &tok
	}
	return out, nil
}

// DescribeLaunchTemplateVersions returns the versions whose LaunchTemplateId
// matches the request, paginated by MaxResults/NextToken. This mirrors the
// live per-template call the reference pass makes.
func (c *fakeEC2) DescribeLaunchTemplateVersions(_ context.Context, in *ec2.DescribeLaunchTemplateVersionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplateVersionsOutput, error) {
	all := c.regionLaunchTemplateVersions()
	filtered := all[:0]
	if in != nil && in.LaunchTemplateId != nil && *in.LaunchTemplateId != "" {
		want := *in.LaunchTemplateId
		for _, v := range all {
			if v.LaunchTemplateId != nil && *v.LaunchTemplateId == want {
				filtered = append(filtered, v)
			}
		}
	}
	start, pageSize := paginationRange(in.MaxResults, in.NextToken, len(filtered))
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	out := &ec2.DescribeLaunchTemplateVersionsOutput{LaunchTemplateVersions: filtered[start:end]}
	if end < len(filtered) {
		tok := strconv.Itoa(end)
		out.NextToken = &tok
	}
	return out, nil
}

// DescribeFleets returns the region's EC2 Fleets paginated by MaxResults/
// NextToken.
func (c *fakeEC2) DescribeFleets(_ context.Context, in *ec2.DescribeFleetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeFleetsOutput, error) {
	fleets := c.regionFleets()
	start, pageSize := paginationRange(in.MaxResults, in.NextToken, len(fleets))
	end := start + pageSize
	if end > len(fleets) {
		end = len(fleets)
	}
	out := &ec2.DescribeFleetsOutput{Fleets: fleets[start:end]}
	if end < len(fleets) {
		tok := strconv.Itoa(end)
		out.NextToken = &tok
	}
	return out, nil
}

// DescribeSpotFleetRequests returns the region's Spot Fleet requests paginated
// by MaxResults/NextToken.
func (c *fakeEC2) DescribeSpotFleetRequests(_ context.Context, in *ec2.DescribeSpotFleetRequestsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotFleetRequestsOutput, error) {
	reqs := c.regionSpotFleetRequests()
	start, pageSize := paginationRange(in.MaxResults, in.NextToken, len(reqs))
	end := start + pageSize
	if end > len(reqs) {
		end = len(reqs)
	}
	out := &ec2.DescribeSpotFleetRequestsOutput{SpotFleetRequestConfigs: reqs[start:end]}
	if end < len(reqs) {
		tok := strconv.Itoa(end)
		out.NextToken = &tok
	}
	return out, nil
}

// fakeAutoScaling is the fixture-backed Auto Scaling API used only by the AMI
// reference pass for DescribeLaunchConfigurations.
type fakeAutoScaling struct {
	region string
	f      *Fixture
}

var _ autoScalingAPI = (*fakeAutoScaling)(nil)

// DescribeLaunchConfigurations returns the region's launch configurations
// paginated by MaxRecords/NextToken, exactly like the live API.
func (c *fakeAutoScaling) DescribeLaunchConfigurations(_ context.Context, in *autoscaling.DescribeLaunchConfigurationsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeLaunchConfigurationsOutput, error) {
	configs := c.regionLaunchConfigurations()
	var maxRecords *int32
	if in != nil {
		maxRecords = in.MaxRecords
	}
	start, pageSize := paginationRange(maxRecords, in.NextToken, len(configs))
	end := start + pageSize
	if end > len(configs) {
		end = len(configs)
	}
	out := &autoscaling.DescribeLaunchConfigurationsOutput{LaunchConfigurations: configs[start:end]}
	if end < len(configs) {
		tok := strconv.Itoa(end)
		out.NextToken = &tok
	}
	return out, nil
}

// paginationRange converts AWS MaxResults/MaxRecords and NextToken into a
// start index and page size. It is deliberately small and shared by every
// fake so pagination behaviour stays identical across calls.
func paginationRange(maxResults *int32, nextToken *string, total int) (start, pageSize int) {
	pageSize = 1000
	if maxResults != nil && *maxResults > 0 {
		pageSize = int(*maxResults)
	}
	start = 0
	if nextToken != nil && *nextToken != "" {
		n, err := strconv.Atoi(*nextToken)
		if err == nil && n >= 0 && n < total {
			start = n
		}
	}
	return start, pageSize
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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

func (c *fakeEC2) regionInstances() []ec2types.Instance {
	if c.f == nil {
		return nil
	}
	rf, ok := c.f.Regions[c.region]
	if !ok || rf == nil {
		return nil
	}
	return rf.Instances
}

func (c *fakeEC2) regionInstanceTypes() []ec2types.InstanceTypeInfo {
	if c.f == nil {
		return nil
	}
	rf, ok := c.f.Regions[c.region]
	if !ok || rf == nil {
		return nil
	}
	return rf.InstanceTypes
}

func (c *fakeEC2) regionImages() []ec2types.Image {
	if c.f == nil {
		return nil
	}
	rf, ok := c.f.Regions[c.region]
	if !ok || rf == nil {
		return nil
	}
	return rf.Images
}

func (c *fakeEC2) regionSnapshots() []ec2types.Snapshot {
	if c.f == nil {
		return nil
	}
	rf, ok := c.f.Regions[c.region]
	if !ok || rf == nil {
		return nil
	}
	return rf.Snapshots
}

func (c *fakeEC2) regionLaunchTemplates() []ec2types.LaunchTemplate {
	if c.f == nil {
		return nil
	}
	rf, ok := c.f.Regions[c.region]
	if !ok || rf == nil {
		return nil
	}
	return rf.LaunchTemplates
}

func (c *fakeEC2) regionLaunchTemplateVersions() []ec2types.LaunchTemplateVersion {
	if c.f == nil {
		return nil
	}
	rf, ok := c.f.Regions[c.region]
	if !ok || rf == nil {
		return nil
	}
	return rf.LaunchTemplateVersions
}

func (c *fakeEC2) regionFleets() []ec2types.FleetData {
	if c.f == nil {
		return nil
	}
	rf, ok := c.f.Regions[c.region]
	if !ok || rf == nil {
		return nil
	}
	return rf.Fleets
}

func (c *fakeEC2) regionSpotFleetRequests() []ec2types.SpotFleetRequestConfig {
	if c.f == nil {
		return nil
	}
	rf, ok := c.f.Regions[c.region]
	if !ok || rf == nil {
		return nil
	}
	return rf.SpotFleetRequests
}

func (c *fakeAutoScaling) regionLaunchConfigurations() []asgtypes.LaunchConfiguration {
	if c.f == nil {
		return nil
	}
	rf, ok := c.f.Regions[c.region]
	if !ok || rf == nil {
		return nil
	}
	return rf.LaunchConfigurations
}
