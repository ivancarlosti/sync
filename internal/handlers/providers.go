package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/services"
)

// providerInput is the payload of PUT /api/providers/:provider. An empty
// client_secret keeps the stored one, which is what lets the form be edited
// without retyping the secret.
type providerInput struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURI  string `json:"redirect_uri"`
	TenantID     string `json:"tenant_id"`
}

// handleListProviders answers GET /api/providers: one entry per provider compiled
// into the binary, telling the operator whether it is configured and where its
// credentials come from. Secrets are never part of the answer.
func (s *Server) handleListProviders(c *gin.Context) {
	providers := make([]services.ProviderCredentialsInfo, 0, 2)
	for _, name := range s.deps.Registry.Names() {
		info, err := s.deps.Credentials.Info(c.Request.Context(), name)
		if err != nil {
			fail(c, err)
			return
		}
		providers = append(providers, info)
	}
	c.JSON(http.StatusOK, gin.H{
		"providers": providers,
		"redirect_hint": gin.H{
			"google":    "/api/oauth/google/callback",
			"microsoft": "/api/oauth/microsoft/callback",
			"note":      "append this path to APP_URL when registering the application",
		},
	})
}

// handleGetProvider answers GET /api/providers/:provider.
func (s *Server) handleGetProvider(c *gin.Context) {
	provider, ok := s.knownProvider(c)
	if !ok {
		return
	}
	info, err := s.deps.Credentials.Info(c.Request.Context(), provider)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, info)
}

// handleUpdateProvider answers PUT /api/providers/:provider: it stores the
// runtime override of the OAuth client (the environment remains the base layer).
func (s *Server) handleUpdateProvider(c *gin.Context) {
	provider, ok := s.knownProvider(c)
	if !ok {
		return
	}
	var input providerInput
	if !decode(c, &input) {
		return
	}
	err := s.deps.Credentials.Update(c.Request.Context(), provider,
		input.ClientID, input.ClientSecret, input.RedirectURI, input.TenantID)
	if err != nil {
		fail(c, err)
		return
	}
	s.respondProvider(c, provider)
}

// handleClearProvider answers DELETE /api/providers/:provider: the override is
// dropped and the environment credentials apply again.
func (s *Server) handleClearProvider(c *gin.Context) {
	provider, ok := s.knownProvider(c)
	if !ok {
		return
	}
	if err := s.deps.Credentials.Clear(c.Request.Context(), provider); err != nil {
		fail(c, err)
		return
	}
	s.respondProvider(c, provider)
}

// respondProvider answers with the refreshed, masked view of a provider.
func (s *Server) respondProvider(c *gin.Context, provider models.ProviderName) {
	info, err := s.deps.Credentials.Info(c.Request.Context(), provider)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, info)
}

// knownProvider reads the `:provider` parameter and checks that this build ships
// the implementation, without requiring usable credentials.
func (s *Server) knownProvider(c *gin.Context) (models.ProviderName, bool) {
	provider := models.ProviderName(strings.ToLower(strings.TrimSpace(c.Param("provider"))))
	if !provider.Valid() {
		abort(c, http.StatusBadRequest, "unknown provider: "+string(provider))
		return "", false
	}
	if _, err := s.deps.Registry.Get(provider); err != nil {
		fail(c, services.ErrNotFound)
		return "", false
	}
	return provider, true
}
