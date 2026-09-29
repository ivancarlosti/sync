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

// BeginAdminConsent starts the tenant-wide consent flow of a provider
// (Microsoft Entra admin consent). Only providers implementing
// providers.ConsentGranter have one, and they need the credentials to be
// configured first.
//
// The returned state is a single-use, ten minute token that the consent redirect
// carries back to the provider callback, which is what makes an unsolicited
// `admin_consent=True` impossible to forge.
func (o *OAuthService) BeginAdminConsent(ctx context.Context, provider models.ProviderName, redirectTo string) (Authorization, error) {
	if !provider.Valid() {
		return Authorization{}, fmt.Errorf("%w: unknown provider %q", ErrValidation, provider)
	}
	implementation, err := o.registry.Get(provider)
	if err != nil {
		return Authorization{}, err
	}
	granter, ok := implementation.(providers.ConsentGranter)
	if !ok {
		return Authorization{}, fmt.Errorf("%w: %s does not have a tenant-wide consent step", ErrValidation, provider)
	}
	creds, err := o.creds.Credentials(ctx, provider)
	if err != nil {
		return Authorization{}, err
	}
	state, err := crypto.RandomToken(32)
	if err != nil {
		return Authorization{}, fmt.Errorf("services: generating the consent state: %w", err)
	}
	row := &models.OAuthState{
		State:      state,
		Flow:       string(models.FlowAdminConsent),
		Provider:   string(provider),
		RedirectTo: SafeRedirect(redirectTo),
		ExpiresAt:  o.now().Add(stateTTL),
	}
	if err := o.store.SaveOAuthState(ctx, row); err != nil {
		return Authorization{}, err
	}
	return Authorization{URL: granter.AdminConsentURL(creds, state), State: state}, nil
}

// CompleteAdminConsent redeems the state an admin-consent redirect carried back
// and records the consent for the tenant. It returns the same-origin page the
// flow asked to return to (possibly empty).
//
// tenant is whatever the identity platform reported (`tenant` query parameter);
// it is stored for display only, since the directory an administrator consented
// for is defined by the sign-in that just happened.
func (o *OAuthService) CompleteAdminConsent(ctx context.Context, provider models.ProviderName, stateValue, tenant string) (string, error) {
	if strings.TrimSpace(stateValue) == "" {
		return "", fmt.Errorf("%w: the consent request is missing its state", ErrValidation)
	}
	row, err := o.store.ConsumeOAuthState(ctx, stateValue)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", fmt.Errorf("%w: this consent link was already used or has expired", ErrUnauthorized)
		}
		return "", err
	}
	if row.Flow != string(models.FlowAdminConsent) || row.Provider != string(provider) {
		return "", fmt.Errorf("%w: the consent request does not belong to %s", ErrValidation, provider)
	}
	if row.Expired(o.now()) {
		return "", fmt.Errorf("%w: the consent request expired, start again", ErrValidation)
	}
	if _, err := o.registry.Get(provider); err != nil {
		return "", err
	}
	if err := o.creds.SetAdminConsent(ctx, provider, tenant); err != nil {
		return "", err
	}
	slog.Info("provider admin consent granted", "provider", provider, "tenant", tenant)
	if o.publisher != nil {
		o.publisher.Publish(ctx, models.EventAccountConnected, map[string]any{
			"provider": string(provider),
			"tenant":   tenant,
			"consent":  "admin",
		})
	}
	return row.RedirectTo, nil
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
		if providers.IsConsentRequired(err) {
			// The permissions were never granted (or the tenant-wide consent is
			// missing): the fix is in the provider console, so this is reported
			// as ErrConsent (412 + `consent_required`) and not as a validation
			// problem with the request.
			return nil, fmt.Errorf("%w: connecting %s failed because the permissions are missing: %v",
				ErrConsent, provider, err)
		}
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

// FlowOf reports the kind of the pending flow a state belongs to without
// consuming it, which is what the provider callback needs in order to tell a
// tenant-wide consent answer from an authorization code before redeeming
// anything. A missing, unknown or foreign state answers ErrNotFound, so the
// caller falls back to the authorization-code path.
func (o *OAuthService) FlowOf(ctx context.Context, provider models.ProviderName, stateValue string) (models.FlowKind, error) {
	if strings.TrimSpace(stateValue) == "" {
		return "", fmt.Errorf("%w: no state in the callback", ErrNotFound)
	}
	row, err := o.store.FindOAuthState(ctx, stateValue)
	if err != nil {
		return "", err
	}
	if row.Provider != string(provider) {
		return "", fmt.Errorf("%w: the state does not belong to %s", ErrNotFound, provider)
	}
	return models.FlowKind(row.Flow), nil
}

// AbandonAdminConsent consumes a consent flow that was refused or cancelled and
// returns the page the operator asked to return to (possibly empty). It is best
// effort on purpose: the operator is being redirected out of a failure, so a
// storage problem must not replace the failure message.
func (o *OAuthService) AbandonAdminConsent(ctx context.Context, provider models.ProviderName, stateValue string) string {
	if strings.TrimSpace(stateValue) == "" {
		return ""
	}
	row, err := o.store.ConsumeOAuthState(ctx, stateValue)
	if err != nil {
		return ""
	}
	if row.Provider != string(provider) || row.Flow != string(models.FlowAdminConsent) {
		return ""
	}
	return row.RedirectTo
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
