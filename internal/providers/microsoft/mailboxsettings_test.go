package microsoft

import (
	"context"
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/providers"
)

// TestGraphAppOnlyTokenExchangesTheCredentials pins the app-only token the
// mailbox purpose read uses: client credentials for Graph, with the application
// permission MailboxSettings.Read granted on the app registration.
func TestGraphAppOnlyTokenExchangesTheCredentials(t *testing.T) {
	transport := &recordingTransport{bodies: map[string]string{
		"/tenant/oauth2/v2.0/token": `{"access_token":"app-token"}`,
	}}
	provider := New()
	provider.httpClient.Transport = transport
	token, err := provider.graphAppOnlyToken(context.Background(), "tenant",
		providers.Credentials{ClientID: "client-id", ClientSecret: "client-secret"})
	if err != nil {
		t.Fatalf("graphAppOnlyToken = %v", err)
	}
	if token != "app-token" {
		t.Errorf("token = %q, want the exchanged token", token)
	}
	if len(transport.urls) != 1 || !strings.HasSuffix(transport.urls[0], "/tenant/oauth2/v2.0/token") {
		t.Errorf("requests = %v, want one token exchange", transport.urls)
	}
}

// TestGraphAppOnlyTokenNeedsCredentials pins the actionable failure of a tenant
// whose report is read without the application credentials.
func TestGraphAppOnlyTokenNeedsCredentials(t *testing.T) {
	provider := New()
	_, err := provider.graphAppOnlyToken(context.Background(), "tenant", providers.Credentials{})
	if err == nil || !strings.Contains(err.Error(), "application credentials") {
		t.Fatalf("graphAppOnlyToken = %v, want the client credentials hint", err)
	}
}
