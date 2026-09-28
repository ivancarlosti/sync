package microsoft

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	auth "github.com/microsoft/kiota-abstractions-go/authentication"
)

// graphHosts is the only host the bearer token is ever sent to. Kiota applies
// this validator before every request, so neither a redirect nor an unexpected
// nextLink can leak the token to another host.
var graphHosts = []string{"graph.microsoft.com"}

// tokenProvider hands the stored access token to the Graph SDK. Refreshing is
// owned by the services layer, which persists the rotated refresh token, so this
// provider never performs network calls of its own.
type tokenProvider struct {
	accessToken string
	validator   auth.AllowedHostsValidator
}

// newTokenProvider wraps an access token into a Kiota access token provider.
func newTokenProvider(accessToken string) *tokenProvider {
	return &tokenProvider{
		accessToken: accessToken,
		validator:   auth.NewAllowedHostsValidator(graphHosts),
	}
}

// GetAuthorizationToken implements auth.AccessTokenProvider.
func (t *tokenProvider) GetAuthorizationToken(_ context.Context, uri *url.URL, _ map[string]interface{}) (string, error) {
	if t.accessToken == "" {
		return "", errors.New("microsoft graph: the account has no access token (reconnect it)")
	}
	if uri != nil && !t.validator.IsUrlHostValid(uri) {
		return "", fmt.Errorf("microsoft graph: refusing to send the access token to %q", uri.Host)
	}
	return t.accessToken, nil
}

// GetAllowedHostsValidator implements auth.AccessTokenProvider.
func (t *tokenProvider) GetAllowedHostsValidator() *auth.AllowedHostsValidator {
	return &t.validator
}
