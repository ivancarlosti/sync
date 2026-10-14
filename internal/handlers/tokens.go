package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/services"
)

// tokenInput is the payload of POST /api/tokens.
type tokenInput struct {
	Name string `json:"name"`
	// Scopes defaults to read + write when omitted.
	Scopes []string `json:"scopes"`
	// ExpiresInDays is how long the token stays valid; 0 means it never expires
	// (the service caps it at services.TokenMaxLifetime).
	ExpiresInDays int `json:"expires_in_days"`
}

// apiTokenView is the public shape of a token: the prefix identifies it, the
// secret is never part of an answer except the one that minted it.
type apiTokenView struct {
	ID         uint       `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	CreatedBy  string     `json:"created_by,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP string     `json:"last_used_ip,omitempty"`
	Requests   int64      `json:"requests"`
	CreatedAt  time.Time  `json:"created_at"`
	// State is the derived lifecycle (`active`, `revoked` or `expired`): the UI
	// shows it as a badge instead of recomputing it from three fields.
	State string `json:"state"`
}

// newAPITokenView decorates a token. `now` decides between active and expired.
func newAPITokenView(token models.APIToken, now time.Time) apiTokenView {
	state := "active"
	if token.RevokedAt != nil {
		state = "revoked"
	} else if token.ExpiresAt != nil && !now.Before(*token.ExpiresAt) {
		state = "expired"
	}
	return apiTokenView{
		ID:         token.ID,
		Name:       token.Name,
		Prefix:     token.Prefix,
		Scopes:     services.ScopesOf(token),
		CreatedBy:  token.CreatedBy,
		ExpiresAt:  token.ExpiresAt,
		RevokedAt:  token.RevokedAt,
		LastUsedAt: token.LastUsedAt,
		LastUsedIP: token.LastUsedIP,
		Requests:   token.Requests,
		CreatedAt:  token.CreatedAt,
		State:      state,
	}
}

// handleListTokens answers GET /api/tokens.
func (s *Server) handleListTokens(c *gin.Context) {
	tokens, err := s.deps.APITokens.List(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	now := s.now()
	views := make([]apiTokenView, 0, len(tokens))
	for _, token := range tokens {
		views = append(views, newAPITokenView(token, now))
	}
	c.JSON(http.StatusOK, gin.H{"tokens": views, "scopes": models.AllScopes})
}

// handleCreateToken answers POST /api/tokens with the one-time secret (201).
func (s *Server) handleCreateToken(c *gin.Context) {
	var input tokenInput
	if !decode(c, &input) {
		return
	}
	token, secret, err := s.deps.APITokens.Create(c.Request.Context(), services.APITokenInput{
		Name:      input.Name,
		Scopes:    input.Scopes,
		Lifetime:  lifetimeOf(input.ExpiresInDays),
		CreatedBy: s.operator(c).Email,
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"token": newAPITokenView(*token, s.now()), "secret": secret})
}

// handleRegenerateToken answers POST /api/tokens/:id/regenerate. The old secret
// stops working as soon as the answer is written.
func (s *Server) handleRegenerateToken(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	token, secret, err := s.deps.APITokens.Regenerate(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": newAPITokenView(*token, s.now()), "secret": secret})
}

// handleRevokeToken answers POST /api/tokens/:id/revoke. Revoking twice is not an
// error, so the endpoint is idempotent.
func (s *Server) handleRevokeToken(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	token, err := s.deps.APITokens.Revoke(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": newAPITokenView(*token, s.now())})
}

// handleDeleteToken answers DELETE /api/tokens/:id.
func (s *Server) handleDeleteToken(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if err := s.deps.APITokens.Delete(c.Request.Context(), id); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// lifetimeOf converts the requested days into a duration; the service refuses a
// negative one and caps a long one.
func lifetimeOf(days int) time.Duration {
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

// presentedToken extracts the credential of a request: a bearer token in the
// `Authorization` header (what curl and every SDK send) or the `X-API-Key`
// header some tools prefer.
func presentedToken(c *gin.Context) string {
	if header := strings.TrimSpace(c.GetHeader("Authorization")); header != "" {
		if rest, ok := strings.CutPrefix(header, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
		if rest, ok := strings.CutPrefix(header, "bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return strings.TrimSpace(c.GetHeader("X-API-Key"))
}

// scopeFor is the scope a request method needs: a safe request only reads.
func scopeFor(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return models.ScopeRead
	default:
		return models.ScopeWrite
	}
}
