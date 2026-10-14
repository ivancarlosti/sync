package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// issueToken mints a token and returns it with its one-time secret.
func issueToken(t *testing.T, service *APITokenService, input APITokenInput) (*models.APIToken, string) {
	t.Helper()
	token, secret, err := service.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("creating the token: %v", err)
	}
	return token, secret
}

// regenerateToken regenerates a token and returns it with the new secret.
func regenerateToken(t *testing.T, service *APITokenService, id uint) (*models.APIToken, string) {
	t.Helper()
	token, secret, err := service.Regenerate(context.Background(), id)
	if err != nil {
		t.Fatalf("regenerating the token: %v", err)
	}
	return token, secret
}

// TestAPITokenLifecycle follows a token from minting to deletion: the shape of
// the secret, what is stored, how it authenticates, and that regenerating or
// revoking it takes the old secret away immediately.
func TestAPITokenLifecycle(t *testing.T) {
	store := newTestStore(t)
	service := NewAPITokenService(store)
	ctx := context.Background()

	token, secret := issueToken(t, service, APITokenInput{
		Name:      "  cost exporter  ",
		Scopes:    []string{models.ScopeRead, models.ScopeRead},
		CreatedBy: "ops@example.test",
	})
	if token.Name != "cost exporter" {
		t.Errorf("name = %q, want the trimmed name", token.Name)
	}
	if token.Scopes != `["read"]` {
		t.Errorf("scopes = %q, want the de-duplicated read scope", token.Scopes)
	}

	rest, ok := strings.CutPrefix(secret, "sync_")
	if !ok {
		t.Fatalf("secret = %q, want the sync_ prefix", secret)
	}
	prefix, body, ok := strings.Cut(rest, ".")
	if !ok || prefix != token.Prefix {
		t.Fatalf("secret = %q, want sync_%s.<secret>", secret, token.Prefix)
	}
	if len(body) < 40 {
		t.Fatalf("secret body = %q, want the base64url of 32 random bytes", body)
	}
	// Only the digest is stored: the secret itself must not be recoverable.
	if token.Hash != hashSecret(body) || strings.Contains(token.Hash, body) {
		t.Fatalf("hash = %q, want the SHA-256 digest of the secret half", token.Hash)
	}
	if token.Requests != 0 || token.LastUsedAt != nil {
		t.Fatalf("token = %+v, want a brand new token with no use", token)
	}

	identity, err := service.Authenticate(ctx, secret, "192.0.2.7")
	if err != nil {
		t.Fatalf("authenticating with the secret: %v", err)
	}
	if !identity.Authenticated || identity.Method != models.AuthModeToken || identity.Token != "cost exporter" {
		t.Fatalf("identity = %+v, want an API-token identity", identity)
	}
	if !HasScope(identity.Scopes, models.ScopeRead) || HasScope(identity.Scopes, models.ScopeWrite) {
		t.Fatalf("scopes = %v, want a read-only credential", identity.Scopes)
	}

	// The use is recorded, so an unused token stands out in the list.
	stored, err := store.GetAPIToken(ctx, token.ID)
	if err != nil {
		t.Fatalf("reloading the token: %v", err)
	}
	if stored.Requests != 1 || stored.LastUsedIP != "192.0.2.7" || stored.LastUsedAt == nil {
		t.Fatalf("stored = %+v, want the usage metadata of the call", stored)
	}

	// A wrong secret, a wrong prefix and a malformed credential all fail the same
	// way, without telling which part was wrong.
	for _, bad := range []string{
		"sync_" + prefix + "." + strings.Repeat("A", 43),
		"sync_deadbeef." + body,
		"not-a-token",
		"",
		"sync_" + prefix,
	} {
		if _, err := service.Authenticate(ctx, bad, "192.0.2.7"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("authenticating with %q: err = %v, want ErrUnauthorized", bad, err)
		}
	}

	// Regenerating replaces both halves, so the previous secret dies at once.
	regenerated, next := regenerateToken(t, service, token.ID)
	if next == secret || regenerated.Prefix == token.Prefix {
		t.Fatal("regenerating a token must replace its prefix and its secret")
	}
	if _, err := service.Authenticate(ctx, secret, "192.0.2.7"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("the previous secret still authenticates: err = %v", err)
	}
	if _, err := service.Authenticate(ctx, next, "192.0.2.7"); err != nil {
		t.Fatalf("the new secret does not authenticate: %v", err)
	}

	// Revoking is idempotent and takes the credential away for good.
	revoked, err := service.Revoke(ctx, regenerated.ID)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("revoking: token = %+v, err = %v", revoked, err)
	}
	if _, err := service.Revoke(ctx, regenerated.ID); err != nil {
		t.Fatalf("revoking twice must be idempotent: %v", err)
	}
	if _, err := service.Authenticate(ctx, next, "192.0.2.7"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a revoked token still authenticates: err = %v", err)
	}
	if _, _, err := service.Regenerate(ctx, regenerated.ID); !errors.Is(err, ErrValidation) {
		t.Fatalf("regenerating a revoked token: err = %v, want ErrValidation", err)
	}

	// The list never carries a secret, and deleting is idempotent.
	rows, err := service.List(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: rows = %+v, err = %v", rows, err)
	}
	if err := service.Delete(ctx, regenerated.ID); err != nil {
		t.Fatalf("deleting the token: %v", err)
	}
	if _, err := store.GetAPIToken(ctx, regenerated.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the token is still there: err = %v", err)
	}
	if err := service.Delete(ctx, 4242); err != nil {
		t.Fatalf("deleting a missing token must be idempotent: %v", err)
	}
	if _, err := service.Revoke(ctx, 4242); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking a missing token: err = %v, want ErrNotFound", err)
	}
}

// TestAPITokenValidationAndExpiry checks the create request, the expiry clamp and
// that an expired credential stops authenticating.
func TestAPITokenValidationAndExpiry(t *testing.T) {
	store := newTestStore(t)
	service := NewAPITokenService(store)
	ctx := context.Background()

	if _, _, err := service.Create(ctx, APITokenInput{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("an unnamed token: err = %v, want ErrValidation", err)
	}
	if _, _, err := service.Create(ctx, APITokenInput{Name: "x", Scopes: []string{"admin"}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("an unknown scope: err = %v, want ErrValidation", err)
	}
	if _, _, err := service.Create(ctx, APITokenInput{Name: "x", Lifetime: -time.Hour}); !errors.Is(err, ErrValidation) {
		t.Fatalf("a negative expiry: err = %v, want ErrValidation", err)
	}

	// An empty scope set means "read and write", and a long expiry is capped.
	token, _ := issueToken(t, service, APITokenInput{Name: "default", Lifetime: 1000 * 24 * time.Hour})
	if scopes := ScopesOf(*token); len(scopes) != 2 || !HasScope(scopes, models.ScopeWrite) {
		t.Fatalf("scopes = %v, want read and write by default", scopes)
	}
	if token.ExpiresAt == nil || token.ExpiresAt.Sub(token.CreatedAt) != TokenMaxLifetime {
		t.Fatalf("expiry = %v, want the %s cap", token.ExpiresAt, TokenMaxLifetime)
	}
	// A lifetime of zero means "never expires".
	permanent, _ := issueToken(t, service, APITokenInput{Name: "permanent"})
	if permanent.ExpiresAt != nil {
		t.Fatalf("expiry = %v, want no expiry at all", permanent.ExpiresAt)
	}

	// An expired token is refused: the clock is moved past its expiry.
	short, secret := issueToken(t, service, APITokenInput{Name: "short", Lifetime: time.Minute})
	expiry := short.ExpiresAt
	service.now = func() time.Time { return expiry.Add(time.Second) }
	if _, err := service.Authenticate(ctx, secret, "192.0.2.7"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("an expired token: err = %v, want ErrUnauthorized", err)
	}
	// The row stays in the table (the API reports it as expired).
	if _, err := store.GetAPIToken(ctx, short.ID); err != nil {
		t.Fatalf("the expired token must stay in the table: %v", err)
	}
}

// TestAPITokenScopeHelpers checks the scope vocabulary and the token shape parser.
func TestAPITokenScopeHelpers(t *testing.T) {
	if !HasScope([]string{models.ScopeWrite}, models.ScopeRead) {
		t.Fatal("a write token must also read")
	}
	if HasScope([]string{models.ScopeRead}, models.ScopeWrite) {
		t.Fatal("a read-only token must not write")
	}
	if HasScope([]string{"ignored"}, models.ScopeWrite) {
		t.Fatal("an unknown granted scope must not satisfy a request")
	}

	// A row whose scope document cannot be read falls back to read-only.
	if scopes := ScopesOf(models.APIToken{ID: 1, Scopes: "{broken"}); len(scopes) != 1 || scopes[0] != models.ScopeRead {
		t.Fatalf("scopes = %v, want the read-only fallback", scopes)
	}

	prefix, secret, ok := splitToken("sync_01234567.abc")
	if !ok || prefix != "01234567" || secret != "abc" {
		t.Fatalf("splitToken = %q/%q/%t, want the two halves", prefix, secret, ok)
	}
	for _, value := range []string{"", "sync_", "sync_01234567", "sync_.abc", "Bearer sync_01234567.abc"} {
		if _, _, ok := splitToken(value); ok {
			t.Fatalf("splitToken(%q) reported a valid token", value)
		}
	}
}
