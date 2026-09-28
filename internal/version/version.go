// Package version exposes the build metadata of the running Sync binary.
//
// The values are injected at compile time by the Docker build (CI passes the
// semantic-release tag as VERSION and the commit SHA as COMMIT):
//
//	go build -ldflags "-X github.com/ivancarlosti/sync/internal/version.Version=1.2.3 \
//	                   -X github.com/ivancarlosti/sync/internal/version.Commit=<sha>" ./cmd/server
//
// When the binary is built without those flags (local `go run ./cmd/server`)
// the placeholders below are used, so /api/version never fails.
package version

import "runtime"

// Name is the product name, also used by the SPA title and the docs.
const Name = "sync"

// Version is the semantic version of this build ("dev" for local builds).
var Version = "dev"

// Commit is the git SHA this binary was built from ("unknown" for local builds).
var Commit = "unknown"

// BuildDate is an optional RFC3339 timestamp; the CI does not set it today but
// the field is kept so a future workflow can fill it without touching the API.
var BuildDate = ""

// Info is the JSON payload returned by GET /api/version and shown in
// Admin > About.
type Info struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
	BuildDate string `json:"build_date,omitempty"`
}

// Get returns the build metadata of the current binary.
func Get() Info {
	return Info{
		Name:      Name,
		Version:   Version,
		Commit:    Commit,
		GoVersion: runtime.Version(),
		BuildDate: BuildDate,
	}
}
