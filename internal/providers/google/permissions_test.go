package google

import (
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// wantScopes is the exact permission set this provider requests. It is pinned on
// purpose: widening it is a deliberate change (a stored grant is not retroactive)
// and must be accompanied by the guide, the docs and the i18n catalogs.
var wantScopes = []string{
	"openid",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/drive",
	"https://www.googleapis.com/auth/calendar",
}

// TestScopesMatchThePermissionTable pins Scopes() to the table the guide renders.
func TestScopesMatchThePermissionTable(t *testing.T) {
	provider := New()
	got := provider.Scopes()
	if strings.Join(got, " ") != strings.Join(wantScopes, " ") {
		t.Fatalf("Scopes() = %v, want %v", got, wantScopes)
	}
	if strings.Join(providers.ScopesOf(provider.Permissions()), " ") != strings.Join(wantScopes, " ") {
		t.Fatal("the permission table and Scopes() drifted apart")
	}
}

// TestPermissionsAreCompleteAndUnique protects the invariants the guide and the
// capability badges rely on.
func TestPermissionsAreCompleteAndUnique(t *testing.T) {
	permissions := New().Permissions()
	seen := map[string]bool{}
	for _, permission := range permissions {
		if strings.TrimSpace(permission.Scope) == "" {
			t.Fatal("a permission has an empty scope")
		}
		if seen[permission.Scope] {
			t.Fatalf("scope %q is declared twice", permission.Scope)
		}
		seen[permission.Scope] = true
		if permission.Title == "" {
			t.Fatalf("scope %q has no i18n title", permission.Scope)
		}
	}
	for _, capability := range providers.CapabilitiesFor(models.ProviderGoogle) {
		if !providers.HasCapability(permissions, wantScopes, capability) {
			t.Fatalf("capability %q is not satisfied by the requested set", capability)
		}
	}
}

// TestEveryScopeIsAUserGrant pins that the sync engine needs no administrator
// consent: the file scopes are granted by the user during the flow.
func TestEveryScopeIsAUserGrant(t *testing.T) {
	for _, permission := range New().Permissions() {
		if permission.AdminConsent {
			t.Fatalf("the file scope %q must not require an administrator", permission.Scope)
		}
	}
}

// TestConsoleURLsCoverTheGuideSteps pins the API enablement pages the guide links
// to, so a removed key is caught before the UI renders an empty href.
func TestConsoleURLsCoverTheGuideSteps(t *testing.T) {
	links := New().ConsoleURLs()
	for _, key := range []string{"drive_api"} {
		if !strings.HasPrefix(links[key], "https://console.cloud.google.com/") {
			t.Fatalf("console link %q is missing or not a Cloud console URL: %q", key, links[key])
		}
	}
}
