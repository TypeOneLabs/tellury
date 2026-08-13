package azure

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resourcegraph/armresourcegraph"
)

// resourceGraphAPI is the subset of the Azure Resource Graph API this provider
// calls. It is what lets a fixture (offline replay) stand in for the live SDK
// client without touching the network or resolving credentials.
type resourceGraphAPI interface {
	Resources(ctx context.Context, query armresourcegraph.QueryRequest, options *armresourcegraph.ClientResourcesOptions) (armresourcegraph.ClientResourcesResponse, error)
}

// resourceGraphBaseQuery is the single Azure Resource Graph query used for
// every Azure inventory scan. Resource Graph returns rule-ready fields for
// both modelled resource types in the same row; no follow-up hydration call is
// needed for Microsoft.Compute/disks or Microsoft.Network/publicIPAddresses.
//
// The query is deliberately a projection, not `project *`: it asks for the
// exact columns the normalizers read, so a missing column fails a test instead
// of silently producing an always-skipped rule.
const resourceGraphBaseQuery = `resources
| where type in~ ('microsoft.compute/disks', 'microsoft.network/publicipaddresses')
| project id, name, type, location, resourceGroup, subscriptionId, sku, managedBy, tags, properties`

// resourceGraphQuery builds the per-subscription ARG request. The query is
// scoped to exactly one subscription so a tenant or management-group scan can
// report each subscription's outcome independently — the same fan-out the AWS
// provider performs per account, and the opposite of handing a management
// group directly to ARG and silently omitting whatever the identity cannot
// read below it.
//
// A resource-group scope adds the KQL filter
//
//	| where resourceGroup =~ '<resource-group-name>'
//
// KQL single quotes are escaped by doubling, matching Azure's string literal
// escaping.
func resourceGraphQuery(subscriptionID, resourceGroup string, skipToken *string) armresourcegraph.QueryRequest {
	query := resourceGraphBaseQuery
	if resourceGroup != "" {
		query += "\n| where resourceGroup =~ '" + strings.ReplaceAll(resourceGroup, "'", "''") + "'"
	}

	return armresourcegraph.QueryRequest{
		Query:         to.Ptr(query),
		Subscriptions: []*string{to.Ptr(subscriptionID)},
		Options: &armresourcegraph.QueryRequestOptions{
			ResultFormat: to.Ptr(armresourcegraph.ResultFormatObjectArray),
			SkipToken:    skipToken,
		},
	}
}

// querySubscription fetches every page of the per-subscription ARG query and
// returns the rows as plain maps. Pagination uses ARG's skip token, passed
// back on the next request exactly as the API documents.
func (p *Provider) querySubscription(ctx context.Context, subscriptionID, resourceGroup string) ([]map[string]any, error) {
	if p.argClient == nil {
		return nil, fmt.Errorf("azure: resource graph client is unavailable")
	}

	var all []map[string]any
	var skipToken *string
	seenTokens := map[string]bool{}

	for {
		req := resourceGraphQuery(subscriptionID, resourceGroup, skipToken)
		resp, err := p.argClient.Resources(ctx, req, nil)
		if err != nil {
			return nil, err
		}

		rows, err := rowsFromData(resp.Data)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)

		if resp.SkipToken == nil || *resp.SkipToken == "" {
			return all, nil
		}
		token := *resp.SkipToken
		if seenTokens[token] {
			return nil, fmt.Errorf("azure: resource graph pagination did not advance")
		}
		seenTokens[token] = true
		skipToken = resp.SkipToken
	}
}

// rowsFromData converts ARG's object-array Data payload into normalizer-ready
// rows. The SDK returns `any`; with ResultFormatObjectArray that value is a
// JSON array of objects, which surfaces as either []any of map[string]any or
// []map[string]any depending on how the value was produced (live SDK response
// vs fixture). Both shapes are accepted; any other shape is an explicit error
// rather than a silent empty inventory.
func rowsFromData(data any) ([]map[string]any, error) {
	switch t := data.(type) {
	case []map[string]any:
		return t, nil
	case []any:
		rows := make([]map[string]any, 0, len(t))
		for _, raw := range t {
			row, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("azure: resource graph returned non-object row %T", raw)
			}
			rows = append(rows, row)
		}
		return rows, nil
	case nil:
		return []map[string]any{}, nil
	default:
		return nil, fmt.Errorf("azure: resource graph Data is %T, want an object array", data)
	}
}
