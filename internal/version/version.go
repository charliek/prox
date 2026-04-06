// Package version provides the application version, set at build time via ldflags.
package version

// Version is set during build via:
//
//	-X github.com/charliek/prox/internal/version.Version=<version>
//
// It defaults to "dev" for development builds.
var Version = "dev"
