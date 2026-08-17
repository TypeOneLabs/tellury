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
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/TypeOneLabs/tellury/pkg/cloud"
	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	metricsaws "github.com/TypeOneLabs/tellury/pkg/metrics/aws"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
	pricingaws "github.com/TypeOneLabs/tellury/pkg/pricing/aws"

	// Ensure the compute metric specs are registered so that Supports()
	// returns true for cpu_utilization_p95.
	_ "github.com/TypeOneLabs/tellury/pkg/metrics/aws/compute"
)

// DefaultRoleName is the conventional cross-account role name AWS
// Organizations sets up when an account is created via the console.
// OrganizationAccountAccessRole is the documented convention; operators who
// use a different name configure it with --aws-role-name.
const DefaultRoleName = "OrganizationAccountAccessRole"

// ec2API is the subset of the EC2 API this provider calls. It is what lets a
// fixture (offline replay) stand in for the live SDK client without touching
// the network: the provider drives DescribeRegions, the DescribeVolumes
// paginator, DescribeAddresses, the DescribeInstances paginator,
// DescribeInstanceTypes, and the AMI discovery and reference APIs through this
// interface, never through the concrete client.
type ec2API interface {
	DescribeRegions(ctx context.Context, params *ec2.DescribeRegionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error)
	DescribeVolumes(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	DescribeAddresses(ctx context.Context, params *ec2.DescribeAddressesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeInstanceTypes(ctx context.Context, params *ec2.DescribeInstanceTypesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)

	// AMI discovery and reference enumeration. These are gated on
	// assetTypeHints so existing volume/address/instance scans do not require
	// the additional permissions.
	DescribeImages(ctx context.Context, params *ec2.DescribeImagesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeSnapshots(ctx context.Context, params *ec2.DescribeSnapshotsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
	DescribeLaunchTemplates(ctx context.Context, params *ec2.DescribeLaunchTemplatesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error)
	DescribeLaunchTemplateVersions(ctx context.Context, params *ec2.DescribeLaunchTemplateVersionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplateVersionsOutput, error)
	DescribeFleets(ctx context.Context, params *ec2.DescribeFleetsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeFleetsOutput, error)
	DescribeSpotFleetRequests(ctx context.Context, params *ec2.DescribeSpotFleetRequestsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSpotFleetRequestsOutput, error)
}

// autoScalingAPI is the subset of the Auto Scaling API the AWS image
// reference pass calls. Launch configurations live in the Auto Scaling API,
// not EC2; the interface keeps the live SDK and the offline fixture behind
// the same seam.
type autoScalingAPI interface {
	DescribeLaunchConfigurations(ctx context.Context, params *autoscaling.DescribeLaunchConfigurationsInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeLaunchConfigurationsOutput, error)
}

// stsAPI is the subset of STS this provider calls. It is an interface so
// tests can provide a fake.
type stsAPI interface {
	AssumeRole(ctx context.Context, params *sts.AssumeRoleInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// imageInventory is the per-region result of the AMI hydration path: the
// self-owned images, the self-owned snapshots used to derive backing sizes,
// and the reference set used to decide whether an image is in use.
type imageInventory struct {
	images               []ec2types.Image
	snapshots            []ec2types.Snapshot
	snapshotByID         map[string]ec2types.Snapshot
	snapshotRefCounts    map[string]int
	references           imageReferenceSet
	snapshotsComplete    bool
	amiReferenceComplete bool
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

	// sizer answers "what else exists in this instance's family", which is
	// what lets a rule recommend a smaller size rather than only stop/delete.
	// Populated during Ingest from ec2:DescribeInstanceTypes.
	sizer *Sizer

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
	p := &Provider{log: slog.Default(), sizer: NewSizer()}
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
func (p *Provider) Sizer() pricing.Sizer {
	if p.sizer == nil {
		return nil
	}
	return p.sizer
}

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
	return p.ingestAccountWithClient(ctx, account, p.ec2Client, p.autoScalingClient, p.discoverer, assetTypeHints)
}

// ingestAccountWithClient ingests a single account using the provided EC2 and
// Auto Scaling client factories and optional discoverer. The discoverer may be
// nil for accounts where Resource Explorer is unavailable; in that case the
// account falls back to DescribeRegions.
func (p *Provider) ingestAccountWithClient(
	ctx context.Context,
	account string,
	ec2Factory func(region string) ec2API,
	autoScalingFactory func(region string) autoScalingAPI,
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

	wantImages := hasAssetTypeHint(assetTypeHints, assetTypeImage)
	wantSnapshots := hasAssetTypeHint(assetTypeHints, assetTypeSnapshot)

	for _, region := range regions {
		client := ec2Factory(region)
		volumes, addrs, instances, shapes, err := p.hydrateRegion(ctx, client)
		if err != nil {
			return nil, fmt.Errorf("aws: %s: %s: %w", account, region, err)
		}

		// Resolve the full size ladder for every family present, so a rule
		// can recommend a smaller sibling rather than only stop/delete.
		// Ingest's own DescribeInstanceTypes call resolves ONLY the types
		// actually running, which describes an instance but cannot rightsize
		// it — the candidates are by definition the types that are not
		// running. Failure is non-fatal and logged: without a ladder the rule
		// degrades to stop/delete, which is the pre-Sizer behaviour.
		if p.sizer != nil && len(instances) > 0 {
			seen := map[string]bool{}
			var families []string
			for i := range instances {
				f := FamilyOf(string(instances[i].InstanceType))
				if f == "" || seen[f] {
					continue
				}
				seen[f] = true
				families = append(families, f)
			}
			sort.Strings(families)
			if err := p.sizer.LoadFamilies(ctx, client, families); err != nil {
				p.log.Warn("aws: could not resolve instance-type families; rightsizing candidates unavailable",
					"region", region, "err", err)
			}
		}

		// AMI discovery and reference enumeration are gated on the asset-type
		// hints produced from the selected rule set. Existing volume/address/
		// instance scans pass no image/snapshot hint and make none of these
		// calls, so they do not require the additional permissions.
		var imageInv imageInventory
		if wantImages || wantSnapshots {
			imageInv, err = p.hydrateImages(ctx, client, autoScalingFactory(region), instances, wantImages)
			if err != nil {
				return nil, fmt.Errorf("aws: %s: %s: %w", account, region, err)
			}
		}

		rn := regionNode(accountToken, account, region)
		if err := addNode(rn); err != nil {
			return nil, err
		}
		emit(graph.Edge{From: rn.ID, To: graph.Ref(accountToken), Kind: graph.EdgeContains})

		// Phase 1: Volumes and Addresses (as before).
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

		// Phase 2: Instances — processed AFTER volumes so that any stub
		// created by an EBS attachment is overwritten by the enriched node.
		// graph.AddNode is "last write wins", so an enriched instance always
		// replaces a stub with the same ID regardless of which was created
		// first. Processing instances last guarantees the enriched node is
		// the final write.
		for i := range instances {
			inst := &instances[i]
			if inst.InstanceId == nil || *inst.InstanceId == "" {
				continue
			}
			itype := string(inst.InstanceType)
			var shape *InstanceTypeInfo
			if info, ok := shapes[itype]; ok {
				shape = &info
			}
			n := NormalizeInstance(inst, shape, account, region)
			if n == nil {
				continue
			}
			if err := addNode(n); err != nil {
				return nil, err
			}
			emit(graph.Edge{From: n.ID, To: rn.ID, Kind: graph.EdgeContains})
		}

		// Phase 3: AMI and snapshot nodes, when their asset types were
		// requested. They are processed after instances so the reference set
		// has already seen the DescribeInstances image IDs.
		if wantImages {
			for i := range imageInv.images {
				img := &imageInv.images[i]
				n := NormalizeImage(img, account, region, imageInv.snapshotByID, imageInv.snapshotRefCounts, imageInv.references, imageInv.snapshotsComplete)
				if n == nil {
					continue
				}
				if err := addNode(n); err != nil {
					return nil, err
				}
				emit(graph.Edge{From: n.ID, To: rn.ID, Kind: graph.EdgeContains})
			}
		}
		if wantSnapshots {
			for i := range imageInv.snapshots {
				snap := &imageInv.snapshots[i]
				snapshotID := ""
				if snap.SnapshotId != nil {
					snapshotID = *snap.SnapshotId
				}
				n := NormalizeSnapshot(snap, account, region, imageInv.snapshotRefCounts[snapshotID], imageInv.amiReferenceComplete)
				if n == nil {
					continue
				}
				if err := addNode(n); err != nil {
					return nil, err
				}
				emit(graph.Edge{From: n.ID, To: rn.ID, Kind: graph.EdgeContains})
			}
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
	// DescribeVolumes + DescribeAddresses + DescribeInstances + DescribeInstanceTypes,
	// plus AMI discovery when the selected rules ask for it).
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

	// An organization almost always contains the account the credentials
	// themselves belong to — usually the management account. You cannot assume
	// a role into yourself unless someone has explicitly created one, and
	// OrganizationAccountAccessRole is created in accounts that Organizations
	// creates, not in the management account. Without this, the one account an
	// operator is guaranteed to have access to is the one account the scan
	// always reports as unreachable.
	callerAccount := p.callerAccountID(ctx)

	for _, accountID := range tree.AccountIDs {
		acctNode, ok := tree.Nodes["accounts/"+accountID]
		acctName := accountID
		if ok && acctNode.Name != "" {
			acctName = acctNode.Name
		}

		// The caller's own account needs no assumption: the credentials in hand
		// already address it.
		if accountID != "" && accountID == callerAccount {
			ownGraph, err := p.ingestAccountWithClient(ctx, accountID, p.ec2Client, p.autoScalingClient, p.discoverer, assetTypeHints)
			if err != nil {
				p.log.Warn("aws: scanning the caller's own account failed",
					"account", accountID, "err", err)
				p.accountStatuses = append(p.accountStatuses, AccountStatus{
					ID: accountID, Name: acctName, Status: "unreachable",
					Reason: fmt.Sprintf("scan failed: %v", err),
				})
				continue
			}
			ownGraph.Nodes(func(n *graph.Node) bool {
				if !n.Container() {
					_ = addNode(n)
				}
				return true
			})
			ownGraph.Nodes(func(n *graph.Node) bool {
				for _, e := range ownGraph.Out(n.ID) {
					if e.Kind != graph.EdgeContains {
						emit(e)
					}
				}
				return true
			})
			scanned++
			p.accountStatuses = append(p.accountStatuses, AccountStatus{
				ID: accountID, Name: acctName, Status: "scanned",
				Reason: "caller's own account; no role assumed",
			})
			continue
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
			return newEC2Client(acctCfg, region)
		}
		acctASG := func(region string) autoScalingAPI {
			return newAutoScalingClient(acctCfg, region)
		}
		acctDiscoverer := NewDiscoverer(acctCfg)

		acctGraph, err := p.ingestAccountWithClient(ctx, accountID, acctEC2, acctASG, acctDiscoverer, assetTypeHints)
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

// callerAccountID returns the account the current credentials belong to, or ""
// if it cannot be determined. sts:GetCallerIdentity needs no IAM permission —
// it is always allowed — so a failure here means the credentials themselves
// are unusable, which the scan will discover anyway.
func (p *Provider) callerAccountID(ctx context.Context) string {
	if p.stsClient == nil {
		return ""
	}
	out, err := p.stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || out == nil || out.Account == nil {
		p.log.Debug("aws: could not determine the caller's own account; every account will be assumed into", "err", err)
		return ""
	}
	return *out.Account
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

// EnrichMetrics implements cloud.Provider. It walks the graph for EC2
// instance nodes, groups them by (account, region), constructs a CloudWatch
// client per pair (reusing the existing credential strategy — the caller's
// own account is reached directly, member accounts are assumed into), and
// delegates to the AWS metrics Client for fan-out across the bounded worker
// pool.
//
// Enrichment failure is non-fatal: the caller (scan.go/buildGraph) logs and
// continues because a rule that cannot get a metric skips rather than guesses
// (invariant I5). Per-job failures within Fill are also isolated — one
// failing (key, account, region) job does not cancel its siblings.
//
// Progress is reported through req.Progress (the "metric enrichment" phase
// denominator) if the caller sets it.
func (p *Provider) EnrichMetrics(ctx context.Context, g *graph.Graph, sc cloud.Scope, req metrics.Request) error {
	if len(req.Keys) == 0 {
		return nil
	}

	// Walk instance nodes to build the per-(account, region) instance list.
	// Only "aws" provider instances with an instance_id attribute are
	// included; instances without one (stubs that were never enriched) are
	// skipped.
	instances := make(map[metricsaws.AccountRegion][]metricsaws.InstanceRef)
	g.Nodes(func(n *graph.Node) bool {
		if n.Kind != graph.KindInstance || n.Provider != "aws" {
			return true
		}
		instanceID, ok := n.Str(AttrInstanceID)
		if !ok || instanceID == "" {
			return true
		}
		ar := metricsaws.AccountRegion{Account: n.Project, Region: n.Location}
		instances[ar] = append(instances[ar], metricsaws.InstanceRef{
			Ref:        n.ID,
			InstanceID: instanceID,
		})
		return true
	})

	if len(instances) == 0 {
		return nil
	}

	// Build CloudWatch clients per (account, region).
	clients := make(map[metricsaws.AccountRegion]metricsaws.CloudWatchAPI)

	// Offline path: CloudWatch is unavailable. Return nil so the caller logs
	// and continues; metric-dependent rules will skip.
	if p.offline {
		p.log.Debug("aws: offline provider cannot enrich metrics; metric-dependent rules will skip",
			"instances", len(instances))
		return nil
	}

	callerAccount := p.callerAccountID(ctx)

	for ar := range instances {
		var cwCfg aws.Config
		if ar.Account != "" && ar.Account == callerAccount {
			// The caller's own account: use the provider's config directly.
			// No role assumption needed — the credentials in hand already
			// address this account.
			//
			// An UNKNOWN caller (callerAccount == "") deliberately falls to
			// the assume-role branch, matching ingestOrganization. Treating
			// unknown as "every account is my own" fails open: an org scan
			// would query every member account's CloudWatch with the caller's
			// own credentials, get nothing back, and skip every
			// metric-dependent rule with no operator-visible reason.
			cwCfg = p.awsCfg
		} else {
			// Member account: assume the cross-account role to get temporary
			// credentials for CloudWatch.
			creds, err := p.assumeRole(ctx, ar.Account)
			if err != nil {
				p.log.Warn("aws: cannot assume role for metric enrichment, skipping account",
					"account", ar.Account, "region", ar.Region, "err", err)
				continue
			}
			cwCfg = p.awsCfg.Copy()
			cwCfg.Credentials = credentials.NewStaticCredentialsProvider(
				creds.AccessKeyID,
				creds.SecretAccessKey,
				creds.SessionToken,
			)
		}

		clients[ar] = cloudwatch.NewFromConfig(cwCfg, func(o *cloudwatch.Options) {
			o.Region = ar.Region
		})
	}

	if len(clients) == 0 {
		return fmt.Errorf("aws: no CloudWatch clients could be constructed for metric enrichment")
	}

	mc := metricsaws.NewClient(p.log, instances, clients)
	return mc.Fill(ctx, req, func(ref graph.Ref, key string, v graph.MetricValue) {
		g.SetMetric(ref, key, v)
	})
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

	// Resource Explorer is the region source. There is no DescribeRegions
	// sweep: sweeping every enabled region cost ~70s against an account whose
	// resources sat in two, and it ran on every scan because the old combined
	// query silently matched nothing.
	//
	// The cost of dropping the sweep is that "which regions" must now be
	// answered correctly or not at all. Each failure below is therefore an
	// error naming its remedy — never a shorter region list, which would look
	// exactly like a clean scan.
	if discoverer == nil {
		return nil, "", fmt.Errorf("aws: no Resource Explorer client; pass --aws-regions to scan specific regions")
	}

	mapped, unmapped := assetTypesToResourceExplorer(assetTypeHints)
	if len(unmapped) > 0 {
		return nil, "", fmt.Errorf("aws: no Resource Explorer resource type is mapped for %s, "+
			"so the regions holding it cannot be discovered; pass --aws-regions to scan "+
			"specific regions, and please report this so the mapping can be added",
			strings.Join(unmapped, ", "))
	}
	if len(mapped) == 0 {
		return nil, "", fmt.Errorf("aws: no asset types requested; nothing to discover")
	}

	// An aggregator index lets one query per type cover every indexed region.
	// Without one, each region has to be asked directly — which needs no setup
	// from the operator, the trade being the first-run gap documented on
	// DiscoverAcrossRegions.
	hasAggregator, err := discoverer.HasAggregatorIndex(ctx)
	if err != nil {
		p.log.Info("could not check for a Resource Explorer aggregator index; sweeping regions instead",
			"err", err)
	}

	var discovered map[string][]string
	if hasAggregator {
		discovered, err = discoverer.Discover(ctx, mapped)
	} else {
		// The sweep needs the region list to sweep. This is the one
		// DescribeRegions call that remains: it enumerates the account's
		// enabled regions, it does not hydrate any of them.
		var enabled []string
		enabled, err = enabledRegions(ctx, ec2Factory)
		if err != nil {
			return nil, "", err
		}

		// Which regions Resource Explorer can actually answer for TODAY.
		// Asking an un-indexed region returns "0 results, Complete: true" and
		// creates its index as a side effect, so resources there are missed on
		// this scan and found on a later one. Report those regions by name:
		// this project does not ship a gap it could have described.
		if indexed, idxErr := discoverer.IndexedRegions(ctx); idxErr != nil {
			p.log.Warn("could not list Resource Explorer indexes; cannot tell which regions it can answer for",
				"err", idxErr)
		} else if missing := regionsWithoutIndex(enabled, indexed); len(missing) > 0 {
			p.log.Warn("regions have no Resource Explorer index yet; resources there are NOT "+
				"reported by this scan. Searching them creates their index, so a later scan "+
				"will see them (minutes for tagged resources, up to ~2h otherwise). "+
				"Use --aws-regions to scan them now",
				"regions", len(missing),
				"list", strings.Join(missing, ","))
		}

		discovered, err = discoverer.DiscoverAcrossRegions(ctx, mapped, enabled)
	}
	if err != nil {
		return nil, "", fmt.Errorf("aws: Resource Explorer discovery failed: %w\n"+
			"Pass --aws-regions to scan specific regions", err)
	}

	// An empty result from a working aggregator index is a real answer: the
	// account holds none of these resource types anywhere. The scan proceeds
	// with no regions and reports no resources, which is true.
	regions := make([]string, 0, len(discovered))
	for r := range discovered {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	p.log.Info("resource explorer resolved regions",
		"regions", len(regions),
		"list", strings.Join(regions, ","),
		"types", strings.Join(mapped, ","),
		"aggregator", hasAggregator)
	return canonicaliseRegions(regions), "resource_explorer", nil
}

// regionsWithoutIndex returns the enabled regions Resource Explorer has no
// index for, sorted. These are exactly the regions whose search will answer a
// confident empty on this scan.
func regionsWithoutIndex(enabled []string, indexed map[string]bool) []string {
	var missing []string
	for _, r := range enabled {
		if !indexed[r] {
			missing = append(missing, r)
		}
	}
	sort.Strings(missing)
	return missing
}

// enabledRegions lists the regions the account has enabled. It is the input to
// the per-region Resource Explorer sweep — the regions to ASK, not the regions
// to hydrate. Hydration still happens only where Resource Explorer found
// something.
func enabledRegions(ctx context.Context, ec2Factory func(string) ec2API) ([]string, error) {
	client := ec2Factory("")
	out, err := client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{AllRegions: aws.Bool(false)})
	if err != nil {
		return nil, fmt.Errorf("aws: DescribeRegions: %w", err)
	}
	regions := make([]string, 0, len(out.Regions))
	for _, r := range out.Regions {
		if r.RegionName != nil && *r.RegionName != "" {
			regions = append(regions, *r.RegionName)
		}
	}
	if len(regions) == 0 {
		return nil, fmt.Errorf("aws: DescribeRegions returned no enabled regions")
	}
	return canonicaliseRegions(regions), nil
}

// assetTypeToCloudFormation maps the provider's own asset-type tokens to
// CloudFormation resource type identifiers.
// assetTypeToResourceExplorer maps an asset type to the resource type string
// Resource Explorer uses.
//
// These are the values Search RETURNS, confirmed against a live index on
// 2026-08-14. The CloudFormation-style spellings that were here before
// ("AWS::EC2::Image") are not reliably accepted: some resolve and some match
// nothing, with no error either way.
//
// Every asset type an AWS rule can declare must appear here. A missing entry is
// not a slow path — resolveRegions refuses to narrow when a needed type has no
// mapping, because a region holding only that type would be dropped from the
// scan without a word.
var assetTypeToResourceExplorer = map[string]string{
	"aws.ec2.volume":   "ec2:volume",
	"aws.ec2.address":  "ec2:elastic-ip",
	"aws.ec2.image":    "ec2:image",
	"aws.ec2.instance": "ec2:instance",
	"aws.ec2.snapshot": "ec2:snapshot",
}

// assetTypesToResourceExplorer converts asset-type hints to Resource Explorer
// resource type strings. unmapped names any hint with no mapping — the caller
// must not narrow the region list when that list is non-empty.
func assetTypesToResourceExplorer(hints []string) (mapped []string, unmapped []string) {
	if len(hints) == 0 {
		return nil, nil
	}
	for _, h := range hints {
		if rt, ok := assetTypeToResourceExplorer[h]; ok {
			mapped = append(mapped, rt)
			continue
		}
		unmapped = append(unmapped, h)
	}
	return mapped, unmapped
}

// hasAssetTypeHint reports whether hints contains exactly want. Empty hints
// mean "the existing default set" and never include the gated image calls.
func hasAssetTypeHint(hints []string, want string) bool {
	for _, h := range hints {
		if strings.TrimSpace(h) == want {
			return true
		}
	}
	return false
}

// hydrateRegion lists every EBS volume, Elastic IP, instance, and instance
// type in one region. It calls DescribeVolumes (paginated), DescribeAddresses,
// DescribeInstances (paginated), and DescribeInstanceTypes (targeted, batched
// into groups of 100). The returned shape map is keyed by instance type string
// (e.g. "t3.medium") and is guaranteed to be non-nil (empty when
// DescribeInstanceTypes fails or returns no results).
func (p *Provider) hydrateRegion(ctx context.Context, client ec2API) ([]ec2types.Volume, []ec2types.Address, []ec2types.Instance, map[string]InstanceTypeInfo, error) {
	// Volumes.
	var volumes []ec2types.Volume
	paginator := ec2.NewDescribeVolumesPaginator(client, &ec2.DescribeVolumesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("DescribeVolumes: %w", err)
		}
		volumes = append(volumes, page.Volumes...)
	}

	// Addresses.
	addrOut, err := client.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("DescribeAddresses: %w", err)
	}

	// Instances (paginated).
	var instances []ec2types.Instance
	instPaginator := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{})
	for instPaginator.HasMorePages() {
		page, err := instPaginator.NextPage(ctx)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("DescribeInstances: %w", err)
		}
		for _, res := range page.Reservations {
			instances = append(instances, res.Instances...)
		}
	}

	// Instance types: collect the distinct set of instance types and resolve
	// them via DescribeInstanceTypes. The API accepts up to 100 types per
	// call; batch larger sets.
	shapes := make(map[string]InstanceTypeInfo, len(instances)/2+1)
	if len(instances) > 0 {
		seen := make(map[string]bool, len(instances))
		var typeNames []ec2types.InstanceType
		for _, inst := range instances {
			t := string(inst.InstanceType)
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			typeNames = append(typeNames, ec2types.InstanceType(t))
		}

		// Batch into groups of 100.
		batchSize := 100
		for start := 0; start < len(typeNames); start += batchSize {
			end := start + batchSize
			if end > len(typeNames) {
				end = len(typeNames)
			}
			batch := typeNames[start:end]
			out, err := client.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
				InstanceTypes: batch,
			})
			if err != nil {
				p.log.Warn("aws: DescribeInstanceTypes failed; instance shapes will be absent",
					"err", err)
				break
			}
			for _, it := range out.InstanceTypes {
				info := InstanceTypeInfo{}
				if it.VCpuInfo != nil && it.VCpuInfo.DefaultVCpus != nil {
					info.VCPU = float64(*it.VCpuInfo.DefaultVCpus)
				}
				if it.MemoryInfo != nil && it.MemoryInfo.SizeInMiB != nil {
					info.MemoryGiB = float64(*it.MemoryInfo.SizeInMiB) / 1024.0
				}
				shapes[string(it.InstanceType)] = info
			}
		}
	}

	return volumes, addrOut.Addresses, instances, shapes, nil
}

// hydrateImages discovers self-owned AMIs and snapshots and enumerates the AMI
// reference sources needed by the unused_ami rule. DescribeImages is a primary
// discovery API and is fatal on error. DescribeSnapshots failure is non-fatal:
// images are still emitted with backing_complete=false so the rule skips them
// as missing data. Reference API failures set references_complete=false rather
// than aborting the scan.
func (p *Provider) hydrateImages(
	ctx context.Context,
	client ec2API,
	asgClient autoScalingAPI,
	instances []ec2types.Instance,
	enumerateReferences bool,
) (imageInventory, error) {
	inv := imageInventory{
		snapshotByID:         map[string]ec2types.Snapshot{},
		snapshotRefCounts:    map[string]int{},
		references:           newImageReferenceSet(),
		snapshotsComplete:    true,
		amiReferenceComplete: true,
	}

	// Self-owned AMIs only.
	imgPaginator := ec2.NewDescribeImagesPaginator(client, &ec2.DescribeImagesInput{
		Owners: []string{"self"},
	})
	for imgPaginator.HasMorePages() {
		page, err := imgPaginator.NextPage(ctx)
		if err != nil {
			return inv, fmt.Errorf("DescribeImages: %w", err)
		}
		inv.images = append(inv.images, page.Images...)
	}

	// Count how many current self-owned AMIs reference each backing snapshot.
	for i := range inv.images {
		for _, snapshotID := range imageSnapshotIDs(&inv.images[i]) {
			inv.snapshotRefCounts[snapshotID]++
		}
	}

	// Self-owned snapshots: the backing-storage inventory. Failure is
	// non-fatal because backing_complete=false lets the rule skip honestly.
	snapPaginator := ec2.NewDescribeSnapshotsPaginator(client, &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"},
	})
	for snapPaginator.HasMorePages() {
		page, err := snapPaginator.NextPage(ctx)
		if err != nil {
			p.log.Warn("aws: DescribeSnapshots failed; AMI backing sizes will be incomplete",
				"err", err)
			inv.snapshotsComplete = false
			inv.snapshots = nil
			inv.snapshotByID = map[string]ec2types.Snapshot{}
			break
		}
		inv.snapshots = append(inv.snapshots, page.Snapshots...)
	}
	for _, snap := range inv.snapshots {
		if snap.SnapshotId != nil {
			inv.snapshotByID[*snap.SnapshotId] = snap
		}
	}

	if !enumerateReferences {
		return inv, nil
	}

	// Running/current instances are already in hand from the core hydration
	// path. Every instance returned by DescribeInstances counts as a
	// reference, regardless of state: a stopped instance still needs its AMI
	// to start again.
	for i := range instances {
		inst := &instances[i]
		if inst.ImageId == nil || *inst.ImageId == "" {
			continue
		}
		source := "instance:" + stringValue(inst.InstanceId)
		inv.references.add(*inst.ImageId, source)
	}

	// Launch templates, all versions.
	if err := p.collectLaunchTemplateReferences(ctx, client, &inv.references); err != nil {
		p.log.Warn("aws: launch template reference enumeration failed; unused_ami will skip",
			"err", err)
		inv.references.complete = false
	}

	// Launch configurations.
	if asgClient == nil {
		inv.references.complete = false
		p.log.Warn("aws: Auto Scaling client unavailable; launch configuration references could not be enumerated")
	} else if err := p.collectLaunchConfigurationReferences(ctx, asgClient, &inv.references); err != nil {
		p.log.Warn("aws: launch configuration reference enumeration failed; unused_ami will skip",
			"err", err)
		inv.references.complete = false
	}

	// EC2 Fleets: inline overrides. Launch-template configs are already
	// covered by the all-versions launch-template pass.
	if err := p.collectFleetReferences(ctx, client, &inv.references); err != nil {
		p.log.Warn("aws: EC2 Fleet reference enumeration failed; unused_ami will skip",
			"err", err)
		inv.references.complete = false
	}

	// Spot Fleet requests: inline launch specifications. Launch-template
	// configs are covered by the all-versions launch-template pass.
	if err := p.collectSpotFleetReferences(ctx, client, &inv.references); err != nil {
		p.log.Warn("aws: Spot Fleet reference enumeration failed; unused_ami will skip",
			"err", err)
		inv.references.complete = false
	}

	return inv, nil
}

// collectLaunchTemplateReferences adds the ImageId from every version of every
// launch template.
func (p *Provider) collectLaunchTemplateReferences(ctx context.Context, client ec2API, refs *imageReferenceSet) error {
	ltPaginator := ec2.NewDescribeLaunchTemplatesPaginator(client, &ec2.DescribeLaunchTemplatesInput{})
	for ltPaginator.HasMorePages() {
		page, err := ltPaginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, lt := range page.LaunchTemplates {
			if lt.LaunchTemplateId == nil || *lt.LaunchTemplateId == "" {
				continue
			}
			versionPaginator := ec2.NewDescribeLaunchTemplateVersionsPaginator(client, &ec2.DescribeLaunchTemplateVersionsInput{
				LaunchTemplateId: lt.LaunchTemplateId,
			})
			for versionPaginator.HasMorePages() {
				versionPage, err := versionPaginator.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, v := range versionPage.LaunchTemplateVersions {
					if v.LaunchTemplateData == nil || v.LaunchTemplateData.ImageId == nil || *v.LaunchTemplateData.ImageId == "" {
						continue
					}
					version := ""
					if v.VersionNumber != nil {
						version = fmt.Sprintf(":%d", *v.VersionNumber)
					}
					source := "launch_template:" + *lt.LaunchTemplateId + version
					refs.add(*v.LaunchTemplateData.ImageId, source)
				}
			}
		}
	}
	return nil
}

// collectLaunchConfigurationReferences adds the ImageId from every Auto
// Scaling launch configuration.
func (p *Provider) collectLaunchConfigurationReferences(ctx context.Context, client autoScalingAPI, refs *imageReferenceSet) error {
	paginator := autoscaling.NewDescribeLaunchConfigurationsPaginator(client, &autoscaling.DescribeLaunchConfigurationsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, lc := range page.LaunchConfigurations {
			if lc.ImageId == nil || *lc.ImageId == "" {
				continue
			}
			name := ""
			if lc.LaunchConfigurationName != nil {
				name = *lc.LaunchConfigurationName
			}
			refs.add(*lc.ImageId, "launch_configuration:"+name)
		}
	}
	return nil
}

// collectFleetReferences adds inline ImageId overrides from EC2 Fleets.
// Launch-template references inside a fleet are intentionally NOT re-added
// here; they are already covered by the all-versions launch-template pass.
func (p *Provider) collectFleetReferences(ctx context.Context, client ec2API, refs *imageReferenceSet) error {
	paginator := ec2.NewDescribeFleetsPaginator(client, &ec2.DescribeFleetsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, fleet := range page.Fleets {
			for _, cfg := range fleet.LaunchTemplateConfigs {
				for _, override := range cfg.Overrides {
					if override.ImageId == nil || *override.ImageId == "" {
						continue
					}
					refs.add(*override.ImageId, "ec2_fleet:"+stringValue(fleet.FleetId))
				}
			}
		}
	}
	return nil
}

// collectSpotFleetReferences adds inline ImageId launch specifications from
// Spot Fleet requests. Launch-template references inside a Spot Fleet request
// are already covered by the all-versions launch-template pass.
func (p *Provider) collectSpotFleetReferences(ctx context.Context, client ec2API, refs *imageReferenceSet) error {
	paginator := ec2.NewDescribeSpotFleetRequestsPaginator(client, &ec2.DescribeSpotFleetRequestsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, req := range page.SpotFleetRequestConfigs {
			if req.SpotFleetRequestConfig == nil {
				continue
			}
			for _, spec := range req.SpotFleetRequestConfig.LaunchSpecifications {
				if spec.ImageId == nil || *spec.ImageId == "" {
					continue
				}
				refs.add(*spec.ImageId, "spot_fleet:"+stringValue(req.SpotFleetRequestId))
			}
		}
	}
	return nil
}

// regionalRegion resolves the region a regional AWS client should be pinned
// to. The explicit region wins; when it is empty, the config's region is
// used; when that is empty too, the call falls back to us-east-1. The
// important rule is that an SDK client must never be constructed with an
// empty o.Region: doing so overrides the config and fails on the first API
// call with "Invalid Configuration: Missing Region".
func regionalRegion(region, fallback string) string {
	if region != "" {
		return region
	}
	if fallback != "" {
		return fallback
	}
	return "us-east-1"
}

// newEC2Client builds a live, region-scoped EC2 client from cfg. It is the
// single live-construction path shared by the caller's own account and the
// assumed-role member-account factories; the offline path is handled by the
// Provider.ec2Client wrapper, not here.
func newEC2Client(cfg aws.Config, region string) ec2API {
	return ec2.NewFromConfig(cfg, func(o *ec2.Options) {
		o.Region = regionalRegion(region, cfg.Region)
	})
}

// newAutoScalingClient builds a live, region-scoped Auto Scaling client from
// cfg. It is the single live-construction path shared by the caller's own
// account and the assumed-role member-account factories; the offline path is
// handled by the Provider.autoScalingClient wrapper, not here.
func newAutoScalingClient(cfg aws.Config, region string) autoScalingAPI {
	return autoscaling.NewFromConfig(cfg, func(o *autoscaling.Options) {
		o.Region = regionalRegion(region, cfg.Region)
	})
}

// ec2Client returns a region-scoped EC2 API. Offline providers return a
// fixture-backed fake; live providers return the SDK client built by the
// shared live-construction helper.
func (p *Provider) ec2Client(region string) ec2API {
	if p.offline {
		return &fakeEC2{region: region, f: p.fixture}
	}
	return newEC2Client(p.awsCfg, region)
}

// autoScalingClient returns a region-scoped Auto Scaling API. Offline
// providers return a fixture-backed fake; live providers return the SDK
// client built by the shared live-construction helper. The client is only
// built and called when an image/snapshot asset type hint is present.
func (p *Provider) autoScalingClient(region string) autoScalingAPI {
	if p.offline {
		return &fakeAutoScaling{region: region, f: p.fixture}
	}
	return newAutoScalingClient(p.awsCfg, region)
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
