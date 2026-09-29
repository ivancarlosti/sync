package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/database"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// Credential sources reported to the UI.
const (
	// SourceEnvironment means the values come from the container environment.
	SourceEnvironment = "environment"
	// SourceDatabase means an operator entered them in Admin > Providers.
	SourceDatabase = "database"
	// SourceNone means the provider cannot be used yet.
	SourceNone = "none"
)

// AdminConsentStatus records the tenant-wide consent granted to the OAuth client
// of a provider (Microsoft Entra "admin consent"). Google has no equivalent
// endpoint, so the value stays empty there.
type AdminConsentStatus struct {
	// Tenant is the directory the administrator consented for.
	Tenant string `json:"tenant,omitempty"`
	// ClientID is the application the consent was granted to: consent never
	// carries over to another app registration.
	ClientID string `json:"client_id,omitempty"`
	// At is when the consent was granted.
	At *time.Time `json:"at,omitempty"`
	// Granted is computed against the client id configured right now, so a
	// stale record (the operator replaced the app registration) reports false.
	Granted bool `json:"granted"`
}

// ProviderCredentialsInfo is the masked view of a provider configuration used
// by Admin > Providers: it never contains the client secret itself.
type ProviderCredentialsInfo struct {
	Provider    models.ProviderName `json:"provider"`
	ClientID    string              `json:"client_id"`
	SecretSet   bool                `json:"secret_set"`
	RedirectURI string              `json:"redirect_uri"`
	TenantID    string              `json:"tenant_id,omitempty"`
	Source      string              `json:"source"`
	Configured  bool                `json:"configured"`
	// AdminConsent is the tenant-wide consent status, for the providers that
	// need one (Microsoft). It is omitted for the others.
	AdminConsent *AdminConsentStatus `json:"admin_consent,omitempty"`
}

// ProviderSettings resolves the OAuth client of a provider. The environment is
// the base layer, the `provider.<name>.*` settings are an override entered at
// runtime (Admin > Providers), and the client secret is always encrypted
// before it reaches the database.
type ProviderSettings struct {
	cfg      *config.Config
	settings *database.Settings
	box      *SecretBox
}

// NewProviderSettings builds the resolver.
func NewProviderSettings(cfg *config.Config, settings *database.Settings, box *SecretBox) *ProviderSettings {
	return &ProviderSettings{cfg: cfg, settings: settings, box: box}
}

// settingKey builds the settings key of one provider field.
func settingKey(provider models.ProviderName, field string) string {
	return models.ProviderConfigPrefix + string(provider) + "." + field
}

// static returns the environment credentials of a provider.
func (p *ProviderSettings) static(provider models.ProviderName) config.ProviderCredentials {
	switch provider {
	case models.ProviderGoogle:
		return p.cfg.Google
	case models.ProviderMicrosoft:
		return p.cfg.Microsoft
	default:
		return config.ProviderCredentials{}
	}
}

// Credentials returns the credentials to use for a provider: the environment
// values with any database override applied. It returns ErrNotConfigured when
// the pair is still incomplete, so callers can answer with an actionable error
// instead of starting a flow that is bound to fail.
func (p *ProviderSettings) Credentials(ctx context.Context, provider models.ProviderName) (providers.Credentials, error) {
	if !provider.Valid() {
		return providers.Credentials{}, fmt.Errorf("%w: unknown provider %q", ErrNotFound, provider)
	}
	base := p.static(provider)
	creds := providers.Credentials{
		ClientID:     base.ClientID,
		ClientSecret: base.ClientSecret,
		// The redirect URI falls back to the one derived from APP_URL when the
		// environment left it empty (see config.OAuthRedirectURL).
		RedirectURI: p.cfg.OAuthRedirectURL(provider),
	}
	if provider == models.ProviderMicrosoft {
		creds.TenantID = p.cfg.MicrosoftTenant
	}

	overrides, err := p.settings.Prefix(ctx, models.ProviderConfigPrefix+string(provider)+".")
	if err != nil {
		return providers.Credentials{}, fmt.Errorf("services: reading %s overrides: %w", provider, err)
	}
	if value := strings.TrimSpace(overrides["client_id"]); value != "" {
		creds.ClientID = value
	}
	if value := strings.TrimSpace(overrides["redirect_uri"]); value != "" {
		creds.RedirectURI = value
	}
	if value := strings.TrimSpace(overrides["tenant_id"]); value != "" && provider == models.ProviderMicrosoft {
		creds.TenantID = value
	}
	if value := strings.TrimSpace(overrides["client_secret"]); value != "" {
		secret, err := p.box.Decrypt(value)
		if err != nil {
			return providers.Credentials{}, fmt.Errorf("services: %s client secret cannot be decrypted: %w", provider, err)
		}
		creds.ClientSecret = secret
	}

	if creds.ClientID == "" && creds.ClientSecret == "" {
		return providers.Credentials{}, fmt.Errorf("%w: %s has no OAuth client (set the environment variables or use Admin > Providers)",
			ErrNotConfigured, provider)
	}
	if !creds.Configured() {
		return providers.Credentials{}, fmt.Errorf("%w: %s OAuth client is incomplete (client id, client secret and redirect URI are required)",
			ErrNotConfigured, provider)
	}
	return creds, nil
}

// Info returns the masked configuration of a provider for the admin screen.
func (p *ProviderSettings) Info(ctx context.Context, provider models.ProviderName) (ProviderCredentialsInfo, error) {
	info := ProviderCredentialsInfo{Provider: provider, Source: SourceNone}
	base := p.static(provider)
	// The redirect URI does not depend on the OAuth client: it is derived from
	// APP_URL, so it is known — and reported — before an operator creates the
	// application, which is the state the setup guide starts from.
	info.RedirectURI = p.cfg.OAuthRedirectURL(provider)
	if base.ClientID != "" {
		info.Source = SourceEnvironment
		info.ClientID = base.ClientID
		info.SecretSet = base.ClientSecret != ""
	}
	if provider == models.ProviderMicrosoft {
		info.TenantID = p.cfg.MicrosoftTenant
	}
	status, err := p.AdminConsent(ctx, provider)
	if err != nil {
		return info, err
	}
	if status.At != nil {
		info.AdminConsent = &status
	}

	overrides, err := p.settings.Prefix(ctx, models.ProviderConfigPrefix+string(provider)+".")
	if err != nil {
		return info, fmt.Errorf("services: reading %s overrides: %w", provider, err)
	}
	if len(overrides) > 0 {
		info.Source = SourceDatabase
	}
	if value := strings.TrimSpace(overrides["client_id"]); value != "" {
		info.ClientID = value
	}
	if value := strings.TrimSpace(overrides["redirect_uri"]); value != "" {
		info.RedirectURI = value
	}
	if value := strings.TrimSpace(overrides["tenant_id"]); value != "" && provider == models.ProviderMicrosoft {
		info.TenantID = value
	}
	if value := strings.TrimSpace(overrides["client_secret"]); value != "" {
		info.SecretSet = true
	}
	info.Configured = info.ClientID != "" && info.SecretSet && info.RedirectURI != ""
	return info, nil
}

// Update stores a provider override. An empty clientSecret keeps the stored
// one, which is what the UI needs to edit the other fields without retyping the
// secret; use Clear to remove the override entirely.
func (p *ProviderSettings) Update(ctx context.Context, provider models.ProviderName, clientID, clientSecret, redirectURI, tenantID string) error {
	if !provider.Valid() {
		return fmt.Errorf("%w: unknown provider %q", ErrValidation, provider)
	}
	values := map[string]string{}
	if value := strings.TrimSpace(clientID); value != "" {
		values[settingKey(provider, "client_id")] = value
	}
	if value := strings.TrimSpace(redirectURI); value != "" {
		values[settingKey(provider, "redirect_uri")] = value
	}
	if value := strings.TrimSpace(tenantID); value != "" && provider == models.ProviderMicrosoft {
		values[settingKey(provider, "tenant_id")] = value
	}
	if clientSecret != "" {
		sealed, err := p.box.Encrypt(clientSecret)
		if err != nil {
			return err
		}
		values[settingKey(provider, "client_secret")] = sealed
	}
	if len(values) == 0 {
		return fmt.Errorf("%w: nothing to update for %s", ErrValidation, provider)
	}
	if err := p.settings.SetMany(ctx, values); err != nil {
		return fmt.Errorf("services: saving %s credentials: %w", provider, err)
	}
	return nil
}

// AdminConsent returns the recorded tenant-wide consent of a provider. The
// Granted flag is computed against the client id configured right now, so a
// record left behind by a replaced app registration reports false.
func (p *ProviderSettings) AdminConsent(ctx context.Context, provider models.ProviderName) (AdminConsentStatus, error) {
	status := AdminConsentStatus{}
	raw, found, err := p.settings.Get(ctx, settingKey(provider, models.SettingAdminConsentSuffix))
	if err != nil {
		return status, fmt.Errorf("services: reading the %s consent: %w", provider, err)
	}
	if !found || strings.TrimSpace(raw) == "" {
		return status, nil
	}
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		// A malformed record is not worth failing a screen: the operator simply
		// grants the consent again.
		return AdminConsentStatus{}, nil
	}
	if creds, err := p.Credentials(ctx, provider); err == nil {
		status.Granted = status.ClientID != "" && strings.EqualFold(status.ClientID, creds.ClientID)
	}
	return status, nil
}

// SetAdminConsent records that an administrator granted the provider's
// permissions for a whole tenant. Only non-secret facts are stored: the tenant,
// the application it was granted to and the moment it happened.
func (p *ProviderSettings) SetAdminConsent(ctx context.Context, provider models.ProviderName, tenant string) error {
	creds, err := p.Credentials(ctx, provider)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	status := AdminConsentStatus{
		Tenant:   strings.TrimSpace(tenant),
		ClientID: creds.ClientID,
		At:       &now,
	}
	payload, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("services: encoding the %s consent: %w", provider, err)
	}
	if err := p.settings.Set(ctx, settingKey(provider, models.SettingAdminConsentSuffix), string(payload)); err != nil {
		return fmt.Errorf("services: saving the %s consent: %w", provider, err)
	}
	return nil
}

// The recorded tenant-wide consent is deliberately kept: it belongs to the app
// registration, not to the credentials, and it is only reported as granted while
// the configured client id matches it.
func (p *ProviderSettings) Clear(ctx context.Context, provider models.ProviderName) error {
	keys := []string{
		settingKey(provider, "client_id"),
		settingKey(provider, "client_secret"),
		settingKey(provider, "redirect_uri"),
		settingKey(provider, "tenant_id"),
	}
	if err := p.settings.Delete(ctx, keys...); err != nil {
		return fmt.Errorf("services: clearing %s credentials: %w", provider, err)
	}
	return nil
}
