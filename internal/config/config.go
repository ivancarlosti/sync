// Package config loads and validates every environment variable Sync needs.
//
// Boot contract (see docs/development.md): the process reads `.env` (current
// working directory) then the real environment, validates everything, and
// either returns a fully usable Config or an error listing *all* problems at
// once. Nothing else in the code base reads `os.Getenv`, which keeps the
// configuration auditable from a single file.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"

	"github.com/ivancarlosti/sync/internal/crypto"
	"github.com/ivancarlosti/sync/internal/models"
)

// SupportedLocales lists the locales shipped by the SPA.
var SupportedLocales = []string{"en-US", "pt-BR", "es-MX", "fr-FR", "h-CN", "hi-IN", "ar-SA"}

// SupportedThemes lists the theme preferences the SPA understands.
var SupportedThemes = []string{"light", "dark", "system"}

// ProviderCredentials holds the OAuth client of one provider. Values may come
// from the environment or be overridden at runtime through Admin > Providers
// (see services.ProviderSettings).
type ProviderCredentials struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

// Configured reports whether the provider has enough credentials to start an
// OAuth flow.
func (c ProviderCredentials) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// Keycloak holds the OIDC settings used in `keycloak` authentication mode.
type Keycloak struct {
	BaseURL      string
	Realm        string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	// Accounts is the allow-list of e-mails or domains permitted to log in.
	Accounts []string
}

// Issuer returns the OIDC issuer URL of the configured realm.
func (k Keycloak) Issuer() string {
	if k.BaseURL == "" || k.Realm == "" {
		return ""
	}
	return strings.TrimSuffix(k.BaseURL, "/") + "/realms/" + k.Realm
}

// Database holds the external MySQL/MariaDB connection settings.
type Database struct {
	Host     string
	Port     int
	Name     string
	Username string
	Password string
	SSL      bool
}

// Config is the validated runtime configuration.
type Config struct {
	AppURL          string
	AppPort         int
	TrustProxy      bool
	LogLevel        string
	EncryptionKey   string
	AuthMethod      models.AuthMode
	AccountLogin    string
	AccountPassword string

	RecaptchaClientID     string
	RecaptchaClientSecret string

	Database Database
	Keycloak Keycloak

	Google    ProviderCredentials
	Microsoft ProviderCredentials
	// MicrosoftTenant is "common", "organizations", "consumers" or a tenant UUID.
	MicrosoftTenant string

	DefaultLocale string
	DefaultTheme  string
	// Dev enables Vite's dev-server proxy target for `npm run dev` (API only).
	Dev bool

	// key is the parsed AES-256 key. It is unexported and never logged; use
	// EncryptionKeyBytes/SessionKey/StateKey.
	key []byte
}

// Load reads `.env` (best effort) plus the process environment, validates the
// result and derives the values that are computed (redirect URIs).
func Load() (*Config, error) {
	// `.env` is optional: docker-compose passes the variables through env_file.
	_ = godotenv.Load(".env", "docker/.env")

	cfg := &Config{
		AppURL:     strings.TrimRight(envString("APP_URL", ""), "/"),
		AppPort:    envInt("APP_PORT", 3000),
		TrustProxy: envBool("APP_TRUST_PROXY", false),
		LogLevel:   strings.ToLower(envString("LOG_LEVEL", "info")),

		EncryptionKey: envString("ENCRYPTION_KEY", ""),

		AuthMethod:      models.AuthMode(strings.ToLower(envString("AUTH_METHOD", string(models.AuthModeAccount)))),
		AccountLogin:    envString("ACCOUNT_LOGIN", ""),
		AccountPassword: envString("ACCOUNT_PASSWORD", ""),

		RecaptchaClientID:     envString("RECAPTCHA_CLIENTID", ""),
		RecaptchaClientSecret: envString("RECAPTCHA_CLIENTSECRET", ""),

		Database: Database{
			Host:     envString("DB_HOST", ""),
			Port:     envInt("DB_PORT", 3306),
			Name:     envString("DB_DATABASE", ""),
			Username: envString("DB_USERNAME", ""),
			Password: envString("DB_PASSWORD", ""),
			SSL:      envBool("DB_SSL", false),
		},

		Keycloak: Keycloak{
			BaseURL:      strings.TrimRight(envString("KEYCLOAK_BASE_URL", ""), "/"),
			Realm:        envString("KEYCLOAK_REALM", ""),
			ClientID:     envString("KEYCLOAK_CLIENT_ID", ""),
			ClientSecret: envString("KEYCLOAK_CLIENT_SECRET", ""),
			RedirectURI:  envString("KEYCLOAK_REDIRECT_URI", ""),
			Accounts:     splitList(envString("KEYCLOAK_ACCOUNTS", "")),
		},

		Google: ProviderCredentials{
			ClientID:     envString("GOOGLE_CLIENT_ID", ""),
			ClientSecret: envString("GOOGLE_CLIENT_SECRET", ""),
			RedirectURI:  envString("GOOGLE_REDIRECT_URI", ""),
		},
		Microsoft: ProviderCredentials{
			ClientID:     envString("MICROSOFT_CLIENT_ID", ""),
			ClientSecret: envString("MICROSOFT_CLIENT_SECRET", ""),
			RedirectURI:  envString("MICROSOFT_REDIRECT_URI", ""),
		},
		MicrosoftTenant: envString("MICROSOFT_TENANT_ID", "common"),

		DefaultLocale: envString("DEFAULT_LOCALE", "en-US"),
		DefaultTheme:  strings.ToLower(envString("DEFAULT_THEME", "system")),
		Dev:           envBool("APP_DEV", false),
	}

	cfg.derive()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Validation already proved the key is 32 bytes of entropy.
	if err := cfg.SetEncryptionKey(cfg.EncryptionKey); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate reports every configuration problem at once so a misconfigured
// container fails fast with an actionable message.
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.AppURL == "" {
		add("APP_URL is required: public URL of this instance (e.g. https://sync.example.com)")
	} else if parsed, err := url.Parse(c.AppURL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
		add("APP_URL must be an absolute URL including the scheme (got %q)", c.AppURL)
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add(`LOG_LEVEL must be one of "debug", "info", "warn", "error" (got %q)`, c.LogLevel)
	}

	if _, err := crypto.ParseKey(c.EncryptionKey); err != nil {
		add("%v", err)
	}

	if c.Database.Host == "" {
		add("DB_HOST is required (the database is external; docker users usually set host.docker.internal)")
	}
	if c.Database.Name == "" {
		add("DB_DATABASE is required")
	}
	if c.Database.Username == "" {
		add("DB_USERNAME is required")
	}
	if c.Database.Password == "" {
		add("DB_PASSWORD is required (use a real password, even for a local database)")
	}
	if c.Database.Port <= 0 || c.Database.Port > 65535 {
		add("DB_PORT must be between 1 and 65535 (got %d)", c.Database.Port)
	}

	if !c.AuthMethod.Valid() {
		add(`AUTH_METHOD must be one of "none", "account" or "keycloak" (got %q)`, c.AuthMethod)
	}
	switch c.AuthMethod {
	case models.AuthModeAccount:
		if c.AccountLogin == "" {
			add("ACCOUNT_LOGIN is required when AUTH_METHOD=account")
		}
		if len(c.AccountPassword) < 8 {
			add("ACCOUNT_PASSWORD must be at least 8 characters when AUTH_METHOD=account (got %d)", len(c.AccountPassword))
		}
		if (c.RecaptchaClientID == "") != (c.RecaptchaClientSecret == "") {
			add("RECAPTCHA_CLIENTID and RECAPTCHA_CLIENTSECRET must be set together (or both empty to disable the captcha)")
		}
	case models.AuthModeKeycloak:
		if c.Keycloak.BaseURL == "" {
			add("KEYCLOAK_BASE_URL is required when AUTH_METHOD=keycloak")
		}
		if c.Keycloak.Realm == "" {
			add("KEYCLOAK_REALM is required when AUTH_METHOD=keycloak")
		}
		if c.Keycloak.ClientID == "" {
			add("KEYCLOAK_CLIENT_ID is required when AUTH_METHOD=keycloak")
		}
		if c.Keycloak.ClientSecret == "" {
			add("KEYCLOAK_CLIENT_SECRET is required when AUTH_METHOD=keycloak")
		}
		if len(c.Keycloak.Accounts) == 0 {
			add("KEYCLOAK_ACCOUNTS must list at least one e-mail or domain allowed to log in (use * to allow every authenticated user)")
		}
	}

	if c.Google.ClientID != "" && c.Google.ClientSecret == "" {
		add("GOOGLE_CLIENT_SECRET is required because GOOGLE_CLIENT_ID is set")
	}
	if c.Microsoft.ClientID != "" && c.Microsoft.ClientSecret == "" {
		add("MICROSOFT_CLIENT_SECRET is required because MICROSOFT_CLIENT_ID is set")
	}

	if !contains(SupportedLocales, c.DefaultLocale) {
		add("DEFAULT_LOCALE must be one of %s (got %q)", strings.Join(SupportedLocales, ", "), c.DefaultLocale)
	}
	if !contains(SupportedThemes, c.DefaultTheme) {
		add("DEFAULT_THEME must be one of %s (got %q)", strings.Join(SupportedThemes, ", "), c.DefaultTheme)
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration (%d problem(s)):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// Address is the listen address of the HTTP server.
func (c *Config) Address() string { return fmt.Sprintf(":%d", c.AppPort) }

// GoogleConfigured reports whether the Google Drive provider can be used.
func (c *Config) GoogleConfigured() bool { return c.Google.Configured() }

// MicrosoftConfigured reports whether the Microsoft Graph provider can be used.
func (c *Config) MicrosoftConfigured() bool { return c.Microsoft.Configured() }

// KeycloakConfigured reports whether an OIDC login can be started.
func (c *Config) KeycloakConfigured() bool {
	return c.Keycloak.BaseURL != "" && c.Keycloak.Realm != "" &&
		c.Keycloak.ClientID != "" && c.Keycloak.ClientSecret != ""
}

// EncryptionKeyBytes returns the parsed AES-256 key used by internal/crypto.
func (c *Config) EncryptionKeyBytes() []byte { return c.key }

// SetEncryptionKey parses the master key, installs it and keeps the raw value.
// Load() calls it during boot; tests use it to build a usable configuration
// without touching the process environment.
func (c *Config) SetEncryptionKey(raw string) error {
	key, err := crypto.ParseKey(raw)
	if err != nil {
		return err
	}
	c.key = key
	c.EncryptionKey = raw
	return nil
}

// SessionKey returns the HMAC key used to sign the session cookie.
func (c *Config) SessionKey() []byte { return crypto.DeriveKey(c.key, "session") }

// StateKey returns the HMAC key used to sign OAuth `state` values.
func (c *Config) StateKey() []byte { return crypto.DeriveKey(c.key, "state") }

// ---------------------------------------------------------------------------
// Environment helpers
// ---------------------------------------------------------------------------

// envString returns the trimmed value of key, or def when it is absent/empty.
func envString(key, def string) string {
	value, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return def
	}
	return value
}

// envBool parses a boolean flag, falling back to def on a missing value and to
// def plus a warning on a malformed one (validation lists the real problems).
func envBool(key string, def bool) bool {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return def
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return def
	}
	return parsed
}

// envInt parses an integer setting, falling back to def when unset/invalid.
func envInt(key string, def int) int {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return def
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return def
	}
	return parsed
}

// splitList splits a space/comma separated environment list.
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// contains reports whether list holds value.
func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// derive fills every value that is computed instead of configured.
func (c *Config) derive() {
	if c.AppPort <= 0 {
		c.AppPort = 3000
	}
	if c.Database.Port <= 0 {
		c.Database.Port = 3306
	}
	if c.MicrosoftTenant == "" {
		c.MicrosoftTenant = "common"
	}
	callback := func(path string) string { return c.AppURL + path }
	if c.Google.RedirectURI == "" {
		c.Google.RedirectURI = callback("/api/oauth/google/callback")
	}
	if c.Microsoft.RedirectURI == "" {
		c.Microsoft.RedirectURI = callback("/api/oauth/microsoft/callback")
	}
	if c.Keycloak.RedirectURI == "" {
		c.Keycloak.RedirectURI = callback("/api/auth/callback")
	}
}
