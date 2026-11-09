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
// The Drive scope covers files; the identity scopes name the account and let the
// token be attributed to a person. Nothing else is requested: the sync engine
// only needs to read and write Drive.
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
	}
}

// ConsoleURLs implements providers.ScopeCatalog. The API enablement pages are
// part of the permission contract — a scope whose API is not enabled fails at
// call time — so they are declared next to the scopes rather than in the guide.
func (p *Provider) ConsoleURLs() map[string]string {
	return map[string]string{
		"drive_api": "https://console.cloud.google.com/apis/library/drive.googleapis.com",
	}
}
