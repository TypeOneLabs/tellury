package aws

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	pricingaws "github.com/TypeOneLabs/tellury/pkg/pricing/aws"
)

// DefaultRoleName is the conventional cross-account role name AWS
// Organizations sets up when an account is created via the console.
// OrganizationAccountAccessRole is the documented convention; operators who
// use a different name configure it with --aws-role-name.
const DefaultRoleName = "OrganizationAccountAccessRole"

// ec2API is the subset of the EC2 API this provider calls. It is what lets a
// fixture (offline replay) stand in for the live SDK client without touching
// the network: the provider drives DescribeRegions, the DescribeVolumes
// paginator and DescribeAddresses through this interface, never through the
// concrete client.
type ec2API interface {
	DescribeRegions(ctx context.Context, params *ec2.DescribeRegionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error)
	DescribeVolumes(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	DescribeAddresses(ctx context.Context, params *ec2.DescribeAddressesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
}

// stsAPI is the subset of STS this provider calls. It is an interface so
// tests can provide a fake.
type stsAPI interface {
	AssumeRole(ctx context.Context, params *sts.AssumeRoleInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

// AccountStatus records the outcome of attempting to scan one member account.
type AccountStatus struct {
	ID     string `json:"account_id"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"` // "scanned", "unreachable", "suspended", "no_resources"
	Reason string `json:"reason,omitempty"`
}

// Provider is the AWS implementation of cloud.Provider. It ingests a single
// AWS account (--aws-account) or traverses an organization/OU
// (--aws-organization / --aws-organizational-unit), building the full
// hierarchy from the Organizations API and assuming a cross-account role to
// scan each member account.
type Provider struct {
	log      *slog.Logger
	offline  bool
	fixture  *Fixture
	awsCfg   aws.Config
	pricer   pricing.Pricer
	explicit []string // --aws-regions values, canonicalised and de-duplicated

	// discoverer queries AWS Resource Explorer to narrow the region list
	// before hydration. It is nil on the offline path.
	discoverer *Discoverer

	// orgClient is the Organizations API client, pinned to us-east-1. The
	// Organizations API is a global service and must be called against
	// us-east-1 regardless of the scan's target region. It is nil on the
	// offline path.
	orgClient orgAPI

	// stsClient is the STS client used for cross-account role assumption. It
	// is nil on the offline path.
	stsClient stsAPI

	// roleName is the name of the IAM role to assume in each member account.
	// Defaults to OrganizationAccountAccessRole.
	roleName string

	// lastRegions carries the region coverage of the most recent Ingest, so the
	// CLI can report "N regions analyzed (source)" in the scan summary
	// without walking the graph or re-deriving the source from the flags.
	lastRegions      []string
	lastRegionSource string

	// accountStatuses records the outcome for every account in the tree
	// (scanned, unreachable, suspended, etc.). It is populated during Ingest
	// and reported in the scan summary and JSON output.
	accountStatuses []AccountStatus
}

var _ cloud.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithLogger sets the provider logger.
func WithLogger(l *slog.Logger) Option { return func(p *Provider) { p.log = l } }

// WithOffline builds a provider that never constructs an AWS SDK client. It is
// for scans whose data comes from local fixtures: the fixture stands in for
// every EC2 call, and pricing uses the TELLURY_PRICE_FIXTURE file if set, or a
// NoPricePricer (all resources skip) otherwise. This is what lets an offline
// AWS scan run on a host with no AWS credentials.
func WithOffline() Option { return func(p *Provider) { p.offline = true } }

// WithFixture supplies the offline data source (LoadFixture's result).
func WithFixture(f *Fixture) Option { return func(p *Provider) { p.fixture = f } }

// WithPricer overrides the cost model (used by tests).
func WithPricer(pr pricing.Pricer) Option { return func(p *Provider) { p.pricer = pr } }

// WithExplicitRegions sets an explicit --aws-regions list. The list is
// canonicalised (an availability-zone form like "us-east-1a" becomes its
// region "us-east-1") and de-duplicated before it is stored.
func WithExplicitRegions(regions []string) Option {
	return func(p *Provider) {
		p.explicit = canonicaliseRegions(regions)
	}
}

// WithRoleName sets the name of the IAM role to assume in member accounts
// during organization/OU scans. Defaults to OrganizationAccountAccessRole.
func WithRoleName(name string) Option {
	return func(p *Provider) { p.roleName = name }
}

// New builds an AWS provider. Unless WithOffline is set, it loads the default
// AWS credential chain — environment variables, shared config and credentials
// files, the profile named by AWS_PROFILE, and the region named by AWS_REGION
// — exactly as the AWS CLI would, via aws-sdk-go-v2/config.LoadDefaultConfig.
// There is deliberately no custom credential resolution and no credentials
// flag: tellury reads no key files, and the GCP side sets that precedent with
// Application Default Credentials.
//
// Pricing: the live Price List API (pricing:GetProducts, cached for the
// scan's duration) is the ONLY source. There is no embedded fallback table.
// A price that cannot be resolved from the live catalogue returns ErrNoPrice
// and the rule skips. When TELLURY_PRICE_FIXTURE is set, the catalogue loads
// from that file instead of calling the API — a test-only hook, never a
// user-facing flag.
//
// credentialResolveTimeout bounds the credential chain's own resolution. The
// IMDS fallback is the slow leg: off an EC2 instance it retries until its
// budget is spent, and an operator with no credentials should be told so in
// under a second rather than after five.
const credentialResolveTimeout = 2 * time.Second

func New(ctx context.Context, opts ...Option) (*Provider, error) {
	p := &Provider{log: slog.Default()}
	for _, opt := range opts {
		opt(p)
	}

	if p.roleName == "" {
		p.roleName = DefaultRoleName
	}

	// Load the AWS SDK config BEFORE building the pricer, so the
	// CatalogPricer gets the real credential chain. Offline path skips this
	// entirely: no SDK client, no credential resolution, no IMDS probe.
	if !p.offline {
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("aws: load default config: %w", err)
		}

		// Resolve credentials HERE rather than letting the first signed call do
		// it. LoadDefaultConfig defers resolution, so with no credentials
		// configured the chain falls through to the EC2 instance metadata
		// service at 169.254.169.254 — and on a machine that is not an EC2
		// instance that probe burns its full retry budget before failing with
		// a message about IMDS, which tells an operator nothing about the
		// credentials they actually need.
		//
		// It also made `go test ./...` reach the network: a test that merely
		// checked provider selection took 5 seconds and would be flaky in any
		// CI environment without IMDS.
		credCtx, cancel := context.WithTimeout(ctx, credentialResolveTimeout)
		defer cancel()
		if _, err := cfg.Credentials.Retrieve(credCtx); err != nil {
			return nil, fmt.Errorf("aws: no usable credentials: set AWS_ACCESS_KEY_ID and "+
				"AWS_SECRET_ACCESS_KEY, or AWS_PROFILE with a profile in ~/.aws/credentials, "+
				"or run on an instance with a role attached: %w", err)
		}
		p.awsCfg = cfg
		p.discoverer = NewDiscoverer(cfg)

		// Organizations client pinned to us-east-1. The Organizations API is
		// a global service; a client built for any other region will fail.
		p.orgClient = newOrgClient(cfg)

		// STS client for cross-account role assumption. STS is a global
		// service but the SDK resolves it through the configured region;
		// us-east-1 is the canonical home for global AWS services.
		p.stsClient = sts.NewFromConfig(cfg, func(o *sts.Options) {
			o.Region = "us-east-1"
		})
	}

	// Build the pricer: live CatalogPricer when online, file-backed or
	// NoPricePricer when offline. There is no embedded fallback table.
	if p.pricer == nil {
		if p.offline {
			p.pricer = offlinePricer(p.log)
		} else {
			cat, err := pricingaws.NewCatalogPricer(ctx, p.log, p.awsCfg)
			if err != nil {
				return nil, err
			}
			p.pricer = cat
		}
	}

	return p, nil
}

// offlinePricer builds a pricer for an offline scan. When TELLURY_PRICE_FIXTURE
// is set, it loads from that file; otherwise it returns a NoPricePricer —
// every resource requiring a price will skip rather than guess.
func offlinePricer(log *slog.Logger) pricing.Pricer {
	if path := os.Getenv("TELLURY_PRICE_FIXTURE"); path != "" {
		static, err := pricingaws.NewStaticPricerFromFile(path)
		if err != nil {
			log.Warn("aws: TELLURY_PRICE_FIXTURE set but could not load; resources requiring prices will skip",
				"path", path, "err", err)
			return pricing.NoPricePricer{}
		}
		log.Debug("aws: offline pricer loaded from TELLURY_PRICE_FIXTURE", "path", path)
		return static
	}
	log.Debug("aws: no price source available; resources requiring prices will skip")
	return pricing.NoPricePricer{}
}

// Name implements cloud.Provider.
func (p *Provider) Name() string { return ProviderName }

// Pricer implements cloud.Provider.
func (p *Provider) Pricer() pricing.Pricer { return p.pricer }

// Sizer implements cloud.Provider. AWS has no rightsizing catalog yet, so nil
// is returned (the CLI's rules Pass treats nil as "no catalog available").
func (p *Provider) Sizer() pricing.Sizer { return nil }

// Close implements cloud.Provider. There are no long-lived clients to release.
func (p *Provider) Close() error { return nil }

// Regions returns the region list and source of the most recent Ingest. The
// source is one of "explicit" (--aws-regions), "resource_explorer" (discovery
// narrowed the list from Resource Explorer), "describe_regions" (fallback:
// Resource Explorer was unavailable or returned no index, so every enabled
// region was swept) or "fixture" (offline replay). The CLI reports both in the
// scan summary so an operator always knows which regions a scan actually
// covered and why.
func (p *Provider) Regions() ([]string, string) {
	return append([]string(nil), p.lastRegions...), p.lastRegionSource
}

// AccountStatuses returns the status of every account in the organization tree
// — which accounts were scanned, which were unreachable and why, and which
// were suspended. It is nil for a single-account scan; the CLI reports it in
// the summary and JSON output when non-empty.
func (p *Provider) AccountStatuses() []AccountStatus {
	return append([]AccountStatus(nil), p.accountStatuses...)
}

// Ingest performs ingestion for the given scope. For a single-account scope
// (--aws-account), it ingests that account directly. For an organization or OU
// scope (--aws-organization / --aws-organizational-unit), it walks the
// Organizations API to build the hierarchy, assumes a cross-account role into
// each member account, and ingests each reachable account.
//
// The graph layout mirrors GCP's: each resource hangs off a per-account
// region container node ("accounts/<id>/regions/<region>"), and each region
// node hangs off the account container node ("accounts/<id>", KindAccount).
// Organization, root, and OU container nodes sit above the accounts with
// EdgeContains edges forming the hierarchy.
func (p *Provider) Ingest(ctx context.Context, sc cloud.Scope, assetTypeHints []string) (*graph.Graph, error) {
	if sc.AWS == nil {
		return nil, fmt.Errorf("aws: scope requires an AWS scope block")
	}

	// Single-account path: the original behaviour, unchanged.
	if sc.AWS.Account != "" {
		return p.ingestAccount(ctx, sc.AWS.Account, assetTypeHints)
	}

	// Organization / OU path.
	if !p.offline && (sc.AWS.Organization != "" || sc.AWS.OrganizationalUnit != "") {
		return p.ingestOrganization(ctx, *sc.AWS, assetTypeHints)
	}

	// Offline org/OU is not supported — fixtures are single-account only.
	if p.offline {
		return nil, fmt.Errorf("aws: offline ingest supports --aws-account only; organization/OU scans require live credentials")
	}

	return nil, fmt.Errorf("aws: scope requires an account, organization, or organizational-unit")
}

// ingestAccount ingests a single AWS account. It is the original Ingest
// behaviour, extracted so both the single-account and organization paths
// share the same per-account hydration logic.
func (p *Provider) ingestAccount(ctx context.Context, account string, assetTypeHints []string) (*graph.Graph, error) {
	return p.ingestAccountWithClient(ctx, account, p.ec2Client, p.discoverer, assetTypeHints)
}

// ingestAccountWithClient ingests a single account using the provided EC2
// client factory and optional discoverer. The discoverer may be nil for
// accounts where Resource Explorer is unavailable; in that case the account
// falls back to DescribeRegions.
func (p *Provider) ingestAccountWithClient(
	ctx context.Context,
	account string,
	ec2Factory func(region string) ec2API,
	discoverer *Discoverer,
	assetTypeHints []string,
) (*graph.Graph, error) {
	regions, source, err := p.resolveRegionsWithClient(ctx, ec2Factory, discoverer, assetTypeHints)
	if err != nil {
		return nil, err
	}
	p.lastRegions = append([]string(nil), regions...)
	p.lastRegionSource = source

	g := graph.New()
	edges := make(map[graph.Edge]struct{}, 1024)

	accountToken := "accounts/" + account
	if err := g.AddNode(accountNode(accountToken, account)); err != nil {
		return nil, err
	}

	addNode := func(n *graph.Node) error { return g.AddNode(n) }
	emit := func(e graph.Edge) { edges[e] = struct{}{} }

	for _, region := range regions {
		client := ec2Factory(region)
		volumes, addrs, err := p.hydrateRegion(ctx, client)
		if err != nil {
			return nil, fmt.Errorf("aws: %s: %s: %w", account, region, err)
		}

		rn := regionNode(accountToken, account, region)
		if err := addNode(rn); err != nil {
			return nil, err
		}
		emit(graph.Edge{From: rn.ID, To: graph.Ref(accountToken), Kind: graph.EdgeContains})

		for i := range volumes {
			v := &volumes[i]
			n := NormalizeVolume(v, account, region)
			if n == nil {
				continue
			}
			if err := addNode(n); err != nil {
				return nil, err
			}
			emit(graph.Edge{From: n.ID, To: rn.ID, Kind: graph.EdgeContains})

			for _, att := range v.Attachments {
				if att.InstanceId == nil || *att.InstanceId == "" {
					continue
				}
				inst := instanceNode(*att.InstanceId, account, region)
				if err := addNode(inst); err != nil {
					return nil, err
				}
				emit(graph.Edge{From: inst.ID, To: n.ID, Kind: graph.EdgeAttachedTo})
			}
		}

		for i := range addrs {
			a := &addrs[i]
			n := NormalizeAddress(a, account, region)
			if n == nil {
				continue
			}
			if err := addNode(n); err != nil {
				return nil, err
			}
			emit(graph.Edge{From: n.ID, To: rn.ID, Kind: graph.EdgeContains})
		}
	}

	for e := range edges {
		if err := g.AddEdge(e); err != nil {
			return nil, err
		}
	}
	g.Freeze()

	p.log.Info("aws account ingest complete",
		"account", account,
		"regions", len(regions),
		"region_source", source,
		"resource_nodes", g.ResourceNodeCount(),
		"container_nodes", g.NodeCount()-g.ResourceNodeCount(),
		"edges", g.EdgeCount(),
		"dangling_edges", g.DanglingEdges())
	return g, nil
}

// ingestOrganization walks the Organizations API, builds the hierarchy tree,
// assumes a cross-account role into each member account, and merges the
// per-account graphs into a single graph carrying the full hierarchy.
func (p *Provider) ingestOrganization(ctx context.Context, scope cloud.AWSScope, assetTypeHints []string) (*graph.Graph, error) {
	// Phase 1: build the organization tree. This is a handful of
	// Organizations calls regardless of organization size.
	tree, err := buildOrgTree(ctx, p.orgClient, scope)
	if err != nil {
		return nil, fmt.Errorf("aws: build organization tree: %w", err)
	}

	p.log.Info("aws organization tree built",
		"org", orgNodeName(tree),
		"ous", countByKind(tree, graph.KindOrganizationalUnit),
		"accounts_active", len(tree.AccountIDs),
		"total_nodes", len(tree.Nodes))

	// Phase 2: assume role into each account and ingest. This is where the
	// cost lives — each account pays roughly the same number of API calls
	// (DescribeRegions or Resource Explorer discovery, then per-region
	// DescribeVolumes + DescribeAddresses).
	g := graph.New()
	edges := make(map[graph.Edge]struct{}, 1024)

	// Add all hierarchy container nodes and edges from the tree.
	for _, n := range tree.Nodes {
		if err := g.AddNode(n); err != nil {
			return nil, err
		}
	}
	for _, e := range tree.Edges {
		edges[e] = struct{}{}
	}

	addNode := func(n *graph.Node) error { return g.AddNode(n) }
	emit := func(e graph.Edge) { edges[e] = struct{}{} }

	scanned := 0
	p.accountStatuses = make([]AccountStatus, 0, len(tree.AccountIDs))

	for _, accountID := range tree.AccountIDs {
		acctNode, ok := tree.Nodes["accounts/"+accountID]
		acctName := accountID
		if ok && acctNode.Name != "" {
			acctName = acctNode.Name
		}

		// Assume the cross-account role.
		creds, err := p.assumeRole(ctx, accountID)
		if err != nil {
			p.log.Warn("aws: cannot assume role into account, skipping",
				"account", accountID,
				"role", p.roleName,
				"err", err)
			p.accountStatuses = append(p.accountStatuses, AccountStatus{
				ID:     accountID,
				Name:   acctName,
				Status: "unreachable",
				Reason: fmt.Sprintf("cannot assume role %s: %v", p.roleName, err),
			})
			continue
		}

		// Build a per-account EC2 client using the assumed role credentials
		// and a fresh Discoverer for Resource Explorer (if available in this
		// account). The Discoverer may return ErrNoIndex; the per-account
		// resolveRegions handles that gracefully by falling back to
		// DescribeRegions.
		acctCfg := p.awsCfg.Copy()
		acctCfg.Credentials = credentials.NewStaticCredentialsProvider(
			creds.AccessKeyID,
			creds.SecretAccessKey,
			creds.SessionToken,
		)

		acctEC2 := func(region string) ec2API {
			return ec2.NewFromConfig(acctCfg, func(o *ec2.Options) { o.Region = region })
		}
		acctDiscoverer := NewDiscoverer(acctCfg)

		acctGraph, err := p.ingestAccountWithClient(ctx, accountID, acctEC2, acctDiscoverer, assetTypeHints)
		if err != nil {
			p.log.Warn("aws: failed to ingest account, skipping",
				"account", accountID,
				"err", err)
			p.accountStatuses = append(p.accountStatuses, AccountStatus{
				ID:     accountID,
				Name:   acctName,
				Status: "unreachable",
				Reason: fmt.Sprintf("ingest failed: %v", err),
			})
			continue
		}

		// Merge the per-account graph into the organization graph. Copy all
		// non-container nodes (resources, instances) from the account graph.
		// Container nodes (account, region) are already in the org tree.
		acctGraph.Nodes(func(n *graph.Node) bool {
			if n.Container() {
				return true
			}
			_ = addNode(n)
			return true
		})

		// Copy non-containment edges from the per-account graph
		// (attached_to, uses, etc.). Containment edges from the org tree are
		// already set.
		acctGraph.Nodes(func(n *graph.Node) bool {
			for _, e := range acctGraph.Out(n.ID) {
				if e.Kind != graph.EdgeContains {
					emit(e)
				}
			}
			return true
		})

		scanned++
		p.accountStatuses = append(p.accountStatuses, AccountStatus{
			ID:     accountID,
			Name:   acctName,
			Status: "scanned",
		})
	}

	// Report suspended accounts from the tree.
	for id, n := range tree.Nodes {
		if n.Kind == graph.KindAccount {
			if st, ok := n.Attrs["account_status"].(string); ok && st == "SUSPENDED" {
				acctID := strings.TrimPrefix(id, "accounts/")
				alreadyReported := false
				for _, as := range p.accountStatuses {
					if as.ID == acctID {
						alreadyReported = true
						break
					}
				}
				if !alreadyReported {
					p.accountStatuses = append(p.accountStatuses, AccountStatus{
						ID:     acctID,
						Name:   n.Name,
						Status: "suspended",
					})
				}
			}
		}
	}

	for e := range edges {
		if err := g.AddEdge(e); err != nil {
			return nil, err
		}
	}
	g.Freeze()

	p.log.Info("aws organization ingest complete",
		"org", orgNodeName(tree),
		"accounts_scanned", scanned,
		"accounts_unreachable", countUnreachable(p.accountStatuses),
		"accounts_suspended", countSuspended(p.accountStatuses),
		"resource_nodes", g.ResourceNodeCount(),
		"container_nodes", g.NodeCount()-g.ResourceNodeCount(),
		"edges", g.EdgeCount(),
		"dangling_edges", g.DanglingEdges())

	return g, nil
}

// assumeRole calls sts:AssumeRole to obtain temporary credentials for the
// named account. The role ARN is constructed from the account ID and the
// configured role name.
func (p *Provider) assumeRole(ctx context.Context, accountID string) (aws.Credentials, error) {
	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, p.roleName)
	sessionName := "tellury-scan"

	out, err := p.stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String(sessionName),
		DurationSeconds: aws.Int32(3600), // 1 hour
	})
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("sts:AssumeRole %s: %w", roleARN, err)
	}

	return aws.Credentials{
		AccessKeyID:     aws.ToString(out.Credentials.AccessKeyId),
		SecretAccessKey: aws.ToString(out.Credentials.SecretAccessKey),
		SessionToken:    aws.ToString(out.Credentials.SessionToken),
		CanExpire:       true,
		Expires:         aws.ToTime(out.Credentials.Expiration),
	}, nil
}

// EnrichMetrics implements cloud.Provider. AWS CloudWatch enrichment arrives
// with metric-dependent rules; this is a no-op today.
func (p *Provider) EnrichMetrics(ctx context.Context, g *graph.Graph, sc cloud.Scope, req metrics.Request) error {
	return nil
}

// resolveRegions returns the sorted, de-duplicated list of regions to scan and
// the source that produced it.
func (p *Provider) resolveRegions(ctx context.Context, assetTypeHints []string) ([]string, string, error) {
	return p.resolveRegionsWithClient(ctx, p.ec2Client, p.discoverer, assetTypeHints)
}

// resolveRegionsWithClient resolves regions using the given EC2 client factory
// and discoverer. It is the shared implementation for both the caller's own
// account (single-account scan) and assumed-role accounts (organization scan).
func (p *Provider) resolveRegionsWithClient(
	ctx context.Context,
	ec2Factory func(region string) ec2API,
	discoverer *Discoverer,
	assetTypeHints []string,
) ([]string, string, error) {
	if len(p.explicit) > 0 {
		return p.explicit, "explicit", nil
	}
	if p.offline {
		if p.fixture == nil {
			return nil, "", fmt.Errorf("aws: offline provider has no fixture")
		}
		return p.fixture.RegionNames(), "fixture", nil
	}

	// Map asset-type hints to CloudFormation resource type identifiers.
	cfTypes := assetTypesToCloudFormation(assetTypeHints)
	if len(cfTypes) > 0 && discoverer != nil {
		discovered, err := discoverer.Discover(ctx, cfTypes)
		if err != nil {
			p.log.Info("resource explorer unavailable, falling back to DescribeRegions sweep",
				"err", err)
		} else if len(discovered) > 0 {
			regions := make([]string, 0, len(discovered))
			for r := range discovered {
				regions = append(regions, r)
			}
			sort.Strings(regions)
			p.log.Info("resource explorer narrowed regions",
				"discovered", len(regions),
				"regions", strings.Join(regions, ","))
			return regions, "resource_explorer", nil
		}
	}

	// DescribeRegions fallback.
	client := ec2Factory("")
	out, err := client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{AllRegions: aws.Bool(false)})
	if err != nil {
		return nil, "", fmt.Errorf("aws: DescribeRegions: %w", err)
	}
	regions := make([]string, 0, len(out.Regions))
	for _, r := range out.Regions {
		if r.RegionName != nil && *r.RegionName != "" {
			regions = append(regions, *r.RegionName)
		}
	}
	if len(regions) == 0 {
		return nil, "", fmt.Errorf("aws: DescribeRegions returned no enabled regions")
	}
	return canonicaliseRegions(regions), "describe_regions", nil
}

// assetTypeToCloudFormation maps the provider's own asset-type tokens to
// CloudFormation resource type identifiers.
var assetTypeToCloudFormation = map[string]string{
	"aws.ec2.volume":  "AWS::EC2::Volume",
	"aws.ec2.address": "AWS::EC2::EIP",
}

// assetTypesToCloudFormation converts asset-type hints to CloudFormation
// resource type strings, dropping any hint that has no mapping.
func assetTypesToCloudFormation(hints []string) []string {
	if len(hints) == 0 {
		return nil
	}
	out := make([]string, 0, len(hints))
	for _, h := range hints {
		if cf, ok := assetTypeToCloudFormation[h]; ok {
			out = append(out, cf)
		}
	}
	return out
}

// hydrateRegion lists every EBS volume and Elastic IP in one region.
func (p *Provider) hydrateRegion(ctx context.Context, client ec2API) ([]ec2types.Volume, []ec2types.Address, error) {
	var volumes []ec2types.Volume
	paginator := ec2.NewDescribeVolumesPaginator(client, &ec2.DescribeVolumesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("DescribeVolumes: %w", err)
		}
		volumes = append(volumes, page.Volumes...)
	}

	addrOut, err := client.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
	if err != nil {
		return nil, nil, fmt.Errorf("DescribeAddresses: %w", err)
	}
	return volumes, addrOut.Addresses, nil
}

// ec2Client returns a region-scoped EC2 API. Offline providers return a
// fixture-backed fake; live providers return the SDK client.
func (p *Provider) ec2Client(region string) ec2API {
	if p.offline {
		return &fakeEC2{region: region, f: p.fixture}
	}
	if region == "" {
		region = p.awsCfg.Region
	}
	if region == "" {
		region = "us-east-1"
	}
	return ec2.NewFromConfig(p.awsCfg, func(o *ec2.Options) { o.Region = region })
}

// canonicaliseRegions normalises each region through the single
// pricing.CanonicalRegion canonicaliser, drops empty results and de-duplicates.
func canonicaliseRegions(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, r := range in {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		c := pricing.CanonicalRegion(r)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// countByKind returns the number of nodes in the tree of the given kind.
func countByKind(t *orgTree, kind graph.ResourceKind) int {
	n := 0
	for _, node := range t.Nodes {
		if node.Kind == kind {
			n++
		}
	}
	return n
}

// countUnreachable counts account statuses that are "unreachable".
func countUnreachable(statuses []AccountStatus) int {
	n := 0
	for _, s := range statuses {
		if s.Status == "unreachable" {
			n++
		}
	}
	return n
}

// countSuspended counts account statuses that are "suspended".
func countSuspended(statuses []AccountStatus) int {
	n := 0
	for _, s := range statuses {
		if s.Status == "suspended" {
			n++
		}
	}
	return n
}
