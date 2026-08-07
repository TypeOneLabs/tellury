package cli

import (
	"github.com/spf13/pflag"

	"github.com/TypeOneLabs/tellury/internal/config"
	"github.com/TypeOneLabs/tellury/pkg/cloud"
)

// addScopeFlags registers every scope flag a provider has declared onto fs,
// each bound to the corresponding named field on cfg. It iterates
// cloud.ScopesFor(provider) — the same registry that owns scope environment
// variables — so the flag surface is driven by provider declarations, not by
// a literal list in the CLI. Adding a new cloud therefore requires no shared
// code change: it registers its scopes and its --<provider>-<scope> flags
// appear here by construction.
//
// The name -> field binding is the only literal table here: it matches a
// registry dimension against the one Scan field that owns that scope value.
// Adding a new scope dimension (say "region") only requires a new Scan field
// plus a case here.
//
// The helper returns the number of flags it registered, which tests assert
// equals the provider's declared scope count.
func addScopeFlags(fs *pflag.FlagSet, provider string, cfg *config.Scan) int {
	n := 0
	for _, sv := range cloud.ScopesFor(provider) {
		// Bind dimension name -> its owning Scan field.
		var dst *string
		switch sv.Name {
		case "project":
			dst = &cfg.Project
		case "folder":
			dst = &cfg.Folder
		case "organization":
			dst = &cfg.Organization
		default:
			// A dimension without a config field cannot be bound; skip it.
			continue
		}
		fs.StringVar(dst, sv.Flag, "", "scope: "+sv.Name)
		n++
	}
	return n
}
