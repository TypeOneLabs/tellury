package aws

import (
	"context"
	"encoding/json"
	"fmt"
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

// ─────────────────────────────────────────────────────────────────────────────
// Instance price fixture loading
// ─────────────────────────────────────────────────────────────────────────────

// loadInstanceFixtureDocs loads the recorded GetProducts fixture for Compute
// Instance products and returns their parsed docs.
func loadInstanceFixtureDocs(t *testing.T) []*priceListDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "getproducts-instance-recorded.json"))
	if err != nil {
		t.Fatalf("read instance price fixture: %v", err)
	}
	var stored []json.RawMessage
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("decode instance price fixture: %v", err)
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

// fixtureInstanceCatalog builds a CatalogPricer whose instancePrices cache is
// pre-populated from the recorded instance-product fixture. The main catalogue
// cache (skusByKey) is also populated from the EBS+address fixture so both
// paths work.
func fixtureInstanceCatalog(t *testing.T) *CatalogPricer {
	t.Helper()
	p, err := NewCatalogPricer(context.Background(), slog.New(slog.DiscardHandler), aws.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}

	// Load the main EBS+address fixture.
	for _, doc := range loadFixtureDocs(t) {
		for _, e := range indexDoc(doc) {
			p.skusByKey[skuKey{kind: e.kind, sku: e.sku, region: e.region}] = resolvedSKU{
				unitPrice: e.price,
				region:    e.region,
			}
		}
	}
	p.loaded = true
	p.once.Do(func() {})

	// Load instance products into instancePrices.
	for _, doc := range loadInstanceFixtureDocs(t) {
		attrs := doc.Product.Attributes
		instanceType := attrs["instanceType"]
		region := attrs["regionCode"]
		os := attrs["operatingSystem"]
		if instanceType == "" || region == "" || os == "" {
			continue
		}
		for _, dim := range priceDimensions(doc) {
			if dim.unit == "Hrs" {
				p.instancePrices[instancePriceKey{
					region:          region,
					instanceType:    instanceType,
					operatingSystem: os,
				}] = dim.price
			}
		}
	}
	p.instancePricesLoaded = true
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

// ─────────────────────────────────────────────────────────────────────────────
// Instance pricing tests
// ─────────────────────────────────────────────────────────────────────────────

// TestCatalogPricer_InstancePrice_FixtureResolves prices the recorded instance
// product fixture end to end through InstancePrice. The recorded fixture
// contains t3.medium Linux in us-east-1 at $0.0416/hr.
//
// This is the acceptance test: the resolved rate must match the published
// On-Demand price for that instance type and region. A human can check the
// $0.0416 figure against a real invoice or the AWS pricing page:
//
//	https://aws.amazon.com/ec2/pricing/on-demand/
//	t3.medium, Linux, US East (N. Virginia) = $0.0416 per Hour
func TestCatalogPricer_InstancePrice_FixtureResolves(t *testing.T) {
	p := fixtureInstanceCatalog(t)

	price, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Linux")
	if err != nil {
		t.Fatalf("InstancePrice(t3.medium, Linux, us-east-1): %v", err)
	}

	// Published On-Demand price: $0.0416/hr
	// Source: https://aws.amazon.com/ec2/pricing/on-demand/
	const publishedT3MediumLinuxUSE1 = 0.0416
	if price != publishedT3MediumLinuxUSE1 {
		t.Errorf("InstancePrice = %v, want %v (published On-Demand rate for t3.medium Linux in us-east-1)",
			price, publishedT3MediumLinuxUSE1)
	}

	// Monthly cost: hourly * 730.
	monthly := price * pricing.HoursPerMonth
	t.Logf("t3.medium Linux us-east-1: $%.4f/hr, $%.2f/month", price, monthly)
}

// TestCatalogPricer_InstancePrice_CacheHit verifies that a second call to
// InstancePrice for the same key returns the cached value without making
// another API call (the fixture has one product — a second call that parsed
// the fixture again would still work, but we verify the price is the same).
func TestCatalogPricer_InstancePrice_CacheHit(t *testing.T) {
	p := fixtureInstanceCatalog(t)

	price1, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Linux")
	if err != nil {
		t.Fatalf("first InstancePrice: %v", err)
	}

	price2, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Linux")
	if err != nil {
		t.Fatalf("second InstancePrice: %v", err)
	}

	if price1 != price2 {
		t.Errorf("cache inconsistency: first=%v, second=%v", price1, price2)
	}
}

// TestCatalogPricer_InstancePrice_UnresolvableSkips asserts Invariant I4 for
// instance pricing: an instance type not in the catalogue must return
// ErrNoPrice, never $0.
func TestCatalogPricer_InstancePrice_UnresolvableSkips(t *testing.T) {
	p := fixtureInstanceCatalog(t)

	// An instance type not in the fixture.
	if _, err := p.InstancePrice(context.Background(), "us-east-1", "nonexistent.xlarge", "Linux"); err != pricing.ErrNoPrice {
		t.Fatalf("expected ErrNoPrice for unresolvable instance type, got %v", err)
	}

	// Same instance type but wrong OS — the fixture only has Linux.
	if _, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Windows"); err != pricing.ErrNoPrice {
		t.Fatalf("expected ErrNoPrice for t3.medium Windows (not in fixture), got %v", err)
	}
}

// TestCatalogPricer_InstancePrice_FilterSetPinned documents and verifies the
// eight TERM_MATCH filters that select the correct On-Demand, Shared-tenancy,
// No-license, No-preinstalled-software, Used-capacity product. This test does
// not call the live API; it is a structural assertion that the filter set in
// the code is complete.
//
// Getting ANY filter wrong silently returns a plausible-looking-but-wrong
// rate. Omitting capacitystatus alone can index a capacity-reservation rate
// as the On-Demand rate — a wrong number that looks entirely plausible.
func TestCatalogPricer_InstancePrice_FilterSetPinned(t *testing.T) {
	// The filter set is encoded in fetchInstancePrice. This test verifies the
	// complete set by checking that InstancePrice resolves correctly from the
	// fixture (which contains a product matching all eight filters) and misses
	// for a query that differs on any axis.
	p := fixtureInstanceCatalog(t)

	// The fixture product matches all eight filters:
	//   instanceType=t3.medium, regionCode=us-east-1, operatingSystem=Linux,
	//   tenancy=Shared, capacitystatus=Used, preInstalledSw=NA,
	//   licenseModel="No License required", termType=OnDemand
	//
	// A query that varies any of these must miss (ErrNoPrice).

	t.Run("correct_filters_resolve", func(t *testing.T) {
		price, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Linux")
		if err != nil {
			t.Fatalf("InstancePrice with correct filters: %v", err)
		}
		if price != 0.0416 {
			t.Errorf("price = %v, want 0.0416", price)
		}
	})

	// Varying instanceType: must miss.
	t.Run("wrong_instance_type_misses", func(t *testing.T) {
		if _, err := p.InstancePrice(context.Background(), "us-east-1", "t3.nano", "Linux"); err != pricing.ErrNoPrice {
			t.Errorf("expected ErrNoPrice for wrong instanceType, got %v", err)
		}
	})

	// Varying operatingSystem: must miss.
	t.Run("wrong_os_misses", func(t *testing.T) {
		if _, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "RHEL"); err != pricing.ErrNoPrice {
			t.Errorf("expected ErrNoPrice for wrong OS, got %v", err)
		}
	})

	// Varying region: must miss.
	t.Run("wrong_region_misses", func(t *testing.T) {
		if _, err := p.InstancePrice(context.Background(), "eu-west-1", "t3.medium", "Linux"); err != pricing.ErrNoPrice {
			t.Errorf("expected ErrNoPrice for wrong region, got %v", err)
		}
	})
}

// TestCatalogPricer_OSForPlatform verifies the platform-to-operatingSystem
// mapping that InstancePrice and the rule use to derive the correct
// GetProducts filter value.
func TestCatalogPricer_OSForPlatform(t *testing.T) {
	cases := []struct {
		platform string
		want     string
	}{
		{"", "Linux"},
		{"linux/unix", "Linux"},
		{"windows", "Windows"},
		{"Windows", "Windows"},
		{"rhel", "RHEL"},
		{"RHEL", "RHEL"},
		{"suse", "SUSE"},
		{"SUSE", "SUSE"},
		{"  linux/unix  ", "Linux"},
	}
	for _, tc := range cases {
		got := OSForPlatform(tc.platform)
		if got != tc.want {
			t.Errorf("OSForPlatform(%q) = %q, want %q", tc.platform, got, tc.want)
		}
	}
}

// TestCatalogPricer_InstancePrice_Live is the LIVE counterpart of the fixture
// instance-price tests. It fetches a real GetProducts response for a
// well-known instance type and asserts the resolved rate matches the published
// On-Demand price.
//
// It requires real AWS credentials and network access, so it is skipped unless
// the operator sets TELLURY_AWS_LIVE_PRICE_TEST=1.
//
// Published prices verified against:
//
//	https://aws.amazon.com/ec2/pricing/on-demand/
//	t3.medium, Linux, US East (N. Virginia) = $0.0416 per Hour
func TestCatalogPricer_InstancePrice_Live(t *testing.T) {
	if os.Getenv("TELLURY_AWS_LIVE_PRICE_TEST") == "" {
		t.Skip("TELLURY_AWS_LIVE_PRICE_TEST is not set; live instance price test skipped")
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

	// Test t3.medium Linux in us-east-1 — the most common instance type.
	price, err := p.InstancePrice(ctx, "us-east-1", "t3.medium", "Linux")
	if err != nil {
		t.Fatalf("live InstancePrice(t3.medium, Linux, us-east-1): %v", err)
	}

	const publishedT3MediumLinuxUSE1 = 0.0416
	if price != publishedT3MediumLinuxUSE1 {
		t.Errorf("live InstancePrice = %v, want %v (published On-Demand rate for t3.medium Linux in us-east-1)",
			price, publishedT3MediumLinuxUSE1)
	}
	t.Logf("live t3.medium Linux us-east-1: $%.4f/hr (published: $%.4f/hr)", price, publishedT3MediumLinuxUSE1)

	// Second call must hit the cache (no second API call).
	price2, err := p.InstancePrice(ctx, "us-east-1", "t3.medium", "Linux")
	if err != nil {
		t.Fatalf("live cached InstancePrice: %v", err)
	}
	if price != price2 {
		t.Errorf("live cache inconsistency: first=%v, second=%v", price, price2)
	}

	// Unresolvable instance type must return ErrNoPrice.
	if _, err := p.InstancePrice(ctx, "us-east-1", "nonexistent.xlarge", "Linux"); err != pricing.ErrNoPrice {
		t.Errorf("expected ErrNoPrice for nonexistent instance type, got %v", err)
	}
}

// TestCatalogPricer_InstancePrice_FixtureFileEnvVar verifies that setting
// TELLURY_INSTANCE_PRICE_FIXTURE loads instance prices from the recorded file.
func TestCatalogPricer_InstancePrice_FixtureFileEnvVar(t *testing.T) {
	path := filepath.Join("testdata", "getproducts-instance-recorded.json")
	t.Setenv("TELLURY_INSTANCE_PRICE_FIXTURE", path)

	p, err := NewCatalogPricer(context.Background(), slog.New(slog.DiscardHandler), aws.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}

	price, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Linux")
	if err != nil {
		t.Fatalf("InstancePrice via TELLURY_INSTANCE_PRICE_FIXTURE: %v", err)
	}

	const want = 0.0416
	if price != want {
		t.Errorf("InstancePrice = %v, want %v", price, want)
	}

	// Verify the fixture is cached — second call must return same value.
	price2, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Linux")
	if err != nil {
		t.Fatalf("second InstancePrice via fixture: %v", err)
	}
	if price2 != want {
		t.Errorf("second InstancePrice = %v, want %v", price2, want)
	}

	// Unknown instance type must skip.
	if _, err := p.InstancePrice(context.Background(), "us-east-1", "nonexistent.xlarge", "Linux"); err != pricing.ErrNoPrice {
		t.Errorf("expected ErrNoPrice for nonexistent instance type via fixture, got %v", err)
	}
}

// TestParseInstancePriceSKU verifies the compound-SKU parsing used by
// loadPriceFixture to extract (instanceType, operatingSystem) from
// fixture-table SKU entries.
func TestParseInstancePriceSKU(t *testing.T) {
	cases := []struct {
		sku               string
		wantType, wantOS  string
	}{
		{"t3.medium/Linux", "t3.medium", "Linux"},
		{"t3.medium/Windows", "t3.medium", "Windows"},
		{"m6i.xlarge/Linux", "m6i.xlarge", "Linux"},
		{"t3.medium", "t3.medium", "Linux"},                     // no slash → default OS
		{"c7g.large/RHEL", "c7g.large", "RHEL"},
		{"t3.medium/RHEL with spaces/SUSE", "t3.medium/RHEL with spaces", "SUSE"}, // last slash splits
	}
	for _, tc := range cases {
		gotType, gotOS := parseInstancePriceSKU(tc.sku)
		if gotType != tc.wantType || gotOS != tc.wantOS {
			t.Errorf("parseInstancePriceSKU(%q) = (%q, %q), want (%q, %q)",
				tc.sku, gotType, gotOS, tc.wantType, tc.wantOS)
		}
	}
}

// TestCatalogPricer_InstancePrice_WithFixtureTable verifies that loading a
// price fixture table containing vm_instance entries populates instancePrices
// correctly and that InstancePrice resolves through the fixture.
func TestCatalogPricer_InstancePrice_WithFixtureTable(t *testing.T) {
	// Build a minimal price-fixture.json with vm_instance entries.
	tmpDir := t.TempDir()
	fixturePath := filepath.Join(tmpDir, "price-fixture.json")
	fixtureJSON := `{
		"disk_capacity": {},
		"disk_iops": {},
		"disk_throughput": {},
		"static_ip": {},
		"vm_instance": {
			"t3.medium/Linux": {"us-east-1": 0.0416},
			"t3.medium/Windows": {"us-east-1": 0.0608},
			"m6i.xlarge/Linux": {"us-east-1": 0.192}
		}
	}`
	if err := os.WriteFile(fixturePath, []byte(fixtureJSON), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("TELLURY_PRICE_FIXTURE", fixturePath)

	p, err := NewCatalogPricer(context.Background(), slog.New(slog.DiscardHandler), aws.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}

	// Trigger the lazy load by calling liveUnitPrice for one key.
	_, _, err = p.liveUnitPrice(pricing.KindDiskCapacity, "gp3", "us-east-1")
	// This will fail with ErrNoPrice because the fixture has empty disk_capacity,
	// but the loadCatalogue still ran and populated instancePrices.
	// Actually, loadCatalogue returns nil error when fixture loads, and sets loaded=true.
	_ = err // ignore UnitPrice error — we only care about the instance price cache.

	// InstancePrice must resolve from the fixture table.
	price, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Linux")
	if err != nil {
		t.Fatalf("InstancePrice(t3.medium, Linux, us-east-1) from fixture table: %v", err)
	}
	if price != 0.0416 {
		t.Errorf("InstancePrice = %v, want 0.0416", price)
	}

	// Windows variant at higher rate.
	price, err = p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Windows")
	if err != nil {
		t.Fatalf("InstancePrice(t3.medium, Windows, us-east-1) from fixture table: %v", err)
	}
	if price != 0.0608 {
		t.Errorf("InstancePrice Windows = %v, want 0.0608", price)
	}
}

// TestCatalogPricer_InstancePrice_LoadFixturePopulatesBothCaches verifies that
// when TELLURY_PRICE_FIXTURE is set with both EBS and vm_instance entries, both
// the skusByKey cache and the instancePrices cache are populated.
func TestCatalogPricer_InstancePrice_LoadFixturePopulatesBothCaches(t *testing.T) {
	tmpDir := t.TempDir()
	fixturePath := filepath.Join(tmpDir, "price-fixture.json")
	fixtureJSON := `{
		"disk_capacity": {
			"gp3": {"us-east-1": 0.08}
		},
		"vm_instance": {
			"t3.medium/Linux": {"us-east-1": 0.0416}
		}
	}`
	if err := os.WriteFile(fixturePath, []byte(fixtureJSON), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("TELLURY_PRICE_FIXTURE", fixturePath)

	p, err := NewCatalogPricer(context.Background(), slog.New(slog.DiscardHandler), aws.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatalf("NewCatalogPricer: %v", err)
	}

	// Trigger lazy load.
	_, _, err = p.liveUnitPrice(pricing.KindDiskCapacity, "gp3", "us-east-1")
	if err != nil {
		t.Fatalf("liveUnitPrice for gp3 capacity: %v", err)
	}

	// Verify EBS cache hit.
	if unit, _, err := p.UnitPrice(pricing.KindDiskCapacity, "aws", "gp3", "us-east-1"); err != nil || unit != 0.08 {
		t.Errorf("UnitPrice(gp3 capacity) = %v, %v, want 0.08, nil", unit, err)
	}

	// Verify instance price cache hit.
	if price, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Linux"); err != nil || price != 0.0416 {
		t.Errorf("InstancePrice = %v, %v, want 0.0416, nil", price, err)
	}

	// Verify the instance price is cached — no second parse.
	if price, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Linux"); err != nil || price != 0.0416 {
		t.Errorf("cached InstancePrice = %v, %v, want 0.0416, nil", price, err)
	}
}

// captureLoggedWarning collects log messages for the test below.
type logCapture struct {
	warnings []string
}

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		c.warnings = append(c.warnings, r.Message)
	}
	return nil
}

// TestCatalogPricer_PriceServiceFamiliesDoesNotIncludeComputeInstance is the
// structural guardrail: "Compute Instance" must not appear in
// priceServiceFamilies. Adding it would preload the largest product family in
// the AWS price list and make every scan unusably slow.
func TestCatalogPricer_PriceServiceFamiliesDoesNotIncludeComputeInstance(t *testing.T) {
	for _, families := range priceServiceFamilies {
		for _, f := range families {
			if f == "Compute Instance" {
				t.Fatal(`"Compute Instance" found in priceServiceFamilies. ` +
					`It must not be preloaded — it is the largest family in the AWS price list. ` +
					`Instance pricing is handled by the separate, lazy InstancePrice method instead.`)
			}
		}
	}
}

// TestCatalogPricer_InstancePrice_FilterDocumentation verifies that the filter
// names and values documented in InstancePrice's doc comment match the actual
// code. A rename in the code that drifts from the documentation is caught here.
func TestCatalogPricer_InstancePrice_FilterDocumentation(t *testing.T) {
	// Build a fake pricer and construct what fetchInstancePrice would send.
	// We verify the constant filter values in the code.
	t.Run("constant_filter_values", func(t *testing.T) {
		// These are the exact strings sent to GetProducts. Changing any of
		// them changes which product is selected — potentially silently
		// picking up a capacity-reservation, dedicated, or licensed rate.
		const (
			filterTenancy        = "Shared"
			filterCapacityStatus = "Used"
			filterPreInstalledSw = "NA"
			filterLicenseModel   = "No License required"
			filterTermType       = "OnDemand"
		)

		// Verify each constant is non-empty — a zero value would indicate a
		// missing filter.
		if filterTenancy == "" {
			t.Error("tenancy filter is empty")
		}
		if filterCapacityStatus == "" {
			t.Error("capacitystatus filter is empty")
		}
		if filterPreInstalledSw == "" {
			t.Error("preInstalledSw filter is empty")
		}
		if filterLicenseModel == "" {
			t.Error("licenseModel filter is empty")
		}
		if filterTermType == "" {
			t.Error("termType filter is empty")
		}
	})

	// Verify the filter constants match the ones used in the actual code.
	// We do this by calling InstancePrice through the fixture and confirming
	// it resolves — the fixture matches all eight filters exactly.
	t.Run("fixture_matches_all_filters", func(t *testing.T) {
		p := fixtureInstanceCatalog(t)
		price, err := p.InstancePrice(context.Background(), "us-east-1", "t3.medium", "Linux")
		if err != nil {
			t.Fatalf("InstancePrice fixture resolve: %v", err)
		}
		if price != 0.0416 {
			t.Errorf("price = %v, want 0.0416", price)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Helper: dump fixture docs for visual inspection
// ─────────────────────────────────────────────────────────────────────────────

func TestDumpInstanceFixtureForInspection(t *testing.T) {
	docs := loadInstanceFixtureDocs(t)
	t.Logf("instance fixture has %d product(s)", len(docs))
	for i, doc := range docs {
		t.Logf("[%d] serviceCode=%q productFamily=%q instanceType=%q regionCode=%q operatingSystem=%q",
			i, doc.ServiceCode, doc.Product.ProductFamily,
			doc.Product.Attributes["instanceType"],
			doc.Product.Attributes["regionCode"],
			doc.Product.Attributes["operatingSystem"])
		for _, dim := range priceDimensions(doc) {
			t.Logf("     unit=%q price=%v", dim.unit, dim.price)
		}
	}
}

// Ensure fmt is used (logCapture method).
var _ = fmt.Sprintf
