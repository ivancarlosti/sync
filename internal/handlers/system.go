package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/config"
)

// handleHealth answers GET /api/health. It is public and deliberately cheap: a
// container orchestrator calls it every few seconds, so it only pings the
// database and reports the in-process counters. A failing database answers 503,
// which is what removes a broken instance from a load balancer.
func (s *Server) handleHealth(c *gin.Context) {
	ctx := c.Request.Context()
	body := gin.H{
		"status":       "ok",
		"database":     "ok",
		"version":      s.deps.Info.Version,
		"auth_mode":    s.deps.Auth.ModeName(),
		"running_jobs": len(s.deps.Sync.RunningJobs()),
	}
	if err := s.deps.Store.DB().WithContext(ctx).Raw("SELECT 1").Error; err != nil {
		logRequestError(c, err)
		body["status"] = "degraded"
		body["database"] = "error"
		c.JSON(http.StatusServiceUnavailable, body)
		return
	}
	c.JSON(http.StatusOK, body)
}

// handleVersion answers GET /api/version. Beyond the build metadata it reports
// the vocabulary of the running build (auth mode, providers, locales, themes) so
// the SPA can adapt its forms to the server it is served by.
func (s *Server) handleVersion(c *gin.Context) {
	names := s.deps.Registry.Names()
	providers := make([]string, 0, len(names))
	for _, name := range names {
		providers = append(providers, string(name))
	}
	c.JSON(http.StatusOK, gin.H{
		"info":      s.deps.Info,
		"auth_mode": s.deps.Auth.ModeName(),
		"providers": providers,
		"locales":   config.SupportedLocales,
		"themes":    config.SupportedThemes,
	})
}
