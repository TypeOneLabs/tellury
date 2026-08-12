package aws

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// sizerFakeEC2 returns a fixed t3 family and records the filters it was asked
// for, so the test can assert the sweep is family-scoped rather than a full
// DescribeInstanceTypes dump.
type sizerFakeEC2 struct {
	ec2API
	gotFilters []string
	pages      int
}

func (f *sizerFakeEC2) DescribeInstanceTypes(_ context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	for _, flt := range in.Filters {
		for _, v := range flt.Values {
			f.gotFilters = append(f.gotFilters, aws.ToString(flt.Name)+"="+v)
		}
	}
	mk := func(name string, vcpu int32, mib int64) ec2types.InstanceTypeInfo {
		return ec2types.InstanceTypeInfo{
			InstanceType: ec2types.InstanceType(name),
			VCpuInfo:     &ec2types.VCpuInfo{DefaultVCpus: aws.Int32(vcpu)},
			MemoryInfo:   &ec2types.MemoryInfo{SizeInMiB: aws.Int64(mib)},
		}
	}
	// Second page proves pagination is followed: t3.small arrives last but
	// must still sort into the middle of the ladder.
	if in.NextToken == nil {
		f.pages++
		return &ec2.DescribeInstanceTypesOutput{
			InstanceTypes: []ec2types.InstanceTypeInfo{
				mk("t3.micro", 2, 1024),
				mk("t3.nano", 2, 512),
				// Missing vCPU/memory: must be DROPPED, not recorded as zero.
				{InstanceType: "t3.broken"},
			},
			NextToken: aws.String("page2"),
		}, nil
	}
	f.pages++
	return &ec2.DescribeInstanceTypesOutput{
		InstanceTypes: []ec2types.InstanceTypeInfo{mk("t3.small", 2, 2048)},
	}, nil
}

func TestSizer_LadderIsSortedAndFamilyScoped(t *testing.T) {
	f := &sizerFakeEC2{}
	s := NewSizer()
	if err := s.LoadFamilies(context.Background(), f, []string{"t3"}); err != nil {
		t.Fatalf("LoadFamilies: %v", err)
	}
	if f.pages != 2 {
		t.Errorf("pagination not followed: %d pages", f.pages)
	}
	want := "instance-type=t3.*"
	if len(f.gotFilters) == 0 || f.gotFilters[0] != want {
		t.Errorf("filter = %v, want %q — an unfiltered sweep returns every type AWS offers", f.gotFilters, want)
	}

	ladder := s.Ladder("t3")
	var names []string
	for _, m := range ladder {
		names = append(names, m.Name)
	}
	// Ascending by (VCPU, MemoryGiB, Name); t3.broken dropped.
	if len(names) != 3 || names[0] != "t3.nano" || names[1] != "t3.micro" || names[2] != "t3.small" {
		t.Errorf("ladder = %v, want [t3.nano t3.micro t3.small]", names)
	}
	for _, m := range ladder {
		if m.VCPU <= 0 || m.MemoryGiB <= 0 {
			t.Errorf("%s has zero shape %v/%v — would sort first and be recommended to everyone", m.Name, m.VCPU, m.MemoryGiB)
		}
	}
}

func TestSizer_SecondSweepOfSameFamilyCostsNoCalls(t *testing.T) {
	f := &sizerFakeEC2{}
	s := NewSizer()
	ctx := context.Background()
	_ = s.LoadFamilies(ctx, f, []string{"t3"})
	before := f.pages
	_ = s.LoadFamilies(ctx, f, []string{"t3"})
	if f.pages != before {
		t.Errorf("family re-swept: %d -> %d calls; a second region must reuse the ladder", before, f.pages)
	}
}

func TestFamilyOf(t *testing.T) {
	for in, want := range map[string]string{
		"t3.micro": "t3", "m5a.2xlarge": "m5a", "c6i.large": "c6i", "weird": "weird",
	} {
		if got := FamilyOf(in); got != want {
			t.Errorf("FamilyOf(%q) = %q, want %q", in, got, want)
		}
	}
}
