package handlers

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/services"
	"github.com/ivancarlosti/sync/internal/version"
)

// loginInput is the payload of POST /api/auth/login.
type loginInput struct {
	Login        string `json:"login"`
	Password     string `json:"password"`
	CaptchaToken string `json:"captcha_token"`
	RedirectTo   string `json:"redirect_to"`
}

// captchaView tells the login form whether it must render a reCAPTCHA widget.
type captchaView struct {
	Enabled bool   `json:"enabled"`
	SiteKey string `json:"site_key,omitempty"`
}

// keycloakView tells the login page whether the OIDC button must be shown.
type keycloakView struct {
	Enabled bool `json:"enabled"`
}

// sessionView is the answer of GET /api/auth/session and the post-login payload.
// It carries everything the SPA needs to render the shell before it knows
// whether the visitor is allowed in: mode, captcha, keycloak, the build metadata
// and the defaults an administrator configured.
type sessionView struct {
	Mode          string               `json:"mode"`
	Authenticated bool                 `json:"authenticated"`
	Open          bool                 `json:"open"`
	Captcha       captchaView          `json:"captcha"`
	Keycloak      keycloakView         `json:"keycloak"`
	Operator      services.Identity    `json:"operator"`
	Defaults      services.AppSettings `json:"defaults"`
	Version       version.Info         `json:"version"`
}

// currentSession builds the session view of the request.
func (s *Server) currentSession(c *gin.Context) sessionView {
	defaults, err := s.deps.Settings.Load(c.Request.Context())
	if err != nil {
		// A settings failure must not hide the login form: the environment
		// defaults are still good enough to render it.
		logRequestError(c, err)
		defaults = services.AppSettings{
			DefaultLocale:       s.deps.Config.DefaultLocale,
			DefaultTheme:        s.deps.Config.DefaultTheme,
			SyncIntervalMinutes: services.DefaultSyncInterval,
			RunTimeoutMinutes:   services.DefaultRunTimeout,
		}
	}
	return sessionView{
		Mode:          s.deps.Auth.ModeName(),
		Authenticated: s.operator(c).Authenticated,
		Open:          s.deps.Auth.Open(),
		Captcha:       captchaView{Enabled: s.deps.Auth.CaptchaEnabled(), SiteKey: s.deps.Auth.CaptchaSiteKey()},
		Keycloak:      keycloakView{Enabled: s.deps.Auth.KeycloakConfigured()},
		Operator:      s.operator(c),
		Defaults:      defaults,
		Version:       s.deps.Info,
	}
}

// handleSession answers GET /api/auth/session. It is public on purpose: the SPA
// has to know the authentication mode before it can render anything.
func (s *Server) handleSession(c *gin.Context) {
	c.JSON(http.StatusOK, s.currentSession(c))
}

// handleLogin answers POST /api/auth/login: it validates the credentials (and
// the reCAPTCHA token when configured), then stores the signed session cookie.
func (s *Server) handleLogin(c *gin.Context) {
	var input loginInput
	if !decode(c, &input) {
		return
	}
	session, err := s.deps.Auth.Login(c.Request.Context(), input.Login, input.Password, input.CaptchaToken, c.ClientIP())
	if err != nil {
		fail(c, err)
		return
	}
	s.writeSessionCookie(c, session)
	// The answer describes the session that was just created, not the anonymous
	// request that carried the credentials, so the SPA can render the shell
	// without a second round trip.
	c.Set(identityKey, session.Identity)
	c.JSON(http.StatusOK, s.currentSession(c))
}

// handleLogout answers POST /api/auth/logout by expiring the session cookie. It
// is idempotent and never fails, so the SPA can always clear its state.
func (s *Server) handleLogout(c *gin.Context) {
	s.clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}

// handleKeycloakStart answers GET /api/auth/keycloak: it returns the realm
// authorization URL the browser must follow. The SPA navigates to it instead of
// being redirected, which lets it show an error without leaving the page.
func (s *Server) handleKeycloakStart(c *gin.Context) {
	if !s.deps.Auth.KeycloakConfigured() {
		fail(c, services.ErrNotConfigured)
		return
	}
	authorization, err := s.deps.Auth.KeycloakBegin(c.Request.Context(), c.Query("redirect_to"))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, authorization)
}

// handleKeycloakCallback answers GET /api/auth/callback, the redirect URI of the
// realm. It exchanges the code, stores the session cookie and sends the browser
// back to the SPA. Failures are reported as query parameters of the login page
// (the SPA translates the code), never as a raw error page: an operator who
// denied the consent must land back on /login with an explanation.
func (s *Server) handleKeycloakCallback(c *gin.Context) {
	if failure := strings.TrimSpace(c.Query("error")); failure != "" {
		// The realm refused the authorization (consent denied, expired link…).
		s.redirectToLogin(c, "denied", failure)
		return
	}
	session, err := s.deps.Auth.KeycloakComplete(c.Request.Context(), c.Query("code"), c.Query("state"))
	if err != nil {
		code := "failed"
		if errors.Is(err, services.ErrForbidden) {
			code = "forbidden"
		}
		if !errors.Is(err, services.ErrForbidden) && !errors.Is(err, services.ErrValidation) {
			logRequestError(c, err)
		}
		s.redirectToLogin(c, code, err.Error())
		return
	}
	s.writeSessionCookie(c, session)
	c.Redirect(http.StatusFound, "/")
}

// redirectToLogin sends the browser to the SPA login page with a translatable
// code and the technical detail of the failure.
func (s *Server) redirectToLogin(c *gin.Context, code, detail string) {
	target := "/login?auth_error=" + url.QueryEscape(code)
	if trimmed := strings.TrimSpace(detail); trimmed != "" {
		target += "&auth_detail=" + url.QueryEscape(trimmed)
	}
	c.Redirect(http.StatusFound, target)
}

// writeSessionCookie stores the signed session. The cookie is HttpOnly (no
// JavaScript access), SameSite=Lax (the OAuth callbacks are top-level
// navigations and therefore keep it) and Secure whenever APP_URL uses HTTPS.
func (s *Server) writeSessionCookie(c *gin.Context, session services.Session) {
	maxAge := int(time.Until(session.ExpiresAt).Seconds())
	if maxAge <= 0 {
		maxAge = int(services.SessionTTL.Seconds())
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(services.SessionCookieName, session.Value, maxAge, "/", "", s.secureCookies(), true)
}

// clearSessionCookie expires the session cookie.
func (s *Server) clearSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(services.SessionCookieName, "", -1, "/", "", s.secureCookies(), true)
}

// secureCookies reports whether the instance is published over HTTPS, which is
// what decides the Secure attribute of the session cookie.
func (s *Server) secureCookies() bool {
	return strings.HasPrefix(strings.ToLower(s.deps.Config.AppURL), "https://")
}
