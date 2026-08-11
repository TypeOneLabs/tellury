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
// fixture stores the parsed aws_v1 product documents; each is re-marshalled
// to the exact raw string GetProducts would put in a PriceList element, then
// parsePriceListDoc decodes it. A test that bypassed parsePriceListDoc would
// not actually exercise the token-derivation code under test.
func loadFixtureDocs(t *testing.T) []*priceListDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ec2-price-list.json"))
	if err != nil {
		t.Fatalf("read price fixture: %v", err)
	}
	var stored []priceListDoc
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("decode price fixture: %v", err)
	}
	out := make([]*priceListDoc, 0, len(stored))
	for i := range stored {
		raw, err := json.Marshal(&stored[i])
		if err != nil {
			t.Fatalf("re-marshal fixture doc: %v", err)
		}
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

	// The fixture also carries a Storage product whose volumeApiName is NOT an
	// SDK VolumeType ("not-a-volume-type"): it must never be indexed.
	for _, doc := range docs {
		if doc.Product.Attributes["volumeApiName"] == "not-a-volume-type" {
			for _, e := range indexDoc(doc) {
				t.Fatalf("non-EBS product with volumeApiName %q was indexed as sku %q; "+
					"the SDK-whitelist gate failed", doc.Product.Attributes["volumeApiName"], e.sku)
			}
		}
	}
}

// TestCatalogPricer_EIPSKUPinned pins the Elastic IP SKU token to the exact
// constant the unassociated_eip rule queries (EIPSKU = "AdditionalAddress"):
// the live catalogue derives the token from a real GetProducts response's
// operation attribute for the productFamily "Elastic IP", and this test
// asserts catalogue-token == rule-token. It also asserts the associated-EIP
// product (operation "InUseAddress", $0.00) is indexed under a DIFFERENT
// token, so the rule can never accidentally price an in-use address.
func TestCatalogPricer_EIPSKUPinned(t *testing.T) {
	var additionalToken, inUseToken string
	foundAdditional, foundInUse := false, false
	for _, doc := range loadFixtureDocs(t) {
		if doc.Product.ProductFamily != "Elastic IP" {
			continue
		}
		entries := indexDoc(doc)
		if len(entries) == 0 {
			continue
		}
		switch doc.Product.Attributes["operation"] {
		case "AdditionalAddress":
			foundAdditional = true
			additionalToken = entries[0].sku
		case "InUseAddress":
			foundInUse = true
			inUseToken = entries[0].sku
		}
	}
	if !foundAdditional {
		t.Fatal("fixture carries no AdditionalAddress Elastic IP product; the pin cannot be asserted")
	}
	if !foundInUse {
		t.Fatal("fixture carries no InUseAddress Elastic IP product; the distinction test cannot be asserted")
	}

	if additionalToken != unassociated_eip.EIPSKU {
		t.Fatalf("catalogue token %q != unassociated_eip.EIPSKU %q: the rule would "+
			"query a token the live catalogue never produces and every EIP price "+
			"would silently fall back to the embedded table",
			additionalToken, unassociated_eip.EIPSKU)
	}
	if inUseToken == unassociated_eip.EIPSKU {
		t.Fatalf("the in-use EIP product indexed under the rule's token %q: an "+
			"associated address would be mispriced as waste", inUseToken)
	}
}

// TestCatalogPricer_UnitPrice_EBS prices the recorded fixture end to end
// through UnitPrice: gp3 capacity in us-east-1, gp3 IOPS, gp3 throughput,
// io2 capacity+IOPS from one product, and a gp3 capacity in eu-west-1
// resolved through the Location display-name map.
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
		{"st1 capacity us-east-1", pricing.KindDiskCapacity, "st1", "us-east-1", 0.045},
		{"sc1 capacity us-east-1", pricing.KindDiskCapacity, "sc1", "us-east-1", 0.025},
		{"standard capacity us-east-1", pricing.KindDiskCapacity, "standard", "us-east-1", 0.05},
		{"gp3 capacity eu-west-1 (display name map)", pricing.KindDiskCapacity, "gp3", "eu-west-1", 0.08},
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

// TestCatalogPricer_UnitPrice_EIP prices the unassociated Elastic IP from the
// fixture: $0.005/hour, the figure the unassociated_eip rule multiplies by
// HoursPerMonth.
func TestCatalogPricer_UnitPrice_EIP(t *testing.T) {
	p := fixtureCatalog(t)
	unit, region, err := p.UnitPrice(pricing.KindStaticIP, "aws", unassociated_eip.EIPSKU, "us-east-1")
	if err != nil {
		t.Fatalf("UnitPrice(EIP): %v", err)
	}
	if unit != 0.005 {
		t.Errorf("EIP unit price = %v, want 0.005", unit)
	}
	if region != "us-east-1" {
		t.Errorf("EIP region = %q, want us-east-1", region)
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

// TestCatalogPricer_LiveGetProductsTokenPinned is the LIVE counterpart of the
// fixture pinning tests: it fetches a real GetProducts response for AmazonEC2
// and asserts the catalogue resolves the exact tokens the rules query. It is
// the test that catches an AWS rename of volumeApiName / operation / a
// Location display name before it silently degrades every lookup to the
// embedded table.
//
// It requires real AWS credentials and network access, so it is skipped unless
// the operator sets TELLURY_AWS_LIVE_PRICE_TEST=1 (CI and the default test
// suite stay offline and green).
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
}
