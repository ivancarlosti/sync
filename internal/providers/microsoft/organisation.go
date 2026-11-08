package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ivancarlosti/sync/internal/providers"
)

// OrganisationName implements providers.OrganisationNamer: the display name of
// the Entra tenant behind the token, with its default verified domain.
//
// It reads GET /organization, which is served by Organization.Read.All — a scope
// the permission table already requests for the organization report section, so
// naming a tenant needs no new consent and no reconnection. That is the whole
// point of putting the lookup here instead of in the collector: the scope is part
// of the connection, so an account connected before this release can already
// answer it.
//
// A tenant that refuses the call (a personal account, or an account whose grant
// predates the scope and was never reissued) answers an error, and the collector
// falls back to the tenant id — the banner is then no worse than it was before.
func (p *Provider) OrganisationName(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) (string, string, error) {
	if tokens == nil || strings.TrimSpace(tokens.AccessToken) == "" {
		return "", "", fmt.Errorf("microsoft: cannot name the organisation without an access token")
	}
	target := collectionTarget("/organization", []string{"id", "displayName", "verifiedDomains"}, nil)
	var answer reportPage
	if err := p.getJSON(ctx, target, tokens.AccessToken, &answer); err != nil {
		return "", "", err
	}
	if len(answer.Value) == 0 {
		// A personal Microsoft account belongs to no directory: there is no
		// organisation record to name, which is not a failure.
		return "", "", nil
	}
	var organization reportOrganization
	if err := json.Unmarshal(answer.Value[0], &organization); err != nil {
		return "", "", fmt.Errorf("microsoft: cannot read the organization record: %w", err)
	}
	domain := ""
	for _, verified := range organization.VerifiedDomains {
		if verified.IsDefault && verified.Name != "" {
			domain = verified.Name
			break
		}
	}
	return strings.TrimSpace(organization.DisplayName), domain, nil
}
