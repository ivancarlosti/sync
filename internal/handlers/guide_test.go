package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/providers/google"
	"github.com/ivancarlosti/sync/internal/services"
)

// configuredOptions installs a complete OAuth client for both providers, which is
// what the setup guide and the admin-consent flow need in order to answer.
func configuredOptions() testOptions {
	return testOptions{configure: func(cfg *config.Config) {
		cfg.Google = config.ProviderCredentials{
			ClientID:     "google-client",
			ClientSecret: "google-secret",
			RedirectURI:  cfg.AppURL + "/api/oauth/google/callback",
		}
		cfg.Microsoft = config.ProviderCredentials{
			ClientID:     "microsoft-client",
			ClientSecret: "microsoft-secret",
			RedirectURI:  cfg.AppURL + "/api/oauth/microsoft/callback",
		}
		cfg.MicrosoftTenant = "common"
	}}
}

// guideAnswer is the part of the guide answer the endpoint tests inspect.
type guideAnswer struct {
	Provider             models.ProviderName    `json:"provider"`
	Configured           bool                   `json:"configured"`
	RedirectURI          string                 `json:"redirect_uri"`
	Permissions          []providers.Permission `json:"permissions"`
	Scopes               []string               `json:"scopes"`
	Capabilities         []providers.Capability `json:"capabilities"`
	AdminConsentRequired bool                   `json:"admin_consent_required"`
	Steps                []services.GuideStep   `json:"steps"`
	Warnings             []string               `json:"warnings"`
}

// TestAdminConsentEndpoint covers the tenant-wide consent flow: it is refused for
// a provider without one, it answers the identity platform URL, and both outcomes
// of the callback land on the setup guide with a translatable code.
func TestAdminConsentEndpoint(t *testing.T) {
	server, _ := newTestServerWith(t, models.AuthModeNone, configuredOptions())

	// Google has no tenant-wide consent step.
	recorder := call(t, server, http.MethodPost, "/api/oauth/google/admin-consent", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)

	// A refused consent consumes the state and reports it on the guide.
	recorder = call(t, server, http.MethodPost, "/api/oauth/microsoft/admin-consent", map[string]any{
		"redirect_to": "/admin/guide/microsoft",
	})
	requireStatus(t, recorder, http.StatusOK)
	refused := decodeJSON[services.Authorization](t, recorder)
	// `organizations` (not `common`): the consent endpoint does not accept the
	// generic tenant that accepts personal accounts at sign-in.
	if !strings.Contains(refused.URL, "/organizations/v2.0/adminconsent?") ||
		!strings.Contains(refused.URL, "client_id=microsoft-client") {
		t.Fatalf("consent URL = %q", refused.URL)
	}
	recorder = call(t, server, http.MethodGet,
		"/api/oauth/microsoft/callback?error=access_denied&error_description=refused&state="+refused.State, nil)
	requireStatus(t, recorder, http.StatusFound)
	location := recorder.Result().Header.Get("Location")
	if !strings.Contains(location, "/admin/guide/microsoft") ||
		!strings.Contains(location, "consent_error=denied") {
		t.Fatalf("location = %q, want the guide with the refusal", location)
	}

	// A granted consent is recorded and then reported by the provider endpoint.
	recorder = call(t, server, http.MethodPost, "/api/oauth/microsoft/admin-consent", map[string]any{
		"redirect_to": "/admin/guide/microsoft",
	})
	requireStatus(t, recorder, http.StatusOK)
	granted := decodeJSON[services.Authorization](t, recorder)
	recorder = call(t, server, http.MethodGet,
		"/api/oauth/microsoft/callback?admin_consent=True&tenant=contoso.onmicrosoft.com&state="+granted.State, nil)
	requireStatus(t, recorder, http.StatusFound)
	location = recorder.Result().Header.Get("Location")
	if !strings.Contains(location, "consent=granted") {
		t.Fatalf("location = %q, want the granted outcome", location)
	}

	recorder = call(t, server, http.MethodGet, "/api/providers/microsoft", nil)
	requireStatus(t, recorder, http.StatusOK)
	info := decodeJSON[services.ProviderCredentialsInfo](t, recorder)
	if info.AdminConsent == nil || !info.AdminConsent.Granted ||
		info.AdminConsent.Tenant != "contoso.onmicrosoft.com" {
		t.Fatalf("admin_consent = %+v", info.AdminConsent)
	}
}

// TestAccountCapabilitiesAreDerived pins that the accounts endpoint reports what
// an older grant covers: a connection made before the directory scopes existed
// keeps working and is flagged for reconnection.
func TestAccountCapabilitiesAreDerived(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	account := saveAccount(t, store, models.ProviderGoogle, "google-1", "g@example.com")
	account.Scopes = "openid https://www.googleapis.com/auth/userinfo.email " +
		"https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/drive"
	if err := store.SaveAccount(context.Background(), account); err != nil {
		t.Fatalf("saving the account: %v", err)
	}

	recorder := call(t, server, http.MethodGet, "/api/accounts", nil)
	requireStatus(t, recorder, http.StatusOK)
	list := decodeJSON[struct {
		Accounts []accountView `json:"accounts"`
	}](t, recorder)
	if len(list.Accounts) != 1 {
		t.Fatalf("accounts = %+v", list.Accounts)
	}
	older := list.Accounts[0]
	if len(older.Capabilities) != 1 || older.Capabilities[0] != providers.CapabilityFiles {
		t.Fatalf("capabilities = %v, want only files", older.Capabilities)
	}
	if !older.NeedsReconnect || len(older.MissingCapabilities) != len(providers.ProviderCapabilities)-1 {
		t.Fatalf("missing = %v (needs_reconnect=%v)", older.MissingCapabilities, older.NeedsReconnect)
	}

	// A grant of the current scope set satisfies every capability.
	account.Scopes = strings.Join(google.New().Scopes(), " ")
	if err := store.SaveAccount(context.Background(), account); err != nil {
		t.Fatalf("saving the account: %v", err)
	}
	recorder = call(t, server, http.MethodGet, "/api/accounts", nil)
	requireStatus(t, recorder, http.StatusOK)
	list = decodeJSON[struct {
		Accounts []accountView `json:"accounts"`
	}](t, recorder)
	current := list.Accounts[0]
	if current.NeedsReconnect || len(current.MissingCapabilities) != 0 {
		t.Fatalf("missing = %v, want none", current.MissingCapabilities)
	}
	if len(current.Capabilities) != len(providers.ProviderCapabilities) {
		t.Fatalf("capabilities = %v, want every one", current.Capabilities)
	}
}

// TestProviderGuideEndpointWithoutAClient pins the walkthrough an operator opens
// before creating the application: the redirect URI is derived from APP_URL, so
// the answer carries it even though no OAuth client exists yet.
func TestProviderGuideEndpointWithoutAClient(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	for _, name := range []models.ProviderName{models.ProviderGoogle, models.ProviderMicrosoft} {
		recorder := call(t, server, http.MethodGet, "/api/providers/"+string(name)+"/guide", nil)
		requireStatus(t, recorder, http.StatusOK)
		guide := decodeJSON[guideAnswer](t, recorder)
		if guide.Configured {
			t.Fatalf("%s: a provider without an OAuth client was reported as configured", name)
		}
		if want := "https://sync.example.com/api/oauth/" + string(name) + "/callback"; guide.RedirectURI != want {
			t.Fatalf("%s: redirect_uri = %q, want %q", name, guide.RedirectURI, want)
		}
		copies := 0
		for _, step := range guide.Steps {
			if step.Copy == services.GuideCopyRedirectURI {
				copies++
			}
		}
		if copies != 1 {
			t.Fatalf("%s: %d steps ask to copy the redirect URI, want 1", name, copies)
		}
	}
}

// TestProviderGuideEndpoint pins GET /api/providers/:provider/guide: the answer
// carries the redirect URI of this instance, the permission table of the running
// binary and the ordered walkthrough.
func TestProviderGuideEndpoint(t *testing.T) {
	server, _ := newTestServerWith(t, models.AuthModeNone, configuredOptions())

	for _, name := range []models.ProviderName{models.ProviderGoogle, models.ProviderMicrosoft} {
		recorder := call(t, server, http.MethodGet, "/api/providers/"+string(name)+"/guide", nil)
		requireStatus(t, recorder, http.StatusOK)
		guide := decodeJSON[guideAnswer](t, recorder)
		if guide.Provider != name || !guide.Configured {
			t.Fatalf("%s: unexpected guide %+v", name, guide)
		}
		if !strings.HasSuffix(guide.RedirectURI, "/api/oauth/"+string(name)+"/callback") {
			t.Fatalf("%s: redirect_uri = %q", name, guide.RedirectURI)
		}
		if len(guide.Permissions) == 0 || len(guide.Permissions) != len(guide.Scopes) {
			t.Fatalf("%s: the permission table and the scope list differ", name)
		}
		if len(guide.Steps) == 0 || len(guide.Warnings) == 0 {
			t.Fatalf("%s: the walkthrough or the caveats are missing", name)
		}
		if len(guide.Capabilities) != len(providers.ProviderCapabilities) {
			t.Fatalf("%s: capabilities = %v", name, guide.Capabilities)
		}
		if want := name == models.ProviderMicrosoft; guide.AdminConsentRequired != want {
			t.Fatalf("%s: admin_consent_required = %v", name, guide.AdminConsentRequired)
		}
		// The wire value the setup screen switches on when it decides between the
		// whole grant in one field and one field per permission.
		wantCopy := services.GuideCopyScopes
		if name == models.ProviderMicrosoft {
			wantCopy = services.GuideCopyPermissions
		}
		copies := 0
		for _, step := range guide.Steps {
			if step.Copy == wantCopy {
				copies++
			}
		}
		if copies != 1 {
			t.Fatalf("%s: %d steps ask to copy %q, want 1", name, copies, wantCopy)
		}
	}

	// An unknown provider is refused before the service is reached.
	recorder := call(t, server, http.MethodGet, "/api/providers/dropbox/guide", nil)
	requireStatus(t, recorder, http.StatusBadRequest)
	requireErrorCode(t, recorder, codeValidation)
}
