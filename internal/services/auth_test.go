package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/models"
)

// testConfig builds a configuration usable by the services without touching the
// process environment (the master key is installed directly).
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{
		AppURL:          "https://sync.example.com",
		AppPort:         3000,
		LogLevel:        "error",
		AuthMethod:      models.AuthModeAccount,
		AccountLogin:    "admin@example.com",
		AccountPassword: "correct-horse-battery",
		DefaultLocale:   "en-US",
		DefaultTheme:    "system",
	}
	if err := cfg.SetEncryptionKey("0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("installing the encryption key: %v", err)
	}
	return cfg
}

func TestSessionRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	auth := NewAuthService(cfg, nil)

	session, err := auth.Session(Identity{Subject: "account:admin", Email: "admin@example.com", Name: "Admin"})
	if err != nil {
		t.Fatalf("Session() error = %v", err)
	}
	if session.Value == "" || !strings.Contains(session.Value, ".") {
		t.Fatalf("session value = %q", session.Value)
	}
	if !session.Identity.Authenticated || !session.Identity.Admin {
		t.Fatalf("identity = %+v", session.Identity)
	}

	identity, ok := auth.FromCookie(session.Value)
	if !ok {
		t.Fatal("the session cookie must verify")
	}
	if identity.Email != "admin@example.com" || identity.Method != models.AuthModeAccount {
		t.Fatalf("identity = %+v", identity)
	}
	if !identity.ExpiresAt.After(time.Now()) {
		t.Fatalf("expires at = %v", identity.ExpiresAt)
	}
}

func TestFromCookieRejectsBadInput(t *testing.T) {
	auth := NewAuthService(testConfig(t), nil)
	session, err := auth.Session(Identity{Subject: "account:admin"})
	if err != nil {
		t.Fatalf("Session() error = %v", err)
	}

	cases := map[string]string{
		"empty":        "",
		"garbage":      "not-a-cookie",
		"no signature": session.Value[:strings.Index(session.Value, ".")+1],
		"tampered":     "x" + session.Value,
	}
	for name, value := range cases {
		if _, ok := auth.FromCookie(value); ok {
			t.Errorf("%s: the cookie must not verify (%q)", name, value)
		}
	}

	// A session signed with another master key is refused.
	other := testConfig(t)
	if err := other.SetEncryptionKey("fedcba9876543210fedcba9876543210"); err != nil {
		t.Fatalf("installing the key: %v", err)
	}
	foreign := NewAuthService(other, nil)
	foreignSession, err := foreign.Session(Identity{Subject: "account:admin"})
	if err != nil {
		t.Fatalf("Session() error = %v", err)
	}
	if _, ok := auth.FromCookie(foreignSession.Value); ok {
		t.Fatal("a cookie signed with another key must be refused")
	}
}

func TestFromCookieHonoursExpiryAndMode(t *testing.T) {
	cfg := testConfig(t)
	auth := NewAuthService(cfg, nil)
	session, err := auth.Session(Identity{Subject: "account:admin"})
	if err != nil {
		t.Fatalf("Session() error = %v", err)
	}
	// Move the clock past the TTL: the same cookie must be refused.
	auth.now = func() time.Time { return time.Now().UTC().Add(SessionTTL + time.Minute) }
	if _, ok := auth.FromCookie(session.Value); ok {
		t.Fatal("an expired session must not verify")
	}

	// A session issued for another authentication mode stops being valid.
	auth.now = func() time.Time { return time.Now().UTC() }
	fresh, err := auth.Session(Identity{Subject: "account:admin"})
	if err != nil {
		t.Fatalf("Session() error = %v", err)
	}
	cfg.AuthMethod = models.AuthModeKeycloak
	if _, ok := auth.FromCookie(fresh.Value); ok {
		t.Fatal("a session issued for another mode must not verify")
	}
}

func TestLoginAccountMode(t *testing.T) {
	cfg := testConfig(t)
	auth := NewAuthService(cfg, nil)
	ctx := context.Background()

	session, err := auth.Login(ctx, "admin@example.com", "correct-horse-battery", "", "10.0.0.1")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	identity, ok := auth.FromCookie(session.Value)
	if !ok {
		t.Fatal("the login must return a usable session")
	}
	if identity.Email != "admin@example.com" || !identity.Admin {
		t.Fatalf("identity = %+v", identity)
	}

	_, err = auth.Login(ctx, "admin@example.com", "wrong", "", "10.0.0.2")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong password error = %v, want ErrUnauthorized", err)
	}
	_, err = auth.Login(ctx, "other@example.com", "correct-horse-battery", "", "10.0.0.3")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong login error = %v, want ErrUnauthorized", err)
	}
	// Surrounding spaces in the login field are tolerated, the password verbatim.
	if _, err := auth.Login(ctx, "  admin@example.com  ", "correct-horse-battery", "", "10.0.0.4"); err != nil {
		t.Fatalf("Login() with a padded login error = %v", err)
	}
	if got := maskLogin("admin@example.com"); got != "a***@example.com" {
		t.Fatalf("maskLogin() = %q", got)
	}
	if got := maskLogin("noseparator"); got != "noseparator" {
		t.Fatalf("maskLogin(plain) = %q", got)
	}
}

func TestLoginThrottlesOneAddress(t *testing.T) {
	auth := NewAuthService(testConfig(t), nil)
	ctx := context.Background()
	now := time.Now().UTC()
	auth.now = func() time.Time { return now }

	for i := 0; i < LoginAttempts; i++ {
		if _, err := auth.Login(ctx, "admin@example.com", "wrong", "", "10.1.1.1"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("attempt %d error = %v, want ErrUnauthorized", i+1, err)
		}
	}
	// The budget is spent: even the right password is refused now.
	if _, err := auth.Login(ctx, "admin@example.com", "correct-horse-battery", "", "10.1.1.1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("blocked error = %v, want ErrForbidden", err)
	}
	// Another address is unaffected.
	if _, err := auth.Login(ctx, "admin@example.com", "correct-horse-battery", "", "10.1.1.2"); err != nil {
		t.Fatalf("a different address must still log in: %v", err)
	}
	// Once the block window elapsed the address may try again.
	now = now.Add(LoginBlockWindow + time.Minute)
	if _, err := auth.Login(ctx, "admin@example.com", "correct-horse-battery", "", "10.1.1.1"); err != nil {
		t.Fatalf("after the block window: %v", err)
	}
	// A successful login clears the counter of that address.
	if _, err := auth.Login(ctx, "admin@example.com", "correct-horse-battery", "", "10.1.1.2"); err != nil {
		t.Fatalf("repeat login: %v", err)
	}
	if _, err := auth.Login(ctx, "admin@example.com", "wrong", "", "10.1.1.2"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("first failure after a success = %v, want ErrUnauthorized", err)
	}
}

func TestLoginIsRefusedInOtherModes(t *testing.T) {
	cfg := testConfig(t)
	cfg.AuthMethod = models.AuthModeKeycloak
	auth := NewAuthService(cfg, nil)
	if _, err := auth.Login(context.Background(), "admin@example.com", "correct-horse-battery", "", "10.2.2.2"); !errors.Is(err, ErrValidation) {
		t.Fatalf("Login() in keycloak mode = %v, want ErrValidation", err)
	}
}

func TestLoginRequiresCaptchaWhenEnabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.RecaptchaClientID = "site-key"
	cfg.RecaptchaClientSecret = "server-key"
	auth := NewAuthService(cfg, nil)

	if !auth.CaptchaEnabled() || auth.CaptchaSiteKey() != "site-key" {
		t.Fatalf("captcha state = %v / %q", auth.CaptchaEnabled(), auth.CaptchaSiteKey())
	}
	_, err := auth.Login(context.Background(), "admin@example.com", "correct-horse-battery", "  ", "10.3.3.3")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("missing captcha token error = %v, want ErrValidation", err)
	}
	// A half configured pair turns the challenge off again.
	cfg.RecaptchaClientSecret = ""
	if auth.CaptchaEnabled() || auth.CaptchaSiteKey() != "" {
		t.Fatalf("a half configured captcha must be off: %v / %q", auth.CaptchaEnabled(), auth.CaptchaSiteKey())
	}
	if _, err := auth.Login(context.Background(), "admin@example.com", "correct-horse-battery", "", "10.3.3.4"); err != nil {
		t.Fatalf("Login() without captcha error = %v", err)
	}
}

func TestAuthModesAndOpenIdentity(t *testing.T) {
	cfg := testConfig(t)
	cfg.AuthMethod = models.AuthModeNone
	auth := NewAuthService(cfg, nil)
	if !auth.Open() || auth.Mode() != models.AuthModeNone || auth.ModeName() != "none" {
		t.Fatalf("mode = %v / %q / open=%v", auth.Mode(), auth.ModeName(), auth.Open())
	}
	identity := auth.OpenIdentity()
	if !identity.Authenticated || !identity.Admin || identity.Email != "" {
		t.Fatalf("open identity = %+v", identity)
	}
	if identity.Method != models.AuthModeNone {
		t.Fatalf("open identity method = %v", identity.Method)
	}
	if _, ok := auth.FromCookie(""); ok {
		t.Fatal("an empty cookie must not authenticate in none mode either")
	}
}

func TestAllowedAccount(t *testing.T) {
	cases := []struct {
		name   string
		list   []string
		email  string
		expect bool
	}{
		{"wildcard", []string{"*"}, "anyone@anywhere.io", true},
		{"exact", []string{"ops@example.com"}, "ops@example.com", true},
		{"exact case", []string{"ops@example.com"}, "OPS@Example.com", true},
		{"other account", []string{"ops@example.com"}, "dev@example.com", false},
		{"domain", []string{"example.com"}, "dev@example.com", true},
		{"domain with at", []string{"@example.com"}, "dev@example.com", true},
		{"other domain", []string{"example.com"}, "dev@other.com", false},
		{"mixed list", []string{"ops@example.com", "corp.io"}, "dev@corp.io", true},
		{"spaces", []string{"  example.com  "}, "dev@example.com", true},
		{"blank entries", []string{"", "   "}, "dev@example.com", false},
		{"empty list", nil, "dev@example.com", false},
		{"empty email", []string{"*"}, "   ", false},
		{"no domain", []string{"example.com"}, "root", false},
	}
	for _, tc := range cases {
		if got := allowedAccount(tc.list, tc.email); got != tc.expect {
			t.Errorf("%s: allowedAccount(%v, %q) = %v, want %v", tc.name, tc.list, tc.email, got, tc.expect)
		}
	}

	cfg := testConfig(t)
	cfg.Keycloak = config.Keycloak{BaseURL: "https://sso.example.com", Realm: "sync", Accounts: []string{"corp.io"}}
	auth := NewAuthService(cfg, nil)
	if !auth.Allowed("dev@corp.io") || auth.Allowed("dev@elsewhere.io") {
		t.Fatal("AuthService.Allowed must follow the configured list")
	}
	if list := auth.AllowedList(); len(list) != 1 {
		t.Fatalf("AllowedList() = %v", list)
	}
	list := auth.AllowedList()
	list[0] = "evil.io"
	if auth.Allowed("evil.io") || !auth.Allowed("dev@corp.io") {
		t.Fatal("AllowedList must hand out a copy")
	}
}

func TestKeycloakGuardsAndDiscoveryFailure(t *testing.T) {
	cfg := testConfig(t)
	auth := NewAuthService(cfg, nil)
	ctx := context.Background()

	if auth.KeycloakConfigured() {
		t.Fatal("an account mode instance must not report Keycloak as configured")
	}
	if _, err := auth.KeycloakBegin(ctx, "/"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("KeycloakBegin() = %v, want ErrNotConfigured", err)
	}
	if _, err := auth.KeycloakComplete(ctx, "code", "state"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("KeycloakComplete() = %v, want ErrNotConfigured", err)
	}

	// Configured, but the realm is unreachable: the error must be explicit and
	// must not be mistaken for a credential refusal.
	cfg.AuthMethod = models.AuthModeKeycloak
	cfg.Keycloak = config.Keycloak{
		BaseURL:      "http://127.0.0.1:1",
		Realm:        "sync",
		ClientID:     "sync",
		ClientSecret: "secret",
		RedirectURI:  "https://sync.example.com/api/auth/callback",
		Accounts:     []string{"*"},
	}
	if !auth.KeycloakConfigured() {
		t.Fatal("KeycloakConfigured() must be true once every field is set")
	}
	if _, err := auth.KeycloakBegin(ctx, "/settings"); err == nil {
		t.Fatal("KeycloakBegin() must fail when the realm is unreachable")
	} else if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrForbidden) {
		t.Fatalf("an unreachable realm must not look like a credential problem: %v", err)
	}
	if _, err := auth.discovery(ctx); err == nil {
		t.Fatal("discovery() must fail when the realm is unreachable")
	}
}
