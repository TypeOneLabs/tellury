package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// FakeLister replays assets from JSON fixtures. Shipped (not _test.go) so rule
// authors can build fixtures for their own rules, and so `tellury scan
// --fixture` reproduces a scan with no credentials.
type FakeLister struct {
	Assets []*RawAsset
}

var _ AssetLister = (*FakeLister)(nil)

// LoadFakeLister reads one or more fixture files. Each file may be one of two
// shapes, both accepted without hand-editing:
//
//  1. The canonical envelope used by the shipped tests:
//     {"assets":[ <CAI asset>, ... ]} where each asset is a RawAsset —
//     name / assetType / updateTime / project / folders / organization plus
//     resource.{version,parent,location,data}. This is what a scan cache
//     that was re-exposed by the export path and the README fixtures use.
//
//  2. A bare JSON array of resources, exactly what
//     `gcloud asset search-all-resources --format=json[,...]` prints. Each
//     element is a ResourceSearchResult — name / assetType / project /
//     folders / organization / location / parentFullResourceName plus a
//     versionedResources[].{version,resource} payload. foldSearchResult maps
//     that onto the RawAsset shape, mirroring (toRawSearchResult) so the
//     offline path normalizes a captured inventory the same way the live
//     SearchAllResources RPC does.
//
// This dual acceptance is the round-trip guarantee behind the documented
// fixture recipe: an operator captures inventory with a plain gcloud command
// and feeds it straight to --fixture with no transformation.
func LoadFakeLister(paths ...string) (*FakeLister, error) {
	f := &FakeLister{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("gcp: read fixture %s: %w", p, err)
		}
		items, err := decodeFixtureAssets(b)
		if err != nil {
			return nil, fmt.Errorf("gcp: fixture %s: %w", p, err)
		}
		for _, item := range items {
			a, err := decodeFixtureAsset(item)
			if err != nil {
				return nil, fmt.Errorf("gcp: fixture %s: %w", p, err)
			}
			if a != nil {
				f.Assets = append(f.Assets, a)
			}
		}
	}
	return f, nil
}

// decodeFixtureAssets returns the per-asset entries of a fixture blob,
// accepting either the canonical {"assets": [...]} wrapper or a bare top-level
// JSON array (the shape `gcloud asset search-all-resources --format=json`
// prints).
func decodeFixtureAssets(b []byte) ([]json.RawMessage, error) {
	var wrap struct {
		Assets []json.RawMessage `json:"assets"`
	}
	if err := json.Unmarshal(b, &wrap); err == nil && wrap.Assets != nil {
		return wrap.Assets, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, errors.New(
			"expected {\"assets\":[...]} or a bare JSON array of assets " +
				"(the shape `gcloud asset search-all-resources --format=json` emits)")
	}
	return arr, nil
}

// decodeFixtureAsset reads one asset entry, dispatching on its shape. An entry
// whose top-level object has a versionedResources key is a captured
// ResourceSearchResult and is folded onto RawAsset; anything else is decoded
// directly as a RawAsset.
func decodeFixtureAsset(item json.RawMessage) (*RawAsset, error) {
	if len(item) == 0 {
		return nil, nil
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(item, &keys); err != nil {
		return nil, fmt.Errorf("invalid asset object: %w", err)
	}
	if _, ok := keys["versionedResources"]; ok {
		return foldSearchResult(item)
	}
	var a RawAsset
	if err := json.Unmarshal(item, &a); err != nil {
		return nil, fmt.Errorf("invalid asset (want a RawAsset or a SearchAllResources result): %w", err)
	}
	return &a, nil
}

// foldSearchResult projects a captured SearchAllResources result — as printed
// by `gcloud asset search-all-resources --format=json` — onto the RawAsset
// shape every downstream consumer (Normalize, Link, hierarchy, fixtures)
// speaks. It is the offline mirror of toRawSearchResult, so a captured
// inventory normalizes identically on the --fixture path and the live path.
//
// The full provider payload is unwrapped from the FIRST non-empty
// versionedResources[].resource (the JSON representation of the GA resource),
// and the envelope location/parent come from the search result's own
// location / parentFullResourceName fields. The hierarchy fields are copied
// verbatim, and a bare organization number is qualified like the live path.
func foldSearchResult(item json.RawMessage) (*RawAsset, error) {
	type versionedResource struct {
		Version  string          `json:"version"`
		Resource json.RawMessage `json:"resource"`
	}
	var sr struct {
		Name         string              `json:"name"`
		AssetType    string              `json:"assetType"`
		Project      string              `json:"project"`
		Folders      []string            `json:"folders"`
		Organization string              `json:"organization"`
		UpdateTime   time.Time           `json:"updateTime"`
		Location     string              `json:"location"`
		Parent       string              `json:"parentFullResourceName"`
		Versioned    []versionedResource `json:"versionedResources"`
	}
	if err := json.Unmarshal(item, &sr); err != nil {
		return nil, err
	}

	out := &RawAsset{
		Name:         sr.Name,
		AssetType:    sr.AssetType,
		Project:      sr.Project,
		Folders:      sr.Folders,
		Organization: orgToken(sr.Organization),
		UpdateTime:   sr.UpdateTime,
		Resource: &RawResource{
			Version:  "",
			Parent:   sr.Parent,
			Location: sr.Location,
		},
	}
	for _, v := range sr.Versioned {
		if v.Version == "" || len(v.Resource) == 0 {
			continue
		}
		out.Resource.Version = v.Version
		out.Resource.Data = v.Resource
		break
	}
	return out, nil
}

// ListAssets implements AssetLister over the fixture set, honouring the
// server-side asset-type filter so tests exercise the same code path as prod.
func (f *FakeLister) ListAssets(_ context.Context, req ListRequest, visit func(*RawAsset) error) error {
	for _, a := range f.Assets {
		if len(req.AssetTypes) > 0 && !containsStr(req.AssetTypes, a.AssetType) {
			continue
		}
		if err := visit(a); err != nil {
			return err
		}
	}
	return nil
}

// Close implements AssetLister.
func (f *FakeLister) Close() error { return nil }
