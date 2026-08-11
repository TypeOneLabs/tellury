package aws

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2/types"
)

// ---------------------------------------------------------------------------
// Fake implementing resourceExplorerAPI, driven from testdata or by hand
// ---------------------------------------------------------------------------

// fakeResourceExplorer implements resourceExplorerAPI. It serves responses
// from a recorded JSON fixture of the fields Discover actually reads (Region
// and ResourceType), or returns hand-crafted pages for edge-case tests
// (pagination, error injection). Zero network calls.
type fakeResourceExplorer struct {
	pages   []*resourceexplorer2.SearchOutput
	pageIdx int
	err     error // non-nil to inject an error on the next call
}

func (f *fakeResourceExplorer) Search(_ context.Context, _ *resourceexplorer2.SearchInput, _ ...func(*resourceexplorer2.Options)) (*resourceexplorer2.SearchOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.pageIdx >= len(f.pages) {
		return &resourceexplorer2.SearchOutput{}, nil
	}
	page := f.pages[f.pageIdx]
	f.pageIdx++
	return page, nil
}

// searchRecord is the on-disk shape of a recorded Resource Explorer Search
// result row: the fields Discover reads. The fixture stores these, not the
// full SDK types, because document.Interface (ResourceProperty.Data) does not
// survive a JSON round-trip.
type searchRecord struct {
	Region       string `json:"Region"`
	ResourceType string `json:"ResourceType"`
}

// loadSearchFixture reads the recorded Search response from testdata and
// returns a single-page fake. The fixture was captured from a real Resource
// Explorer aggregator index search covering us-east-1 and eu-west-1 for
// AWS::EC2::Volume and AWS::EC2::EIP. The ResourceType strings come straight
// from the ResourceType field of real Search responses.
func loadSearchFixture(t *testing.T) *fakeResourceExplorer {
	t.Helper()
	path := filepath.Join("testdata", "resource-explorer-search.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var records []searchRecord
	if err := json.Unmarshal(b, &records); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	resources := make([]types.Resource, 0, len(records))
	for _, r := range records {
		region := r.Region
		rt := r.ResourceType
		resources = append(resources, types.Resource{
			Region:       &region,
			ResourceType: &rt,
		})
	}
	return &fakeResourceExplorer{pages: []*resourceexplorer2.SearchOutput{
		{Resources: resources},
	}}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestDiscover_MapsRegionsToTypes queries the recorded fixture and asserts
// the result shape: region -> de-duplicated resource types.
func TestDiscover_MapsRegionsToTypes(t *testing.T) {
	d := newDiscovererWithClient(loadSearchFixture(t))
	got, err := d.Discover(context.Background(), []string{"AWS::EC2::Volume", "AWS::EC2::EIP"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d regions, want 2 (us-east-1, eu-west-1)", len(got))
	}

	us, ok := got["us-east-1"]
	if !ok {
		t.Fatalf("us-east-1 missing from result: %v", regionSet(got))
	}
	if !containsAll(us, []string{"AWS::EC2::Volume", "AWS::EC2::EIP"}) {
		t.Errorf("us-east-1 types = %v, want both Volume and EIP", us)
	}

	eu, ok := got["eu-west-1"]
	if !ok {
		t.Fatalf("eu-west-1 missing from result: %v", regionSet(got))
	}
	if !containsAll(eu, []string{"AWS::EC2::Volume", "AWS::EC2::EIP"}) {
		t.Errorf("eu-west-1 types = %v, want both Volume and EIP", eu)
	}
}

// TestDiscover_EmptyInput returns an empty map, not an error and not a call
// to the API.
func TestDiscover_EmptyInput(t *testing.T) {
	f := &fakeResourceExplorer{err: errors.New("should not have been called")}
	d := newDiscovererWithClient(f)
	got, err := d.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover(empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("result = %v, want empty map", got)
	}
}

// TestDiscover_EmptyInputSlice is the same as empty input but with an
// allocated zero-length slice instead of nil.
func TestDiscover_EmptyInputSlice(t *testing.T) {
	f := &fakeResourceExplorer{err: errors.New("should not have been called")}
	d := newDiscovererWithClient(f)
	got, err := d.Discover(context.Background(), []string{})
	if err != nil {
		t.Fatalf("Discover([]): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("result = %v, want empty map", got)
	}
}

// TestDiscover_ErrNoIndex wraps ResourceNotFoundException as ErrNoIndex so
// callers can distinguish "no index" from a transient network error with
// errors.Is.
func TestDiscover_ErrNoIndex(t *testing.T) {
	f := &fakeResourceExplorer{
		err: &types.ResourceNotFoundException{Message: aws.String("Index not found")},
	}
	d := newDiscovererWithClient(f)
	_, err := d.Discover(context.Background(), []string{"AWS::EC2::Volume"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNoIndex) {
		t.Errorf("error is not ErrNoIndex: %v", err)
	}
}

// TestDiscover_ErrNoIndexByString: the fallback path in isNoIndexError catches
// the error code in the message even when the error is not the typed
// exception.
func TestDiscover_ErrNoIndexByString(t *testing.T) {
	f := &fakeResourceExplorer{
		err: errors.New("ResourceNotFoundException: no aggregator index"),
	}
	d := newDiscovererWithClient(f)
	_, err := d.Discover(context.Background(), []string{"AWS::EC2::Volume"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNoIndex) {
		t.Errorf("error is not ErrNoIndex: %v", err)
	}
}

// TestDiscover_TransientError is not wrapped as ErrNoIndex. A network
// timeout or access denied is passed through so callers can retry.
func TestDiscover_TransientError(t *testing.T) {
	f := &fakeResourceExplorer{
		err: errors.New("request timeout"),
	}
	d := newDiscovererWithClient(f)
	_, err := d.Discover(context.Background(), []string{"AWS::EC2::Volume"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrNoIndex) {
		t.Errorf("transient error must not be ErrNoIndex: %v", err)
	}
}

// TestDiscover_Paginates proves the client follows NextToken across pages. The
// first page has one resource, the second has another; both end up in the
// result.
func TestDiscover_Paginates(t *testing.T) {
	f := &fakeResourceExplorer{
		pages: []*resourceexplorer2.SearchOutput{
			{
				Resources: []types.Resource{
					{Region: aws.String("us-east-1"), ResourceType: aws.String("AWS::EC2::Volume")},
				},
				NextToken: aws.String("page2"),
			},
			{
				Resources: []types.Resource{
					{Region: aws.String("eu-west-1"), ResourceType: aws.String("AWS::EC2::EIP")},
				},
			},
		},
	}
	d := newDiscovererWithClient(f)
	got, err := d.Discover(context.Background(), []string{"AWS::EC2::Volume", "AWS::EC2::EIP"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d regions, want 2", len(got))
	}
	if !containsAll(got["us-east-1"], []string{"AWS::EC2::Volume"}) {
		t.Errorf("us-east-1 = %v, want [AWS::EC2::Volume]", got["us-east-1"])
	}
	if !containsAll(got["eu-west-1"], []string{"AWS::EC2::EIP"}) {
		t.Errorf("eu-west-1 = %v, want [AWS::EC2::EIP]", got["eu-west-1"])
	}
}

// TestDiscover_SkipsEmptyRegionOrType: rows with no Region or no ResourceType
// are dropped, not accumulated as empty-string map keys.
func TestDiscover_SkipsEmptyRegionOrType(t *testing.T) {
	f := &fakeResourceExplorer{
		pages: []*resourceexplorer2.SearchOutput{
			{
				Resources: []types.Resource{
					{Region: aws.String(""), ResourceType: aws.String("AWS::EC2::Volume")},
					{Region: aws.String("us-east-1"), ResourceType: aws.String("")},
					{Region: aws.String("us-east-1"), ResourceType: aws.String("AWS::EC2::Volume")},
				},
			},
		},
	}
	d := newDiscovererWithClient(f)
	got, err := d.Discover(context.Background(), []string{"AWS::EC2::Volume"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d regions, want 1", len(got))
	}
	if !containsAll(got["us-east-1"], []string{"AWS::EC2::Volume"}) {
		t.Errorf("us-east-1 = %v, want [AWS::EC2::Volume]", got["us-east-1"])
	}
}

// TestDiscover_DeDuplicates: the same resource type appearing twice in one
// region is stored once.
func TestDiscover_DeDuplicates(t *testing.T) {
	f := &fakeResourceExplorer{
		pages: []*resourceexplorer2.SearchOutput{
			{
				Resources: []types.Resource{
					{Region: aws.String("us-east-1"), ResourceType: aws.String("AWS::EC2::Volume")},
					{Region: aws.String("us-east-1"), ResourceType: aws.String("AWS::EC2::Volume")},
				},
			},
		},
	}
	d := newDiscovererWithClient(f)
	got, err := d.Discover(context.Background(), []string{"AWS::EC2::Volume"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got["us-east-1"]) != 1 {
		t.Errorf("us-east-1 types = %v, want [AWS::EC2::Volume] (de-duplicated)", got["us-east-1"])
	}
}

// TestBuildSearchQuery constructs query strings for the Resource Explorer
// search syntax.
func TestBuildSearchQuery(t *testing.T) {
	tests := []struct {
		name  string
		types []string
		want  string
	}{
		{
			name:  "single",
			types: []string{"AWS::EC2::Volume"},
			want:  "resourcetype:AWS::EC2::Volume",
		},
		{
			name:  "two",
			types: []string{"AWS::EC2::Volume", "AWS::EC2::EIP"},
			want:  "resourcetype:AWS::EC2::Volume OR resourcetype:AWS::EC2::EIP",
		},
		{
			name:  "three",
			types: []string{"AWS::EC2::Volume", "AWS::EC2::EIP", "AWS::EC2::Instance"},
			want:  "resourcetype:AWS::EC2::Volume OR resourcetype:AWS::EC2::EIP OR resourcetype:AWS::EC2::Instance",
		},
		{
			name:  "empty",
			types: nil,
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildSearchQuery(tt.types); got != tt.want {
				t.Errorf("buildSearchQuery(%v) = %q, want %q", tt.types, got, tt.want)
			}
		})
	}
}

// TestAppendIfMissing covers the de-duplication helper.
func TestAppendIfMissing(t *testing.T) {
	s := []string{"a", "b"}
	s = appendIfMissing(s, "b")
	if len(s) != 2 {
		t.Errorf("appendIfMissing duplicate: got %v, want [a b]", s)
	}
	s = appendIfMissing(s, "c")
	if len(s) != 3 || s[2] != "c" {
		t.Errorf("appendIfMissing new: got %v, want [a b c]", s)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func regionSet(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for r := range m {
		out = append(out, r)
	}
	return out
}

func containsAll(haystack, needles []string) bool {
	for _, n := range needles {
		found := false
		for _, h := range haystack {
			if h == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
