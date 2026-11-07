package services

import (
	"encoding/base64"
	"testing"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/providers/microsoft"
)

// TestTokenManagerOrgIDReadsTheTenantClaim pins the fallback the collector uses
// for the accounts connected before connected_accounts.org_id was filled: the
// tenant of the `tid` claim of the access token, lower-cased, without a provider
// call — and an empty answer for a personal account or an unreadable token, which
// is what keeps the collector from guessing an organisation.
func TestTokenManagerOrgIDReadsTheTenantClaim(t *testing.T) {
	cfg := testConfig(t)
	store := newTestStore(t)
	box := NewSecretBox(cfg.EncryptionKeyBytes())
	tokens := NewTokenManager(store, providers.NewRegistry(microsoft.New()), NewProviderSettings(cfg, store.Settings(), box), box)

	claim := func(payload string) string {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
		return "header." + encoded + ".signature"
	}
	cases := []struct {
		name    string
		token   string
		want    string
		noToken bool
	}{
		{
			name:  "work account",
			token: claim(`{"tid":"11111111-2222-3333-4444-555555555555"}`),
			want:  "11111111-2222-3333-4444-555555555555",
		},
		{
			name:  "tenant claim is lower-cased",
			token: claim(`{"tid":"AAAABBBB-CCCC-DDDD-EEEE-FFFF00001111"}`),
			want:  "aaaabbbb-cccc-dddd-eeee-ffff00001111",
		},
		{
			name:  "personal account",
			token: claim(`{"tid":"9188040d-6c67-4c5b-b112-36a304b66dad"}`),
			want:  "",
		},
		{
			name:  "opaque token",
			token: "not-a-jwt",
			want:  "",
		},
		{
			name:    "no stored token",
			noToken: true,
			want:    "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			account := &models.ConnectedAccount{Provider: string(models.ProviderMicrosoft)}
			if !testCase.noToken {
				encrypted, err := box.Encrypt(testCase.token)
				if err != nil {
					t.Fatalf("encrypting the token: %v", err)
				}
				account.AccessToken = encrypted
			}
			if got := tokens.OrgID(account); got != testCase.want {
				t.Fatalf("OrgID() = %q, want %q", got, testCase.want)
			}
		})
	}
	if got := tokens.OrgID(nil); got != "" {
		t.Fatalf("OrgID(nil) = %q, want an empty organisation", got)
	}
}
