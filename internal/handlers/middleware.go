package handlers

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/services"
)

// identityKey is the gin context key holding the authenticated operator.
const identityKey = "sync.operator"

// securityHeaders hardens every answer. The SPA is served by the same origin, so
// a strict policy costs nothing; `frame-ancestors 'none'` plus
// `X-Frame-Options: DENY` keep Sync out of an embedding page (clickjacking), and
// `Referrer-Policy: same-origin` avoids leaking the OAuth callback URLs.
func (s *Server) securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		headers := c.Writer.Header()
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set("X-Frame-Options", "DENY")
		headers.Set("Referrer-Policy", "same-origin")
		headers.Set("Content-Security-Policy",
			"default-src 'self'; base-uri 'self'; frame-ancestors 'none'; "+
				"img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self' https://www.google.com https://www.gstatic.com; "+
				"connect-src 'self'; frame-src https://www.google.com https://www.gstatic.com")
		c.Next()
	}
}

// session resolves the operator identity of every request once.
//
//   - `none` mode: every request carries the open administrator identity.
//   - `account` and `keycloak` modes: the signed cookie is verified; an invalid,
//     tampered or expired cookie simply leaves the request unauthenticated, and
//     the guard answers 401.
//
// The middleware never aborts: public endpoints (health, version, session,
// login, callbacks) are served from the same chain.
func (s *Server) session() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.deps.Auth.Open() {
			c.Set(identityKey, s.deps.Auth.OpenIdentity())
			c.Next()
			return
		}
		if value, err := c.Cookie(services.SessionCookieName); err == nil {
			if identity, ok := s.deps.Auth.FromCookie(value); ok {
				c.Set(identityKey, identity)
			}
		}
		c.Next()
	}
}

// operator returns the identity of the current request; the zero value means
// "not authenticated". It is safe to call from any handler.
func (s *Server) operator(c *gin.Context) services.Identity {
	value, ok := c.Get(identityKey)
	if !ok {
		return services.Identity{}
	}
	identity, ok := value.(services.Identity)
	if !ok {
		return services.Identity{}
	}
	return identity
}

// requireSession guards the endpoints that operate on the operator's data.
//
// Sync has a single role: every authenticated operator is an administrator (the
// `keycloak` allow-list is what restricts who may log in), so a second guard for
// "admin only" routes would be a distinction without a difference. The method is
// kept separate so a future role model only has to touch this file.
func (s *Server) requireSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.operator(c).Authenticated {
			abort(c, http.StatusUnauthorized, "sign in to continue")
			return
		}
		c.Next()
	}
}

// logRequestError records a server side failure with the request it belongs to.
// Tokens and secrets are never part of the logged fields.
func logRequestError(c *gin.Context, err error) {
	slog.Error("api request failed",
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
		"client", c.ClientIP(),
		"error", err,
	)
}
