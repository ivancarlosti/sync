package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/providers/google"
	"github.com/ivancarlosti/sync/internal/providers/microsoft"
)

// localesDir is the SPA catalog directory, relative to this package. It is read
// by the drift test below so the walkthrough the API describes and the strings
// the UI renders can never be edited apart.
const localesDir = "../../web/src/i18n/locales"

// guideFixture wires a guide service on the in-memory store of this package,
// with both providers configured.
func guideFixture(t *testing.T) (*GuideService, *ProviderSettings) {
	t.Helper()
	store := newTestStore(t)
	cfg := testConfig(t)
	cfg.Google = config.ProviderCredentials{
		ClientID:     "google-client-id",
		ClientSecret: "google-client-secret",
		RedirectURI:  "https://sync.example.com/api/oauth/google/callback",
	}
	cfg.Microsoft = config.ProviderCredentials{
		ClientID:     "microsoft-client-id",
		ClientSecret: "microsoft-client-secret",
		RedirectURI:  "https://sync.example.com/api/oauth/microsoft/callback",
	}
	cfg.MicrosoftTenant = "common"
	creds := NewProviderSettings(cfg, store.Settings(), NewSecretBox(cfg.EncryptionKeyBytes()))
	registry := providers.NewRegistry(google.New(), microsoft.New())
	return NewGuideService(registry, creds), creds
}

// TestGuideDescribesBothProviders pins the shape of the walkthrough: an ordered
// step list whose first steps link to the console, the redirect URI of this
// instance, the permission table of the provider and every capability.
func TestGuideDescribesBothProviders(t *testing.T) {
	guide, _ := guideFixture(t)
	ctx := context.Background()

	for _, name := range []models.ProviderName{models.ProviderGoogle, models.ProviderMicrosoft} {
		built, err := guide.Guide(ctx, name)
		if err != nil {
			t.Fatalf("Guide(%s) error = %v", name, err)
		}
		if !built.Configured {
			t.Fatalf("%s: a configured provider was reported as unconfigured", name)
		}
		if !strings.HasSuffix(built.RedirectURI, "/api/oauth/"+string(name)+"/callback") {
			t.Fatalf("%s: unexpected redirect URI %q", name, built.RedirectURI)
		}
		if len(built.Permissions) != len(built.Scopes) || len(built.Scopes) == 0 {
			t.Fatalf("%s: the permission table and the scope list differ", name)
		}
		if len(built.Capabilities) != len(providers.ProviderCapabilities) {
			t.Fatalf("%s: %v does not cover every capability", name, built.Capabilities)
		}
		if len(built.Steps) < 6 {
			t.Fatalf("%s: %d steps is too short for a registration walkthrough", name, len(built.Steps))
		}
		last := built.Steps[len(built.Steps)-1]
		if last.ID != "connect" {
			t.Fatalf("%s: the walkthrough does not end on the connection step (%q)", name, last.ID)
		}
		for _, step := range built.Steps {
			if step.ID == "" {
				t.Fatalf("%s: a step has no id", name)
			}
		}
		if len(built.Warnings) == 0 {
			t.Fatalf("%s: no caveat was reported", name)
		}
		if name == models.ProviderGoogle && built.AdminConsentRequired {
			t.Fatal("google must not require a tenant-wide consent step")
		}
		if name == models.ProviderMicrosoft && !built.AdminConsentRequired {
			t.Fatal("microsoft must require a tenant-wide consent step")
		}
		if built.ConsoleURLs == nil {
			t.Fatalf("%s: the console links are missing", name)
		}
	}
}

// TestGuideUnknownProvider pins that an unknown provider is a 404 and not an
// empty walkthrough.
func TestGuideUnknownProvider(t *testing.T) {
	guide, _ := guideFixture(t)
	if _, err := guide.Guide(context.Background(), models.ProviderName("dropbox")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Guide() error = %v, want ErrNotFound", err)
	}
}

// TestGuideReportsTheRecordedConsent pins that the guide (and therefore the
// setup screen) reports the tenant-wide consent of this instance.
func TestGuideReportsTheRecordedConsent(t *testing.T) {
	guide, creds := guideFixture(t)
	ctx := context.Background()

	before, err := guide.Guide(ctx, models.ProviderMicrosoft)
	if err != nil {
		t.Fatalf("Guide() error = %v", err)
	}
	if before.AdminConsent.Granted || before.AdminConsent.At != nil {
		t.Fatalf("a fresh instance reported a consent: %+v", before.AdminConsent)
	}
	if err := creds.SetAdminConsent(ctx, models.ProviderMicrosoft, "contoso.onmicrosoft.com"); err != nil {
		t.Fatalf("SetAdminConsent() error = %v", err)
	}
	after, err := guide.Guide(ctx, models.ProviderMicrosoft)
	if err != nil {
		t.Fatalf("Guide() error = %v", err)
	}
	if !after.AdminConsent.Granted {
		t.Fatalf("the recorded consent was not reported: %+v", after.AdminConsent)
	}
	if after.AdminConsent.Tenant != "contoso.onmicrosoft.com" || after.AdminConsent.At == nil {
		t.Fatalf("the consent lost its tenant or timestamp: %+v", after.AdminConsent)
	}
}

// TestAdminConsentBelongsToTheClientItWasGrantedTo pins that a consent never
// carries over to another app registration: replacing the client id makes the
// stored record report not granted.
func TestAdminConsentBelongsToTheClientItWasGrantedTo(t *testing.T) {
	store := newTestStore(t)
	cfg := testConfig(t)
	cfg.Microsoft = config.ProviderCredentials{
		ClientID:     "first-client",
		ClientSecret: "secret",
		RedirectURI:  "https://sync.example.com/api/oauth/microsoft/callback",
	}
	ctx := context.Background()
	creds := NewProviderSettings(cfg, store.Settings(), NewSecretBox(cfg.EncryptionKeyBytes()))

	if err := creds.SetAdminConsent(ctx, models.ProviderMicrosoft, "contoso.onmicrosoft.com"); err != nil {
		t.Fatalf("SetAdminConsent() error = %v", err)
	}
	if status, err := creds.AdminConsent(ctx, models.ProviderMicrosoft); err != nil || !status.Granted {
		t.Fatalf("AdminConsent() = %+v, %v", status, err)
	}
	if err := creds.Update(ctx, models.ProviderMicrosoft, "second-client", "", "", ""); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	status, err := creds.AdminConsent(ctx, models.ProviderMicrosoft)
	if err != nil {
		t.Fatalf("AdminConsent() error = %v", err)
	}
	if status.Granted {
		t.Fatalf("a consent of another client was reported as granted: %+v", status)
	}
	if status.Tenant == "" || status.At == nil {
		t.Fatal("the record should survive a client change as history")
	}
}

// TestInfoReportsTheConsent pins the Admin > Providers payload: the consent is
// only present once it was granted.
func TestInfoReportsTheConsent(t *testing.T) {
	store := newTestStore(t)
	cfg := testConfig(t)
	cfg.Microsoft = config.ProviderCredentials{
		ClientID:     "client",
		ClientSecret: "secret",
		RedirectURI:  "https://sync.example.com/api/oauth/microsoft/callback",
	}
	ctx := context.Background()
	creds := NewProviderSettings(cfg, store.Settings(), NewSecretBox(cfg.EncryptionKeyBytes()))

	info, err := creds.Info(ctx, models.ProviderMicrosoft)
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.AdminConsent != nil {
		t.Fatalf("an unconsented provider reported a consent: %+v", info.AdminConsent)
	}
	if err := creds.SetAdminConsent(ctx, models.ProviderMicrosoft, "contoso.onmicrosoft.com"); err != nil {
		t.Fatalf("SetAdminConsent() error = %v", err)
	}
	info, err = creds.Info(ctx, models.ProviderMicrosoft)
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.AdminConsent == nil || !info.AdminConsent.Granted {
		t.Fatalf("the granted consent is missing from the provider info: %+v", info.AdminConsent)
	}
}

// TestClearKeepsTheConsentRecord pins that clearing the credentials does not lose
// the consent: it belongs to the app registration, not to the secret.
func TestClearKeepsTheConsentRecord(t *testing.T) {
	store := newTestStore(t)
	cfg := testConfig(t)
	cfg.Microsoft = config.ProviderCredentials{
		ClientID:     "client",
		ClientSecret: "secret",
		RedirectURI:  "https://sync.example.com/api/oauth/microsoft/callback",
	}
	ctx := context.Background()
	creds := NewProviderSettings(cfg, store.Settings(), NewSecretBox(cfg.EncryptionKeyBytes()))
	if err := creds.SetAdminConsent(ctx, models.ProviderMicrosoft, "contoso.onmicrosoft.com"); err != nil {
		t.Fatalf("SetAdminConsent() error = %v", err)
	}
	if err := creds.Clear(ctx, models.ProviderMicrosoft); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	status, err := creds.AdminConsent(ctx, models.ProviderMicrosoft)
	if err != nil {
		t.Fatalf("AdminConsent() error = %v", err)
	}
	if status.Tenant == "" {
		t.Fatal("clearing the overrides removed the consent record")
	}
}
