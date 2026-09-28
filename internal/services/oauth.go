package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/crypto"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// stateTTL is how long an authorization request stays redeemable. It is short
// on purpose: the operator is in the middle of a browser redirect.
const stateTTL = 10 * time.Minute

// OAuthService drives the provider authorization-code flows (Google Drive and
// Microsoft Graph). It never talks to HTTP itself: the provider implementations
// own their endpoints, this service owns the state, the exchange and the
// persistence of the resulting account.
type OAuthService struct {
	store     *Store
	registry  *providers.Registry
	creds     *ProviderSettings
	tokens    *TokenManager
	publisher EventPublisher
	now       func() time.Time
}

// Authorization is the result of Begin: where to send the browser and the state
// the callback must carry back.
type Authorization struct {
	URL   string `json:"url"`
	State string `json:"state"`
}

// NewOAuthService builds the service.
func NewOAuthService(store *Store, registry *providers.Registry, creds *ProviderSettings, tokens *TokenManager) *OAuthService {
	return &OAuthService{
		store:    store,
		registry: registry,
		creds:    creds,
		tokens:   tokens,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// SetPublisher plugs the notifier in. It is optional: the OAuth flows work in a
// build without notification channels.
func (o *OAuthService) SetPublisher(publisher EventPublisher) { o.publisher = publisher }

// Providers returns the providers of this build that have a usable OAuth client,
// which is what the Connect page lists.
func (o *OAuthService) Providers(ctx context.Context) []ProviderCredentialsInfo {
	infos := make([]ProviderCredentialsInfo, 0, 2)
	for _, name := range o.registry.Names() {
		info, err := o.creds.Info(ctx, name)
		if err != nil {
			slog.Warn("reading provider information", "provider", name, "error", err)
			continue
		}
		infos = append(infos, info)
	}
	return infos
}

// Begin creates the server side of a flow (CSRF state + PKCE verifier) and
// returns the authorization URL to redirect the browser to.
func (o *OAuthService) Begin(ctx context.Context, provider models.ProviderName, redirectTo string) (Authorization, error) {
	if !provider.Valid() {
		return Authorization{}, fmt.Errorf("%w: unknown provider %q", ErrValidation, provider)
	}
	implementation, err := o.registry.Get(provider)
	if err != nil {
		return Authorization{}, err
	}
	creds, err := o.creds.Credentials(ctx, provider)
	if err != nil {
		return Authorization{}, err
	}

	state, err := crypto.RandomToken(32)
	if err != nil {
		return Authorization{}, fmt.Errorf("services: generating the oauth state: %w", err)
	}
	verifier, challenge, err := crypto.PKCEPair()
	if err != nil {
		return Authorization{}, fmt.Errorf("services: generating the PKCE pair: %w", err)
	}

	row := &models.OAuthState{
		State:        state,
		Flow:         string(models.FlowOAuth),
		Provider:     string(provider),
		CodeVerifier: verifier,
		RedirectTo:   SafeRedirect(redirectTo),
		ExpiresAt:    o.now().Add(stateTTL),
	}
	if err := o.store.SaveOAuthState(ctx, row); err != nil {
		return Authorization{}, err
	}
	// The flow uses PKCE; the verifier never leaves the server.
	return Authorization{URL: implementation.AuthCodeURL(creds, state, challenge), State: state}, nil
}

// Complete redeems the authorization code returned by the provider, reads the
// remote identity and stores (or rotates) the connected account.
//
// The state row is consumed first, so a replayed callback is rejected before any
// network call happens.
func (o *OAuthService) Complete(ctx context.Context, provider models.ProviderName, code, stateValue string) (*models.ConnectedAccount, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("%w: the provider did not return an authorization code", ErrValidation)
	}
	if strings.TrimSpace(stateValue) == "" {
		return nil, fmt.Errorf("%w: the authorization request is missing its state", ErrValidation)
	}
	row, err := o.store.ConsumeOAuthState(ctx, stateValue)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: this authorization link was already used or has expired", ErrUnauthorized)
		}
		return nil, err
	}
	if row.Flow != string(models.FlowOAuth) || row.Provider != string(provider) {
		return nil, fmt.Errorf("%w: the authorization request does not belong to %s", ErrValidation, provider)
	}
	if row.Expired(o.now()) {
		return nil, fmt.Errorf("%w: the authorization request expired, start again", ErrValidation)
	}

	implementation, err := o.registry.Get(provider)
	if err != nil {
		return nil, err
	}
	creds, err := o.creds.Credentials(ctx, provider)
	if err != nil {
		return nil, err
	}
	tokens, err := implementation.Exchange(ctx, creds, code, row.CodeVerifier)
	if err != nil {
		return nil, fmt.Errorf("%w: exchanging the %s authorization code failed: %v", ErrValidation, provider, err)
	}
	remote, err := implementation.Account(ctx, creds, tokens)
	if err != nil {
		return nil, fmt.Errorf("services: reading the %s account failed: %w", provider, err)
	}
	if remote == nil || strings.TrimSpace(remote.ID) == "" {
		return nil, fmt.Errorf("%w: %s did not describe the authorized account", ErrValidation, provider)
	}

	account, err := o.store.FindAccount(ctx, provider, remote.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		account = &models.ConnectedAccount{Provider: string(provider), ProviderAccountID: remote.ID}
	case err != nil:
		return nil, err
	}
	account.Email = remote.Email
	if remote.Name != "" {
		account.DisplayName = remote.Name
	}
	account.AvatarURL = remote.AvatarURL
	if err := o.tokens.Persist(ctx, account, tokens); err != nil {
		return nil, err
	}

	slog.Info("connected provider account",
		"provider", provider, "account", account.MaskEmail(), "account_id", account.ID)
	if o.publisher != nil {
		o.publisher.Publish(ctx, models.EventAccountConnected, map[string]any{
			"provider": string(provider),
			"email":    account.Email,
			"id":       account.ID,
		})
	}
	return account, nil
}

// PruneStates deletes the authorization requests that were never redeemed.
func (o *OAuthService) PruneStates(ctx context.Context) (int64, error) {
	result := o.store.DB().WithContext(ctx).
		Where("expires_at < ?", o.now().Add(-stateTTL)).
		Delete(&models.OAuthState{})
	if result.Error != nil {
		return 0, fmt.Errorf("services: pruning oauth states: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// SafeRedirect keeps only a same-origin path from a caller supplied redirect
// target. Anything absolute, protocol relative or containing a control character
// is dropped, so a crafted `redirect_to` cannot turn the OAuth callback into an
// open redirect.
func SafeRedirect(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return ""
	}
	if strings.ContainsAny(value, "\\\r\n\t") {
		return ""
	}
	return value
}
