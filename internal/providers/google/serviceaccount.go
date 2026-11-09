// Domain-wide delegation for the Google calls a normal OAuth grant cannot make.
//
// Gmail's `users.settings.delegates` is the one API Sync uses that Google
// restricts to service accounts with domain-wide delegation: a delegated OAuth
// token of the administrator answers
//
//	403 Access restricted to service accounts that have been delegated
//	domain-wide authority
//
// even for the administrator's own mailbox. The service account signs a JWT for
// the mailbox it impersonates and exchanges it for a short-lived access token;
// the Workspace administrator authorises the service account's client id for the
// Gmail settings scope in the Admin console (see docs/management.md).
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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

const (
	// gmailSettingsSharingScope is the scope the delegates endpoint requires.
	// It is the same scope the OAuth permission table declares for the feature,
	// but the service account grant is what Google checks.
	gmailSettingsSharingScope = "https://www.googleapis.com/auth/gmail.settings.sharing"
	// serviceAccountTokenURL is the default token endpoint of a service account
	// key. The key usually carries its own `token_uri`; this is the fallback.
	serviceAccountTokenURL = "https://oauth2.googleapis.com/token"
)

// serviceAccountKey is the subset of a Google service-account JSON key this
// provider reads. The key is a credential: it is never logged or stored in a
// report.
type serviceAccountKey struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// delegatedTokens mints an access token that impersonates subject for scopes. It
// answers a clear setup error when the installation has no service-account key,
// because the Google 403 alone does not say what to configure.
func (p *Provider) delegatedTokens(ctx context.Context, subject string, scopes []string) (*providers.Tokens, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, fmt.Errorf("google: a mailbox is required to impersonate with domain-wide delegation")
	}
	path := strings.TrimSpace(p.serviceAccountFile)
	if path == "" {
		return nil, fmt.Errorf("google: Gmail delegates need a service account with domain-wide delegation; " +
			"set GOOGLE_SERVICE_ACCOUNT_FILE to its JSON key (see docs/management.md)")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("google: cannot read the service-account key %s: %w", path, err)
	}
	var key serviceAccountKey
	if err := json.Unmarshal(raw, &key); err != nil {
		return nil, fmt.Errorf("google: the service-account key %s is not valid JSON: %w", path, err)
	}
	key.ClientEmail = strings.TrimSpace(key.ClientEmail)
	key.PrivateKey = strings.TrimSpace(key.PrivateKey)
	if key.ClientEmail == "" || key.PrivateKey == "" {
		return nil, fmt.Errorf("google: the service-account key %s has no client_email or private_key", path)
	}
	tokenURL := strings.TrimSpace(key.TokenURI)
	if tokenURL == "" {
		tokenURL = serviceAccountTokenURL
	}
	assertion, err := serviceAccountAssertion(key, subject, scopes, tokenURL, time.Now())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("google: cannot build the delegation token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := p.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("google: the delegation token request failed: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		// The answer carries the reason (invalid_grant, unauthorized_client, a
		// missing scope); the private key is never part of it.
		return nil, fmt.Errorf("google: the delegation token endpoint answered http %d: %s",
			response.StatusCode, strings.TrimSpace(string(body)))
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("google: cannot decode the delegation token answer: %w", err)
	}
	if answer.AccessToken == "" {
		return nil, fmt.Errorf("google: the delegation token endpoint answered no access token")
	}
	expiry := time.Now().Add(time.Hour)
	if answer.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(answer.ExpiresIn) * time.Second)
	}
	return &providers.Tokens{
		AccessToken: answer.AccessToken,
		TokenType:   answer.TokenType,
		Expiry:      expiry,
		Scopes:      append([]string(nil), scopes...),
	}, nil
}

// serviceAccountAssertion builds the RS256 JWT the token endpoint exchanges. The
// claims are the documented delegation shape: the service account is the issuer,
// the mailbox is the subject and the requested scopes are the delegation.
func serviceAccountAssertion(key serviceAccountKey, subject string, scopes []string, audience string, now time.Time) (string, error) {
	block, _ := pem.Decode([]byte(key.PrivateKey))
	if block == nil {
		return "", fmt.Errorf("google: the service-account private_key is not a PEM block")
	}
	privateKey, err := parseServiceAccountKey(block.Bytes)
	if err != nil {
		return "", err
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	payload := map[string]any{
		"iss":   key.ClientEmail,
		"sub":   strings.TrimSpace(subject),
		"scope": strings.Join(scopes, " "),
		"aud":   audience,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	encodedHeader, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("google: cannot encode the delegation header: %w", err)
	}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("google: cannot encode the delegation claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(encodedHeader) + "." +
		base64.RawURLEncoding.EncodeToString(encodedPayload)
	hashed := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("google: cannot sign the delegation assertion: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// parseServiceAccountKey accepts both private-key encodings Google emits
// (PKCS#8 today, PKCS#1 on older keys).
func parseServiceAccountKey(der []byte) (*rsa.PrivateKey, error) {
	if parsed, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if key, ok := parsed.(*rsa.PrivateKey); ok {
			return key, nil
		}
		return nil, fmt.Errorf("google: the service-account private_key is not an RSA key")
	}
	key, err := x509.ParsePKCS1PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("google: cannot parse the service-account private_key: %w", err)
	}
	return key, nil
}

// accountEmail answers the address of the connection, which is the mailbox a
// caller means by an empty/"me" mailbox.
func (p *Provider) accountEmail(ctx context.Context, tokens *providers.Tokens) (string, error) {
	account, err := p.Account(ctx, providers.Credentials{}, tokens)
	if err != nil {
		return "", err
	}
	if email := strings.TrimSpace(account.Email); email != "" {
		return email, nil
	}
	return "", fmt.Errorf("google: the connection does not expose an account email")
}

// delegateMailbox resolves the mailbox a delegate operation names.
func (p *Provider) delegateMailbox(ctx context.Context, tokens *providers.Tokens, mailbox string) (string, error) {
	mailbox = strings.TrimSpace(mailbox)
	if mailbox != "" && !strings.EqualFold(mailbox, "me") {
		return mailbox, nil
	}
	return p.accountEmail(ctx, tokens)
}

// delegateClient builds the Gmail client of one impersonated mailbox.
func (p *Provider) delegateClient(ctx context.Context, tokens *providers.Tokens, mailbox string) (*http.Client, string, error) {
	address, err := p.delegateMailbox(ctx, tokens, mailbox)
	if err != nil {
		return nil, "", err
	}
	delegated, err := p.delegatedTokens(ctx, address, []string{gmailSettingsSharingScope})
	if err != nil {
		return nil, "", err
	}
	return p.client(delegated), address, nil
}
