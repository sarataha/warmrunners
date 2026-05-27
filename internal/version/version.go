// Package version exposes build-time identity for the warmrunners binary.
//
// The variables below are intentionally `var` (not `const`) so they can be
// overridden at link time via -ldflags, e.g.:
//
//	go build -ldflags "\
//	  -X 'github.com/sarataha/warmrunners/internal/version.Version=$(VERSION)' \
//	  -X 'github.com/sarataha/warmrunners/internal/version.Commit=$(COMMIT)'  \
//	  -X 'github.com/sarataha/warmrunners/internal/version.BuildDate=$(DATE)'" ./...
//
// When built without ldflags (e.g. `go run`, `go test`), the defaults below
// signal a development build.
package version

var (
	// Version is the semantic version of the build (e.g. "v0.1.1").
	Version = "dev"
	// Commit is the git commit SHA the binary was built from.
	Commit = "none"
	// BuildDate is the RFC3339 UTC timestamp of the build.
	BuildDate = "unknown"
)
