// Package version holds build information injected with -ldflags.
package version

// Set via -ldflags "-X github.com/thasami/backyard/internal/version.Version=v1.2.3 ...".
var (
	Version = "0.1.0-dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a human-readable version string.
func String() string { return Version + " (" + Commit + ", " + Date + ")" }
