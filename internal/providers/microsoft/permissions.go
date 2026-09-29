package microsoft

import (
	"net/url"
	"strings"

	"github.com/ivancarlosti/sync/internal/providers"
)

// Compile time guard: the setup guide and the capability badges rely on the
// permission table, so a renamed method must fail the build.
var _ providers.ScopeCatalog = (*Provider)(nil)

// permissions is the single source of truth for the Microsoft Graph scopes this
// provider requests. Scopes() builds the authorization request from it, the setup
// guide renders it (GET /api/providers/microsoft/guide) and the Accounts screen
// derives the capability badges from the grant.
//
// Every Microsoft Graph scope in this table is an *admin consent* permission:
// the tenant administrator has to grant it once for the whole directory, which is
// why the provider also implements providers.ConsentGranter (the adminconsent
// flow). A directory operation still requires the acting identity to hold the
// matching Entra role — the grant is what the application may do, not what the
// signed-in user is allowed to do.
//
// Graph scopes are declared in their fully qualified form because that is what
// the token response carries; matching is prefix tolerant (see
// providers.normalizeScope), so both forms of a Graph scope are recognised.
func (p *Provider) Permissions() []providers.Permission {
	return []providers.Permission{
		{Capability: providers.CapabilityFiles, Title: "files", Scope: "offline_access"},
		{Capability: providers.CapabilityFiles, Title: "files", Scope: "openid"},
		{Capability: providers.CapabilityFiles, Title: "files", Scope: "profile"},
		{Capability: providers.CapabilityFiles, Title: "files", Scope: "email"},
		{Capability: providers.CapabilityFiles, Title: "files", Scope: graphScope("User.Read")},
		{Capability: providers.CapabilityFiles, Title: "files", Scope: graphScope("Files.ReadWrite.All")},
		{Capability: providers.CapabilityFiles, Title: "files", Scope: graphScope("Sites.ReadWrite.All")},

		{Capability: providers.CapabilityUsers, Title: "users", Scope: graphScope("User.Read.All"), AdminConsent: true},
		{Capability: providers.CapabilityUsers, Title: "users", Scope: graphScope("User.ReadWrite.All"), AdminConsent: true},

		// Distribution lists are Microsoft 365 groups, so creating and renaming
		// them is a group write; Directory.ReadWrite.All is what allows creating
		// the object and reading the tenant-wide catalogue it lives in.
		{Capability: providers.CapabilityGroups, Title: "groups", Scope: graphScope("Group.ReadWrite.All"), AdminConsent: true},
		{Capability: providers.CapabilityGroups, Title: "groups", Scope: graphScope("Directory.ReadWrite.All"), AdminConsent: true},

		{Capability: providers.CapabilityMembers, Title: "members", Scope: graphScope("GroupMember.ReadWrite.All"), AdminConsent: true},

		{Capability: providers.CapabilityDomains, Title: "domains", Scope: graphScope("Domain.Read.All"), AdminConsent: true},

		// Administrative units are the closest Graph equivalent of the Google
		// organisational units the same screen manages.
		{Capability: providers.CapabilityOrgUnits, Title: "orgunits", Scope: graphScope("AdministrativeUnit.ReadWrite.All"), AdminConsent: true},

		{Capability: providers.CapabilityRoles, Title: "roles", Scope: graphScope("RoleManagement.Read.All"), AdminConsent: true},

		// Organization.Read.All lists the subscribed SKUs; the write scope is what
		// assigns or removes a licence.
		{Capability: providers.CapabilityLicenses, Title: "licenses", Scope: graphScope("Organization.Read.All"), AdminConsent: true},
		{Capability: providers.CapabilityLicenses, Title: "licenses", Scope: graphScope("LicenseAssignment.ReadWrite.All"), AdminConsent: true},
	}
}

// ConsoleURLs implements providers.ScopeCatalog: the Entra blades the setup guide
// links to (application list, redirect URI, client secret, API permissions) plus
// the permission reference used to justify each requested scope.
func (p *Provider) ConsoleURLs() map[string]string {
	return map[string]string{
		"entra_apps":            "https://entra.microsoft.com/#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade",
		"entra_authentication":  "https://entra.microsoft.com/#view/Microsoft_AAD_RegisteredApps/ApplicationMenuBlade/~/Authentication",
		"entra_credentials":     "https://entra.microsoft.com/#view/Microsoft_AAD_RegisteredApps/ApplicationMenuBlade/~/Credentials",
		"entra_api_permissions": "https://entra.microsoft.com/#view/Microsoft_AAD_RegisteredApps/ApplicationMenuBlade/~/CallAnAPI",
		"graph_permissions":     "https://learn.microsoft.com/en-us/graph/permissions-reference",
	}
}

// graphResource is the resource prefix of a Microsoft Graph delegated scope. It
// is declared here so the permission table and the matching helper agree on it.
const graphResource = "https://graph.microsoft.com/"

// graphScope qualifies a Graph delegated permission with its resource.
func graphScope(permission string) string {
	return graphResource + permission
}

// AdminConsentURL implements providers.ConsentGranter: the tenant-wide consent
// endpoint, carrying the same scope list as the authorization request so the
// administrator consents to exactly what the connection will ask for.
//
// redirect_uri must be the registered callback (the flow comes back to
// `/api/oauth/microsoft/callback` with `admin_consent=True`), and state is the
// server side CSRF token of the stored flow.
func (p *Provider) AdminConsentURL(creds providers.Credentials, state string) string {
	query := url.Values{}
	query.Set("client_id", creds.ClientID)
	query.Set("scope", strings.Join(p.Scopes(), " "))
	if redirect := strings.TrimSpace(creds.RedirectURI); redirect != "" {
		query.Set("redirect_uri", redirect)
	}
	if state != "" {
		query.Set("state", state)
	}
	return loginBaseURL + "/" + tenantID(creds) + "/v2.0/adminconsent?" + query.Encode()
}
