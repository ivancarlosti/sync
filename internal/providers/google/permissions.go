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
			// The customer record behind the `organization` section of a
			// directory report: the primary domain, the postal address and the
			// administrator address of the tenant. It is the one call of a
			// report the scopes above do not cover, so a grant made before this
			// row existed answers 403 there — and the collector then derives the
			// row from the domain list instead of failing the section (see
			// google/report.go).
			Capability:   providers.CapabilityDomains,
			Title:        "domains",
			Scope:        "https://www.googleapis.com/auth/admin.directory.customer.readonly",
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
		{
			// The Reports API behind the Audit Logs screen. One read-only scope
			// covers every application of the feed (drive, user_accounts,
			// groups, admin, login), which is why the collector asks for this
			// one and nothing else.
			Capability:   providers.CapabilityLogs,
			Title:        "logs",
			Scope:        reportsScope,
			AdminConsent: true,
		},
		{
			// Gmail: read a mailbox and insert into another one (the mailbox
			// copy of the Admin operations screen). Copying across mailboxes
			// needs domain-wide delegation on the OAuth client, which a
			// Workspace administrator grants in the console.
			Capability:   providers.CapabilityMail,
			Title:        "mail",
			Scope:        "https://www.googleapis.com/auth/gmail.modify",
			AdminConsent: true,
		},
		{
			// Calendar: read the calendars of a user and grant owner access to
			// another user (the calendar transfer of the Admin operations
			// screen).
			Capability:   providers.CapabilityCalendars,
			Title:        "calendars",
			Scope:        "https://www.googleapis.com/auth/calendar",
			AdminConsent: true,
		},
		{
			// Gmail settings sharing: add and remove delegates of a mailbox.
			Capability:   providers.CapabilityDelegation,
			Title:        "delegation",
			Scope:        "https://www.googleapis.com/auth/gmail.settings.sharing",
			AdminConsent: true,
		},
		{
			// Analytics Admin API: the `analytics` section of a report (the
			// accounts and properties the granted identity can reach, with the
			// date each property was created). Read-only: the report never
			// writes an Analytics resource.
			Capability:   providers.CapabilityAnalytics,
			Title:        "analytics",
			Scope:        "https://www.googleapis.com/auth/analytics.readonly",
			AdminConsent: true,
		},
		{
			// YouTube Data API: the `youtube` section of a report (the channels
			// the granted identity owns, with their counters). Read-only, and
			// scoped to the connection: the domain-wide listing needs a content
			// owner, which is a partner identity Sync does not have (see
			// docs/reports.md).
			Capability:   providers.CapabilityYouTube,
			Title:        "youtube",
			Scope:        "https://www.googleapis.com/auth/youtube.readonly",
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
		"analytics_api": "https://console.cloud.google.com/apis/library/analyticsadmin.googleapis.com",
		"youtube_api":   "https://console.cloud.google.com/apis/library/youtube.googleapis.com",
	}
}
