// AWS Resource Explorer discovery client.
//
// Resource Explorer is a discovery index, not an inventory. Its Search
// response carries Arn, LastReportedAt, OwningAccountId, Properties, Region,
// ResourceType and Service — and no configuration. It answers "which regions
// are worth asking" so the caller fetches attributes through the service APIs
// (DescribeVolumes, DescribeAddresses, etc.).
//
// TWO THINGS ABOUT THE QUERY LANGUAGE, both measured against the live API on
// 2026-08-14 and both of which made this whole path return nothing:
//
//  1. There is no OR. Terms are ANDed, and a literal "OR" is just another term,
//     so "resourcetype:A OR resourcetype:B" matches NOTHING — not A, not B.
//     A single query per type, unioned by the caller, is the only way to ask
//     for several types.
//  2. Resource types are the values Search itself returns — "ec2:volume",
//     "ec2:image", "ec2:snapshot". Some CloudFormation-style aliases happen to
//     resolve ("AWS::EC2::Volume" -> 3 hits) and others silently do not
//     ("AWS::EC2::Image" -> 0 while "ec2:image" -> 1), so the alias form cannot
//     be trusted for any type.
//
// A REGION IS ONLY VISIBLE IF IT HAS AN INDEX. A LOCAL index answers for its
// own region alone, and an aggregator index only replicates the local indexes
// that exist — AWS: "If you do not create a user-owned index in a Region,
// resources from that Region will not appear in cross-region search results
// from other Regions." A region with no index answers "0 results, Complete:
// true": a confident empty, indistinguishable from a region that is genuinely
// idle.
//
// Hence two paths. With an aggregator index, one query per type covers every
// indexed region. Without one, DiscoverAcrossRegions asks each region directly,
// which needs no setup but has the first-run gap described on that method.
package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2/types"
)

// resourceExplorerAPI is the subset of the Resource Explorer API this client
// calls. A fake implementing this interface stands in for the live SDK client
// in tests so they never reach the network.
type resourceExplorerAPI interface {
	Search(ctx context.Context, params *resourceexplorer2.SearchInput, optFns ...func(*resourceexplorer2.Options)) (*resourceexplorer2.SearchOutput, error)
	ListIndexes(ctx context.Context, params *resourceexplorer2.ListIndexesInput, optFns ...func(*resourceexplorer2.Options)) (*resourceexplorer2.ListIndexesOutput, error)
}

// Discoverer queries AWS Resource Explorer to find which regions hold
// resources of given types.
type Discoverer struct {
	client resourceExplorerAPI
}

// NewDiscoverer builds a Discoverer from an AWS config. The client is
// region-agnostic — Resource Explorer Search queries the aggregator index,
// not a single region — so no region pin is needed.
func NewDiscoverer(cfg aws.Config) *Discoverer {
	return &Discoverer{client: resourceexplorer2.NewFromConfig(cfg)}
}

// newDiscovererWithClient builds a Discoverer with a specific API for tests.
func newDiscovererWithClient(c resourceExplorerAPI) *Discoverer {
	return &Discoverer{client: c}
}

// ErrNoIndex is returned when Resource Explorer is not set up at all and the
// caller lacks permission to create an index. Distinguishable with errors.Is so
// the caller can report the remedy.
var ErrNoIndex = errors.New("resource explorer: no aggregator index exists")

// IndexedRegions returns the regions that currently have a Resource Explorer
// index of any type — exactly the regions a Search can answer for.
//
// The caller compares this against the regions it is about to sweep. A region
// outside this set answers "0 results, Complete: true" and has an index created
// as a side effect of being asked, so it reports nothing on THIS scan and
// becomes answerable on a later one. Naming those regions is what keeps that
// gap from being silent.
func (d *Discoverer) IndexedRegions(ctx context.Context) (map[string]bool, error) {
	out, err := d.client.ListIndexes(ctx, &resourceexplorer2.ListIndexesInput{})
	if err != nil {
		if isNoIndexError(err) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("resource explorer list indexes: %w", err)
	}
	regions := make(map[string]bool, len(out.Indexes))
	for _, idx := range out.Indexes {
		if r := aws.ToString(idx.Region); r != "" {
			regions[r] = true
		}
	}
	return regions, nil
}

// HasAggregatorIndex reports whether the account has an aggregator index, the
// only index type whose Search covers every region.
func (d *Discoverer) HasAggregatorIndex(ctx context.Context) (bool, error) {
	out, err := d.client.ListIndexes(ctx, &resourceexplorer2.ListIndexesInput{
		Type: types.IndexTypeAggregator,
	})
	if err != nil {
		if isNoIndexError(err) {
			return false, fmt.Errorf("%w: %w", ErrNoIndex, err)
		}
		return false, fmt.Errorf("resource explorer list indexes: %w", err)
	}
	return len(out.Indexes) > 0, nil
}

// maxConcurrentSearches bounds simultaneous Resource Explorer Search calls
// during a per-region sweep. A sweep issues regions x types queries — 17 x 5
// on a default account — and Resource Explorer throttles a burst of them.
const maxConcurrentSearches = 8

// DiscoverAcrossRegions runs the per-region sweep: it asks EVERY given region
// which of the resource types it holds, and returns the union.
//
// This is the path taken when the account has no aggregator index, which is
// most accounts. It needs no setup from the operator, at two costs that the
// caller must not hide:
//
//   - A region with no index answers "0 results, Complete: true" — a confident
//     empty. Searching also CREATES a local index in that region, so the region
//     becomes answerable on a later scan but reports nothing on this one.
//     Resources there are missed on the first run and found once the index
//     populates (minutes for tagged resources, up to ~2 hours otherwise).
//   - That index creation is a write, performed by an otherwise read-only tool.
//
// Both are documented in docs/aws-setup.md. Neither is silent: the caller logs
// the region count it resolved.
func (d *Discoverer) DiscoverAcrossRegions(ctx context.Context, resourceTypes, regions []string) (map[string][]string, error) {
	if len(resourceTypes) == 0 || len(regions) == 0 {
		return map[string][]string{}, nil
	}

	var (
		mu     sync.Mutex
		result = make(map[string][]string)
		errs   []error
		wg     sync.WaitGroup
		sem    = make(chan struct{}, maxConcurrentSearches)
	)

	for _, region := range regions {
		for _, rt := range resourceTypes {
			wg.Add(1)
			go func(region, rt string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				found := make(map[string][]string)
				if err := d.searchType(ctx, rt, found, withRegion(region)); err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("%s: %w", region, err))
					mu.Unlock()
					return
				}
				mu.Lock()
				for r, types := range found {
					for _, t := range types {
						result[r] = appendIfMissing(result[r], t)
					}
				}
				mu.Unlock()
			}(region, rt)
		}
	}
	wg.Wait()

	// One region failing must not silently shrink the answer.
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return result, nil
}

// withRegion pins one Search call to a region's endpoint. A LOCAL index only
// answers for its own region, so the sweep must ask each region directly.
func withRegion(region string) func(*resourceexplorer2.Options) {
	return func(o *resourceexplorer2.Options) { o.Region = region }
}

// Discover reports which regions hold resources of the given types in the
// account the credentials belong to.
//
// resourceTypes are the strings Search itself returns, e.g. "ec2:volume" and
// "ec2:elastic-ip" — never the CloudFormation aliases, which resolve for some
// types and silently match nothing for others.
//
// This is the aggregator path: one query per type, no region pin, covering
// every region that has an index. Use DiscoverAcrossRegions when the account
// has no aggregator index.
//
// The result maps each region code (e.g. "us-east-1") to the list of
// resource types discovered there, de-duplicated.
func (d *Discoverer) Discover(ctx context.Context, resourceTypes []string) (map[string][]string, error) {
	if len(resourceTypes) == 0 {
		return map[string][]string{}, nil
	}

	// ONE QUERY PER TYPE. Resource Explorer has no OR, so a combined query
	// matches nothing at all — see the package comment.
	result := make(map[string][]string)
	for _, rt := range resourceTypes {
		if err := d.searchType(ctx, rt, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// searchType pages one resourcetype query into result.
func (d *Discoverer) searchType(ctx context.Context, resourceType string, result map[string][]string, optFns ...func(*resourceexplorer2.Options)) error {
	query := buildSearchQuery(resourceType)
	var nextToken *string

	for {
		out, err := d.client.Search(ctx, &resourceexplorer2.SearchInput{
			QueryString: aws.String(query),
			NextToken:   nextToken,
		}, optFns...)
		if err != nil {
			if isNoIndexError(err) {
				return fmt.Errorf("%w: %w", ErrNoIndex, err)
			}
			return fmt.Errorf("resource explorer search %q: %w", resourceType, err)
		}

		for _, r := range out.Resources {
			region := aws.ToString(r.Region)
			rt := aws.ToString(r.ResourceType)
			if region == "" || rt == "" {
				continue
			}
			result[region] = appendIfMissing(result[region], rt)
		}

		if out.NextToken == nil || *out.NextToken == "" {
			return nil
		}
		nextToken = out.NextToken
	}
}

// buildSearchQuery constructs the Resource Explorer query for ONE resource
// type, e.g. "ec2:volume" produces "resourcetype:ec2:volume".
//
// One type per query is not a simplification: joining several with " OR "
// returns zero results, because Resource Explorer ANDs terms and treats "OR" as
// a literal one. That is how this path came to look like it worked while
// narrowing nothing.
func buildSearchQuery(resourceType string) string {
	return "resourcetype:" + resourceType
}

// appendIfMissing appends v to s if v is not already present, and returns the
// (possibly extended) slice.
func appendIfMissing(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// isNoIndexError detects the error Resource Explorer returns when no
// aggregator index exists. The API returns ResourceNotFoundException with a
// message about the index. We check the typed error first, then fall back to
// the error string so a future SDK rename of the type cannot silently turn
// "no index" into a fatal error the caller cannot recover from.
func isNoIndexError(err error) bool {
	if err == nil {
		return false
	}
	var rnf *types.ResourceNotFoundException
	if errors.As(err, &rnf) {
		return true
	}
	msg := err.Error()
	if strings.Contains(msg, "ResourceNotFoundException") {
		return true
	}
	return false
}
