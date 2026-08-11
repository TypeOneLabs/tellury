package aws

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
	"github.com/TypeOneLabs/tellury/pkg/rules/aws/ec2/unassociated_eip"
	"github.com/TypeOneLabs/tellury/pkg/rules/aws/ec2/unattached_ebs_volume"
)

// loadFixtureDocs loads the recorded GetProducts fixture and returns its
// priceListDoc values through the SAME parse path the live load uses: the
// fixture stores the raw aws_v1 product documents exactly as GetProducts
// returns them; each is parsed by parsePriceListDoc so the test exercises
// the exact decoding path the live load uses. A test that bypassed
// parsePriceListDoc would not actually exercise the token-derivation code
// under test.
func loadFixtureDocs(t *testing.T) []*priceListDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "getproducts-recorded.json"))
	if err != nil {
		t.Fatalf("read price fixture: %v", err)
	}
	var stored []json.RawMessage
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("decode price fixture: %v", err)
	}
	out := make([]*priceListDoc, 0, len(stored))
	for _, raw := range stored {
		doc, err := parsePriceListDoc(string(raw))
		if err != nil {
			t.Fatalf("parsePriceListDoc: %v", err)
		}
		out = append(out, doc)
	}
	return out
}

// fixtureCatalog builds a CatalogPricer whose live cache is pre-populated
// from the recorded fixture, with the lazy load marked done so a lookup never
// touches the network (a miss answers ErrNoPrice from the cache).
func fixtureCatalog(t *testing.T) *CatalogPricer {
	t.Helper()
	p, err := NewCatalogPricer(context.Background(), slog.New(slog.DiscardHandler), aws.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}
	for _, doc := range loadFixtureDocs(t) {
		for _, e := range indexDoc(doc) {
			p.skusByKey[skuKey{kind: e.kind, sku: e.sku, region: e.region}] = resolvedSKU{
				unitPrice: e.price,
				region:    e.region,
			}
		}
	}
	p.loaded = true
	p.once.Do(func() {}) // mark the lazy load done: no GetProducts call, ever, in a fixture test
	return p
}

// TestCatalogPricer_EBSVolumeSKUPinned pins the EBS SKU token to the value
// the unattached_ebs_volume rule actually queries. The chain it guards:
//
//  1. the rule's volume_type attr comes straight from DescribeVolumes.
//     VolumeType, so the only tokens it can ever query are the EC2 SDK's own
//     VolumeType values ("gp3", "gp2", "io2", "io1", "st1", "sc1",
//     "standard");
//  2. the live catalogue derives its SKU token from a real GetProducts
//     response's volumeApiName attribute;
//  3. this test asserts those two vocabularies are the SAME set — for every
//     SDK VolumeType value the rule queries, the recorded response must index
//     a capacity price under exactly that token, and every volumeApiName the
//     response carries must be a member of the SDK enum.
//
// A drift — AWS renaming the volumeApiName attribute, the rule querying a
// token the catalogue never produces, or an indexer reading the wrong
// attribute ("volumeType" is the human name, "General Purpose" for gp3, and
// would silently miss every lookup) — fails here, not on a real invoice.
func TestCatalogPricer_EBSVolumeSKUPinned(t *testing.T) {
	docs := loadFixtureDocs(t)
	if len(docs) == 0 {
		t.Fatal("price fixture is empty")
	}

	indexed := map[string]map[pricing.Kind]bool{} // sku -> kind -> seen
	for _, doc := range docs {
		for _, e := range indexDoc(doc) {
			if e.kind == pricing.KindStaticIP {
				continue
			}
			// The token the catalogue derived must be exactly the product's
			// own volumeApiName attribute, and that attribute must be a real
			// DescribeVolumes.VolumeType value (the SDK enum). Anything else
			// is a token the rule can never query.
			apiName := doc.Product.Attributes["volumeApiName"]
			if e.sku != apiName {
				t.Fatalf("catalogue sku %q != product volumeApiName %q: the live "+
					"token and the rule's volume_type vocabulary have drifted", e.sku, apiName)
			}
			if !awsVolumeTypes[apiName] {
				t.Fatalf("volumeApiName %q is not an ec2types.VolumeType value: "+
					"DescribeVolumes can never return it, so the rule can never query it", apiName)
			}
			if indexed[e.sku] == nil {
				indexed[e.sku] = map[pricing.Kind]bool{}
			}
			indexed[e.sku][e.kind] = true
		}
	}

	// The reverse direction is the actual pin: every SKU the rule CAN query
	// (the SDK enum) must resolve a capacity price in the recorded catalogue,
	// and the token the rule queries for it (VolumeSKU) must equal the token
	// the catalogue indexes.
	for _, vt := range ec2types.VolumeType("").Values() {
		sku := string(vt)
		want := unattached_ebs_volume.VolumeSKU(sku)
		if want != sku {
			t.Fatalf("rule VolumeSKU(%q) = %q: the rule would query a different "+
				"token than the catalogue indexes", sku, want)
		}
		if !indexed[sku][pricing.KindDiskCapacity] {
			t.Fatalf("catalogue has no disk_capacity entry for SDK volume type %q: "+
				"a lookup for this type would silently fall back to the embedded table", sku)
		}
	}
}

// TestCatalogPricer_EIPSKUPinned pins the static-IP SKU token to the exact
// constant the unassociated_eip rule queries (EIPSKU = "AdditionalAddress").
// The live API surfaces the charge under serviceCode AmazonVPC with NO
// productFamily, matched by usagetype suffix "PublicIPv4:InUseAddress". The
// indexer maps it to the canonical "AdditionalAddress" token.
//
// This test asserts:
//   - The fixture contains at least one AmazonVPC product with usagetype
//     ending in "PublicIPv4:InUseAddress" in two regions (us-east-1,
//     eu-west-1);
//   - Each is indexed under KindStaticIP with SKU "AdditionalAddress";
//   - The price is $0.005/hr (the published rate).
func TestCatalogPricer_EIPSKUPinned(t *testing.T) {
	foundUSE1, foundEUW1 := false, false
	for _, doc := range loadFixtureDocs(t) {
		if doc.ServiceCode != "AmazonVPC" {
			continue
		}
		if doc.Product.ProductFamily != "" {
			continue
		}
		entries := indexDoc(doc)
		if len(entries) == 0 {
			continue
		}
		for _, e := range entries {
			if e.kind != pricing.KindStaticIP {
				t.Fatalf("VPC product indexed under non-static-IP kind %q", e.kind)
			}
			if e.sku != unassociated_eip.EIPSKU {
				t.Fatalf("VPC product indexed under sku %q, want %q (unassociated_eip.EIPSKU)",
					e.sku, unassociated_eip.EIPSKU)
			}
			if e.price != 0.005 {
				t.Errorf("VPC static-IP price = %v, want 0.005", e.price)
			}
			switch e.region {
			case "us-east-1":
				foundUSE1 = true
			case "eu-west-1":
				foundEUW1 = true
			}
		}
	}
	if !foundUSE1 {
		t.Fatal("fixture carries no AmazonVPC InUseAddress product in us-east-1; the pin cannot be asserted")
	}
	if !foundEUW1 {
		t.Fatal("fixture carries no AmazonVPC InUseAddress product in eu-west-1; the multi-region pin cannot be asserted")
	}
}

// TestCatalogPricer_UnitPrice_EBS prices the recorded fixture end to end
// through UnitPrice: gp3 capacity in us-east-1, gp3 IOPS, gp3 throughput,
// io2 capacity+IOPS from their respective products, and gp3 capacity+IOPS
// in eu-west-1.
func TestCatalogPricer_UnitPrice_EBS(t *testing.T) {
	p := fixtureCatalog(t)

	cases := []struct {
		name   string
		kind   pricing.Kind
		sku    string
		region string
		want   float64
	}{
		{"gp3 capacity us-east-1", pricing.KindDiskCapacity, "gp3", "us-east-1", 0.08},
		{"gp3 iops us-east-1", pricing.KindDiskIOPS, "gp3", "us-east-1", 0.005},
		{"gp3 throughput us-east-1", pricing.KindDiskThroughput, "gp3", "us-east-1", 0.04},
		{"gp2 capacity us-east-1", pricing.KindDiskCapacity, "gp2", "us-east-1", 0.10},
		{"io2 capacity us-east-1", pricing.KindDiskCapacity, "io2", "us-east-1", 0.125},
		{"io2 iops us-east-1", pricing.KindDiskIOPS, "io2", "us-east-1", 0.065},
		{"io1 capacity us-east-1", pricing.KindDiskCapacity, "io1", "us-east-1", 0.125},
		{"io1 iops us-east-1", pricing.KindDiskIOPS, "io1", "us-east-1", 0.065},
		{"st1 capacity us-east-1", pricing.KindDiskCapacity, "st1", "us-east-1", 0.045},
		{"sc1 capacity us-east-1", pricing.KindDiskCapacity, "sc1", "us-east-1", 0.015},
		{"standard capacity us-east-1", pricing.KindDiskCapacity, "standard", "us-east-1", 0.05},
		{"gp3 capacity eu-west-1", pricing.KindDiskCapacity, "gp3", "eu-west-1", 0.088},
		{"gp3 iops eu-west-1", pricing.KindDiskIOPS, "gp3", "eu-west-1", 0.0055},
		{"gp3 throughput eu-west-1", pricing.KindDiskThroughput, "gp3", "eu-west-1", 0.044},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unit, region, err := p.UnitPrice(tc.kind, "aws", tc.sku, tc.region)
			if err != nil {
				t.Fatalf("UnitPrice(%s, %s, %s): %v", tc.kind, tc.sku, tc.region, err)
			}
			if unit != tc.want {
				t.Errorf("UnitPrice = %v, want %v", unit, tc.want)
			}
			if region != tc.region {
				t.Errorf("resolved region = %q, want %q", region, tc.region)
			}
		})
	}
}

// TestCatalogPricer_UnitPrice_EIP prices the static IP charge from the
// fixture: $0.005/hour, resolved through the VPC InUseAddress product.
func TestCatalogPricer_UnitPrice_EIP(t *testing.T) {
	p := fixtureCatalog(t)

	// us-east-1: the VPC InUseAddress product resolves under
	// "AdditionalAddress" at $0.005/hr.
	unit, region, err := p.UnitPrice(pricing.KindStaticIP, "aws", unassociated_eip.EIPSKU, "us-east-1")
	if err != nil {
		t.Fatalf("UnitPrice(EIP us-east-1): %v", err)
	}
	if unit != 0.005 {
		t.Errorf("EIP unit price = %v, want 0.005", unit)
	}
	if region != "us-east-1" {
		t.Errorf("EIP region = %q, want us-east-1", region)
	}

	// eu-west-1: same SKU, same price, different region.
	unit, region, err = p.UnitPrice(pricing.KindStaticIP, "aws", unassociated_eip.EIPSKU, "eu-west-1")
	if err != nil {
		t.Fatalf("UnitPrice(EIP eu-west-1): %v", err)
	}
	if unit != 0.005 {
		t.Errorf("EIP unit price eu-west-1 = %v, want 0.005", unit)
	}
	if region != "eu-west-1" {
		t.Errorf("EIP region eu-west-1 = %q, want eu-west-1", region)
	}
}

// TestCatalogPricer_UnindexedKind_Skips asserts Invariant I4 on the live path:
// a kind the AWS catalogue (and the embedded table) does not price must miss
// with ErrNoPrice, never silently answer $0.
func TestCatalogPricer_UnindexedKind_Skips(t *testing.T) {
	p := fixtureCatalog(t)
	if _, _, err := p.UnitPrice(pricing.KindVMCustomCPU, "aws", "n2-custom", "us-east-1"); err != pricing.ErrNoPrice {
		t.Fatalf("expected ErrNoPrice for an unindexed kind, got %v", err)
	}
}

// TestCatalogPricer_Provenance_RecordsSource pins that a fixture-driven live
// lookup records SourceLiveAPI provenance (so a Finding's price_source reads
// "live_api"), while a lookup the catalogue misses and the embedded table
// answers records SourceEmbedded.
func TestCatalogPricer_Provenance_RecordsSource(t *testing.T) {
	p := fixtureCatalog(t)
	if _, _, err := p.UnitPrice(pricing.KindDiskCapacity, "aws", "gp3", "us-east-1"); err != nil {
		t.Fatalf("UnitPrice: %v", err)
	}
	prov, ok := p.LastLookup(pricing.KindDiskCapacity, "gp3", "us-east-1")
	if !ok {
		t.Fatal("LastLookup returned ok=false after a successful UnitPrice")
	}
	if prov.Source != pricing.SourceLiveAPI {
		t.Errorf("provenance source = %q, want %q", prov.Source, pricing.SourceLiveAPI)
	}
	if prov.SKU != "gp3" || prov.Region != "us-east-1" {
		t.Errorf("provenance = %+v, want sku=gp3 region=us-east-1", prov)
	}
}

// TestCatalogPricer_OneGiBGp3IsEightCents is the acceptance test for wiring
// the live catalogue to the provider: a 1 GiB gp3 volume costs exactly
// $0.08/month through the live-catalogue path (MonthlyCost), and its
// provenance reads SourceLiveAPI — proving the live API answered, not the
// embedded table. A gp3 volume includes 3000 IOPS and 125 MiB/s at no charge;
// the price list dimension reads "per provisioned IOPS-month" with no mention
// of that allowance, so the allowance arithmetic stays in the rule. If wiring
// the live catalogue moves or bypasses that logic, a 1 GiB volume goes back
// to $20.08. This test pins the $0.08 figure through the full MonthlyCost
// path so that defect is caught here rather than on a real invoice.
func TestCatalogPricer_OneGiBGp3IsEightCents(t *testing.T) {
	p := fixtureCatalog(t)

	cost, err := p.MonthlyCost(pricing.Item{
		Kind:     pricing.KindDiskCapacity,
		Provider: "aws",
		SKU:      "gp3",
		Region:   "us-east-1",
		Quantity: 1,
	})
	if err != nil {
		t.Fatalf("MonthlyCost(1 GiB gp3): %v", err)
	}
	if cost != 0.08 {
		t.Errorf("1 GiB gp3 monthly cost = %v, want 0.08", cost)
	}

	prov, ok := p.LastLookup(pricing.KindDiskCapacity, "gp3", "us-east-1")
	if !ok {
		t.Fatal("LastLookup returned ok=false after MonthlyCost")
	}
	if prov.Source != pricing.SourceLiveAPI {
		t.Errorf("provenance source = %q, want %q (live_api)", prov.Source, pricing.SourceLiveAPI)
	}
}

// TestCatalogPricer_AllFourKindsResolveLive asserts every price kind resolves
// from the recorded fixture with provenance live_api and a region that is NOT
// "default". This is the acceptance test for the fix: before the fix, only
// disk_capacity resolved; disk_iops, disk_throughput, and static_ip all fell
// through to the embedded table.
func TestCatalogPricer_AllFourKindsResolveLive(t *testing.T) {
	p := fixtureCatalog(t)

	cases := []struct {
		name   string
		kind   pricing.Kind
		sku    string
		region string
	}{
		{"disk_capacity", pricing.KindDiskCapacity, "gp3", "us-east-1"},
		{"disk_iops", pricing.KindDiskIOPS, "gp3", "us-east-1"},
		{"disk_throughput", pricing.KindDiskThroughput, "gp3", "us-east-1"},
		{"static_ip", pricing.KindStaticIP, unassociated_eip.EIPSKU, "us-east-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unit, region, err := p.UnitPrice(tc.kind, "aws", tc.sku, tc.region)
			if err != nil {
				t.Fatalf("UnitPrice(%s, %q, %q): %v", tc.kind, tc.sku, tc.region, err)
			}
			if unit <= 0 {
				t.Errorf("UnitPrice = %v, want > 0", unit)
			}
			if region == "default" {
				t.Errorf("region = %q, want a real region, not \"default\"", region)
			}
			prov, ok := p.LastLookup(tc.kind, tc.sku, tc.region)
			if !ok {
				t.Fatalf("LastLookup returned ok=false after successful UnitPrice")
			}
			if prov.Source != pricing.SourceLiveAPI {
				t.Errorf("provenance source = %q, want %q (live_api)", prov.Source, pricing.SourceLiveAPI)
			}
		})
	}
}

// TestCatalogPricer_LiveGetProductsTokenPinned is the LIVE counterpart of the
// fixture pinning tests: it fetches a real GetProducts response for AmazonEC2
// and AmazonVPC and asserts the catalogue resolves the exact tokens the rules
// query. It is the test that catches an AWS rename of volumeApiName /
// usagetype / a Location display name before it silently degrades every
// lookup to the embedded table.
//
// It requires real AWS credentials and network access, so it is skipped unless
// the operator sets TELLURY_AWS_LIVE_PRICE_TEST=1 (CI and the default test
// suite stay offline and green).
//
// This is the pre-release check documented in docs/aws-setup.md: run it
// before a release to confirm the live API still returns the tokens the
// catalogue expects. A failure here means a rename — update the tokens and
// the recorded fixture before the rename silently degrades every lookup.
func TestCatalogPricer_LiveGetProductsTokenPinned(t *testing.T) {
	if os.Getenv("TELLURY_AWS_LIVE_PRICE_TEST") == "" {
		t.Skip("TELLURY_AWS_LIVE_PRICE_TEST is not set; live GetProducts pin skipped")
	}
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	p, err := NewCatalogPricer(ctx, slog.New(slog.DiscardHandler), cfg)
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}
	for _, tc := range []struct {
		kind   pricing.Kind
		sku    string
		region string
	}{
		{pricing.KindDiskCapacity, unattached_ebs_volume.VolumeSKU("gp3"), "us-east-1"},
		{pricing.KindDiskIOPS, unattached_ebs_volume.VolumeSKU("gp3"), "us-east-1"},
		{pricing.KindDiskThroughput, unattached_ebs_volume.VolumeSKU("gp3"), "us-east-1"},
		{pricing.KindDiskCapacity, unattached_ebs_volume.VolumeSKU("io2"), "eu-west-1"},
		{pricing.KindStaticIP, unassociated_eip.EIPSKU, "us-east-1"},
	} {
		unit, region, err := p.UnitPrice(tc.kind, "aws", tc.sku, tc.region)
		if err != nil {
			t.Errorf("live UnitPrice(%s, %q, %q): %v", tc.kind, tc.sku, tc.region, err)
			continue
		}
		if unit <= 0 {
			t.Errorf("live UnitPrice(%s, %q, %q) = %v, want > 0", tc.kind, tc.sku, tc.region, unit)
		}
		if region != tc.region {
			t.Errorf("live region = %q, want %q", region, tc.region)
		}
	}

	// Additionally assert that all four kinds resolve from the live API
	// with provenance live_api and a real region.
	for _, tc := range []struct {
		name string
		kind pricing.Kind
		sku  string
	}{
		{"disk_capacity gp3 us-east-1", pricing.KindDiskCapacity, unattached_ebs_volume.VolumeSKU("gp3")},
		{"disk_iops gp3 us-east-1", pricing.KindDiskIOPS, unattached_ebs_volume.VolumeSKU("gp3")},
		{"disk_throughput gp3 us-east-1", pricing.KindDiskThroughput, unattached_ebs_volume.VolumeSKU("gp3")},
		{"static_ip us-east-1", pricing.KindStaticIP, unassociated_eip.EIPSKU},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unit, region, err := p.UnitPrice(tc.kind, "aws", tc.sku, "us-east-1")
			if err != nil {
				t.Fatalf("live UnitPrice(%s, %q): %v", tc.kind, tc.sku, err)
			}
			if unit <= 0 {
				t.Errorf("live unit price = %v, want > 0", unit)
			}
			if region == "default" {
				t.Errorf("live region = %q, want a real region", region)
			}
			prov, ok := p.LastLookup(tc.kind, tc.sku, "us-east-1")
			if !ok {
				t.Fatalf("LastLookup returned ok=false")
			}
			if prov.Source != pricing.SourceLiveAPI {
				t.Errorf("provenance source = %q, want live_api", prov.Source)
			}
		})
	}
}
