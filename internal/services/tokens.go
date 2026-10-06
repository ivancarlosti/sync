package services

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// TokenManager owns the lifecycle of the stored OAuth tokens: it decrypts them,
// refreshes the ones about to expire, hands a usable token set to a callback
// and persists whatever the provider rotated.
//
// Nothing else in the code base reads connected_accounts.access_token or
// refresh_token, which is what keeps "tokens are encrypted at rest" true.
type TokenManager struct {
	store    *Store
	registry *providers.Registry
	creds    *ProviderSettings
	box      *SecretBox
	now      func() time.Time
}

// NewTokenManager builds the manager.
func NewTokenManager(store *Store, registry *providers.Registry, creds *ProviderSettings, box *SecretBox) *TokenManager {
	return &TokenManager{
		store:    store,
		registry: registry,
		creds:    creds,
		box:      box,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// ProviderSession is everything a provider call needs: the account row, its
// provider implementation, the credentials of that provider and a valid token
// set. The name is explicit so it is never confused with the operator session
// (services.Session) used by the browser login.
type ProviderSession struct {
	Account     *models.ConnectedAccount
	Provider    providers.Provider
	Credentials providers.Credentials
	Tokens      *providers.Tokens
}

// Use loads an account, guarantees a fresh access token and runs fn with it.
//
// Provider failures are returned untouched so the caller can decide (the sync
// engine treats "not found" as a deletion); token failures are marked on the
// account and wrapped in ErrReconnect so the UI can ask for a reconnection.
func (m *TokenManager) Use(ctx context.Context, accountID uint, fn func(session ProviderSession) error) error {
	session, err := m.Resolve(ctx, accountID)
	if err != nil {
		return err
	}
	return fn(session)
}

// Resolve loads an account, its provider, its credentials and a valid token set
// in one step. Use() is the callback flavour of it; the sync engine needs both
// sides of a job at once, so it calls Resolve twice instead.
func (m *TokenManager) Resolve(ctx context.Context, accountID uint) (ProviderSession, error) {
	account, err := m.store.GetAccount(ctx, accountID)
	if err != nil {
		return ProviderSession{}, err
	}
	provider, err := m.registry.Get(models.ProviderName(account.Provider))
	if err != nil {
		return ProviderSession{}, err
	}
	creds, err := m.creds.Credentials(ctx, models.ProviderName(account.Provider))
	if err != nil {
		return ProviderSession{}, err
	}

	tokens, err := m.tokens(account)
	if err != nil {
		_ = m.store.MarkAccountError(ctx, account.ID, err.Error())
		return ProviderSession{}, err
	}
	if tokens.Expired(m.now()) {
		tokens, err = m.refresh(ctx, provider, creds, account, tokens)
		if err != nil {
			return ProviderSession{}, err
		}
	}
	return ProviderSession{Account: account, Provider: provider, Credentials: creds, Tokens: tokens}, nil
}

// Reissue forces a renewal of the access token of an account and persists it.
//
// Resolve only refreshes when the stored token is already expired, which is
// enough for a short call but not for a long sync run: the engine holds its own
// token snapshot for the whole run (an access token lives about an hour, a run
// may last up to the configured timeout). When a provider answers 401 mid-run the
// engine calls Reissue to swap that snapshot, so the run recovers instead of
// failing every remaining file with the same opaque error. A failure here means
// the account really has to be connected again, so it is wrapped in ErrReconnect
// (and marked on the account) exactly like the proactive refresh path.
func (m *TokenManager) Reissue(ctx context.Context, accountID uint) (*providers.Tokens, error) {
	account, err := m.store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	provider, err := m.registry.Get(models.ProviderName(account.Provider))
	if err != nil {
		return nil, err
	}
	creds, err := m.creds.Credentials(ctx, models.ProviderName(account.Provider))
	if err != nil {
		return nil, err
	}
	tokens, err := m.tokens(account)
	if err != nil {
		_ = m.store.MarkAccountError(ctx, account.ID, err.Error())
		return nil, err
	}
	return m.refresh(ctx, provider, creds, account, tokens)
}

// tokens decrypts the stored token pair of an account.
func (m *TokenManager) tokens(account *models.ConnectedAccount) (*providers.Tokens, error) {
	access, err := m.box.Decrypt(account.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("%w: the stored access token of %s cannot be decrypted (was ENCRYPTION_KEY changed?)",
			ErrReconnect, account.MaskEmail())
	}
	refresh, err := m.box.Decrypt(account.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("%w: the stored refresh token of %s cannot be decrypted (was ENCRYPTION_KEY changed?)",
			ErrReconnect, account.MaskEmail())
	}
	return &providers.Tokens{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    account.TokenType,
		Expiry:       account.ExpiresAt,
		Scopes:       splitScopes(account.Scopes),
	}, nil
}

// refresh renews the access token and stores the result. A provider that does
// not return a new refresh token (Google) keeps the stored one.
func (m *TokenManager) refresh(ctx context.Context, provider providers.Provider, creds providers.Credentials, account *models.ConnectedAccount, tokens *providers.Tokens) (*providers.Tokens, error) {
	if tokens.RefreshToken == "" {
		err := fmt.Errorf("%w: %s has no refresh token, connect the account again", ErrReconnect, account.MaskEmail())
		_ = m.store.MarkAccountError(ctx, account.ID, err.Error())
		return nil, err
	}
	renewed, err := provider.Refresh(ctx, creds, tokens.RefreshToken)
	if err != nil {
		wrapped := fmt.Errorf("%w: refreshing the %s token of %s failed: %v",
			ErrReconnect, account.Provider, account.MaskEmail(), err)
		_ = m.store.MarkAccountError(ctx, account.ID, wrapped.Error())
		return nil, wrapped
	}
	if renewed.RefreshToken == "" {
		renewed.RefreshToken = tokens.RefreshToken
	}
	if err := m.Persist(ctx, account, renewed); err != nil {
		return nil, err
	}
	slog.Debug("refreshed provider token",
		"provider", account.Provider,
		"account", account.MaskEmail(),
		"expires_at", renewed.Expiry.UTC().Format(time.RFC3339))
	return renewed, nil
}

// Persist encrypts a token set and writes it on the account row, keeping the
// status and the "reconnect required" message consistent.
func (m *TokenManager) Persist(ctx context.Context, account *models.ConnectedAccount, tokens *providers.Tokens) error {
	access, err := m.box.Encrypt(tokens.AccessToken)
	if err != nil {
		return err
	}
	refresh, err := m.box.Encrypt(tokens.RefreshToken)
	if err != nil {
		return err
	}
	now := m.now()
	account.AccessToken = access
	account.RefreshToken = refresh
	account.TokenType = tokens.TokenType
	account.ExpiresAt = tokens.Expiry
	account.Scopes = strings.Join(tokens.Scopes, " ")
	account.Status = string(models.AccountConnected)
	account.LastError = ""
	account.RefreshedAt = &now
	return m.store.SaveAccount(ctx, account)
}

// RefreshExpiring walks the accounts whose access token expires soon and renews
// them, so a scheduled run never starts on an expired token. It returns how many
// accounts were refreshed.
func (m *TokenManager) RefreshExpiring(ctx context.Context, within time.Duration) int {
	accounts, err := m.store.ListAccounts(ctx)
	if err != nil {
		slog.Error("listing accounts for the token refresh", "error", err)
		return 0
	}
	refreshed := 0
	for i := range accounts {
		account := accounts[i]
		if account.RefreshToken == "" || !account.Expired(m.now().Add(within)) {
			continue
		}
		provider, err := m.registry.Get(models.ProviderName(account.Provider))
		if err != nil {
			continue
		}
		creds, err := m.creds.Credentials(ctx, models.ProviderName(account.Provider))
		if err != nil {
			continue
		}
		tokens, err := m.tokens(&account)
		if err != nil {
			_ = m.store.MarkAccountError(ctx, account.ID, err.Error())
			continue
		}
		if _, err := m.refresh(ctx, provider, creds, &account, tokens); err != nil {
			slog.Warn("proactive token refresh failed",
				"provider", account.Provider, "account", account.MaskEmail(), "error", err)
			continue
		}
		refreshed++
	}
	return refreshed
}

// splitScopes splits the space separated scope string stored on an account.
// A grant stored by a release that kept the percent-encoded value Microsoft
// Entra returns is decoded here, so the tokens of an existing account are read
// as the same list (see providers.SplitScopes).
func splitScopes(raw string) []string {
	return providers.SplitScopes(raw)
}
