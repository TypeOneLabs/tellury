// AWS Resource Explorer discovery client.
//
// Resource Explorer is a discovery index, not an inventory. Its Search
// response carries Arn, LastReportedAt, OwningAccountId, Properties, Region,
// ResourceType and Service — and no configuration. It answers "which regions
// are worth asking" so the caller fetches attributes through the service APIs
// (DescribeVolumes, DescribeAddresses, etc.).
//
// The index is created automatically on first Search if the caller has
// iam:CreateServiceLinkedRole. When no index exists and the caller cannot
// create one, Discover returns ErrNoIndex so the caller can fall back to the
// DescribeRegions sweep.
package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2/types"
)

// resourceExplorerAPI is the subset of the Resource Explorer API this client
// calls. A fake implementing this interface stands in for the live SDK client
// in tests so they never reach the network.
type resourceExplorerAPI interface {
	Search(ctx context.Context, params *resourceexplorer2.SearchInput, optFns ...func(*resourceexplorer2.Options)) (*resourceexplorer2.SearchOutput, error)
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

// ErrNoIndex is returned when Resource Explorer has no aggregator index and
// the caller lacks permission to create one. The caller can distinguish this
// from a transient API error with errors.Is, and can fall back to a
// DescribeRegions sweep.
//
// With iam:CreateServiceLinkedRole an index is created automatically on first
// Search, so "no index" is not always fatal.
var ErrNoIndex = errors.New("resource explorer: no aggregator index exists")

// Discover reports which regions hold resources of the given types in the
// account the credentials belong to.
//
// resourceTypes are AWS CloudFormation-style type strings, e.g.
// "AWS::EC2::Volume" and "AWS::EC2::EIP". The strings come from the
// ResourceType field of real Search responses, never invented.
//
// The result maps each region code (e.g. "us-east-1") to the list of
// resource types discovered there, de-duplicated.
func (d *Discoverer) Discover(ctx context.Context, resourceTypes []string) (map[string][]string, error) {
	if len(resourceTypes) == 0 {
		return map[string][]string{}, nil
	}

	query := buildSearchQuery(resourceTypes)
	result := make(map[string][]string)
	var nextToken *string

	for {
		out, err := d.client.Search(ctx, &resourceexplorer2.SearchInput{
			QueryString: aws.String(query),
			NextToken:   nextToken,
		})
		if err != nil {
			if isNoIndexError(err) {
				return nil, fmt.Errorf("%w: %w", ErrNoIndex, err)
			}
			return nil, fmt.Errorf("resource explorer search: %w", err)
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
			break
		}
		nextToken = out.NextToken
	}

	return result, nil
}

// buildSearchQuery constructs a Resource Explorer query string that matches
// any of the given resource types. The query uses the "resourcetype:" filter
// joined with OR. Example input ["AWS::EC2::Volume", "AWS::EC2::EIP"] produces
// "resourcetype:AWS::EC2::Volume OR resourcetype:AWS::EC2::EIP".
func buildSearchQuery(types []string) string {
	parts := make([]string, 0, len(types))
	for _, t := range types {
		parts = append(parts, "resourcetype:"+t)
	}
	return strings.Join(parts, " OR ")
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
