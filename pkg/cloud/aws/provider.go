package aws

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	pricingaws "github.com/TypeOneLabs/tellury/pkg/pricing/aws"
)

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

// Provider is the AWS implementation of cloud.Provider. It ingests a single
// AWS account (--aws-account) through the EC2 API — EBS volumes and Elastic
// IPs, per region — normalized into the provider-neutral graph under an
// account container node and per-account region nodes, exactly as GCP hangs
// resources off a region under a project.
//
// This step deliberately scopes to --aws-account only. Organizations
// traversal, cross-account role assumption and Resource Explorer arrive in
// later steps; the design sequences them that way so something works end to
// end first.
type Provider struct {
	log      *slog.Logger
	offline  bool
	fixture  *Fixture
	awsCfg   aws.Config
	pricer   pricing.Pricer
	explicit []string // --aws-regions values, canonicalised and de-duplicated

	// lastScan carries the region coverage of the most recent Ingest, so the
	// CLI can report "N regions analyzed (source)" in the scan summary
	// without walking the graph or re-deriving the source from the flags.
	lastRegions      []string
	lastRegionSource string
}

var _ cloud.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithLogger sets the provider logger.
func WithLogger(l *slog.Logger) Option { return func(p *Provider) { p.log = l } }

// WithOffline builds a provider that never constructs an AWS SDK client. It is
// for scans whose data comes from local fixtures: the fixture stands in for
// every EC2 call, and the static price table prices the replay. This is what
// lets an offline AWS scan run on a host with no AWS credentials.
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

// New builds an AWS provider. Unless WithOffline is set, it loads the default
// AWS credential chain — environment variables, shared config and credentials
// files, the profile named by AWS_PROFILE, and the region named by AWS_REGION
// — exactly as the AWS CLI would, via aws-sdk-go-v2/config.LoadDefaultConfig.
// There is deliberately no custom credential resolution and no credentials
// flag: tellury reads no key files, and the GCP side sets that precedent with
// Application Default Credentials.
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
	if p.pricer == nil {
		static, err := pricingaws.NewStaticPricer()
		if err != nil {
			return nil, err
		}
		p.pricer = static
	}
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
	}
	return p, nil
}

// Name implements cloud.Provider.
func (p *Provider) Name() string { return ProviderName }

// Pricer implements cloud.Provider. AWS has no price catalogue shipped yet —
// no AWS rule prices anything — so this is a pure, offline StaticPricer whose
// table is empty and whose lookups answer pricing.ErrNoPrice until the AWS
// pricing step lands.
func (p *Provider) Pricer() pricing.Pricer { return p.pricer }

// Sizer implements cloud.Provider. AWS has no rightsizing catalog yet, so nil
// is returned (the CLI's rules Pass treats nil as "no catalog available").
func (p *Provider) Sizer() pricing.Sizer { return nil }

// Close implements cloud.Provider. There are no long-lived clients to release.
func (p *Provider) Close() error { return nil }

// Regions returns the region list and source of the most recent Ingest. The
// source is one of "explicit" (--aws-regions), "describe_regions" (the
// default DescribeRegions sweep of the account's enabled regions) or
// "fixture" (offline replay). The CLI reports both in the scan summary so an
// operator always knows which regions a scan actually covered and why.
func (p *Provider) Regions() ([]string, string) {
	return append([]string(nil), p.lastRegions...), p.lastRegionSource
}

// Ingest performs one pass over the account's EC2 resources and returns a
// frozen graph. The scope must be an AWS account scope (--aws-account); the
// other two AWS dimensions are Organizations features this build does not
// implement yet, and are rejected rather than silently ignored.
//
// The graph layout mirrors GCP's: each resource hangs off a per-account
// region container node ("accounts/<id>/regions/<region>"), and each region
// node hangs off the account container node ("accounts/<id>", KindAccount).
// Volume attachments are modelled as instance -> volume EdgeAttachedTo edges
// with a minimal instance node standing in for the attached instance.
//
// Regions are resolved in this order:
//
//  1. --aws-regions (explicit list, canonicalised)
//  2. ec2:DescribeRegions with AllRegions=false (the default: every region
//     enabled for the account, never a blind sweep of regions the account
//     cannot use)
//  3. an offline fixture's region keys (fixture replay)
//
// The regions actually covered, and how they were chosen, are recorded on the
// provider (see Regions) so the CLI can report them in the scan summary.
func (p *Provider) Ingest(ctx context.Context, sc cloud.Scope, assetTypeHints []string) (*graph.Graph, error) {
	if sc.AWS == nil || sc.AWS.Account == "" {
		return nil, fmt.Errorf("aws: scope requires account (--aws-account)")
	}
	if !p.offline {
		if sc.AWS.OrganizationalUnit != "" || sc.AWS.Organization != "" {
			return nil, fmt.Errorf("aws: this build ingests --aws-account only; organizational-unit and organization scans arrive with Organizations traversal")
		}
	}
	account := sc.AWS.Account

	regions, source, err := p.resolveRegions(ctx)
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
		client := p.ec2Client(region)
		volumes, addrs, err := p.hydrateRegion(ctx, client)
		if err != nil {
			return nil, fmt.Errorf("aws: %s: %w", region, err)
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

			// Volume attachments: for each attachment, create a minimal
			// instance node and an attached_to edge so the graph's topology
			// says "attached" exactly when the normalizer's attachment list
			// says so. A future DescribeInstances step enriches these nodes;
			// today they exist to keep the no-edge/no-finding invariant
			// truthful.
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

	p.log.Info("aws ingest complete",
		"account", account,
		"regions", len(regions),
		"region_source", source,
		"resource_nodes", g.ResourceNodeCount(),
		"container_nodes", g.NodeCount()-g.ResourceNodeCount(),
		"edges", g.EdgeCount(),
		"dangling_edges", g.DanglingEdges())
	return g, nil
}

// EnrichMetrics implements cloud.Provider. AWS CloudWatch enrichment arrives
// with metric-dependent rules; this is a no-op today.
func (p *Provider) EnrichMetrics(ctx context.Context, g *graph.Graph, sc cloud.Scope, req metrics.Request) error {
	return nil
}

// resolveRegions returns the sorted, de-duplicated list of regions to scan and
// the source that produced it. Precedence: --aws-regions, then the offline
// fixture's regions, then ec2:DescribeRegions (AllRegions=false — only regions
// enabled for the account, so the default is never a blind sweep of regions
// the account cannot use).
func (p *Provider) resolveRegions(ctx context.Context) ([]string, string, error) {
	if len(p.explicit) > 0 {
		return p.explicit, "explicit", nil
	}
	if p.offline {
		if p.fixture == nil {
			return nil, "", fmt.Errorf("aws: offline provider has no fixture")
		}
		return p.fixture.RegionNames(), "fixture", nil
	}

	// DescribeRegions is region-agnostic; use the resolved default region (or
	// us-east-1 as a safe fallback) only to address the call.
	client := p.ec2Client("")
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

// hydrateRegion lists every EBS volume and Elastic IP in one region.
//
// Volumes are paginated with the SDK's DescribeVolumesPaginator (MaxResults/
// NextToken), so a region with thousands of volumes is fetched completely.
// DescribeAddresses has no paginator in the SDK — the API returns all
// addresses in one call — so one call is the complete, correct fetch.
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
// "us-east-1a" becomes "us-east-1", so an operator can pass an availability
// zone and still get the region's data. The result is sorted for determinism.
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
