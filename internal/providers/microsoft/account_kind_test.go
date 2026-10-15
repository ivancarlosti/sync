package microsoft

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/providers"
)

// workTenant is a directory that is not the well known consumer tenant: any
// tenant id but the MSA one means "work or school account".
const workTenant = "a1b2c3d4-1111-2222-3333-444455556666"

// jwt shapes claims into a compact JWT: the payload is what this package reads,
// the header and the signature are only there to keep the token realistic.
func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("encoding the claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

// TestAccountKindOfReadsTheTenantClaim pins the classification: the `tid` claim
// answers it, the tenant segment of `iss` answers it when `tid` is absent, and
// anything unreadable stays unknown so nothing is filtered out for it.
func TestAccountKindOfReadsTheTenantClaim(t *testing.T) {
	provider := New()

	cases := []struct {
		name   string
		tokens *providers.Tokens
		want   providers.AccountKind
	}{
		{
			name:   "personal account",
			tokens: &providers.Tokens{AccessToken: jwt(t, map[string]any{"tid": consumerTenantID})},
			want:   providers.AccountKindPersonal,
		},
		{
			name:   "personal account in another case",
			tokens: &providers.Tokens{AccessToken: jwt(t, map[string]any{"tid": strings.ToUpper(consumerTenantID)})},
			want:   providers.AccountKindPersonal,
		},
		{
			name:   "work account",
			tokens: &providers.Tokens{AccessToken: jwt(t, map[string]any{"tid": workTenant})},
			want:   providers.AccountKindWork,
		},
		{
			name:   "v2 issuer instead of tid",
			tokens: &providers.Tokens{AccessToken: jwt(t, map[string]any{"iss": "https://login.microsoftonline.com/" + consumerTenantID + "/v2.0"})},
			want:   providers.AccountKindPersonal,
		},
		{
			name:   "v1 issuer instead of tid",
			tokens: &providers.Tokens{AccessToken: jwt(t, map[string]any{"iss": "https://sts.windows.net/" + workTenant + "/"})},
			want:   providers.AccountKindWork,
		},
		{
			name:   "no tokens at all",
			tokens: nil,
			want:   providers.AccountKindUnknown,
		},
		{
			name:   "empty access token",
			tokens: &providers.Tokens{},
			want:   providers.AccountKindUnknown,
		},
		{
			name:   "opaque access token",
			tokens: &providers.Tokens{AccessToken: "EwBIA8l6BAAU..."},
			want:   providers.AccountKindUnknown,
		},
		{
			name:   "payload is not base64url",
			tokens: &providers.Tokens{AccessToken: "header.%%%.signature"},
			want:   providers.AccountKindUnknown,
		},
		{
			name:   "payload is not a JSON object",
			tokens: &providers.Tokens{AccessToken: "header." + base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".signature"},
			want:   providers.AccountKindUnknown,
		},
		{
			name:   "claims without a tenant",
			tokens: &providers.Tokens{AccessToken: jwt(t, map[string]any{"aud": graphResource, "sub": "abc"})},
			want:   providers.AccountKindUnknown,
		},
		{
			name:   "issuer without a tenant segment",
			tokens: &providers.Tokens{AccessToken: jwt(t, map[string]any{"iss": "https://sts.windows.net"})},
			want:   providers.AccountKindUnknown,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := provider.AccountKindOf(testCase.tokens); got != testCase.want {
				t.Fatalf("AccountKindOf() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestProviderImplementsTheAccountClassifier pins the optional contract the
// accounts handler relies on: without it the handler answers no account_kind at
// all, and the directory permissions of a personal account turn red again.
func TestProviderImplementsTheAccountClassifier(t *testing.T) {
	var implementation providers.Provider = New()
	if _, ok := implementation.(providers.AccountClassifier); !ok {
		t.Fatal("*Provider no longer implements providers.AccountClassifier")
	}
}
