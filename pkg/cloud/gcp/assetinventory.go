package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	asset "cloud.google.com/go/asset/apiv1"
	assetpb "cloud.google.com/go/asset/apiv1/assetpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// ─────────────────────────────────────────────────────────────────────────────
// Ingestion contract
// ─────────────────────────────────────────────────────────────────────────────

// RawAsset is our minimal projection of a Cloud Asset Inventory resource as it
// is consumed after a SearchAllResources call (and, for the shared offline
// path, the exact JSON shape parseable from a --fixture file). Keeping the
// projection local (rather than exposing assetpb) is what lets every ingestion
// test run from a JSON fixture with no SDK, no credentials and no network.
//
// The shape mirrors the ListAssets envelope (name / assetType / updateTime /
// resource.{version,parent,location,data}) because that is the shape every
// downstream consumer (Normalize, Link, fixtures) already speaks. For
// SearchAllResources the resource payload arrives in a different envelope
// (VersionedResources[]), which toRawSearchResult folds onto this same shape.
//
// The resource-hierarchy fields (Project, Folders, Organization) are carried
// as their own top-level fields because SearchAllResources surfaces them on
// the result envelope (ResourceSearchResult.Project/Folders/Organization),
// not inside the versioned resource payload. They are what the ingestion
// pass uses to build the hierarchy container nodes (project / folder /
// organization) without any extra Cloud Resource Manager call.
type RawAsset struct {
	Name       string    `json:"name"`
	AssetType  string    `json:"assetType"`
	UpdateTime time.Time `json:"updateTime"`
	// Project is the project this resource belongs to, "projects/<N>".
	Project string `json:"project"`
	// Folders are the folders (immediate and ancestors) that contain this
	// resource, each "folders/<N>".
	Folders []string `json:"folders"`
	// Organization is the organization this resource belongs to,
	// "organizations/<N>".
	Organization string       `json:"organization"`
	Resource     *RawResource `json:"resource"`
}

// RawResource is the resource envelope of an asset: the version, the owning
// parent, the authoritative location, and the full provider payload.
type RawResource struct {
	Version  string `json:"version"`
	Parent   string `json:"parent"`
	Location string `json:"location"`
	// Data is the provider payload: the JSON representation of the resource
	// (the same content the GA REST API returns for a resource of this type).
	Data json.RawMessage `json:"data"`
}

// Data returns the resource payload, or nil.
func (a *RawAsset) Data() json.RawMessage {
	if a == nil || a.Resource == nil {
		return nil
	}
	return a.Resource.Data
}

// Location returns the authoritative envelope location.
func (a *RawAsset) Location() string {
	if a == nil || a.Resource == nil {
		return ""
	}
	return a.Resource.Location
}

// Parent returns the envelope parent, e.g.
// "//cloudresourcemanager.googleapis.com/projects/123".
func (a *RawAsset) Parent() string {
	if a == nil || a.Resource == nil {
		return ""
	}
	return a.Resource.Parent
}

// ListRequest is our minimal projection of assetpb.ListAssetsRequest. It is
// the request shape the AssetLister contract exposes; the live client maps it
// onto a SearchAllResources RPC (see CAIClient.ListAssets).
type ListRequest struct {
	// Parent is "projects/<id>", "folders/<n>" or "organizations/<n>".
	Parent string
	// AssetTypes filters server-side. Empty = all supported types.
	AssetTypes []string
	// PageSize caps items per RPC. ListAssets accepted up to 1000;
	// SearchAllResources caps the server side at 500 regardless of what is
	// sent here. We default to 1000 and let the server clamp.
	PageSize int32
}

// AssetLister streams Cloud Asset Inventory resources.
//
// Contract:
//   - Implementations MUST invoke visit once per resource, in API order.
//   - A non-nil error from visit aborts the stream and is returned verbatim.
//   - Pagination is the implementation's responsibility; callers never page.
//   - The stream carries the FULL resource payload (status, addressType,
//     sizeGb, lifecycle, etc.), never the bare search projection, so no
//     per-resource GET fan-out is ever required.
type AssetLister interface {
	ListAssets(ctx context.Context, req ListRequest, visit func(*RawAsset) error) error
	Close() error
}

// ErrNotImplemented marks a seam that is defined but not yet wired to the
// Google Cloud SDK. It is kept as a distinct sentinel (rather than deleted
// now that the live client is wired) because it is still the correct error to
// wrap for any lister implementation that legitimately has no transport
// (there are none left in this build, but the seam remains part of the
// AssetLister contract for future backends, e.g. a REST-only fallback).
var ErrNotImplemented = errors.New("gcp: live API client not wired in this build")

// CAIClient is the production AssetLister, backed by the official Cloud Asset
// Inventory SDK (cloud.google.com/go/asset/apiv1). It authenticates via
// Application Default Credentials — the SDK resolves ADC itself from
// GOOGLE_APPLICATION_CREDENTIALS, the gcloud user/adc file, or the compute/
// GKE/Cloud Run metadata server, in that order. tellury never reads a key file
// or accepts a credentials flag; that is deliberate (operators already have
// ADC configured, and a hand-rolled credentials path is one more way to leak
// a key).
//
// The ingestion source is the asset-inventory module's SearchAllResources RPC,
// NOT ListAssets. SearchAllResources is the live, realtime view of a scope;
// ListAssets is fronted by an eventually-consistent snapshot that can be hours
// stale and can elide resources. For a tool whose job is pricing waste it is
// only acceptable to price what is true right now, so SearchAllResources is the
// correct source. A read mask of "*" is sent so the result carries the
// versioned resource payload (the same full JSON the GA REST API returns):
// without it a bare search result omits status/addressType/sizeGb/lifecycle
// and every normalizer would silently skip.
//
// Required IAM on the scope (project/folder/organization):
// roles/cloudasset.viewer (or an equivalent custom role granting
// cloudasset.assets.searchAllResources).
type CAIClient struct {
	log    *slog.Logger
	client *asset.Client
}

var _ AssetLister = (*CAIClient)(nil)

// NewCAIClient builds a client authenticated with Application Default
// Credentials. It performs no RPCs itself: asset.NewClient dials lazily, so
// construction only fails when ADC cannot be resolved at all (e.g. no
// credentials anywhere on the host).
func NewCAIClient(ctx context.Context, log *slog.Logger) (*CAIClient, error) {
	if log == nil {
		log = slog.Default()
	}
	c, err := asset.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: create Cloud Asset Inventory client (check Application Default Credentials): %w", err)
	}
	return &CAIClient{log: log, client: c}, nil
}

// Close releases the underlying client's connection pool.
func (c *CAIClient) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// ListAssets streams resources for req by issuing a SearchAllResources RPC and
// walking the SDK's paginated iterator, honouring the server-side AssetTypes
// filter and the caller's context deadline (the CLI's --timeout ends up here
// via context.WithTimeout; the iterator's underlying RPCs all inherit ctx, so a
// page fetched after the deadline fires returns context.DeadlineExceeded
// rather than hanging).
//
// SearchAllResources is the correctness fix over ListAssets: the latter fronts
// an eventually-consistent snapshot that can lag a live resource by hours (a
// reserved address was reported as RESERVING with a creation-time updateTime
// while SearchAllResources already reported RESERVED) and can return a
// strict subset of a scope's resources. Requesting the full payload
// (ReadMask="*") is mandatory: a bare search result drops the versioned
// resource fields the normalizers read.
func (c *CAIClient) ListAssets(ctx context.Context, req ListRequest, visit func(*RawAsset) error) error {
	if req.Parent == "" {
		return errors.New("gcp: ListRequest.Parent is required")
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 1000
	}

	it := c.client.SearchAllResources(ctx, &assetpb.SearchAllResourcesRequest{
		Scope:      req.Parent,
		AssetTypes: req.AssetTypes,
		PageSize:   pageSize,
		// A bare search result returns only a curated projection and drops the
		// versioned resource payload (status, addressType, sizeGb, lifecycle,
		// machineType, ...). The normalizers read those fields, so the full
		// payload must be requested explicitly.
		ReadMask: &fieldmaskpb.FieldMask{Paths: []string{"*"}},
	})

	n := 0
	for {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return mapListAssetsError(req.Parent, err)
		}
		n++
		if err := visit(toRawSearchResult(r)); err != nil {
			return err
		}
	}
	c.log.Debug("cloud asset inventory search complete", "parent", req.Parent, "results", n)
	return nil
}

// toRawSearchResult projects a SearchAllResources result onto the
// provider-neutral RawAsset shape every downstream consumer (Normalize, Link,
// fixtures) already speaks.
//
// SearchAllResources surfaces the resource differently from ListAssets: the
// full provider payload lives in VersionedResources[].Resource (a
// structpb.Struct), not in a resource.data field. This function unwraps the
// first non-empty version and marshals that struct to JSON so the normalizers
// see exactly the same Data they did from ListAssets — the raw GA REST
// representation of the resource. With ReadMask="*" at least one versioned
// resource is always populated for the asset types tellury models.
//
// The envelope parent is taken from ParentFullResourceName, which for a GCS
// bucket is "//cloudresourcemanager.googleapis.com/projects/<n>" — the same
// value the old ListAssets resource.parent carried, so the bucket project
// fallback keeps working.
//
// The resource-hierarchy fields are copied verbatim from the result envelope:
// SearchAllResources.Reproject/Folders/Organization are exactly the
// "projects/<N>", "folders/<N>", "organizations/<N>" tokens the hierarchy
// builder consumes. They are populated from the search result only; no extra
// Cloud Resource Manager call is ever made.
func toRawSearchResult(r *assetpb.ResourceSearchResult) *RawAsset {
	if r == nil {
		return nil
	}
	out := &RawAsset{
		Name:      r.GetName(),
		AssetType: r.GetAssetType(),
		Project:   r.GetProject(),
		Folders:   r.GetFolders(),
		// The organization token is already qualified ("organizations/<N>")
		// by the API; keep it verbatim so the hierarchy builder can use a
		// single spelling without re-deriving the prefix.
		Organization: orgToken(r.GetOrganization()),
	}
	if ut := r.GetUpdateTime(); ut != nil {
		out.UpdateTime = ut.AsTime()
	}

	rr := &RawResource{
		Location: r.GetLocation(),
		Parent:   r.GetParentFullResourceName(),
	}
	for _, vr := range r.GetVersionedResources() {
		if vr == nil || vr.Resource == nil || vr.GetVersion() == "" {
			continue
		}
		// A versioned resource's Resource is the JSON payload for that API
		// version. Take the first populated one — for the types tellury models
		// there is exactly one active version, and any of them carries the
		// fields the normalizers read.
		rr.Version = vr.GetVersion()
		if b, err := vr.Resource.MarshalJSON(); err == nil {
			rr.Data = json.RawMessage(b)
		}
		break
	}
	out.Resource = rr
	return out
}

// orgToken normalizes the SearchAllResources organization token. The API may
// return either "organizations/<N>" or a bare "<N>"; the hierarchy builder
// consumes "organizations/<N>", so a bare number is qualified here. A token
// that is already qualified, or an empty string, is returned unchanged.
func orgToken(s string) string {
	if s == "" || hasPrefix(s, "organizations/") || hasPrefix(s, "folders/") || hasPrefix(s, "projects/") {
		return s
	}
	return "organizations/" + s
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// mapListAssetsError turns a raw gRPC status from the SearchAllResources RPC
// into a message an operator can act on without knowing what a gRPC status
// code is. The most common live-path failure is a missing IAM binding, which
// arrives as codes.PermissionDenied with no indication in the plain error
// string of which role to grant — so we say it explicitly.
//
// The cancellation family is deliberately split: a `codes.Canceled` status
// means the CALLER (or the operator) canceled the context, whereas
// `codes.DeadlineExceeded` means the caller's deadline actually expired. The
// two wrap distinct sentinels (context.Canceled vs context.DeadlineExceeded)
// so an operator who cancels a scan is told the scan was canceled, not that
// the deadline expired — distingushable via errors.Is.
func mapListAssetsError(parent string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("gcp: search resources for %s: %w", parent, err)
	}
	switch st.Code() {
	case codes.PermissionDenied:
		return fmt.Errorf(
			"gcp: permission denied searching resources for %s: grant roles/cloudasset.viewer "+
				"(or cloudasset.assets.searchAllResources) on this scope to the identity behind your "+
				"Application Default Credentials: %s", parent, st.Message())
	case codes.NotFound:
		return fmt.Errorf("gcp: scope %s not found (check --project/--folder/--organization): %s", parent, st.Message())
	case codes.InvalidArgument:
		return fmt.Errorf("gcp: invalid request for scope %s: %s", parent, st.Message())
	case codes.DeadlineExceeded:
		return fmt.Errorf("gcp: search resources for %s: %w", parent, context.DeadlineExceeded)
	case codes.Canceled:
		return fmt.Errorf("gcp: search resources for %s canceled: %w", parent, context.Canceled)
	case codes.ResourceExhausted:
		return fmt.Errorf("gcp: Cloud Asset Inventory quota exceeded for %s; retry later or request a quota increase: %s", parent, st.Message())
	case codes.Unauthenticated:
		return fmt.Errorf(
			"gcp: unauthenticated searching resources for %s: no valid Application Default Credentials found "+
				"(run `gcloud auth application-default login` or set GOOGLE_APPLICATION_CREDENTIALS): %s",
			parent, st.Message())
	default:
		return fmt.Errorf("gcp: search resources for %s: %s: %s", parent, st.Code(), st.Message())
	}
}
