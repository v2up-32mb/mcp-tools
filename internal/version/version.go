// Package version holds the build-time version string.
//
// The default value "dev" is overridden at build time via:
//
//	go build -ldflags "-X github.com/v2up-32mb/mcp-tools/internal/version.Version=v1.2.1"
package version

// Version is the program version. Injected by CI via -ldflags at release time.
var Version = "dev"
