package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// FakeLister replays assets from JSON fixtures. Shipped (not _test.go) so rule
// authors can build fixtures for their own rules, and so `tellury scan
// --fixture` reproduces a scan with no credentials.
type FakeLister struct {
	Assets []*RawAsset
}

var _ AssetLister = (*FakeLister)(nil)

// LoadFakeLister reads one or more fixture files, each shaped
// {"assets":[ <CAI asset>, ... ]}.
func LoadFakeLister(paths ...string) (*FakeLister, error) {
	f := &FakeLister{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("gcp: read fixture %s: %w", p, err)
		}
		var wrap struct {
			Assets []*RawAsset `json:"assets"`
		}
		if err := json.Unmarshal(b, &wrap); err != nil {
			return nil, fmt.Errorf("gcp: decode fixture %s: %w", p, err)
		}
		f.Assets = append(f.Assets, wrap.Assets...)
	}
	return f, nil
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
