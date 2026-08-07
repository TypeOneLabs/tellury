package cloud

import (
	"context"
	"fmt"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// Scope identifies what to scan. Exactly one of Project/Folder/Organization.
type Scope struct {
	Project      string
	Folder       string
	Organization string
}

// Validate enforces that exactly one scope dimension is set. A cache/fixture
// replay names its own scope and may legitimately skip this check upstream.
func (s Scope) Validate() error {
	n := 0
	if s.Project != "" {
		n++
	}
	if s.Folder != "" {
		n++
	}
	if s.Organization != "" {
		n++
	}
	if n != 1 {
		return fmt.Errorf("cloud: scope requires exactly one of project/folder/organization")
	}
	return nil
}

// Parent renders the CAI-style resource container: "projects/<id>",
// "folders/<n>" or "organizations/<n>".
func (s Scope) Parent() string {
	switch {
	case s.Project != "":
		return "projects/" + s.Project
	case s.Folder != "":
		return "folders/" + s.Folder
	case s.Organization != "":
		return "organizations/" + s.Organization
	default:
		return ""
	}
}

// String implements fmt.Stringer; identical to Parent, exposed for readable
// report/log output.
func (s Scope) String() string { return s.Parent() }

// UnknownProviderError is returned when --provider names a cloud tellury does
// not implement.
func UnknownProviderError(name string) error {
	return fmt.Errorf("cloud: unknown provider %q", name)
}

// Provider is the single contract a cloud implementation must satisfy.
type Provider interface {
	Name() string

	Ingest(ctx context.Context, sc Scope, assetTypeHints []string) (*graph.Graph, error)

	EnrichMetrics(ctx context.Context, g *graph.Graph, sc Scope, req metrics.Request) error

	Pricer() pricing.Pricer
}
