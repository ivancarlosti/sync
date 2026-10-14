package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// Shape, name and lifetime bounds of an API token.
const (
	// apiTokenLabel prefixes every secret, so a leaked string is recognizable in
	// a log or a paste.
	apiTokenLabel = "sync_"
	// apiTokenPrefixBytes is the entropy of the public half: 4 bytes render as 8
	// hex characters, which keeps the collision probability between live tokens
	// negligible while fitting the unique index with room to spare.
	apiTokenPrefixBytes = 4
	// apiTokenSecretBytes is the entropy of the secret half (256 bits);
	// base64url renders it as 43 characters.
	apiTokenSecretBytes = 32
	// apiTokenNameLimit mirrors the size of the name column.
	apiTokenNameLimit = 191
	// TokenMaxLifetime caps the expiry an operator may request. 0 means "never".
	TokenMaxLifetime = 365 * 24 * time.Hour
)

// APITokenService mints, verifies and manages the programmatic credentials of the
// API (see docs/authentication.md).
//
// The secret is never stored, only its SHA-256 digest, which is looked up through
// the public prefix: authentication is one indexed SELECT plus a constant-time
// digest comparison. Creating or regenerating a token is therefore the only
// answer that carries the secret.
type APITokenService struct {
	store *Store
	now   func() time.Time
}

// NewAPITokenService builds the service.
func NewAPITokenService(store *Store) *APITokenService {
	return &APITokenService{store: store, now: func() time.Time { return time.Now().UTC() }}
}

// APITokenInput is a create request.
type APITokenInput struct {
	Name string
	// Scopes defaults to read + write when empty.
	Scopes []string
	// Lifetime is how long the token stays valid; 0 means it never expires.
	Lifetime time.Duration
	// CreatedBy is the operator that minted the token.
	CreatedBy string
}

// Create mints a token and returns it with its one-time secret.
func (s *APITokenService) Create(ctx context.Context, input APITokenInput) (*models.APIToken, string, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, "", fmt.Errorf("%w: a name is required", ErrValidation)
	}
	if len([]rune(name)) > apiTokenNameLimit {
		return nil, "", fmt.Errorf("%w: the name is longer than %d characters", ErrValidation, apiTokenNameLimit)
	}
	scopes, err := normaliseScopes(input.Scopes)
	if err != nil {
		return nil, "", err
	}
	lifetime, err := clampLifetime(input.Lifetime)
	if err != nil {
		return nil, "", err
	}

	// The unique index on the prefix is what makes a collision a hard failure, so
	// a taken prefix is redrawn instead of surfacing as a constraint error.
	for attempt := 0; attempt < 5; attempt++ {
		prefix, err := randomHex(apiTokenPrefixBytes)
		if err != nil {
			return nil, "", err
		}
		if _, err := s.store.FindAPITokenByPrefix(ctx, prefix); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return nil, "", err
		}
		secret, err := randomSecret()
		if err != nil {
			return nil, "", err
		}
		encoded, err := json.Marshal(scopes)
		if err != nil {
			return nil, "", fmt.Errorf("services: encoding the token scopes: %w", err)
		}
		now := s.now()
		token := &models.APIToken{
			Name:      name,
			Prefix:    prefix,
			Hash:      hashSecret(secret),
			Scopes:    string(encoded),
			CreatedBy: strings.TrimSpace(input.CreatedBy),
			CreatedAt: now,
			UpdatedAt: now,
		}
		if lifetime > 0 {
			expires := now.Add(lifetime)
			token.ExpiresAt = &expires
		}
		if err := s.store.CreateAPIToken(ctx, token); err != nil {
			return nil, "", err
		}
		return token, apiTokenLabel + prefix + "." + secret, nil
	}
	return nil, "", fmt.Errorf("%w: could not allocate a unique token prefix", ErrValidation)
}

// Regenerate replaces the prefix and the secret of a token in place, so the old
// secret stops working immediately while the row keeps its name, its scopes and
// its history.
func (s *APITokenService) Regenerate(ctx context.Context, id uint) (*models.APIToken, string, error) {
	token, err := s.store.GetAPIToken(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if token.RevokedAt != nil {
		return nil, "", fmt.Errorf("%w: a revoked token cannot be regenerated", ErrValidation)
	}
	for attempt := 0; attempt < 5; attempt++ {
		prefix, err := randomHex(apiTokenPrefixBytes)
		if err != nil {
			return nil, "", err
		}
		if _, err := s.store.FindAPITokenByPrefix(ctx, prefix); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return nil, "", err
		}
		secret, err := randomSecret()
		if err != nil {
			return nil, "", err
		}
		token.Prefix = prefix
		token.Hash = hashSecret(secret)
		token.UpdatedAt = s.now()
		if err := s.store.SaveAPIToken(ctx, token); err != nil {
			return nil, "", err
		}
		return token, apiTokenLabel + prefix + "." + secret, nil
	}
	return nil, "", fmt.Errorf("%w: could not allocate a unique token prefix", ErrValidation)
}

// List returns every token, newest first.
func (s *APITokenService) List(ctx context.Context) ([]models.APIToken, error) {
	return s.store.ListAPITokens(ctx)
}

// Revoke soft-deletes a token: the row survives (its history stays readable) but
// authentication with it fails from now on. Revoking twice is not an error.
func (s *APITokenService) Revoke(ctx context.Context, id uint) (*models.APIToken, error) {
	token, err := s.store.GetAPIToken(ctx, id)
	if err != nil {
		return nil, err
	}
	if token.RevokedAt == nil {
		now := s.now()
		token.RevokedAt = &now
		token.UpdatedAt = now
		if err := s.store.SaveAPIToken(ctx, token); err != nil {
			return nil, err
		}
	}
	return token, nil
}

// Delete removes a token and its row.
func (s *APITokenService) Delete(ctx context.Context, id uint) error {
	return s.store.DeleteAPIToken(ctx, id)
}

// Authenticate verifies a presented secret and returns the identity of the
// request. The lookup goes through the public prefix and the digest comparison is
// constant time, so a wrong secret reveals nothing about the stored one.
func (s *APITokenService) Authenticate(ctx context.Context, presented, ip string) (Identity, error) {
	prefix, secret, ok := splitToken(presented)
	if !ok {
		return Identity{}, ErrUnauthorized
	}
	token, err := s.store.FindAPITokenByPrefix(ctx, prefix)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Identity{}, ErrUnauthorized
		}
		return Identity{}, err
	}
	if !secretMatches(token.Hash, secret) {
		return Identity{}, ErrUnauthorized
	}
	now := s.now()
	if token.RevokedAt != nil {
		return Identity{}, ErrUnauthorized
	}
	if token.ExpiresAt != nil && !now.Before(*token.ExpiresAt) {
		return Identity{}, ErrUnauthorized
	}
	// The usage metadata is best effort: failing to record it must not refuse a
	// credential that verified.
	if err := s.store.TouchAPIToken(ctx, token.ID, now, ip); err != nil {
		slog.Warn("recording the use of the api token failed", "token", token.ID, "error", err)
	}
	return Identity{
		Authenticated: true,
		Subject:       "token:" + token.Name,
		Name:          token.Name,
		Method:        models.AuthModeToken,
		Admin:         true,
		Token:         token.Name,
		Scopes:        ScopesOf(*token),
	}, nil
}

// ScopesOf decodes the scope list stored on a token. A row whose document cannot
// be read is treated as read-only, which is the safe answer.
func ScopesOf(token models.APIToken) []string {
	if strings.TrimSpace(token.Scopes) == "" {
		return []string{models.ScopeRead}
	}
	var scopes []string
	if err := json.Unmarshal([]byte(token.Scopes), &scopes); err != nil || len(scopes) == 0 {
		slog.Warn("the scopes of an api token could not be read", "token", token.ID, "error", err)
		return []string{models.ScopeRead}
	}
	return scopes
}

// HasScope reports whether a scope set allows an operation. A `write` token is a
// superset of a `read` one.
func HasScope(scopes []string, scope string) bool {
	for _, granted := range scopes {
		if granted == scope || granted == models.ScopeWrite {
			return true
		}
	}
	return false
}

// splitToken parses `sync_<prefix>.<secret>`.
func splitToken(presented string) (string, string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(presented), apiTokenLabel)
	if !ok {
		return "", "", false
	}
	prefix, secret, ok := strings.Cut(rest, ".")
	if !ok || prefix == "" || secret == "" {
		return "", "", false
	}
	return prefix, secret, true
}

// hashSecret is the only form in which the secret reaches the database.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// secretMatches compares the digest of a presented secret with the stored one.
func secretMatches(hash, secret string) bool {
	stored, err := hex.DecodeString(hash)
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(stored, sum[:]) == 1
}

// randomHex draws n cryptographically random bytes as hex characters.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("services: drawing a random token prefix: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// randomSecret draws the secret half of a token as URL-safe base64 without
// padding, so the secret never carries a character a shell would need quoting.
func randomSecret() (string, error) {
	buf := make([]byte, apiTokenSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("services: drawing a random token secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// normaliseScopes validates and de-duplicates a requested scope set, defaulting
// to read + write and keeping the canonical order.
func normaliseScopes(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{models.ScopeRead, models.ScopeWrite}, nil
	}
	seen := map[string]bool{}
	for _, value := range values {
		scope := strings.ToLower(strings.TrimSpace(value))
		if scope == "" {
			continue
		}
		if !models.ValidScope(scope) {
			return nil, fmt.Errorf("%w: unknown scope %q", ErrValidation, value)
		}
		seen[scope] = true
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("%w: at least one scope is required", ErrValidation)
	}
	scopes := make([]string, 0, len(seen))
	for _, scope := range models.AllScopes {
		if seen[scope] {
			scopes = append(scopes, scope)
		}
	}
	return scopes, nil
}

// clampLifetime bounds a requested expiry: a negative duration is refused and a
// longer one is capped, so a typo can neither mint a credential that is already
// dead nor one that outlives the documented maximum.
func clampLifetime(requested time.Duration) (time.Duration, error) {
	if requested < 0 {
		return 0, fmt.Errorf("%w: the expiry cannot be negative", ErrValidation)
	}
	if requested > TokenMaxLifetime {
		return TokenMaxLifetime, nil
	}
	return requested, nil
}
