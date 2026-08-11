package aws

import (
	"os"
	"path/filepath"
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules/aws/ec2/unassociated_eip"
	"github.com/TypeOneLabs/tellury/pkg/rules/aws/ec2/unattached_ebs_volume"
)

// TestStaticPricer_SKUTokensAgreeWithRules pins the embedded table's SKU
// vocabulary to the exact tokens the two AWS rules query, so the live answer
// and the offline fallback can never resolve different keys (the static-IP /
// snapshot failure mode on the GCP side):
//
//   - every volume type the unattached_ebs_volume rule can query (the EC2
//     SDK's own VolumeType enum, via VolumeSKU) must resolve a capacity
//     price in the embedded table;
//   - gp3 must also resolve IOPS and throughput; io1/io2 must also resolve
//     IOPS;
//   - the unassociated_eip rule's EIPSKU must resolve an hourly price.
func TestStaticPricer_SKUTokensAgreeWithRules(t *testing.T) {
	p, err := NewStaticPricer()
	if err != nil {
		t.Fatalf("NewStaticPricer: %v", err)
	}

	for _, vt := range ec2types.VolumeType("").Values() {
		sku := string(vt)
		ruleToken := unattached_ebs_volume.VolumeSKU(sku)
		if ruleToken != sku {
			t.Fatalf("rule VolumeSKU(%q) = %q; embedded table and rule use different spellings", sku, ruleToken)
		}
		if _, _, err := p.UnitPrice(pricing.KindDiskCapacity, "aws", ruleToken, "us-east-1"); err != nil {
			t.Errorf("embedded table has no disk_capacity entry for rule token %q: %v", ruleToken, err)
		}
	}

	// Provisioned dimensions must exist for the types that bill them.
	for _, tc := range []struct {
		sku  string
		kind pricing.Kind
	}{
		{"gp3", pricing.KindDiskIOPS},
		{"gp3", pricing.KindDiskThroughput},
		{"io1", pricing.KindDiskIOPS},
		{"io2", pricing.KindDiskIOPS},
	} {
		if _, _, err := p.UnitPrice(tc.kind, "aws", tc.sku, "us-east-1"); err != nil {
			t.Errorf("embedded table has no %s entry for %q: %v", tc.kind, tc.sku, err)
		}
	}

	if unit, _, err := p.UnitPrice(pricing.KindStaticIP, "aws", unassociated_eip.EIPSKU, "us-east-1"); err != nil {
		t.Errorf("embedded table has no static_ip entry for rule token %q: %v", unassociated_eip.EIPSKU, err)
	} else if unit != 0.005 {
		t.Errorf("embedded EIP hourly rate = %v, want 0.005", unit)
	}
}

// TestStaticPricer_EmbeddedValuesArePublishedRates pins the embedded snapshot
// against the published AWS list prices (the us-east-1 baseline the "default"
// key answers for every region). These exact values flow into an offline
// scan's findings, so a typo here would misstate every offline AWS finding.
func TestStaticPricer_EmbeddedValuesArePublishedRates(t *testing.T) {
	p, err := NewStaticPricer()
	if err != nil {
		t.Fatalf("NewStaticPricer: %v", err)
	}
	cases := []struct {
		kind pricing.Kind
		sku  string
		want float64
	}{
		{pricing.KindDiskCapacity, "gp3", 0.08},
		{pricing.KindDiskCapacity, "gp2", 0.10},
		{pricing.KindDiskCapacity, "io1", 0.125},
		{pricing.KindDiskCapacity, "io2", 0.125},
		{pricing.KindDiskCapacity, "st1", 0.045},
		{pricing.KindDiskCapacity, "sc1", 0.025},
		{pricing.KindDiskCapacity, "standard", 0.05},
		{pricing.KindDiskIOPS, "gp3", 0.005},
		{pricing.KindDiskIOPS, "io1", 0.065},
		{pricing.KindDiskIOPS, "io2", 0.065},
		{pricing.KindDiskThroughput, "gp3", 0.04},
		{pricing.KindStaticIP, unassociated_eip.EIPSKU, 0.005},
	}
	for _, tc := range cases {
		unit, _, err := p.UnitPrice(tc.kind, "aws", tc.sku, "us-east-1")
		if err != nil {
			t.Errorf("UnitPrice(%s, %s): %v", tc.kind, tc.sku, err)
			continue
		}
		if unit != tc.want {
			t.Errorf("UnitPrice(%s, %s) = %v, want %v", tc.kind, tc.sku, unit, tc.want)
		}
	}
}

// TestStaticPricer_RegionFallsBackToDefault: the embedded table keys every
// rate under "default" (the commercial-region baseline), so a lookup for a
// region the table does not spell resolves the default rate — never ErrNoPrice
// — and reports the resolved key as "default".
func TestStaticPricer_RegionFallsBackToDefault(t *testing.T) {
	p, err := NewStaticPricer()
	if err != nil {
		t.Fatalf("NewStaticPricer: %v", err)
	}
	unit, region, err := p.UnitPrice(pricing.KindDiskCapacity, "aws", "gp3", "ap-southeast-2")
	if err != nil {
		t.Fatalf("region fallback must resolve, not miss: %v", err)
	}
	if unit != 0.08 || region != "default" {
		t.Errorf("region fallback = %v/%q, want 0.08/default", unit, region)
	}
}

// TestStaticPricer_OverlayFile_Merges: a --price-file entry for AWS lands on
// top of the embedded table (per Kind/SKU/region) and is visible to the next
// UnitPrice call.
func TestStaticPricer_OverlayFile_Merges(t *testing.T) {
	p, err := NewStaticPricer()
	if err != nil {
		t.Fatalf("NewStaticPricer: %v", err)
	}
	path := filepath.Join(t.TempDir(), "aws-prices.json")
	if err := os.WriteFile(path, []byte(`{"disk_capacity": {"gp3": {"us-east-1": 0.09}}}`), 0o644); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	if err := p.OverlayFile(path); err != nil {
		t.Fatalf("OverlayFile: %v", err)
	}
	unit, region, err := p.UnitPrice(pricing.KindDiskCapacity, "aws", "gp3", "us-east-1")
	if err != nil {
		t.Fatalf("UnitPrice after overlay: %v", err)
	}
	if unit != 0.09 || region != "us-east-1" {
		t.Errorf("overlaid price = %v/%q, want 0.09/us-east-1", unit, region)
	}
	// An untouched key keeps the embedded value.
	if unit, _, err := p.UnitPrice(pricing.KindDiskCapacity, "aws", "gp2", "us-east-1"); err != nil || unit != 0.10 {
		t.Errorf("untouched key = %v (err %v), want 0.10", unit, err)
	}
}
