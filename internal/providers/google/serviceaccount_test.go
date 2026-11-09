package google

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

// testServiceAccount writes a throwaway service-account key and answers the key
// and its PEM path.
func testServiceAccount(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey = %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey = %v", err)
	}
	path := filepath.Join(t.TempDir(), "service-account.json")
	payload, err := json.Marshal(serviceAccountKey{
		ClientEmail: "sync-delegates@project.iam.gserviceaccount.com",
		PrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		TokenURI:    "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("encoding the key: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	return key, path
}

// TestServiceAccountAssertion pins the delegated JWT: the service account is the
// issuer, the mailbox is the subject, the Gmail settings scope is requested and
// the signature verifies with the key's public half.
func TestServiceAccountAssertion(t *testing.T) {
	key, path := testServiceAccount(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	var parsed serviceAccountKey
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("Unmarshal = %v", err)
	}
	assertion, err := serviceAccountAssertion(parsed, "ivan@agencia.pub",
		[]string{gmailSettingsSharingScope}, "https://oauth2.googleapis.com/token", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("serviceAccountAssertion = %v", err)
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion = %q, want three parts", assertion)
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatalf("decoding claims JSON: %v", err)
	}
	for name, want := range map[string]string{
		"iss":   parsed.ClientEmail,
		"sub":   "ivan@agencia.pub",
		"scope": gmailSettingsSharingScope,
		"aud":   "https://oauth2.googleapis.com/token",
	} {
		if got := claims[name]; got != want {
			t.Errorf("claim %s = %v, want %q", name, got, want)
		}
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decoding signature: %v", err)
	}
	hashed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hashed[:], signature); err != nil {
		t.Errorf("assertion signature does not verify: %v", err)
	}
}

// TestDelegatedTokensRequiresTheKey pins the actionable failure of an
// installation that has not configured a service account.
func TestDelegatedTokensRequiresTheKey(t *testing.T) {
	provider := New()
	_, err := provider.delegatedTokens(context.Background(), "ivan@agencia.pub", []string{gmailSettingsSharingScope})
	if err == nil || !strings.Contains(err.Error(), "GOOGLE_SERVICE_ACCOUNT_FILE") {
		t.Fatalf("delegatedTokens = %v, want the GOOGLE_SERVICE_ACCOUNT_FILE hint", err)
	}
}

// TestDelegatedTokensExchangesTheAssertion drives the token exchange against a
// fake endpoint and pins the bearer token it answers.
func TestDelegatedTokensExchangesTheAssertion(t *testing.T) {
	_, path := testServiceAccount(t)
	transport := &reportTransport{bodies: map[string][]string{
		"/token": {`{"access_token":"dwd-access-token","expires_in":3600,"token_type":"Bearer"}`},
	}}
	provider := New()
	provider.httpClient = &http.Client{Transport: transport}
	provider.SetServiceAccountFile(path)

	tokens, err := provider.delegatedTokens(context.Background(), "ivan@agencia.pub", []string{gmailSettingsSharingScope})
	if err != nil {
		t.Fatalf("delegatedTokens = %v", err)
	}
	if tokens.AccessToken != "dwd-access-token" {
		t.Errorf("access token = %q, want the exchanged token", tokens.AccessToken)
	}
	if tokens.Expired(time.Now()) {
		t.Error("the exchanged token is already expired")
	}
	if len(transport.urls) != 1 || !strings.HasSuffix(transport.urls[0], "/token") {
		t.Errorf("requests = %v, want one token exchange", transport.urls)
	}
}

// Compile time guard: the provider still satisfies the interface the registry
// hands to the management service.
var _ providers.MailboxDelegator = (*Provider)(nil)
