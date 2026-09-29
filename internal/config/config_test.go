package config

import (
	"testing"

	"github.com/ivancarlosti/sync/internal/models"
)

// TestOAuthCallbackPath pins the callback paths: the setup guide copies the
// absolute URL built from them, so they must stay stable and per provider.
func TestOAuthCallbackPath(t *testing.T) {
	cases := map[models.ProviderName]string{
		models.ProviderGoogle:          "/api/oauth/google/callback",
		models.ProviderMicrosoft:       "/api/oauth/microsoft/callback",
		models.ProviderName("dropbox"): "",
	}
	for provider, want := range cases {
		if got := OAuthCallbackPath(provider); got != want {
			t.Fatalf("OAuthCallbackPath(%s) = %q, want %q", provider, got, want)
		}
	}
}

// TestOAuthRedirectURL pins the value the API reports: the configured redirect
// URI when there is one, the APP_URL derived one otherwise — never empty for a
// shipped provider, even before an OAuth client exists.
func TestOAuthRedirectURL(t *testing.T) {
	cfg := &Config{AppURL: "https://sync.example.com"}
	if got := cfg.OAuthRedirectURL(models.ProviderGoogle); got != "https://sync.example.com/api/oauth/google/callback" {
		t.Fatalf("google = %q", got)
	}
	if got := cfg.OAuthRedirectURL(models.ProviderMicrosoft); got != "https://sync.example.com/api/oauth/microsoft/callback" {
		t.Fatalf("microsoft = %q", got)
	}
	if got := cfg.OAuthRedirectURL(models.ProviderName("dropbox")); got != "" {
		t.Fatalf("an unknown provider must have no redirect URI: %q", got)
	}

	cfg.Google.RedirectURI = "https://other.example.com/callback"
	if got := cfg.OAuthRedirectURL(models.ProviderGoogle); got != "https://other.example.com/callback" {
		t.Fatalf("an explicit redirect URI must win: %q", got)
	}
}

// TestDeriveFillsTheRedirectURIs pins that a configuration loaded from the
// environment always carries the callback URLs, without overwriting the ones an
// operator set explicitly.
func TestDeriveFillsTheRedirectURIs(t *testing.T) {
	cfg := &Config{AppURL: "https://sync.example.com"}
	cfg.derive()

	if cfg.Google.RedirectURI != "https://sync.example.com/api/oauth/google/callback" {
		t.Fatalf("google = %q", cfg.Google.RedirectURI)
	}
	if cfg.Microsoft.RedirectURI != "https://sync.example.com/api/oauth/microsoft/callback" {
		t.Fatalf("microsoft = %q", cfg.Microsoft.RedirectURI)
	}
	if cfg.Keycloak.RedirectURI != "https://sync.example.com/api/auth/callback" {
		t.Fatalf("keycloak = %q", cfg.Keycloak.RedirectURI)
	}
	if cfg.AppPort != 3000 || cfg.Database.Port != 3306 || cfg.MicrosoftTenant != "common" {
		t.Fatalf("derive() left a default unset: %+v", cfg)
	}

	cfg.Microsoft.RedirectURI = "https://custom.example.com/callback"
	cfg.derive()
	if cfg.Microsoft.RedirectURI != "https://custom.example.com/callback" {
		t.Fatalf("derive() overwrote an explicit value: %q", cfg.Microsoft.RedirectURI)
	}
}
