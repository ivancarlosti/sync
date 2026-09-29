package google

import "github.com/ivancarlosti/sync/internal/providers"

// Compile time guard: the setup guide and the capability badges rely on the
// permission table, so a renamed method must fail the build.
var _ providers.ScopeCatalog = (*Provider)(nil)

// permissions is the single source of truth for the Google scopes this provider
// requests. Scopes() builds the authorization request from it, the setup guide
// renders it (GET /api/providers/google/guide) and the Accounts screen derives
// the capability badges from the grant.
//
// The Drive scope covers files; everything below it is the Admin SDK Directory
// API, the Enterprise License Manager API and the read-only role catalogue used
// by the directory features (users, groups, distribution lists, organisation
// units, licences). They require a Google Workspace domain, the matching APIs
// enabled in the same Cloud project and an administrator identity (see
// docs/app-registration.md).
func (p *Provider) Permissions() []providers.Permission {
	return []providers.Permission{
		{
			Capability: providers.CapabilityFiles,
			Title:      "files",
			Scope:      "openid",
		},
		{
			Capability: providers.CapabilityFiles,
			Title:      "files",
			Scope:      "https://www.googleapis.com/auth/userinfo.email",
		},
		{
			Capability: providers.CapabilityFiles,
			Title:      "files",
			Scope:      "https://www.googleapis.com/auth/userinfo.profile",
		},
		{
			Capability: providers.CapabilityFiles,
			Title:      "files",
			Scope:      "https://www.googleapis.com/auth/drive",
		},
		{
			Capability:   providers.CapabilityUsers,
			Title:        "users",
			Scope:        "https://www.googleapis.com/auth/admin.directory.user",
			AdminConsent: true,
		},
		{
			Capability:   providers.CapabilityUsers,
			Title:        "users",
			Scope:        "https://www.googleapis.com/auth/admin.directory.user.alias",
			AdminConsent: true,
		},
		{
			Capability:   providers.CapabilityGroups,
			Title:        "groups",
			Scope:        "https://www.googleapis.com/auth/admin.directory.group",
			AdminConsent: true,
		},
		{
			Capability:   providers.CapabilityMembers,
			Title:        "members",
			Scope:        "https://www.googleapis.com/auth/admin.directory.group.member",
			AdminConsent: true,
		},
		{
			Capability:   providers.CapabilityDomains,
			Title:        "domains",
			Scope:        "https://www.googleapis.com/auth/admin.directory.domain.readonly",
			AdminConsent: true,
		},
		{
			Capability:   providers.CapabilityOrgUnits,
			Title:        "orgunits",
			Scope:        "https://www.googleapis.com/auth/admin.directory.orgunit",
			AdminConsent: true,
		},
		{
			Capability:   providers.CapabilityRoles,
			Title:        "roles",
			Scope:        "https://www.googleapis.com/auth/admin.directory.rolemanagement.readonly",
			AdminConsent: true,
		},
		{
			Capability:   providers.CapabilityLicenses,
			Title:        "licenses",
			Scope:        "https://www.googleapis.com/auth/apps.licensing",
			AdminConsent: true,
		},
	}
}

// ConsoleURLs implements providers.ScopeCatalog. The API enablement pages are
// part of the permission contract — a scope whose API is not enabled fails at
// call time — so they are declared next to the scopes rather than in the guide.
func (p *Provider) ConsoleURLs() map[string]string {
	return map[string]string{
		"drive_api":     "https://console.cloud.google.com/apis/library/drive.googleapis.com",
		"admin_api":     "https://console.cloud.google.com/apis/library/admin.googleapis.com",
		"licensing_api": "https://console.cloud.google.com/apis/library/licensing.googleapis.com",
	}
}
