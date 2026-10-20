package config

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
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

// validConfig returns a configuration that passes Validate. Each test changes
// one field so a failure points at exactly one rule.
func validConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		AppURL:          "https://sync.example.com",
		AppPort:         3000,
		LogLevel:        "info",
		EncryptionKey:   "0123456789abcdef0123456789abcdef",
		AuthMethod:      models.AuthModeAccount,
		AccountLogin:    "operator@example.com",
		AccountPassword: "correct-horse-battery",
		Database: Database{
			Host:     "db.internal",
			Port:     3306,
			Name:     "sync",
			Username: "sync",
			Password: "db-credentials-2026",
		},
		DefaultLocale: "en-US",
		DefaultTheme:  "system",
	}
	cfg.derive()
	if err := cfg.SetEncryptionKey(cfg.EncryptionKey); err != nil {
		t.Fatalf("building the baseline configuration: %v", err)
	}
	return cfg
}

// TestValidateAcceptsTheBaseline guards the other cases: they only mean
// something while an untouched configuration still boots.
func TestValidateAcceptsTheBaseline(t *testing.T) {
	if err := validConfig(t).Validate(); err != nil {
		t.Fatalf("the baseline configuration must validate: %v", err)
	}
}

// TestValidateRejectsPublishedEncryptionKeys pins that the master keys that
// were committed to this repository are refused: they derive the session and
// state keys and decrypt every stored provider token.
func TestValidateRejectsPublishedEncryptionKeys(t *testing.T) {
	for _, key := range publishedEncryptionKeys {
		cfg := validConfig(t)
		cfg.EncryptionKey = key
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "publicly known example value") {
			t.Fatalf("published key %q must be rejected, got %v", key, err)
		}
	}
}

// TestValidateRejectsAReEncodedPublishedKey pins that the decoded bytes are
// compared too: base64 to hex must not slip past the denylist.
func TestValidateRejectsAReEncodedPublishedKey(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(publishedEncryptionKeys[0])
	if err != nil {
		t.Fatalf("decoding the published key: %v", err)
	}
	cfg := validConfig(t)
	cfg.EncryptionKey = hex.EncodeToString(raw)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "publicly known example value") {
		t.Fatalf("a re-encoded published key must be rejected, got %v", err)
	}
}

// TestValidateAccountPasswordPolicy pins the rules for the only credential of
// an `account` instance, including the historic "admin123" that used to pass
// the 8-character check.
func TestValidateAccountPasswordPolicy(t *testing.T) {
	cases := []struct {
		name     string
		password string
		want     string
	}{
		{"empty", "", "ACCOUNT_PASSWORD is required"},
		{"published default", "admin123", "publicly known default"},
		{"published default in another case", "Admin123", "publicly known default"},
		{"too short", "short-pass1", "at least 12 characters"},
		{"same as login", "operator@example.com", "must differ from ACCOUNT_LOGIN"},
		{"strong", "correct-horse-battery", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.AccountPassword = tc.password
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("a strong password must be accepted: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("password %q must fail with %q, got %v", tc.password, tc.want, err)
			}
		})
	}
}

// TestValidateRejectsPublishedSecretPlaceholders pins that the other secrets
// shipped in the templates are rejected as well.
func TestValidateRejectsPublishedSecretPlaceholders(t *testing.T) {
	t.Run("database password", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Database.Password = "secret"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "DB_PASSWORD is a publicly known default") {
			t.Fatalf("the shipped database password must be rejected, got %v", err)
		}
	})

	t.Run("keycloak client secret", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.AuthMethod = models.AuthModeKeycloak
		cfg.Keycloak = Keycloak{
			BaseURL:      "https://sso.example.com",
			Realm:        "sync",
			ClientID:     "sync",
			ClientSecret: "YOURSECRET",
			Accounts:     []string{"operator@example.com"},
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "KEYCLOAK_CLIENT_SECRET is a publicly known placeholder") {
			t.Fatalf("the shipped Keycloak secret must be rejected, got %v", err)
		}
	})

	t.Run("provider client secret", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Google = ProviderCredentials{
			ClientID:     "client-id",
			ClientSecret: "YOURSECRET",
			RedirectURI:  "https://sync.example.com/api/oauth/google/callback",
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "GOOGLE_CLIENT_SECRET is a publicly known placeholder") {
			t.Fatalf("the shipped provider placeholder must be rejected, got %v", err)
		}
	})

	t.Run("recaptcha secret", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.RecaptchaClientID = "site-key"
		cfg.RecaptchaClientSecret = "changeme"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "RECAPTCHA_CLIENTSECRET is a publicly known placeholder") {
			t.Fatalf("a placeholder captcha secret must be rejected, got %v", err)
		}
	})
}

// TestExampleEnvironmentShipsNoSecrets is the regression guard for the actual
// incident: the template used to carry a base64url string that ParseKey
// accepts as a 32-byte master key, plus admin123 and the database password
// "secret". Every secret in the template must stay empty, so an unedited copy
// fails validation instead of running with public credentials.
func TestExampleEnvironmentShipsNoSecrets(t *testing.T) {
	raw, err := os.ReadFile("../../docker/.env.example")
	if err != nil {
		t.Fatalf("reading the environment template: %v", err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	for _, name := range []string{
		"ENCRYPTION_KEY",
		"ACCOUNT_PASSWORD",
		"DB_PASSWORD",
		"KEYCLOAK_CLIENT_SECRET",
		"GOOGLE_CLIENT_SECRET",
		"MICROSOFT_CLIENT_SECRET",
	} {
		if values[name] != "" {
			t.Errorf("%s in docker/.env.example must be empty, got %q", name, values[name])
		}
	}
}
