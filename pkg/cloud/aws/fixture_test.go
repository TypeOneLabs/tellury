package aws

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// TestLoadFixture_ParsesEnvelopeShape loads the shipped testdata fixture (the
// canonical {"regions": {...}} envelope) and pins the values the normalizers
// read, proving the offline fixture path accepts a capture without hand
// editing.
func TestLoadFixture_ParsesEnvelopeShape(t *testing.T) {
	f, err := LoadFixture(filepath.Join("testdata", "aws-ec2-fixture.json"))
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	if len(f.Regions) != 2 {
		t.Fatalf("fixture has %d regions, want 2", len(f.Regions))
	}
	us, ok := f.Regions["us-east-1"]
	if !ok {
		t.Fatalf("fixture missing us-east-1; got %v", regionNames(f))
	}
	if len(us.Volumes) != 2 {
		t.Fatalf("us-east-1 has %d volumes, want 2", len(us.Volumes))
	}
	if got := *us.Volumes[0].VolumeId; got != "vol-0aaa" {
		t.Errorf("volume[0].VolumeId = %q, want vol-0aaa", got)
	}
	if got := *us.Volumes[0].Size; got != 100 {
		t.Errorf("volume[0].Size = %d, want 100", got)
	}
	if got := string(us.Volumes[0].VolumeType); got != "gp3" {
		t.Errorf("volume[0].VolumeType = %q, want gp3", got)
	}
	if len(us.Volumes[1].Attachments) != 1 {
		t.Fatalf("volume[1] has %d attachments, want 1", len(us.Volumes[1].Attachments))
	}
	if got := *us.Volumes[1].Attachments[0].InstanceId; got != "i-0cafe" {
		t.Errorf("volume[1] attachment InstanceId = %q, want i-0cafe", got)
	}
	if len(us.Addresses) != 2 {
		t.Fatalf("us-east-1 has %d addresses, want 2", len(us.Addresses))
	}
	if got := string(us.Addresses[0].Domain); got != "vpc" {
		t.Errorf("address[0].Domain = %q, want vpc", got)
	}
	if us.Addresses[0].AssociationId != nil {
		t.Errorf("address[0] must be unassociated; got association %q", *us.Addresses[0].AssociationId)
	}
	if got := *us.Addresses[1].AssociationId; got != "eipassoc-0e1" {
		t.Errorf("address[1].AssociationId = %q, want eipassoc-0e1", got)
	}

	// Instance data from the fixture.
	if len(us.Instances) != 2 {
		t.Fatalf("us-east-1 has %d instances, want 2", len(us.Instances))
	}
	if got := *us.Instances[0].InstanceId; got != "i-0cafe" {
		t.Errorf("instance[0].InstanceId = %q, want i-0cafe", got)
	}
	if got := string(us.Instances[0].InstanceType); got != "t3.medium" {
		t.Errorf("instance[0].InstanceType = %q, want t3.medium", got)
	}
	if us.Instances[0].State == nil || string(us.Instances[0].State.Name) != "running" {
		t.Errorf("instance[0].State.Name = %v, want running", us.Instances[0].State)
	}
	// Second instance is spot.
	if got := string(us.Instances[1].InstanceLifecycle); got != "spot" {
		t.Errorf("instance[1].InstanceLifecycle = %q, want spot", got)
	}

	// Instance types from the fixture.
	if len(us.InstanceTypes) != 2 {
		t.Fatalf("us-east-1 has %d instance_types, want 2", len(us.InstanceTypes))
	}
	if got := string(us.InstanceTypes[0].InstanceType); got != "t3.medium" {
		t.Errorf("instance_type[0].InstanceType = %q, want t3.medium", got)
	}
	if us.InstanceTypes[0].VCpuInfo == nil || us.InstanceTypes[0].VCpuInfo.DefaultVCpus == nil || *us.InstanceTypes[0].VCpuInfo.DefaultVCpus != 2 {
		t.Errorf("instance_type[0].VCpuInfo.DefaultVCpus = %v, want 2", us.InstanceTypes[0].VCpuInfo)
	}
}

// TestLoadFixture_AcceptsBareRegionMap pins the second accepted shape: a bare
// JSON object mapping region name to per-region data (no {"regions": ...}
// envelope).
func TestLoadFixture_AcceptsBareRegionMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bare.json")
	body := `{
		"us-east-1": {
			"volumes": [{"volumeId": "vol-1", "size": 10, "volumeType": "gp3", "state": "available"}],
			"addresses": [],
			"instances": [],
			"instance_types": []
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f, err := LoadFixture(path)
	if err != nil {
		t.Fatalf("LoadFixture(bare): %v", err)
	}
	if len(f.Regions) != 1 || f.Regions["us-east-1"] == nil {
		t.Fatalf("bare fixture parsed to %v, want us-east-1", regionNames(f))
	}
	if got := *f.Regions["us-east-1"].Volumes[0].VolumeId; got != "vol-1" {
		t.Errorf("volume VolumeId = %q, want vol-1", got)
	}
}

// TestLoadFixture_RejectsGarbage: a file that is neither shape is a hard
// error naming the file, not a silent empty fixture.
func TestLoadFixture_RejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := LoadFixture(path); err == nil {
		t.Fatal("LoadFixture(garbage) must fail")
	}
}

// TestFixture_RegionNamesCanonicalises: an availability-zone key ("us-east-1a")
// resolves to its region ("us-east-1") through the single
// pricing.CanonicalRegion wrapper, de-duplicated and sorted.
func TestFixture_RegionNamesCanonicalises(t *testing.T) {
	f := &Fixture{Regions: map[string]*RegionFixture{
		"us-east-1a": {},
		"eu-west-1":  {},
		"us-east-1":  {}, // duplicate after canonicalisation
		"":           {},
	}}
	got := f.RegionNames()
	want := []string{"eu-west-1", "us-east-1"}
	if len(got) != len(want) {
		t.Fatalf("RegionNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RegionNames() = %v, want %v", got, want)
		}
	}
}

// TestFakeEC2_DescribeVolumesPaginates proves the offline path drives the
// SAME DescribeVolumesPaginator the live path does, and that the fixture fake
// honours MaxResults/NextToken: 2500 volumes come back in three pages of
// 1000/1000/500, exactly as the real EC2 API pages them.
func TestFakeEC2_DescribeVolumesPaginates(t *testing.T) {
	f := &Fixture{Regions: map[string]*RegionFixture{
		"us-east-1": {Volumes: buildTestVolumes(2500)},
	}}
	client := &fakeEC2{region: "us-east-1", f: f}

	var got []string
	paginator := ec2.NewDescribeVolumesPaginator(client, &ec2.DescribeVolumesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.Background())
		if err != nil {
			t.Fatalf("NextPage: %v", err)
		}
		for _, v := range page.Volumes {
			got = append(got, *v.VolumeId)
		}
	}
	if len(got) != 2500 {
		t.Fatalf("paginator returned %d volumes, want 2500 (all pages)", len(got))
	}
	if got[0] != "vol-000000" || got[2499] != "vol-002499" {
		t.Fatalf("pagination lost ordering: first=%q last=%q", got[0], got[2499])
	}
}

// TestFakeEC2_DescribeInstancesPaginates proves the offline path drives the
// SAME DescribeInstancesPaginator the live path does, and that the fixture
// fake honours MaxResults/NextToken for instances too.
func TestFakeEC2_DescribeInstancesPaginates(t *testing.T) {
	insts := buildTestInstances(2500)
	f := &Fixture{Regions: map[string]*RegionFixture{
		"us-east-1": {Instances: insts},
	}}
	client := &fakeEC2{region: "us-east-1", f: f}

	var got []string
	paginator := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.Background())
		if err != nil {
			t.Fatalf("NextPage: %v", err)
		}
		for _, res := range page.Reservations {
			for _, inst := range res.Instances {
				got = append(got, *inst.InstanceId)
			}
		}
	}
	if len(got) != 2500 {
		t.Fatalf("paginator returned %d instances, want 2500 (all pages)", len(got))
	}
	if got[0] != "i-000000" || got[2499] != "i-002499" {
		t.Fatalf("pagination lost ordering: first=%q last=%q", got[0], got[2499])
	}
}

// TestFakeEC2_DescribeInstanceTypes_FiltersByRequestedTypes: when the caller
// passes InstanceTypes, only matching shapes are returned.
func TestFakeEC2_DescribeInstanceTypes_FiltersByRequestedTypes(t *testing.T) {
	f := &Fixture{Regions: map[string]*RegionFixture{
		"us-east-1": {
			InstanceTypes: []ec2types.InstanceTypeInfo{
				{InstanceType: ec2types.InstanceTypeT3Medium},
				{InstanceType: ec2types.InstanceTypeC6iXlarge},
				{InstanceType: ec2types.InstanceTypeM6iLarge},
			},
		},
	}}
	client := &fakeEC2{region: "us-east-1", f: f}

	out, err := client.DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []ec2types.InstanceType{ec2types.InstanceTypeT3Medium, ec2types.InstanceTypeM6iLarge},
	})
	if err != nil {
		t.Fatalf("DescribeInstanceTypes: %v", err)
	}
	if len(out.InstanceTypes) != 2 {
		t.Fatalf("got %d instance types, want 2 (filtered)", len(out.InstanceTypes))
	}
	if got := string(out.InstanceTypes[0].InstanceType); got != "t3.medium" {
		t.Errorf("first = %q, want t3.medium", got)
	}
	if got := string(out.InstanceTypes[1].InstanceType); got != "m6i.large" {
		t.Errorf("second = %q, want m6i.large", got)
	}
}

// TestFakeEC2_DescribeInstanceTypes_NoFilterReturnsAll: without InstanceTypes
// in the input, all shapes in the fixture are returned.
func TestFakeEC2_DescribeInstanceTypes_NoFilterReturnsAll(t *testing.T) {
	f := &Fixture{Regions: map[string]*RegionFixture{
		"us-east-1": {
			InstanceTypes: []ec2types.InstanceTypeInfo{
				{InstanceType: ec2types.InstanceTypeT3Medium},
				{InstanceType: ec2types.InstanceTypeC6iXlarge},
			},
		},
	}}
	client := &fakeEC2{region: "us-east-1", f: f}

	out, err := client.DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{})
	if err != nil {
		t.Fatalf("DescribeInstanceTypes: %v", err)
	}
	if len(out.InstanceTypes) != 2 {
		t.Fatalf("got %d instance types, want 2 (all)", len(out.InstanceTypes))
	}
}

// buildTestVolumes returns n volumes with volume ids vol-000000 .. vol-00NNNN.
func buildTestVolumes(n int) []ec2types.Volume {
	out := make([]ec2types.Volume, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("vol-%06d", i)
		out = append(out, ec2types.Volume{VolumeId: &id})
	}
	return out
}

// buildTestInstances returns n instances with instance ids i-000000 .. i-00NNNN.
func buildTestInstances(n int) []ec2types.Instance {
	out := make([]ec2types.Instance, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("i-%06d", i)
		state := ec2types.InstanceStateNameRunning
		out = append(out, ec2types.Instance{
			InstanceId: &id,
			State:      &ec2types.InstanceState{Name: state},
		})
	}
	return out
}

func regionNames(f *Fixture) []string {
	out := make([]string, 0, len(f.Regions))
	for r := range f.Regions {
		out = append(out, r)
	}
	return out
}
