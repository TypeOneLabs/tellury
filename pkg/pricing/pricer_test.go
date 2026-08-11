package pricing

import "testing"

// TestCanonicalRegion pins the single "what place is this" canonicaliser
// across every shape it must tell apart. The three zone/region shapes are
// distinguished by WHAT the last dash-separated segment is, never by how long
// it is: a GCP zone ends in a single LETTER, an AWS region ends in DIGITS, and
// an AWS availability zone ends in DIGITS FOLLOWED BY ONE LETTER. Multi-region
// and global locations are single tokens that pass through lowercased.
//
// The two AWS rows are the regression guard for the length-based defect:
// RegionOf("us-east-1") -> "us-east" (a region AWS never had) and
// RegionOf("us-east-1a") -> "us-east-1a" (a zone never flattened). Restoring
// the old `len(parts) >= 3 && len(last) == 1` heuristic makes exactly those
// two rows fail.
func TestCanonicalRegion(t *testing.T) {
	tests := []struct {
		name     string
		location string
		want     string
	}{
		// GCP: a zone's last segment is a single letter; dropping it is right.
		{"gcp zone", "us-central1-a", "us-central1"},
		{"gcp zone another region", "europe-west4-a", "europe-west4"},
		// GCP: a region passes through, lowercased — never mistaken for a zone.
		{"gcp region", "us-central1", "us-central1"},
		{"gcp region uppercase input", "EUROPE-WEST4", "europe-west4"},
		// AWS: a numeric suffix is part of the region name and must be kept.
		{"aws region", "us-east-1", "us-east-1"},
		{"aws region another", "us-west-2", "us-west-2"},
		// AWS: an availability zone is digits then one letter; flatten it.
		{"aws availability zone", "us-east-1a", "us-east-1"},
		{"aws availability zone another", "eu-central-1a", "eu-central-1"},
		// Multi-region and global single tokens pass through lowercased.
		{"multi-region US", "US", "us"},
		{"multi-region EU", "EU", "eu"},
		{"global", "global", "global"},
		// Empty stays empty; the caller decides its empty default.
		{"empty", "", ""},
		// Whitespace is trimmed, then lowercased.
		{"whitespace trimmed", "  US  ", "us"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanonicalRegion(tt.location); got != tt.want {
				t.Errorf("CanonicalRegion(%q) = %q, want %q", tt.location, got, tt.want)
			}
		})
	}
}

// TestRegionOf_EmptyMapsToPricingDefault pins the thin wrapper's one job:
// RegionOf is CanonicalRegion except for the empty input, which pricing keys
// as "default" (the regionless/global key every price table indexes). This is
// the whole difference between the two callers; the shared canonicaliser never
// sees it.
func TestRegionOf_EmptyMapsToPricingDefault(t *testing.T) {
	if got := RegionOf(""); got != "default" {
		t.Errorf("RegionOf(\"\") = %q, want %q (pricing's regionless key)", got, "default")
	}
	// Non-empty locations delegate to the canonicaliser unchanged.
	if got := RegionOf("us-central1-a"); got != "us-central1" {
		t.Errorf("RegionOf(\"us-central1-a\") = %q, want %q", got, "us-central1")
	}
	if got := RegionOf("us-east-1"); got != "us-east-1" {
		t.Errorf("RegionOf(\"us-east-1\") = %q, want %q (AWS region must not be flattened)", got, "us-east-1")
	}
}
