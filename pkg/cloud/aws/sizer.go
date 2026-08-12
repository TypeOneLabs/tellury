package aws

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// Sizer is the AWS implementation of pricing.Sizer: it answers "what shape is
// this instance type" and "what else exists in its family", which is what lets
// a rule recommend a smaller size instead of only ever saying "stop it".
//
// The shapes come from ec2:DescribeInstanceTypes, live, exactly like the
// per-instance shapes ingest already resolves. There is deliberately no
// embedded AWS instance-type table: this project removed its static price
// tables one release ago because hand-maintained cloud data goes stale
// silently, and a shape table would reintroduce the same failure with the
// same invisibility.
//
// Ingest resolves shapes only for the instance types actually RUNNING, which
// is enough to describe an instance but not to rightsize it — a ladder needs
// the siblings that are not running, since those are precisely the candidates.
// So the family sweep below asks for every size in each family present.
type Sizer struct {
	mu     sync.RWMutex
	byName map[string]pricing.MachineSpec
	// families records which families have already been swept, so a second
	// region carrying the same family costs no further API calls. Shapes are
	// global — a t3.micro is 2 vCPU / 1 GiB everywhere — even though which
	// types are OFFERED varies by region.
	families map[string]bool
}

// NewSizer returns an empty Sizer. It is populated by LoadFamilies during
// ingest and read by rules afterwards.
func NewSizer() *Sizer {
	return &Sizer{
		byName:   map[string]pricing.MachineSpec{},
		families: map[string]bool{},
	}
}

// FamilyOf returns the family prefix of an instance type: "t3.micro" -> "t3",
// "m5a.2xlarge" -> "m5a". A type with no "." (which the real API does not
// return) is its own family.
func FamilyOf(instanceType string) string {
	if i := strings.Index(instanceType, "."); i > 0 {
		return instanceType[:i]
	}
	return instanceType
}

// Spec implements pricing.Sizer.
func (s *Sizer) Spec(instanceType string) (pricing.MachineSpec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	spec, ok := s.byName[instanceType]
	return spec, ok
}

// Family implements pricing.Sizer.
func (s *Sizer) Family(instanceType string) string { return FamilyOf(instanceType) }

// Ladder implements pricing.Sizer: every known member of a family, sorted
// ascending by (VCPU, MemoryGiB, Name) so a caller walking it from the start
// meets the smallest candidate first. The order is total and deterministic —
// a rule's recommendation must not depend on map iteration order.
func (s *Sizer) Ladder(family string) []pricing.MachineSpec {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []pricing.MachineSpec
	for _, spec := range s.byName {
		if spec.Family == family {
			out = append(out, spec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].VCPU != out[j].VCPU {
			return out[i].VCPU < out[j].VCPU
		}
		if out[i].MemoryGiB != out[j].MemoryGiB {
			return out[i].MemoryGiB < out[j].MemoryGiB
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// LoadFamilies sweeps every family in families that has not been swept before,
// resolving all of its sizes through DescribeInstanceTypes and recording them.
//
// One call per family, filtered by the "instance-type" wildcard ("t3.*"),
// paginated. A family whose sweep fails is left unswept rather than half
// recorded: a partial ladder would silently narrow the candidate set, and a
// rule that recommends the second-smallest size because the smallest was
// dropped is worse than one that recommends nothing.
func (s *Sizer) LoadFamilies(ctx context.Context, client ec2API, families []string) error {
	var firstErr error
	for _, family := range families {
		if family == "" {
			continue
		}
		s.mu.RLock()
		done := s.families[family]
		s.mu.RUnlock()
		if done {
			continue
		}

		specs, err := describeFamily(ctx, client, family)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		s.mu.Lock()
		for _, spec := range specs {
			s.byName[spec.Name] = spec
		}
		s.families[family] = true
		s.mu.Unlock()
	}
	return firstErr
}

// describeFamily pages DescribeInstanceTypes for one family and converts each
// result to a MachineSpec. A type missing vCPU or memory is DROPPED rather
// than recorded as zero: a zero-vCPU shape would sort to the front of the
// ladder and be recommended to everyone.
func describeFamily(ctx context.Context, client ec2API, family string) ([]pricing.MachineSpec, error) {
	var (
		out       []pricing.MachineSpec
		nextToken *string
	)
	for {
		resp, err := client.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
			Filters: []ec2types.Filter{{
				Name:   aws.String("instance-type"),
				Values: []string{family + ".*"},
			}},
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, it := range resp.InstanceTypes {
			name := string(it.InstanceType)
			if name == "" {
				continue
			}
			if it.VCpuInfo == nil || it.VCpuInfo.DefaultVCpus == nil {
				continue
			}
			if it.MemoryInfo == nil || it.MemoryInfo.SizeInMiB == nil {
				continue
			}
			vcpu := float64(*it.VCpuInfo.DefaultVCpus)
			memGiB := float64(*it.MemoryInfo.SizeInMiB) / 1024.0
			if vcpu <= 0 || memGiB <= 0 {
				continue
			}
			out = append(out, pricing.MachineSpec{
				Name:      name,
				Family:    FamilyOf(name),
				VCPU:      vcpu,
				MemoryGiB: memGiB,
				// Burstable families (t2/t3/t4g) are shared-core. The rule
				// does not cross families, so this is descriptive only.
				SharedCore: it.BurstablePerformanceSupported != nil && *it.BurstablePerformanceSupported,
			})
		}
		if resp.NextToken == nil || *resp.NextToken == "" {
			break
		}
		nextToken = resp.NextToken
	}
	return out, nil
}
