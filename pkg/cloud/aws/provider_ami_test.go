package aws

import (
	"context"
	"testing"

	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
)

func strPtr(s string) *string { return &s }
func int64Ptr(v int64) *int64 { return &v }
func int32Ptr(v int32) *int32 { return &v }

func amiFixture() *Fixture {
	return &Fixture{Regions: map[string]*RegionFixture{
		"us-east-1": {
			Volumes:       []ec2types.Volume{},
			Addresses:     []ec2types.Address{},
			Instances:     []ec2types.Instance{},
			InstanceTypes: []ec2types.InstanceTypeInfo{},
			Images: []ec2types.Image{
				{
					ImageId:        strPtr("ami-0used"),
					Name:           strPtr("used-image"),
					CreationDate:   strPtr("2024-01-01T00:00:00Z"),
					State:          ec2types.ImageStateAvailable,
					RootDeviceType: ec2types.DeviceTypeEbs,
					BlockDeviceMappings: []ec2types.BlockDeviceMapping{{
						DeviceName: strPtr("/dev/sda1"),
						Ebs:        &ec2types.EbsBlockDevice{SnapshotId: strPtr("snap-0a")},
					}},
				},
				{
					ImageId:        strPtr("ami-0unused"),
					Name:           strPtr("unused-image"),
					CreationDate:   strPtr("2024-01-01T00:00:00Z"),
					State:          ec2types.ImageStateAvailable,
					RootDeviceType: ec2types.DeviceTypeEbs,
					BlockDeviceMappings: []ec2types.BlockDeviceMapping{{
						DeviceName: strPtr("/dev/sda1"),
						Ebs:        &ec2types.EbsBlockDevice{SnapshotId: strPtr("snap-0b")},
					}},
				},
			},
			Snapshots: []ec2types.Snapshot{
				{SnapshotId: strPtr("snap-0a"), VolumeSize: int32Ptr(50)},
				{SnapshotId: strPtr("snap-0b"), VolumeSize: int32Ptr(100)},
			},
			LaunchTemplates: []ec2types.LaunchTemplate{
				{LaunchTemplateId: strPtr("lt-0a")},
			},
			LaunchTemplateVersions: []ec2types.LaunchTemplateVersion{
				{
					LaunchTemplateId: strPtr("lt-0a"),
					VersionNumber:    int64Ptr(1),
					LaunchTemplateData: &ec2types.ResponseLaunchTemplateData{
						ImageId: strPtr("ami-0used"),
					},
				},
			},
		},
	}}
}

func TestIngest_ImageHintNormalizesAMIsAndReferences(t *testing.T) {
	p, err := New(context.Background(),
		WithOffline(),
		WithFixture(amiFixture()),
		WithLogger(newTestLogger()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	sc := cloud.Scope{Provider: "aws", AWS: &cloud.AWSScope{Account: "123456789012"}}
	gr, err := p.Ingest(context.Background(), sc, []string{TypeImage})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if got := gr.CountByKind(graph.KindImage); got != 2 {
		t.Fatalf("image nodes = %d, want 2", got)
	}
	if got := gr.CountByKind(graph.KindSnapshot); got != 0 {
		t.Fatalf("snapshot nodes = %d, want 0 (only aws.ec2.image was requested)", got)
	}

	usedID := graph.Ref("accounts/123456789012/regions/us-east-1/images/ami-0used")
	used, ok := gr.Node(usedID)
	if !ok {
		t.Fatalf("image node %s missing", usedID)
	}
	if got, _ := used.Num(AttrReferenceCount); got != 1 {
		t.Errorf("ami-0used reference_count = %v, want 1 (launch template only)", got)
	}
	if got, _ := used.Num(AttrBackingSizeGB); got != 50 {
		t.Errorf("ami-0used backing_size_gb = %v, want 50", got)
	}
	if got, _ := used.Num(AttrBackingExclusiveSizeGB); got != 50 {
		t.Errorf("ami-0used backing_exclusive_size_gb = %v, want 50", got)
	}
	if complete, _ := used.Bool(AttrReferencesComplete); !complete {
		t.Error("ami-0used references_complete = false, want true")
	}
	if complete, _ := used.Bool(AttrBackingComplete); !complete {
		t.Error("ami-0used backing_complete = false, want true")
	}
	sources, ok := used.Attrs[AttrReferenceSources].([]string)
	if !ok || len(sources) != 1 || sources[0] != "launch_template:lt-0a:1" {
		t.Errorf("ami-0used reference_sources = %#v, want [launch_template:lt-0a:1]", used.Attrs[AttrReferenceSources])
	}

	unusedID := graph.Ref("accounts/123456789012/regions/us-east-1/images/ami-0unused")
	unused, ok := gr.Node(unusedID)
	if !ok {
		t.Fatalf("image node %s missing", unusedID)
	}
	if got, _ := unused.Num(AttrReferenceCount); got != 0 {
		t.Errorf("ami-0unused reference_count = %v, want 0", got)
	}
	if got, _ := unused.Num(AttrBackingExclusiveSizeGB); got != 100 {
		t.Errorf("ami-0unused backing_exclusive_size_gb = %v, want 100", got)
	}
	if mappings, ok := unused.Attrs[AttrBlockDeviceMappings].([]map[string]any); !ok || len(mappings) != 1 {
		t.Errorf("ami-0unused block_device_mappings = %#v, want one mapping", unused.Attrs[AttrBlockDeviceMappings])
	}
}

func TestIngest_NoImageHintSkipsAMIDiscovery(t *testing.T) {
	p, err := New(context.Background(),
		WithOffline(),
		WithFixture(amiFixture()),
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

	if got := gr.CountByKind(graph.KindImage); got != 0 {
		t.Fatalf("image nodes = %d, want 0 (AMI discovery is gated on asset type hints)", got)
	}
	if got := gr.CountByKind(graph.KindSnapshot); got != 0 {
		t.Fatalf("snapshot nodes = %d, want 0", got)
	}
}

// allReferenceSourcesFixture adds an AMI per reference source beyond launch
// templates: an Auto Scaling launch configuration, an EC2 Fleet inline
// override, and a Spot Fleet inline launch specification.
//
// Each of those collectors was previously reachable only in production. A wrong
// field path or a missing nil check in any of them would compile, pass the
// suite, and quietly report an in-use AMI as deletable — which is precisely how
// the AWS Auto Scaling guard shipped dead for a release while its own test
// passed against a hand-built label.
func allReferenceSourcesFixture() *Fixture {
	f := amiFixture()
	r := f.Regions["us-east-1"]

	for _, id := range []string{"ami-0lc", "ami-0fleet", "ami-0spot"} {
		r.Images = append(r.Images, ec2types.Image{
			ImageId:        strPtr(id),
			Name:           strPtr(id + "-name"),
			CreationDate:   strPtr("2024-01-01T00:00:00Z"),
			State:          ec2types.ImageStateAvailable,
			RootDeviceType: ec2types.DeviceTypeEbs,
			BlockDeviceMappings: []ec2types.BlockDeviceMapping{{
				DeviceName: strPtr("/dev/sda1"),
				Ebs:        &ec2types.EbsBlockDevice{SnapshotId: strPtr("snap-" + id)},
			}},
		})
		r.Snapshots = append(r.Snapshots, ec2types.Snapshot{
			SnapshotId: strPtr("snap-" + id), VolumeSize: int32Ptr(10),
		})
	}

	r.LaunchConfigurations = []asgtypes.LaunchConfiguration{
		{LaunchConfigurationName: strPtr("lc-0a"), ImageId: strPtr("ami-0lc")},
	}
	r.Fleets = []ec2types.FleetData{{
		FleetId: strPtr("fleet-0a"),
		LaunchTemplateConfigs: []ec2types.FleetLaunchTemplateConfig{{
			Overrides: []ec2types.FleetLaunchTemplateOverrides{{ImageId: strPtr("ami-0fleet")}},
		}},
	}}
	r.SpotFleetRequests = []ec2types.SpotFleetRequestConfig{{
		SpotFleetRequestId: strPtr("sfr-0a"),
		SpotFleetRequestConfig: &ec2types.SpotFleetRequestConfigData{
			LaunchSpecifications: []ec2types.SpotFleetLaunchSpecification{{ImageId: strPtr("ami-0spot")}},
		},
	}}
	return f
}

// TestIngest_EveryReferenceSourceMarksItsAMIReferenced drives all five sources
// through the real ingest path and asserts each AMI is seen as referenced.
func TestIngest_EveryReferenceSourceMarksItsAMIReferenced(t *testing.T) {
	p, err := New(context.Background(),
		WithOffline(),
		WithFixture(allReferenceSourcesFixture()),
		WithLogger(newTestLogger()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	sc := cloud.Scope{Provider: "aws", AWS: &cloud.AWSScope{Account: "123456789012"}}
	g, err := p.Ingest(context.Background(), sc, []string{TypeImage})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	refCount := map[string]float64{}
	complete := map[string]bool{}
	g.Nodes(func(n *graph.Node) bool {
		if n.Kind != graph.KindImage {
			return true
		}
		id, _ := n.Str(AttrImageID)
		c, _ := n.Num(AttrReferenceCount)
		rc, _ := n.Bool(AttrReferencesComplete)
		refCount[id], complete[id] = c, rc
		return true
	})

	for _, id := range []string{"ami-0used", "ami-0lc", "ami-0fleet", "ami-0spot"} {
		if refCount[id] < 1 {
			t.Errorf("%s reference count = %v, want >= 1: its reference source is not being "+
				"enumerated, so an in-use AMI would be reported as deletable", id, refCount[id])
		}
		if !complete[id] {
			t.Errorf("%s references_complete = false, want true", id)
		}
	}
	if refCount["ami-0unused"] != 0 {
		t.Errorf("ami-0unused reference count = %v, want 0", refCount["ami-0unused"])
	}
}
