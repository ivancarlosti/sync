package google

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// Provider talks to Google Drive with a stored OAuth token set.
type Provider struct {
	httpClient *http.Client
}

// New builds the provider. The transport is shared by every call, which is what
// makes connection reuse (and therefore listing a large tree) fast.
func New() *Provider {
	return &Provider{httpClient: &http.Client{
		Timeout: 0, // per-request contexts bound the duration (streaming allowed)
		Transport: &http.Transport{
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
		},
	}}
}

// Name implements providers.Provider.
func (p *Provider) Name() models.ProviderName { return models.ProviderGoogle }

// Scopes implements providers.Provider. The list is built from the permission
// table below (permissions.go), which is also what the setup guide renders.
//
// The full drive scope is required to see shared drives and to write into any
// folder the user can access; the openid scopes are only used to display the
// connected identity. The admin.directory.*, apps.licensing and
// admin.directory.rolemanagement.readonly scopes need a Google Workspace domain
// and an administrator identity; they are requested up front so a connection
// made today is already ready for the directory features (see
// docs/app-registration.md).
func (p *Provider) Scopes() []string {
	return providers.ScopesOf(p.Permissions())
}

// AuthCodeURL implements providers.Provider. offline access plus
// prompt=consent guarantees a refresh token on every (re)connection.
func (p *Provider) AuthCodeURL(creds providers.Credentials, state, codeChallenge string) string {
	options := []oauth2.AuthCodeOption{
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("include_granted_scopes", "true"),
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
		return nil, fmt.Errorf("google drive: authorization code exchange failed: %w", authFailure(err))
	}
	return tokensFromOAuth(token, ""), nil
}

// authFailure turns the error document of an authorization-server refusal into a
// providers.AuthError, which carries the OAuth error code the services layer maps
// to `consent_required` (a denied prompt, a scope the OAuth client may not use).
// Every other failure is returned untouched.
func authFailure(err error) error {
	var retrieve *oauth2.RetrieveError
	if !errors.As(err, &retrieve) {
		return err
	}
	return providers.NewAuthError(models.ProviderGoogle, retrieve.ErrorCode, retrieve.ErrorDescription)
}

// Refresh implements providers.Provider. Google only returns a new refresh
// token when it rotates it, so the stored one is kept otherwise.
func (p *Provider) Refresh(ctx context.Context, creds providers.Credentials, refreshToken string) (*providers.Tokens, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.httpClient)
	source := oauthConfig(creds).TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
	token, err := source.Token()
	if err != nil {
		return nil, fmt.Errorf("google drive: token refresh failed (reconnect the account): %w", err)
	}
	return tokensFromOAuth(token, refreshToken), nil
}

// tokensFromOAuth normalises an oauth2 token into the provider token set.
func tokensFromOAuth(token *oauth2.Token, previousRefresh string) *providers.Tokens {
	refresh := token.RefreshToken
	if refresh == "" {
		refresh = previousRefresh
	}
	scopes := []string{}
	if extra, ok := token.Extra("scope").(string); ok && extra != "" {
		for _, scope := range splitScopes(extra) {
			scopes = append(scopes, scope)
		}
	}
	return &providers.Tokens{
		AccessToken:  token.AccessToken,
		RefreshToken: refresh,
		TokenType:    normaliseTokenType(token.TokenType),
		Expiry:       token.Expiry.UTC(),
		Scopes:       scopes,
	}
}

// normaliseTokenType defaults to Bearer when Google omits the field.
func normaliseTokenType(raw string) string {
	if raw == "" {
		return "Bearer"
	}
	return raw
}

// Account implements providers.Provider using the OpenID userinfo endpoint.
func (p *Provider) Account(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) (*providers.Account, error) {
	var info struct {
		Sub     string `json:"sub"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Picture string `json:"picture"`
	}
	if err := p.doJSON(ctx, p.client(tokens), http.MethodGet, userInfoURL, nil, &info); err != nil {
		return nil, err
	}
	account := &providers.Account{
		ID:        info.Sub,
		Email:     info.Email,
		Name:      info.Name,
		AvatarURL: info.Picture,
	}
	if account.ID == "" {
		account.ID = info.Email
	}
	if account.ID == "" {
		return nil, fmt.Errorf("google drive: the token does not expose an account identity")
	}
	return account, nil
}
