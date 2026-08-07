// Package buildinfo carries the values stamped in by -ldflags at release time.
package buildinfo

import "runtime/debug"

// Version is stamped at release time; Commit and Date are normally left alone,
// because Long() reads them from the Go toolchain's own VCS stamps:
//
//	go build -ldflags "-X github.com/TypeOneLabs/tellury/internal/buildinfo.Version=v0.1.0" \
//	  -o tellury ./cmd/tellury
//
// A plain `go build` in a git checkout still reports the commit and build time —
// Go embeds vcs.revision and vcs.time automatically once the repository has at
// least one commit. Only Version needs stamping, since the toolchain has no way
// to know your release number.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// Short renders the one-line version string.
func Short() string { return Version + " (" + Commit + ")" }

// Long renders the full version block, falling back to the Go module's own VCS
// stamps when the binary was built without ldflags.
func Long() string {
	commit, date := Commit, Date
	if commit == "none" || date == "unknown" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					if commit == "none" {
						commit = s.Value
					}
				case "vcs.time":
					if date == "unknown" {
						date = s.Value
					}
				}
			}
		}
	}
	return "tellury " + Version + "\n  commit: " + commit + "\n  built:  " + date
}
