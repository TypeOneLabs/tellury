package aws

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/resourceexplorer2"
)

// regionResolverTransport is an http.RoundTripper that records whether the
// AWS SDK got far enough to send a request. The request is never put on the
// wire: RoundTrip returns an error, so a test that asserts transportCalled
// true is asserting "region resolution succeeded and a request was built" —
// not that the network was reached.
type regionResolverTransport struct {
	called atomic.Bool
}

func (t *regionResolverTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.called.Store(true)
	return nil, errors.New("unexpected network call")
}

// TestEnabledRegions_PerAccountFactoryResolvesEmptyRegion is the regression
// test for organization member-account scans. enabledRegions calls the
// factory with "" (empty region) first; the per-account factory built from an
// assumed-role config must fall back to a real region instead of constructing
// the SDK client with o.Region = "".
//
// The test drives the actual newEC2Client helper and a real SDK client, but
// with an HTTP transport that fails before any bytes leave the process. If
// the region fallback is removed, the SDK fails endpoint resolution before
// reaching the transport, with "Invalid Configuration: Missing Region".
func TestEnabledRegions_PerAccountFactoryResolvesEmptyRegion(t *testing.T) {
	transport := &regionResolverTransport{}
	acctCfg := aws.Config{
		Region:      "", // the assumed-role config did not inherit a region
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", "SESSION"),
		HTTPClient:  &http.Client{Transport: transport},
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}

	// This is the same factory shape ingestOrganization builds for a member
	// account, with the assumed-role credentials swapped into acctCfg.
	acctEC2 := func(region string) ec2API {
		return newEC2Client(acctCfg, region)
	}

	_, err := enabledRegions(context.Background(), acctEC2)
	if err == nil {
		t.Fatal("enabledRegions returned nil, want the transport error")
	}
	if strings.Contains(err.Error(), "Missing Region") {
		t.Fatalf("per-account EC2 factory passed an empty region to the SDK: %v", err)
	}
	if !transport.called.Load() {
		t.Fatalf("transport was not reached; region resolution failed before a request could be built: %v", err)
	}
}

// TestNewAutoScalingClient_EmptyRegionFallsBack pins the Auto Scaling half of
// the same defect: the shared live-construction helper must not write an
// empty region into the SDK client's options.
func TestNewAutoScalingClient_EmptyRegionFallsBack(t *testing.T) {
	cfg := aws.Config{
		Region:      "eu-west-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", "SESSION"),
	}

	client := newAutoScalingClient(cfg, "")
	withOptions, ok := client.(interface{ Options() autoscaling.Options })
	if !ok {
		t.Fatalf("newAutoScalingClient returned %T, want *autoscaling.Client", client)
	}
	if got := withOptions.Options().Region; got != "eu-west-1" {
		t.Fatalf("Auto Scaling region = %q, want eu-west-1 (config fallback)", got)
	}

	emptyCfg := aws.Config{
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", "SESSION"),
	}
	client = newAutoScalingClient(emptyCfg, "")
	withOptions, ok = client.(interface{ Options() autoscaling.Options })
	if !ok {
		t.Fatalf("newAutoScalingClient returned %T, want *autoscaling.Client", client)
	}
	if got := withOptions.Options().Region; got != "us-east-1" {
		t.Fatalf("Auto Scaling region = %q, want us-east-1 (default fallback)", got)
	}
}

// TestNewDiscoverer_EmptyConfigRegionFallsBack pins the acctDiscoverer check:
// NewDiscoverer must not hand Resource Explorer an SDK config with an absent
// region, because Resource Explorer's client fails endpoint resolution with
// "Invalid Configuration: Missing Region" before any request is built.
func TestNewDiscoverer_EmptyConfigRegionFallsBack(t *testing.T) {
	d := NewDiscoverer(aws.Config{})
	withOptions, ok := d.client.(interface {
		Options() resourceexplorer2.Options
	})
	if !ok {
		t.Fatalf("NewDiscoverer client = %T, want *resourceexplorer2.Client", d.client)
	}
	if got := withOptions.Options().Region; got != "us-east-1" {
		t.Fatalf("Resource Explorer region = %q, want us-east-1", got)
	}
}
