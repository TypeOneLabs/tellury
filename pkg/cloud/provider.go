package cloud

import (
	"context"
	"fmt"

	"github.com/TypeOneLabs/tellury/pkg/graph"
	"github.com/TypeOneLabs/tellury/pkg/metrics"
	"github.com/TypeOneLabs/tellury/pkg/pricing"
)

// Scope identifies what to scan for one provider. It is deliberately
// provider-neutral: GCP vocabulary (project/folder) and AWS vocabulary
// (account/organizational-unit) live only inside the provider-specific blocks
// below, never as fields on the shared type. Exactly one block is populated,
// matching Provider, and within that block exactly one dimension is set.
type Scope struct {
	// Provider is the cloud provider name: "gcp" or "aws".
	Provider string

	// GCP is the GCP scope block, populated exactly when Provider == "gcp".
	GCP *GCPScope

	// AWS is the AWS scope block, populated exactly when Provider == "aws".
	AWS *AWSScope
}

// GCPScope is the GCP half of a Scope. Exactly one of Project/Folder/
// Organization is set.
type GCPScope struct {
	Project      string
	Folder       string
	Organization string
}

// AWSScope is the AWS half of a Scope. Exactly one of Account/
// OrganizationalUnit/Organization is set.
type AWSScope struct {
	Account            string
	OrganizationalUnit string
	Organization       string
}

// Validate checks that the provider is known, its scope block is present, and
// exactly one dimension inside the block is set.
func (s Scope) Validate() error {
	switch s.Provider {
	case "gcp":
		if s.GCP == nil {
			return fmt.Errorf("cloud: gcp scope: no scope block")
		}
		return s.GCP.Validate()
	case "aws":
		if s.AWS == nil {
			return fmt.Errorf("cloud: aws scope: no scope block")
		}
		return s.AWS.Validate()
	default:
		return UnknownProviderError(s.Provider)
	}
}

// Validate enforces that exactly one GCP scope dimension is set. The message
// is the historical one, byte for byte, so a live GCP scan with no scope
// reports exactly what it always did.
func (g GCPScope) Validate() error {
	n := 0
	if g.Project != "" {
		n++
	}
	if g.Folder != "" {
		n++
	}
	if g.Organization != "" {
		n++
	}
	if n != 1 {
		return fmt.Errorf("cloud: scope requires exactly one of project/folder/organization")
	}
	return nil
}

// Validate enforces that exactly one AWS scope dimension is set.
func (a AWSScope) Validate() error {
	n := 0
	if a.Account != "" {
		n++
	}
	if a.OrganizationalUnit != "" {
		n++
	}
	if a.Organization != "" {
		n++
	}
	if n != 1 {
		return fmt.Errorf("cloud: scope requires exactly one of account/organizational-unit/organization")
	}
	return nil
}

// Parent renders the API parent scope the owning provider expects.
//
// GCP's Cloud Asset Inventory is scoped by "projects/<id>", "folders/<n>" or
// "organizations/<n>"; the GCP provider passes this verbatim to
// SearchAllResources. AWS has no equivalent single parent string — its
// provider drives Organizations and per-account calls from the scope fields
// directly — so AWS renders "".
func (s Scope) Parent() string {
	switch s.Provider {
	case "gcp":
		if s.GCP == nil {
			return ""
		}
		return s.GCP.Parent()
	case "aws":
		return ""
	default:
		return ""
	}
}

// Parent renders the GCP CAI-style parent: "projects/<id>", "folders/<n>" or
// "organizations/<n>".
func (g GCPScope) Parent() string {
	switch {
	case g.Project != "":
		return "projects/" + g.Project
	case g.Folder != "":
		return "folders/" + g.Folder
	case g.Organization != "":
		return "organizations/" + g.Organization
	default:
		return ""
	}
}

// String renders the scope for report/log display in the owning provider's
// vocabulary.
func (s Scope) String() string {
	switch s.Provider {
	case "gcp":
		if s.GCP == nil {
			return ""
		}
		return s.GCP.String()
	case "aws":
		if s.AWS == nil {
			return ""
		}
		return s.AWS.String()
	default:
		return ""
	}
}

// String renders the GCP scope exactly as Parent does — "projects/<id>",
// "folders/<n>" or "organizations/<n>" — exposed for readable report output.
func (g GCPScope) String() string { return g.Parent() }

// String renders the AWS scope in AWS vocabulary: "accounts/<id>",
// "organizational-units/<ou-id>" or "organizations/<org-id>".
func (a AWSScope) String() string {
	switch {
	case a.Account != "":
		return "accounts/" + a.Account
	case a.OrganizationalUnit != "":
		return "organizational-units/" + a.OrganizationalUnit
	case a.Organization != "":
		return "organizations/" + a.Organization
	default:
		return ""
	}
}

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

	// Sizer exposes the machine catalog the CLI puts on the rules Pass so
	// shape/candidate rules can price alternatives. A provider without a
	// rightsizing catalog (AWS until such a rule ships) returns nil.
	Sizer() pricing.Sizer

	// Close releases the provider's underlying cloud clients. Called once,
	// after the scan, even on the error path.
	Close() error
}
