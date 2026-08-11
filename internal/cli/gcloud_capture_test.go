package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/TypeOneLabs/tellury/internal/config"
)

// TestGcloudCaptureLoadsUnedited guards the `gcloud asset search-all-resources`
// recipe documented in the README. testdata/gcloud-capture.json is real,
// unedited output from that command: a bare JSON array of ResourceSearchResult
// objects, carrying the `versionedResources` payload that `--read-mask='*'`
// includes.
//
// The README promises the capture feeds `--fixture` "without any hand-editing".
// Nothing verified that promise — the file sat in testdata unread — and a
// documented-but-unexercised example is exactly how a broken `graph export`
// recipe shipped once before. If the fixture reader stops folding
// `versionedResources` onto RawAsset, this fails instead of the README quietly
// going stale.
func TestGcloudCaptureLoadsUnedited(t *testing.T) {
	gcpPriceFixtureEnv(t)

	cfg := config.Scan{
		Provider: "gcp",
		Project:  "my-project",
		Fixture:  []string{"testdata/gcloud-capture.json"},
		Format:   "table",
		OutDir:   filepath.Join(t.TempDir(), "out"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	var out, errOut strings.Builder
	g := &globalFlags{LogLevel: "warn"}
	if err := runScan(t.Context(), &out, &errOut, g, cfg, artifactNow); err != nil {
		t.Fatalf("runScan over the gcloud capture: %v", err)
	}

	// The capture holds one detached disk. Asserting the finding — not merely
	// that parsing returned no error — is what proves the versioned resource
	// payload survived: an empty-shell node parses fine and finds nothing,
	// which is the precise failure mode the README's --read-mask note warns of.
	got := out.String()
	if !strings.Contains(got, "detached_disk") {
		t.Errorf("gcloud capture produced no detached_disk finding; the versioned\n"+
			"resource payload was likely dropped. Output:\n%s", got)
	}
	if !strings.Contains(got, "pd-standard-01") {
		t.Errorf("gcloud capture did not name the disk pd-standard-01; output:\n%s", got)
	}
}
