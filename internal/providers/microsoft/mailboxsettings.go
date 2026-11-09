package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ivancarlosti/sync/internal/providers"
)

// Gmail and Exchange disagree about how mailbox settings are read. Microsoft
// Graph exposes `mailboxSettings.userPurpose` per user, but a delegated token
// only reads the signed-in user's own settings; reading every user of the
// directory requires the *application* permission `MailboxSettings.Read`. That
// is the permission the n8n collector this screen replaces used, and it is what
// classifies shared mailboxes, rooms and equipment without a reconnect for each
// account.
const graphAppScope = graphResource + ".default"

// graphAppOnlyToken mints an application token for Microsoft Graph. The tenant
// comes from the connected account (its `tid` claim, or the configured tenant),
// so one client can report several directories.
func (p *Provider) graphAppOnlyToken(ctx context.Context, tenant string, creds providers.Credentials) (string, error) {
	tenant = strings.ToLower(strings.TrimSpace(tenant))
	if !isTenantID(tenant) {
		return "", fmt.Errorf("microsoft: mailbox settings need a tenant id (connect a work account or set MICROSOFT_TENANT_ID)")
	}
	if strings.TrimSpace(creds.ClientID) == "" || strings.TrimSpace(creds.ClientSecret) == "" {
		return "", fmt.Errorf("microsoft: mailbox settings need the application credentials (client id and secret)")
	}
	form := url.Values{}
	form.Set("client_id", creds.ClientID)
	form.Set("client_secret", creds.ClientSecret)
	form.Set("scope", graphAppScope)
	form.Set("grant_type", "client_credentials")

	target := loginBaseURL + "/" + tenant + "/oauth2/v2.0/token"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("microsoft: cannot build the mailbox settings token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := p.doRequest(ctx, request)
	if err != nil {
		return "", fmt.Errorf("microsoft: the mailbox settings token request failed: %w", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", fmt.Errorf("microsoft: mailbox settings need the Graph application permission MailboxSettings.Read and an administrator consent (%s)",
			oauthErrorSummary(raw, response.StatusCode))
	}
	var answer struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || answer.AccessToken == "" {
		return "", errors.New("microsoft: the mailbox settings token response carried no access token")
	}
	return answer.AccessToken, nil
}
