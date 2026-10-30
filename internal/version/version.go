// Package version holds build metadata injected at link time via -ldflags.
//
// Example:
//
//	go build -ldflags "\
//	  -X github.com/Ariazonaa/statwarden/internal/version.Version=v1.0.0 \
//	  -X github.com/Ariazonaa/statwarden/internal/version.Commit=abc1234 \
//	  -X github.com/Ariazonaa/statwarden/internal/version.Date=2026-01-01T00:00:00Z" ./cmd/statwarden
package version

var (
	// Version is the semantic version or git describe output of this build.
	Version = "dev"
	// Commit is the short git commit hash of this build.
	Commit = "none"
	// Date is the UTC build timestamp in RFC 3339 format.
	Date = "unknown"
)
