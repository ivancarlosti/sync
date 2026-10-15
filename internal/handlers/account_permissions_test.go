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
// OneDrive scopes and the four OpenID Connect ones Entra ID never reports back.
const msaGrant = "https://graph.microsoft.com/User.Read " +
	"https://graph.microsoft.com/Files.ReadWrite.All offline_access openid profile email"

// directoryCapabilities are the capabilities a personal Microsoft account can
// never satisfy: exactly the ones the directory scopes back. CapabilityFiles is
// deliberately not among them — a personal account does reach its OneDrive.
var directoryCapabilities = []providers.Capability{
	providers.CapabilityUsers,
	providers.CapabilityGroups,
	providers.CapabilityMembers,
	providers.CapabilityDomains,
	providers.CapabilityOrgUnits,
	providers.CapabilityRoles,
	providers.CapabilityLicenses,
}

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
// the tenant claim of the access token exactly as it does in production.
func saveMicrosoftAccount(t *testing.T, deps Deps, access, kind string) {
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
		Scopes:            msaGrant,
		Status:            "connected",
		ExpiresAt:         fixedNow.Add(time.Hour),
		Kind:              kind,
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
		// wantAllMissing expects the pre-existing report of an account no signal
		// could classify: every capability missing, none not applicable. The
		// other cases expect the seven directory capabilities to be reported as
		// not applicable instead of missing.
		wantAllMissing bool
	}{
		{
			name:     "a personal account connected before kinds existed, classified from its token",
			claims:   map[string]any{"tid": msaTenant},
			wantKind: string(providers.AccountKindPersonal),
		},
		{
			name:     "a personal account already classified by its connection",
			kind:     string(providers.AccountKindPersonal),
			wantKind: string(providers.AccountKindPersonal),
		},
		{
			name:           "a work account keeps the report it always had",
			claims:         map[string]any{"tid": workTenant},
			wantKind:       string(providers.AccountKindWork),
			wantAllMissing: true,
		},
		{
			name:           "an unclassifiable account keeps the report it always had",
			wantKind:       "",
			wantAllMissing: true,
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
			saveMicrosoftAccount(t, deps, access, testCase.kind)

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

			if testCase.wantAllMissing {
				if !view.NeedsReconnect ||
					fmt.Sprint(view.MissingCapabilities) != fmt.Sprint(providers.ProviderCapabilities) {
					t.Fatalf("missing = %v (needs_reconnect=%v), want every capability",
						view.MissingCapabilities, view.NeedsReconnect)
				}
				if len(view.NotApplicableCapabilities) != 0 {
					t.Fatalf("not_applicable_capabilities = %v, want none", view.NotApplicableCapabilities)
				}
				return
			}

			if view.NeedsReconnect || len(view.MissingCapabilities) != 0 {
				t.Fatalf("a personal account is asked to reconnect for %v", view.MissingCapabilities)
			}
			if fmt.Sprint(view.Capabilities) != fmt.Sprint([]providers.Capability{providers.CapabilityFiles}) {
				t.Fatalf("capabilities = %v, want only files", view.Capabilities)
			}
			if fmt.Sprint(view.NotApplicableCapabilities) != fmt.Sprint(directoryCapabilities) {
				t.Fatalf("not_applicable_capabilities = %v, want %v",
					view.NotApplicableCapabilities, directoryCapabilities)
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
