// Package microsoft implements the providers.Provider interface on top of
// Microsoft Graph (OneDrive, OneDrive for Business and SharePoint document
// libraries) and the Microsoft identity platform v2.0 OAuth endpoints.
//
// Every metadata call — listing drives, walking folders, resolving one item,
// creating folders, patching timestamps, deleting, discovering sites — is issued
// through the official Graph SDK request builders
// (github.com/microsoftgraph/msgraph-sdk-go). That keeps the OpenAPI contract,
// the retry/redirect middleware, the OData error mapping and pagination in one
// place, and it is what the project brief asks for.
//
// Binary transfers are the one deliberate exception. The Go flavour of Kiota can
// only send an in-memory []byte body, so routing a multi-gigabyte upload through
// it would buffer the whole file in RAM. Download and Upload therefore build
// their request with the SDK (URL, authentication and options still come from the
// generated builders) and then execute the resulting *http.Request with the
// streaming client from transfer.go. Metadata responses are still decoded through
// the SDK models.
package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	abstractions "github.com/microsoft/kiota-abstractions-go"
	auth "github.com/microsoft/kiota-abstractions-go/authentication"
	kiotahttp "github.com/microsoft/kiota-http-go"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	msgraphgocore "github.com/microsoftgraph/msgraph-sdk-go-core"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
	"github.com/microsoftgraph/msgraph-sdk-go/users"
	"golang.org/x/oauth2"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

const (
	// baseURL is the Graph v1.0 endpoint. The SDK sets it on the request
	// adapter; it is repeated here for the hand-built transfer requests.
	baseURL = "https://graph.microsoft.com/v1.0"
	// loginBaseURL is the Microsoft identity platform v2.0 root.
	loginBaseURL = "https://login.microsoftonline.com"
	// defaultTenant accepts work/school and personal accounts, which is what a
	// multi-tenant registration needs: any directory can be connected and the
	// consent is granted by each directory's own administrator. An operator that
	// must lock the app to one directory sets MICROSOFT_TENANT_ID.
	defaultTenant = "common"
	// organizationsTenant is the tenant segment of the admin-consent endpoint when
	// the credentials use one of the generic tenants above. Microsoft documents
	// `common` as unsupported there ("Do not use common") and points at
	// `organizations` instead, which consents for any work/school directory.
	organizationsTenant = "organizations"

	// requestTimeout bounds a single metadata call.
	requestTimeout = 5 * time.Minute
	// uploadTimeout bounds a whole upload (session creation plus bytes).
	uploadTimeout = 60 * time.Minute

	// simpleUploadLimit is Graph's ceiling for PUT .../content. Larger files are
	// sent through a resumable upload session.
	simpleUploadLimit = 250 << 20
	// uploadChunkSize must stay a multiple of 320 KiB, as upload sessions
	// require, and is the unit used for every slice after the first one.
	uploadChunkSize = 10 << 20

	// adapterCacheLimit bounds the per-token adapter cache so a long lived
	// process with rotating tokens cannot grow without bound.
	adapterCacheLimit = 32
)

// Compile time guards: the registry hands these out as interfaces, so a drifted
// signature must fail the build rather than a job at runtime.
var (
	_ providers.Provider     = (*Provider)(nil)
	_ providers.SiteBrowser  = (*Provider)(nil)
	_ providers.SiteResolver = (*Provider)(nil)
)

// Provider talks to Microsoft Graph with a stored OAuth token set.
type Provider struct {
	httpClient *http.Client
	// streaming executes the byte transfers (downloads and uploads). It shares
	// the connection pool of httpClient but adds an explicit redirect policy.
	streaming *http.Client
	// adapters caches one request adapter per access token: rebuilding it on
	// every call would also rebuild the middleware pipeline for every folder of
	// a full scan.
	adapters sync.Map
	cached   atomic.Int32
}

// New builds the provider. A single transport serves every call, which is what
// makes walking a large drive tree fast.
func New() *Provider {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Provider{
		// No global timeout: every call carries a context deadline and an
		// upload may legitimately run for minutes.
		httpClient: &http.Client{Transport: transport},
		streaming:  &http.Client{Transport: transport, CheckRedirect: stripTokenOnHostChange},
	}
}

// transport returns the round tripper the SDK middleware pipeline runs on top
// of. It is the provider's pooled transport, or whatever else the HTTP client
// was given, so a custom transport is never silently dropped.
func (p *Provider) transport() http.RoundTripper {
	if p.httpClient != nil && p.httpClient.Transport != nil {
		return p.httpClient.Transport
	}
	return http.DefaultTransport
}

// Name implements providers.Provider.
func (p *Provider) Name() models.ProviderName { return models.ProviderMicrosoft }

// Scopes implements providers.Provider. The list is built from the permission
// table in permissions.go, which is also what the setup guide renders.
//
// Files.ReadWrite.All covers OneDrive and every SharePoint document library the
// user can reach, Sites.ReadWrite.All resolves a site into its default drive and
// offline_access is what makes the token set refreshable. Everything above
// User.Read is a directory permission, which is why the tenant administrator has
// to grant the consent once (AdminConsentURL).
func (p *Provider) Scopes() []string {
	return providers.ScopesOf(p.Permissions())
}

// AuthCodeURL implements providers.Provider. prompt=consent guarantees that a
// reconnection returns a refresh token even when the user already granted the
// scopes, and PKCE protects the code exchange of a public/confidential client
// without a client secret round trip.
func (p *Provider) AuthCodeURL(creds providers.Credentials, state, codeChallenge string) string {
	options := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("response_mode", "query"),
	}
	if codeChallenge != "" {
		options = append(options,
			oauth2.SetAuthURLParam("code_challenge", codeChallenge),
			oauth2.SetAuthURLParam("code_challenge_method", "S256"))
	}
	return oauthConfig(creds).AuthCodeURL(state, options...)
}

// Exchange implements providers.Provider.
func (p *Provider) Exchange(ctx context.Context, creds providers.Credentials, code, codeVerifier string) (*providers.Tokens, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.httpClient)
	options := []oauth2.AuthCodeOption{}
	if codeVerifier != "" {
		options = append(options, oauth2.SetAuthURLParam("code_verifier", codeVerifier))
	}
	token, err := oauthConfig(creds).Exchange(ctx, code, options...)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: authorization code exchange failed: %w", authFailure(err))
	}
	return tokensFromOAuth(token, ""), nil
}

// authFailure turns the error document of an identity platform refusal into a
// providers.AuthError, which carries the OAuth error code the services layer maps
// to `consent_required` (a tenant-wide admin consent that was never granted, a
// denied prompt, a scope the application may not use). Every other failure is
// returned untouched.
func authFailure(err error) error {
	var retrieve *oauth2.RetrieveError
	if !errors.As(err, &retrieve) {
		return err
	}
	return providers.NewAuthError(models.ProviderMicrosoft, retrieve.ErrorCode, retrieve.ErrorDescription)
}

// Refresh implements providers.Provider. Microsoft rotates the refresh token on
// every use, so the stored one is only kept when the response omits it (an
// error would otherwise silently break the account).
func (p *Provider) Refresh(ctx context.Context, creds providers.Credentials, refreshToken string) (*providers.Tokens, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.httpClient)
	source := oauthConfig(creds).TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
	token, err := source.Token()
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: token refresh failed (reconnect the account): %w", err)
	}
	return tokensFromOAuth(token, refreshToken), nil
}

// oauthConfig builds the OAuth 2.0 client configuration for one credential set.
// The tenant selects which directory (and therefore which endpoint host path)
// signs the user in.
func oauthConfig(creds providers.Credentials) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
		RedirectURL:  creds.RedirectURI,
		Scopes:       (&Provider{}).Scopes(),
		Endpoint:     endpoint(creds),
	}
}

// endpoint is the Microsoft identity platform v2.0 endpoint of a tenant.
func endpoint(creds providers.Credentials) oauth2.Endpoint {
	path := "/" + tenantID(creds) + "/oauth2/v2.0"
	return oauth2.Endpoint{
		AuthURL:   loginBaseURL + path + "/authorize",
		TokenURL:  loginBaseURL + path + "/token",
		AuthStyle: oauth2.AuthStyleInParams,
	}
}

// tenantID returns the tenant segment, defaulting to "common". Only the
// character set a tenant id or a verified domain can contain is accepted, and
// path navigation is rejected outright, so a crafted value can never escape the
// "/{tenant}/oauth2/v2.0" path of the sign-in host.
func tenantID(creds providers.Credentials) string {
	raw := strings.TrimSpace(creds.TenantID)
	if raw == "" {
		return defaultTenant
	}
	if raw == "." || raw == ".." || strings.HasPrefix(raw, ".") || strings.HasSuffix(raw, ".") {
		return defaultTenant
	}
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '.', r == '_':
		default:
			return defaultTenant
		}
	}
	return raw
}

// consentTenant returns the tenant segment of the admin-consent endpoint, which
// is not always the one of the sign-in endpoints: `common` and `consumers` accept
// personal accounts at sign-in, but the consent that a directory administrator
// grants only exists for a work/school directory, where Microsoft documents
// `common` as unsupported. `organizations` is the generic value that works for
// any of them, as is a concrete tenant id or verified domain.
func consentTenant(creds providers.Credentials) string {
	switch tenant := tenantID(creds); tenant {
	case defaultTenant, "consumers":
		return organizationsTenant
	default:
		return tenant
	}
}

// tokensFromOAuth normalises an oauth2 token into the provider token set.
func tokensFromOAuth(token *oauth2.Token, previousRefresh string) *providers.Tokens {
	refresh := token.RefreshToken
	if refresh == "" {
		refresh = previousRefresh
	}
	scopes := []string{}
	if extra, ok := token.Extra("scope").(string); ok && extra != "" {
		scopes = append(scopes, splitScopes(extra)...)
	}
	return &providers.Tokens{
		AccessToken:  token.AccessToken,
		RefreshToken: refresh,
		TokenType:    normaliseTokenType(token.TokenType),
		Expiry:       token.Expiry.UTC(),
		Scopes:       scopes,
	}
}

// normaliseTokenType canonicalises the token type and defaults to Bearer when
// the platform omits the field.
func normaliseTokenType(raw string) string {
	if raw == "" || strings.EqualFold(raw, "bearer") {
		return "Bearer"
	}
	return raw
}

// splitScopes splits the scope string Microsoft returns. Entra ID may
// percent-encode the value (`https%3A%2F%2Fgraph.microsoft.com%2Fmail.read`), so
// the splitting is shared with the rest of the stack (providers.SplitScopes)
// instead of being a plain strings.Fields.
func splitScopes(raw string) []string {
	return providers.SplitScopes(raw)
}

// graphError is the error shape of the raw transfer requests in transfer.go.
// Calls made through the SDK surface an *odataerrors.ODataError instead; both
// render a diagnostic message that never contains the access token.
type graphError struct {
	Status  int
	Code    string
	Message string
}

// Error renders the Graph failure for logs and notifications.
func (e *graphError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("microsoft graph api: http %d (%s): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("microsoft graph api: http %d: %s", e.Status, e.Message)
}

// Is lets errors.Is recognise a throttle Graph kept refusing after the retries
// (HTTP 429, or 503 with a Retry-After), so the layers above can answer "try
// again later" instead of a server fault. The classification does not wrap the
// error, which keeps IsNotFound working.
func (e *graphError) Is(target error) bool {
	if target != providers.ErrRateLimited {
		return false
	}
	return e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable
}

// decodeGraphError turns a non-2xx response into a *graphError, unwrapping the
// standard {"error":{"code":...,"message":...}} envelope when present.
func decodeGraphError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	message := strings.TrimSpace(string(raw))
	code := ""
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		code = envelope.Error.Code
		message = envelope.Error.Message
	}
	return &graphError{Status: resp.StatusCode, Code: code, Message: message}
}

// IsNotFound reports whether err means "this item is already gone", so the sync
// engine can treat it as a deletion instead of a failure.
func (p *Provider) IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *graphError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusGone
	}
	var odata *odataerrors.ODataError
	if errors.As(err, &odata) {
		if main := odata.GetErrorEscaped(); main != nil && main.GetCode() != nil {
			switch strings.ToLower(*main.GetCode()) {
			case "itemnotfound", "notfound", "resourcenotfound":
				return true
			}
		}
	}
	return false
}

// Account implements providers.Provider through the Graph /me endpoint.
func (p *Provider) Account(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) (*providers.Account, error) {
	client, err := p.graphClient(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	user, err := client.Me().Get(ctx, &users.UserItemRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.UserItemRequestBuilderGetQueryParameters{
			Select: []string{"id", "displayName", "userPrincipalName", "mail"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot read the connected identity: %w", err)
	}

	account := &providers.Account{Name: deref(user.GetDisplayName())}
	if mail := deref(user.GetMail()); mail != "" {
		account.Email = mail
	} else {
		account.Email = deref(user.GetUserPrincipalName())
	}
	// The immutable object id is preferred: it survives renames, while the user
	// principal name (and the mail alias) may change.
	if id := deref(user.GetId()); id != "" {
		account.ID = id
	} else {
		account.ID = account.Email
	}
	if account.ID == "" {
		return nil, fmt.Errorf("microsoft graph: the token does not expose an account identity")
	}
	return account, nil
}

// graphClient returns a Graph client bound to tokens.
func (p *Provider) graphClient(tokens *providers.Tokens) (*msgraphsdk.GraphServiceClient, error) {
	client, _, err := p.graphSession(tokens)
	return client, err
}

// graphSession returns the Graph client plus the request adapter behind it,
// which the page iterator needs in order to follow OData nextLinks.
func (p *Provider) graphSession(tokens *providers.Tokens) (*msgraphsdk.GraphServiceClient, abstractions.RequestAdapter, error) {
	if tokens == nil || tokens.AccessToken == "" {
		return nil, nil, errors.New("microsoft graph: the account has no access token (reconnect it)")
	}
	if cached, ok := p.adapters.Load(tokens.AccessToken); ok {
		adapter := cached.(abstractions.RequestAdapter)
		return msgraphsdk.NewGraphServiceClient(adapter), adapter, nil
	}

	adapter, err := p.newAdapter(tokens.AccessToken)
	if err != nil {
		return nil, nil, err
	}
	p.cacheAdapter(tokens.AccessToken, adapter)
	return msgraphsdk.NewGraphServiceClient(adapter), adapter, nil
}

// newAdapter builds a Kiota request adapter whose bearer token comes from the
// stored credentials and whose transport is the provider's own HTTP client, so
// the whole SDK shares one connection pool and one set of middlewares.
//
// The pipeline is Graph's, not Kiota's plain default. The generated client asks
// for Me() as /users/me-token-to-replace and only Graph's UrlReplaceHandler
// rewrites that sentinel to /me, so dropping it makes every Me()-scoped call
// (the identity read and the drive list) fail with a "resource does not exist"
// error from Graph. GetDefaultClient cannot be used here: it would install the
// right middlewares on a transport of its own, discarding the pooled one.
func (p *Provider) newAdapter(accessToken string) (abstractions.RequestAdapter, error) {
	options := msgraphsdk.GetDefaultClientOptions()
	middlewares := msgraphgocore.GetDefaultMiddlewaresWithOptions(&options)

	httpClient := kiotahttp.GetDefaultClient()
	httpClient.Transport = kiotahttp.NewCustomTransportWithParentTransport(p.transport(), middlewares...)

	credential := auth.NewBaseBearerTokenAuthenticationProvider(newTokenProvider(accessToken))
	adapter, err := msgraphsdk.NewGraphRequestAdapterWithParseNodeFactoryAndSerializationWriterFactoryAndHttpClient(
		credential, nil, nil, httpClient)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot build the request adapter: %w", err)
	}
	return adapter, nil
}

// cacheAdapter stores adapter under the access token, recycling the whole cache
// when it reaches adapterCacheLimit (tokens rotate hourly, so this stays tiny).
func (p *Provider) cacheAdapter(token string, adapter abstractions.RequestAdapter) {
	if p.cached.Load() >= adapterCacheLimit {
		p.adapters.Range(func(key, _ any) bool {
			p.adapters.Delete(key)
			return true
		})
		p.cached.Store(0)
	}
	p.adapters.Store(token, adapter)
	p.cached.Add(1)
}

// deref returns the value behind an optional string.
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ptr returns a pointer to value, which is how the generated builders express
// their optional query parameters.
func ptr[T any](value T) *T { return &value }
