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

// handleAdminConsentStart answers POST /api/oauth/:provider/admin-consent: it
// stores a consent flow and returns the URL the browser has to follow so a
// tenant administrator grants the permissions for the whole directory
// (Microsoft Entra admin consent). Providers without such a step answer 400.
func (s *Server) handleAdminConsentStart(c *gin.Context) {
	provider, ok := s.providerParam(c)
	if !ok {
		return
	}
	input := oauthStartInput{}
	if c.Request.ContentLength > 0 {
		if !decode(c, &input) {
			return
		}
	}
	authorization, err := s.deps.OAuth.BeginAdminConsent(c.Request.Context(), provider, input.RedirectTo)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, authorization)
}

// handleOAuthCallback answers GET /api/oauth/:provider/callback, the redirect
// URI registered with the provider. It exchanges the code, stores the connected
// account and sends the browser back to the SPA, which renders the result.
//
// Microsoft answers the tenant-wide consent on this very URL, so the state of the
// pending flow decides what the callback means before anything is redeemed.
func (s *Server) handleOAuthCallback(c *gin.Context) {
	provider, ok := s.providerParam(c)
	if !ok {
		return
	}
	state := strings.TrimSpace(c.Query("state"))
	if flow, err := s.deps.OAuth.FlowOf(c.Request.Context(), provider, state); err == nil &&
		flow == models.FlowAdminConsent {
		s.handleAdminConsentCallback(c, provider, state)
		return
	}
	if failure := strings.TrimSpace(c.Query("error")); failure != "" {
		detail := strings.TrimSpace(c.Query("error_description"))
		if detail == "" {
			detail = failure
		}
		s.redirectToAccounts(c, string(provider), connectErrorCode(failure), detail)
		return
	}
	account, err := s.deps.OAuth.Complete(c.Request.Context(), provider, c.Query("code"), state)
	if err != nil {
		code := codeFor(err)
		if code == codeInternal {
			logRequestError(c, err)
		}
		s.redirectToAccounts(c, string(provider), code, err.Error())
		return
	}
	detail := account.Email
	if detail == "" {
		detail = account.DisplayName
	}
	s.redirectToAccounts(c, string(provider), "", detail)
}

// handleAdminConsentCallback finishes a tenant-wide consent flow. The identity
// platform either reports `admin_consent=True` or refuses with an error, and
// both arrive next to the state of the flow.
func (s *Server) handleAdminConsentCallback(c *gin.Context, provider models.ProviderName, state string) {
	if failure := strings.TrimSpace(c.Query("error")); failure != "" {
		detail := strings.TrimSpace(c.Query("error_description"))
		if detail == "" {
			detail = failure
		}
		// The state is single use: a refused consent consumes it, so a replayed
		// link cannot resurrect the flow.
		s.redirectToGuide(c, provider, s.deps.OAuth.AbandonAdminConsent(c.Request.Context(), provider, state),
			connectErrorCode(failure), detail)
		return
	}
	target, err := s.deps.OAuth.CompleteAdminConsent(c.Request.Context(), provider, state, c.Query("tenant"))
	if err != nil {
		code := codeFor(err)
		if code == codeInternal {
			logRequestError(c, err)
		}
		s.redirectToGuide(c, provider, "", code, err.Error())
		return
	}
	s.redirectToGuide(c, provider, target, "", "")
}

// connectErrorCode maps the OAuth error code a provider answered with onto the
// code the SPA translates. `consent_required` tells the UI to offer the setup
// guide instead of a retry, which is exactly what a missing tenant-wide consent
// needs.
func connectErrorCode(failure string) string {
	switch strings.ToLower(strings.TrimSpace(failure)) {
	case "access_denied":
		return "denied"
	case "consent_required", "admin_consent_required", "interaction_required":
		return codeConsentRequired
	case "invalid_scope", "unauthorized_client":
		return codeConsentRequired
	default:
		return "failed"
	}
}

// redirectToAccounts sends the browser back to the accounts screen, tagging the
// outcome in the query string. The SPA translates the code, so no message text
// travels through the redirect. A failure also carries the provider, which is
// what lets the screen point at its setup guide.
func (s *Server) redirectToAccounts(c *gin.Context, provider, code, detail string) {
	query := url.Values{}
	if provider != "" {
		if code == "" {
			query.Set("connected", provider)
		} else {
			query.Set("provider", provider)
		}
	}
	if code != "" {
		query.Set("connect_error", code)
	}
	if trimmed := strings.TrimSpace(detail); trimmed != "" {
		query.Set("detail", trimmed)
	}
	c.Redirect(http.StatusFound, "/accounts?"+query.Encode())
}

// redirectToGuide sends the browser back to the setup guide of a provider, which
// is where a tenant-wide consent flow starts and therefore where its outcome is
// reported. `target` is the path the flow asked to return to; it is ignored when
// it is not a same-origin path (see services.SafeRedirect). The SPA translates
// the code.
func (s *Server) redirectToGuide(c *gin.Context, provider models.ProviderName, target, code, detail string) {
	path := services.SafeRedirect(target)
	if path == "" {
		path = "/admin/guide/" + string(provider)
	}
	query := url.Values{}
	if code != "" {
		query.Set("consent_error", code)
	} else {
		query.Set("consent", "granted")
	}
	if trimmed := strings.TrimSpace(detail); trimmed != "" {
		query.Set("detail", trimmed)
	}
	c.Redirect(http.StatusFound, path+"?"+query.Encode())
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
