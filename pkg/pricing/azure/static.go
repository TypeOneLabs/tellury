package azure

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// retailFixtureEntry is one recorded Retail Prices API response. The fixture
// stores the exact URL that produced the response and the raw JSON body, so a
// test can inspect the live request and a human can re-run it against the
// public API. The body is never hand-authored from what the code expects.
type retailFixtureEntry struct {
	Name string          `json:"name"`
	URL  string          `json:"url"`
	Body json.RawMessage `json:"body"`
}

// loadFixtureEntries reads a recorded Retail Prices API fixture file. The file
// is a JSON array of retailFixtureEntry values.
func loadFixtureEntries(path string) ([]retailFixtureEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []retailFixtureEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// StaticPricer reads recorded Retail Prices API responses from a JSON file on
// disk. It is used by tests and offline scans via TELLURY_PRICE_FIXTURE.
//
// There is no embedded fallback table. A price that cannot be resolved from
// this file returns ErrNoPrice, and the rule skips rather than guessing.
type StaticPricer struct {
	entries []retailFixtureEntry

	mu    sync.Mutex
	cache map[skuKey]resolvedSKU
}

var _ pricing.Pricer = (*StaticPricer)(nil)

// NewStaticPricerFromFile loads recorded Retail Prices API responses from the
// given JSON file. The file format is the recorded-response fixture format
// (a JSON array of {name, url, body} entries), NOT a hand-authored
// kind->SKU->region table.
func NewStaticPricerFromFile(path string) (*StaticPricer, error) {
	entries, err := loadFixtureEntries(path)
	if err != nil {
		return nil, fmt.Errorf("pricing: read Azure price fixture %q: %w", path, err)
	}
	return &StaticPricer{
		entries: entries,
		cache:   map[skuKey]resolvedSKU{},
	}, nil
}

// UnitPrice resolves kind -> SKU -> region from the recorded responses. It
// applies exactly the same filter table and unit normalization as the live
// CatalogPricer, so a fixture answer and a live answer can never disagree
// about which row is the price.
func (p *StaticPricer) UnitPrice(kind pricing.Kind, provider, sku, region string) (float64, string, error) {
	region = LocationRegion(region)
	key := skuKey{kind: kind, sku: sku, region: region}

	p.mu.Lock()
	if res, ok := p.cache[key]; ok {
		p.mu.Unlock()
		return res.unitPrice, res.region, nil
	}
	p.mu.Unlock()

	filters, err := lookupFilters(kind, sku, region)
	if err != nil {
		return 0, "", pricing.ErrNoPrice
	}

	var rows []retailItem
	for _, entry := range p.entries {
		page, err := parseRetailPage(entry.Body)
		if err != nil {
			continue
		}
		rows = append(rows, page.Items...)
	}

	res, ok := selectPrice(rows, filters, kind)
	if !ok {
		return 0, "", pricing.ErrNoPrice
	}

	p.mu.Lock()
	p.cache[key] = res
	p.mu.Unlock()
	return res.unitPrice, res.region, nil
}

// MonthlyCost is a convenience wrapper: price * quantity, using UnitPrice.
func (p *StaticPricer) MonthlyCost(it pricing.Item) (float64, error) {
	unit, _, err := p.UnitPrice(it.Kind, it.Provider, it.SKU, it.Region)
	if err != nil {
		return 0, err
	}
	return unit * it.Quantity, nil
}
