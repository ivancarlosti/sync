package google

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/ivancarlosti/sync/internal/providers"
)

// OrganisationName implements providers.OrganisationNamer: the label of the
// Workspace customer behind the token.
//
// It reads GET /customer/{customerKey}, which the admin.directory.customer.readonly
// scope already covers (the scope the organization report section needs), so
// naming a domain costs no extra consent. The name is the organisation name of
// the customer profile when an administrator set one, and the primary domain
// otherwise, which is why a Google label reads "Acme Inc" but falls back to
// "acme.com" instead of showing nothing.
//
// The Workspace organisation *is* its domain, so the domain is answered alongside
// the name; the collector collapses the two when they are equal.
func (p *Provider) OrganisationName(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) (string, string, error) {
	if tokens == nil || strings.TrimSpace(tokens.AccessToken) == "" {
		return "", "", fmt.Errorf("google workspace: cannot name the organisation without an access token")
	}
	var customer reportCustomer
	if err := p.doJSON(ctx, p.client(tokens), http.MethodGet, adminBase+"/customers/"+customerKey, nil, &customer); err != nil {
		return "", "", err
	}
	domain := strings.TrimSpace(customer.CustomerDomain)
	name := strings.TrimSpace(customer.PostalAddress.OrganizationName)
	if name == "" {
		name = domain
	}
	return name, domain, nil
}
