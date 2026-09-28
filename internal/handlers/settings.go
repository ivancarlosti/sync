package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/services"
)

// handleGetSettings answers GET /api/settings. The answer also carries the
// supported locales and themes, so the settings form is generated from the
// server instead of duplicating the list in the SPA.
func (s *Server) handleGetSettings(c *gin.Context) {
	current, err := s.deps.Settings.Load(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"settings": current,
		"locales":  config.SupportedLocales,
		"themes":   config.SupportedThemes,
	})
}

// handleUpdateSettings answers PUT /api/settings. The service validates the
// locale, the theme and the bounds of the two numeric options.
func (s *Server) handleUpdateSettings(c *gin.Context) {
	var input services.AppSettings
	if !decode(c, &input) {
		return
	}
	updated, err := s.deps.Settings.Update(c.Request.Context(), input)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": updated})
}

// handleRawSettings answers GET /api/settings/raw: every stored key with its raw
// value, which is what the "advanced" panel of Admin > Settings shows. Provider
// credentials are excluded: their values are encrypted and belong to
// /api/providers, which never returns a secret either.
func (s *Server) handleRawSettings(c *gin.Context) {
	values, err := s.deps.Settings.Raw(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	visible := make(map[string]string, len(values))
	for key, value := range values {
		if strings.HasPrefix(key, models.ProviderConfigPrefix) {
			continue
		}
		visible[key] = value
	}
	c.JSON(http.StatusOK, gin.H{"values": visible})
}
