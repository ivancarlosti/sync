// Package web embeds the compiled Vue SPA into the Go binary.
//
// The SPA is built by stage 1 of the Dockerfile (`npm run build`) and produced
// as `web/dist`; the Go build then embeds that directory, so the released image
// contains a single static binary and needs no Node at runtime.
//
// `web/dist/.gitkeep` is committed on purpose: `//go:embed` refuses to compile
// when the pattern matches nothing, which would break a fresh clone (and
// `go run ./cmd/server`) before the first frontend build.
package web

import (
	"embed"
	"io/fs"
)

// embedded holds the SPA build. The `all:` prefix also embeds files whose name
// starts with `_` or `.`, so a partial build never surprises the server.
//
//go:embed all:dist
var embedded embed.FS

// Dist returns the SPA rooted at `dist`, which is what the HTTP handlers serve.
func Dist() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		// Unreachable: the embed directive above guarantees the directory.
		panic("web: the embedded dist directory is missing: " + err.Error())
	}
	return sub
}
