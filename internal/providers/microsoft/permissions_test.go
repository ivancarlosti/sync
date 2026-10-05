package microsoft

import (
	"net/url"
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/providers"
)

// wantScopes is the exact permission set this provider requests, in request
// order. It is pinned on purpose: widening it is a deliberate change (a stored
// grant is not retroactive) and every directory scope needs the tenant-wide
// admin consent.
var wantScopes = []string{
	"offline_access",
	"openid",
	"profile",
	"email",
	"https://graph.microsoft.com/User.Read",
	"https://graph.microsoft.com/Files.ReadWrite.All",
	"https://graph.microsoft.com/Sites.ReadWrite.All",
	"https://graph.microsoft.com/User.Read.All",
	"https://graph.microsoft.com/User.ReadWrite.All",
	"https://graph.microsoft.com/Group.ReadWrite.All",
	"https://graph.microsoft.com/Directory.ReadWrite.All",
	"https://graph.microsoft.com/GroupMember.ReadWrite.All",
	"https://graph.microsoft.com/Domain.Read.All",
	"https://graph.microsoft.com/AdministrativeUnit.ReadWrite.All",
	"https://graph.microsoft.com/RoleManagement.Read.All",
	"https://graph.microsoft.com/Organization.Read.All",
	"https://graph.microsoft.com/LicenseAssignment.ReadWrite.All",
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
			t.Fatalf("a permission has an empty scope: %+v", permission)
		}
		if seen[permission.Scope] {
			t.Fatalf("scope %q is declared twice", permission.Scope)
		}
		seen[permission.Scope] = true
		if permission.Title == "" {
			t.Fatalf("scope %q has no i18n title", permission.Scope)
		}
		if !strings.HasPrefix(permission.Scope, "https://graph.microsoft.com/") &&
			permission.Scope != "offline_access" && permission.Scope != "openid" &&
			permission.Scope != "profile" && permission.Scope != "email" {
			t.Fatalf("scope %q is neither an OIDC scope nor a Graph scope", permission.Scope)
		}
	}
	for _, capability := range providers.ProviderCapabilities {
		if !providers.HasCapability(permissions, wantScopes, capability) {
			t.Fatalf("capability %q is not satisfied by the requested set", capability)
		}
	}
}

// TestDirectoryScopesRequireAdminConsent pins decision 2.3 of the plan: every
// directory permission is an admin consent permission, and the file scopes are
// not (they are granted by the user during the flow).
func TestDirectoryScopesRequireAdminConsent(t *testing.T) {
	for _, permission := range New().Permissions() {
		if permission.Capability == providers.CapabilityFiles {
			if permission.AdminConsent {
				t.Fatalf("the file scope %q must not require an administrator", permission.Scope)
			}
			continue
		}
		if !permission.AdminConsent {
			t.Fatalf("the directory scope %q must require an administrator", permission.Scope)
		}
	}
}

// TestGrantAsEntraReportsItSatisfiesEveryCapability reproduces the token response
// of Microsoft Entra ID: it carries the Graph resource scopes it granted, in the
// percent-encoded form Microsoft documents, and it never echoes the four OpenID
// Connect scopes. A grant like that is complete, so the Accounts screen must not
// tell the operator to reconnect the account.
func TestGrantAsEntraReportsItSatisfiesEveryCapability(t *testing.T) {
	provider := New()
	// An Entra token response carries the Graph resource scopes only: the four
	// OpenID Connect scopes it granted are never reported back, and the values
	// are percent-encoded.
	encoded := []string{}
	for _, permission := range provider.Permissions() {
		if !strings.HasPrefix(permission.Scope, graphResource) {
			continue
		}
		encoded = append(encoded, url.QueryEscape(permission.Scope))
	}
	if len(encoded) == 0 {
		t.Fatal("the table declares no Graph resource scope")
	}
	granted := providers.SplitScopes(strings.Join(encoded, "%20"))
	if len(granted) != len(encoded) {
		t.Fatalf("granted = %v, want %d scopes", granted, len(encoded))
	}
	if missing := providers.MissingCapabilities(provider.Permissions(), granted); len(missing) != 0 {
		t.Fatalf("a grant as Entra reports it is missing %v", missing)
	}
	if got := providers.CapabilitiesOf(provider.Permissions(), granted); len(got) != len(providers.ProviderCapabilities) {
		t.Fatalf("capabilities = %v, want all of them", got)
	}
}

// TestOnlyTheOIDCScopesAreInformational pins which declarations do not prove a
// capability: exactly the four OpenID Connect protocol scopes, because Entra ID
// never reports them back in the token response.
func TestOnlyTheOIDCScopesAreInformational(t *testing.T) {
	informational := map[string]bool{}
	for _, permission := range New().Permissions() {
		if permission.Informational {
			informational[permission.Scope] = true
		}
	}
	want := []string{"offline_access", "openid", "profile", "email"}
	if len(informational) != len(want) {
		t.Fatalf("informational = %v, want %v", informational, want)
	}
	for _, scope := range want {
		if !informational[scope] {
			t.Fatalf("scope %q is not marked informational", scope)
		}
	}
}

// TestAdminConsentURLCarriesTheContract pins the tenant-wide consent URL: the
// consented directory, the client, the registered callback, the same scope list
// as the authorization request and the state that ties the callback to this flow.
func TestAdminConsentURLCarriesTheContract(t *testing.T) {
	provider := New()
	creds := providers.Credentials{
		ClientID:    "11111111-2222-3333-4444-555555555555",
		RedirectURI: "https://sync.example.com/api/oauth/microsoft/callback",
		TenantID:    "contoso.onmicrosoft.com",
	}
	raw := provider.AdminConsentURL(creds, "state-value")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the consent URL is not a URL: %v", err)
	}
	if parsed.Host != "login.microsoftonline.com" || parsed.Path != "/contoso.onmicrosoft.com/v2.0/adminconsent" {
		t.Fatalf("unexpected consent endpoint: %s", parsed.String())
	}
	query := parsed.Query()
	if query.Get("client_id") != creds.ClientID {
		t.Fatalf("client_id = %q", query.Get("client_id"))
	}
	if query.Get("redirect_uri") != creds.RedirectURI {
		t.Fatalf("redirect_uri = %q", query.Get("redirect_uri"))
	}
	if query.Get("state") != "state-value" {
		t.Fatalf("state = %q", query.Get("state"))
	}
	if query.Get("scope") != strings.Join(wantScopes, " ") {
		t.Fatalf("scope = %q", query.Get("scope"))
	}
}

// TestAdminConsentURLRejectsATenantThatNavigates guards the tenant segment the
// URL is built from: a crafted value must fall back to the generic work/school
// directory rather than escape the adminconsent path.
func TestAdminConsentURLRejectsATenantThatNavigates(t *testing.T) {
	raw := New().AdminConsentURL(providers.Credentials{
		ClientID: "app",
		TenantID: "../../malicious",
	}, "state")
	if !strings.Contains(raw, "/"+organizationsTenant+"/v2.0/adminconsent") {
		t.Fatalf("a crafted tenant was not rejected: %s", raw)
	}
}

// TestAdminConsentURLAcceptsEveryDirectory pins the multi-tenant consent: an
// operator that leaves the tenant id empty (or sets one of the generic tenants)
// gets the URL every directory administrator can open for their own directory,
// because Microsoft documents `common` as unsupported by adminconsent.
func TestAdminConsentURLAcceptsEveryDirectory(t *testing.T) {
	provider := New()
	for _, tenant := range []string{"", "common", "organizations", "consumers"} {
		raw := provider.AdminConsentURL(providers.Credentials{ClientID: "app", TenantID: tenant}, "state")
		if !strings.Contains(raw, "/"+organizationsTenant+"/v2.0/adminconsent?") {
			t.Fatalf("tenant %q: unexpected consent endpoint: %s", tenant, raw)
		}
	}
}
