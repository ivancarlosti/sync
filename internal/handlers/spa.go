package handlers

import (
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

// assetCacheControl follows the naming of a Vite build: everything under
// `assets/` carries a content hash and can be cached forever, while the entry
// document must always be revalidated or a browser would keep an old SPA after
// an upgrade.
const (
	immutableCache = "public, max-age=31536000, immutable"
	revalidate     = "no-cache"
)

// spa serves the embedded SPA and implements the history fallback: a request for
// an existing file is answered with that file, anything else with index.html, so
// a deep link such as /jobs/12 reaches the router of the application. Requests
// under /api/ never fall back: an unknown endpoint is a JSON 404, which is what
// an API client expects.
func (s *Server) spa() gin.HandlerFunc {
	assets := s.deps.Assets
	return func(c *gin.Context) {
		requested := strings.TrimPrefix(path.Clean(c.Request.URL.Path), "/")
		if strings.HasPrefix(c.Request.URL.Path, "/api/") || requested == "api" {
			c.JSON(http.StatusNotFound, errorBody{Code: codeNotFound, Error: "unknown endpoint " + c.Request.URL.Path})
			return
		}
		if requested != "" && requested != "." {
			if info, err := fs.Stat(assets, requested); err == nil && !info.IsDir() {
				serveAsset(c, assets, requested)
				return
			}
		}
		index, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			// The binary was built without the frontend (the embed only found
			// the placeholder). Say so instead of answering an empty page.
			c.JSON(http.StatusServiceUnavailable, errorBody{
				Code:  codeNotConfigured,
				Error: "the web interface is not part of this build",
			})
			return
		}
		c.Header("Cache-Control", revalidate)
		c.Data(http.StatusOK, "text/html; charset=utf-8", index)
	}
}

// serveAsset writes one embedded file with its content type and cache policy.
func serveAsset(c *gin.Context, assets fs.FS, name string) {
	data, err := fs.ReadFile(assets, name)
	if err != nil {
		c.JSON(http.StatusNotFound, errorBody{Code: codeNotFound, Error: "asset not found: " + name})
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if strings.HasPrefix(name, "assets/") {
		c.Header("Cache-Control", immutableCache)
	} else {
		c.Header("Cache-Control", revalidate)
	}
	c.Data(http.StatusOK, contentType, data)
}
