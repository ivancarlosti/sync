package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/services"
)

// The account kind of a Microsoft connection: the tenant of a personal account
// (an MSA) is the well known *consumers* tenant, any other tenant id is a work or
// school directory. See internal/providers/microsoft/account_kind.go.
const (
	msaTenant  = "9188040d-6c67-4c5b-b112-36a304b66dad"
	workTenant = "a1b2c3d4-1111-2222-3333-444455556666"
)

// msaGrant is the scope set a personal Microsoft account really receives: the
// OneDrive scopes, the calendar scope and the four OpenID Connect ones Entra ID
// never reports back.
const msaGrant = "https://graph.microsoft.com/User.Read " +
	"https://graph.microsoft.com/Files.ReadWrite.All " +
	"https://graph.microsoft.com/Calendars.ReadWrite " +
	"offline_access openid profile email"

// msaLegacyGrant is the same grant without the calendar scope: it is what an
// account connected before calendar sync existed holds, so the only capability a
// reconnect can still add is `calendars`.
const msaLegacyGrant = "https://graph.microsoft.com/User.Read " +
	"https://graph.microsoft.com/Files.ReadWrite.All offline_access openid profile email"

// accountJWT shapes claims into a JWT-shaped access token: the classifier reads
// the payload, the header and the signature are only there to look realistic.
func accountJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("encoding the claims: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

// saveMicrosoftAccount stores a Microsoft account the way the OAuth callback
// does: the tokens are sealed with the key of the instance, so a handler can read
// the tenant claim of the access token exactly as it does in production. org is
// the connected_accounts.org_id column: empty for a row connected before the
// column existed (or before the provider reported one).
func saveMicrosoftAccount(t *testing.T, deps Deps, access, kind, org string) {
	t.Helper()
	saveMicrosoftAccountWithScopes(t, deps, access, kind, org, msaGrant)
}

// saveMicrosoftAccountWithScopes is saveMicrosoftAccount with an explicit grant,
// which is how a connection made before a scope was requested is reproduced.
func saveMicrosoftAccountWithScopes(t *testing.T, deps Deps, access, kind, org, scopes string) {
	t.Helper()
	box := services.NewSecretBox(deps.Config.EncryptionKeyBytes())
	sealed, err := box.Encrypt(access)
	if err != nil {
		t.Fatalf("sealing the access token: %v", err)
	}
	refresh, err := box.Encrypt("refresh-token")
	if err != nil {
		t.Fatalf("sealing the refresh token: %v", err)
	}
	account := &models.ConnectedAccount{
		Provider:          string(models.ProviderMicrosoft),
		ProviderAccountID: "ms-personal",
		Email:             "operator@outlook.com",
		AccessToken:       sealed,
		RefreshToken:      refresh,
		Scopes:            scopes,
		Status:            "connected",
		ExpiresAt:         fixedNow.Add(time.Hour),
		Kind:              kind,
		OrgID:             org,
	}
	if err := deps.Store.SaveAccount(context.Background(), account); err != nil {
		t.Fatalf("saving the account: %v", err)
	}
}

// TestPersonalAccountsNeverAskToReconnectForDirectoryPermissions reproduces the
// reported failure end to end: a personal Microsoft account was listed with every
// capability red and a "reconnect to grant them" alert, although a personal
// account can never be granted the directory permissions. The kind comes either
// from the connection (the stored column) or, for an account connected before the
// column existed, from the tenant claim of its stored access token.
func TestPersonalAccountsNeverAskToReconnectForDirectoryPermissions(t *testing.T) {
	cases := []struct {
		name string
		// claims of the stored access token; nil stores an opaque token.
		claims map[string]any
		// kind is the connected_accounts.kind column, empty when a connection
		// never reported one.
		kind     string
		wantKind string
		// scopes is the grant stored with the connection; empty means the grant
		// a connection made today receives.
		scopes string
		// wantCapabilities is the granted list, in the display order of the
		// vocabulary.
		wantCapabilities []providers.Capability
		// wantMissing is the actionable remainder: the capabilities a reconnect
		// could still add. A personal account is never missing a directory
		// capability, whether it was classified or not.
		wantMissing []providers.Capability
	}{
		{
			name:             "a personal account connected before kinds existed, classified from its token",
			claims:           map[string]any{"tid": msaTenant},
			wantKind:         string(providers.AccountKindPersonal),
			wantCapabilities: []providers.Capability{providers.CapabilityFiles, providers.CapabilityCalendars},
		},
		{
			name:             "a personal account already classified by its connection",
			kind:             string(providers.AccountKindPersonal),
			wantKind:         string(providers.AccountKindPersonal),
			wantCapabilities: []providers.Capability{providers.CapabilityFiles, providers.CapabilityCalendars},
		},
		{
			// The grant of a connection made before calendar sync existed. The
			// only thing a reconnect adds is the calendar capability: the
			// directory rows stay out of the report, because a personal account
			// can never hold them.
			name:             "a personal account connected before the calendar scope ignores it",
			kind:             string(providers.AccountKindPersonal),
			wantKind:         string(providers.AccountKindPersonal),
			scopes:           msaLegacyGrant,
			wantCapabilities: []providers.Capability{providers.CapabilityFiles},
			wantMissing:      []providers.Capability{providers.CapabilityCalendars},
		},
		{
			name:        "a work account keeps the report it always had",
			claims:      map[string]any{"tid": workTenant},
			wantKind:    string(providers.AccountKindWork),
			scopes:      msaLegacyGrant,
			wantMissing: providers.CapabilitiesFor(models.ProviderMicrosoft),
		},
		{
			name:        "an unclassifiable account keeps the report it always had",
			wantKind:    "",
			scopes:      msaLegacyGrant,
			wantMissing: providers.CapabilitiesFor(models.ProviderMicrosoft),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			deps := testDepsWith(t, models.AuthModeNone, testOptions{})
			server, err := New(deps)
			if err != nil {
				t.Fatalf("building the server: %v", err)
			}
			access := "EwBIA8l6BAAUopaque"
			if testCase.claims != nil {
				access = accountJWT(t, testCase.claims)
			}
			scopes := testCase.scopes
			if scopes == "" {
				scopes = msaGrant
			}
			saveMicrosoftAccountWithScopes(t, deps, access, testCase.kind, "", scopes)

			recorder := call(t, server, http.MethodGet, "/api/accounts", nil)
			requireStatus(t, recorder, http.StatusOK)
			list := decodeJSON[struct {
				Accounts []accountView `json:"accounts"`
			}](t, recorder)
			if len(list.Accounts) != 1 {
				t.Fatalf("accounts = %+v", list.Accounts)
			}
			view := list.Accounts[0]
			if view.AccountKind != testCase.wantKind {
				t.Fatalf("account_kind = %q, want %q", view.AccountKind, testCase.wantKind)
			}

			if len(view.NotApplicableCapabilities) != 0 {
				t.Fatalf("not_applicable_capabilities = %v, want none", view.NotApplicableCapabilities)
			}
			if fmt.Sprint(view.MissingCapabilities) != fmt.Sprint(testCase.wantMissing) {
				t.Fatalf("missing = %v, want %v", view.MissingCapabilities, testCase.wantMissing)
			}
			if view.NeedsReconnect != (len(testCase.wantMissing) > 0) {
				t.Fatalf("needs_reconnect = %v, want %v", view.NeedsReconnect, len(testCase.wantMissing) > 0)
			}
			if testCase.wantCapabilities != nil && fmt.Sprint(view.Capabilities) != fmt.Sprint(testCase.wantCapabilities) {
				t.Fatalf("capabilities = %v, want %v", view.Capabilities, testCase.wantCapabilities)
			}
		})
	}
}

// TestVerifyAccountStoresTheAccountKind pins the write-back of the kind: the
// identity refresh that "test this account" performs keeps the kind the provider
// reported, so the next list does not have to read the token again — and a
// provider that cannot classify the account this time must not clear it.
func TestVerifyAccountStoresTheAccountKind(t *testing.T) {
	stub := &stubProvider{
		name:    models.ProviderMicrosoft,
		account: providers.Account{ID: "ms-1", Email: "operator@outlook.com", Kind: providers.AccountKindPersonal},
	}
	server, store := newTestServerWith(t, models.AuthModeNone, testOptions{
		configure: func(cfg *config.Config) {
			cfg.Microsoft = config.ProviderCredentials{
				ClientID:     "stub-client",
				ClientSecret: "stub-secret",
				RedirectURI:  "https://sync.example.com/api/oauth/microsoft/callback",
			}
		},
		providers: []providers.Provider{stub},
	})

	account := saveAccount(t, store, models.ProviderMicrosoft, "ms-1", "operator@outlook.com")
	if err := server.deps.Tokens.Persist(context.Background(), account, &providers.Tokens{
		AccessToken:  "stub-access",
		RefreshToken: "stub-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("storing the tokens: %v", err)
	}
	id := strconv.FormatUint(uint64(account.ID), 10)

	recorder := call(t, server, http.MethodPost, "/api/accounts/"+id+"/verify", nil)
	requireStatus(t, recorder, http.StatusOK)
	verified := decodeJSON[accountView](t, recorder)
	if verified.AccountKind != string(providers.AccountKindPersonal) {
		t.Fatalf("account_kind = %q, want personal", verified.AccountKind)
	}

	// The stub cannot classify the account this time; the stored kind stays.
	stub.account.Kind = providers.AccountKindUnknown
	recorder = call(t, server, http.MethodPost, "/api/accounts/"+id+"/verify", nil)
	requireStatus(t, recorder, http.StatusOK)
	verified = decodeJSON[accountView](t, recorder)
	if verified.AccountKind != string(providers.AccountKindPersonal) {
		t.Fatalf("an unclassifying refresh cleared the account kind: %q", verified.AccountKind)
	}

	stored, err := store.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("reloading the account: %v", err)
	}
	if stored.Kind != string(providers.AccountKindPersonal) {
		t.Fatalf("stored kind = %q, want personal", stored.Kind)
	}
}
