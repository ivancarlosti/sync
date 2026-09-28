package handlers

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/services"
)

// oauthStartInput is the optional payload of POST /api/oauth/:provider/start: the
// SPA may ask to come back to a specific page once the flow is over.
type oauthStartInput struct {
	RedirectTo string `json:"redirect_to"`
}

// handleOAuthProviders answers GET /api/oauth with the providers of this build
// that have a usable OAuth client, which is exactly what the "Connect an
// account" screen lists.
func (s *Server) handleOAuthProviders(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"providers": s.deps.OAuth.Providers(c.Request.Context())})
}

// handleOAuthStart answers POST /api/oauth/:provider/start. It stores the server
// side of the flow (state + PKCE verifier) and returns the URL the browser has
// to follow.
func (s *Server) handleOAuthStart(c *gin.Context) {
	provider, ok := s.providerParam(c)
	if !ok {
		return
	}
	// The body is optional: a start request without payload is valid.
	input := oauthStartInput{}
	if c.Request.ContentLength > 0 {
		if !decode(c, &input) {
			return
		}
	}
	authorization, err := s.deps.OAuth.Begin(c.Request.Context(), provider, input.RedirectTo)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, authorization)
}

// handleOAuthCallback answers GET /api/oauth/:provider/callback, the redirect
// URI registered with the provider. It exchanges the code, stores the connected
// account and sends the browser back to the SPA, which renders the result.
func (s *Server) handleOAuthCallback(c *gin.Context) {
	provider, ok := s.providerParam(c)
	if !ok {
		return
	}
	if failure := strings.TrimSpace(c.Query("error")); failure != "" {
		detail := strings.TrimSpace(c.Query("error_description"))
		if detail == "" {
			detail = failure
		}
		s.redirectToAccounts(c, "", "denied", detail)
		return
	}
	account, err := s.deps.OAuth.Complete(c.Request.Context(), provider, c.Query("code"), c.Query("state"))
	if err != nil {
		code := codeFor(err)
		if code == codeInternal {
			logRequestError(c, err)
		}
		s.redirectToAccounts(c, "", code, err.Error())
		return
	}
	detail := account.Email
	if detail == "" {
		detail = account.DisplayName
	}
	s.redirectToAccounts(c, string(provider), "", detail)
}

// redirectToAccounts sends the browser back to the accounts screen, tagging the
// outcome in the query string. The SPA translates the code, so no message text
// travels through the redirect.
func (s *Server) redirectToAccounts(c *gin.Context, provider, code, detail string) {
	query := url.Values{}
	if provider != "" {
		query.Set("connected", provider)
	}
	if code != "" {
		query.Set("connect_error", code)
	}
	if trimmed := strings.TrimSpace(detail); trimmed != "" {
		query.Set("detail", trimmed)
	}
	c.Redirect(http.StatusFound, "/accounts?"+query.Encode())
}

// providerParam reads and validates the `:provider` path parameter.
func (s *Server) providerParam(c *gin.Context) (models.ProviderName, bool) {
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
