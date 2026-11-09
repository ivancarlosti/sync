package microsoft

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

// TestLastLoginFromRecord pins the shape of a UserLoggedIn audit record: only a
// successful sign-in is an answer, the address is the user principal name (with
// ObjectId as the fallback of a workload that left UserId empty), and a record
// without a readable time is skipped rather than guessed.
func TestLastLoginFromRecord(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		address string
		ok      bool
	}{
		{
			name:    "successful sign in",
			raw:     `{"Operation":"UserLoggedIn","CreationTime":"2026-11-08T09:00:00","UserId":"Ada@Contoso.com","ResultStatus":"success"}`,
			address: "ada@contoso.com",
			ok:      true,
		},
		{
			name: "failed sign in",
			raw:  `{"Operation":"UserLoggedIn","CreationTime":"2026-11-08T09:00:00","UserId":"ada@contoso.com","ResultStatus":"failed"}`,
			ok:   false,
		},
		{
			name: "another operation",
			raw:  `{"Operation":"Add user.","CreationTime":"2026-11-08T09:00:00","UserId":"ada@contoso.com","ResultStatus":"success"}`,
			ok:   false,
		},
		{
			name:    "object id fallback",
			raw:     `{"Operation":"UserLoggedIn","CreationTime":"2026-11-08T09:00:00","ObjectId":"grace@contoso.com","ResultStatus":"success"}`,
			address: "grace@contoso.com",
			ok:      true,
		},
		{
			name: "no readable time",
			raw:  `{"Operation":"UserLoggedIn","CreationTime":"","UserId":"ada@contoso.com"}`,
			ok:   false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			address, occurred, ok := lastLoginFromRecord(json.RawMessage(testCase.raw))
			if ok != testCase.ok {
				t.Fatalf("ok = %v, want %v", ok, testCase.ok)
			}
			if !ok {
				return
			}
			if address != testCase.address {
				t.Errorf("address = %q, want %q", address, testCase.address)
			}
			if occurred.IsZero() {
				t.Error("a successful record answered a zero time")
			}
		})
	}
}

// TestSignInActivityOfPrefersTheSuccessfulValue pins the column a user reads: the
// last successful sign-in first, then the last interactive attempt, then the
// non-interactive one.
func TestSignInActivityOfPrefersTheSuccessfulValue(t *testing.T) {
	user := reportUser{SignInActivity: &reportSignInActivity{
		LastSignInDateTime:               "2026-11-07T10:00:00Z",
		LastNonInteractiveSignInDateTime: "2026-11-06T10:00:00Z",
		LastSuccessfulSignInDateTime:     "2026-11-08T10:00:00Z",
	}}
	if got := signInActivityOf(user); got != "2026-11-08T10:00:00Z" {
		t.Errorf("signInActivityOf = %q, want the successful sign-in", got)
	}
	user.SignInActivity.LastSuccessfulSignInDateTime = ""
	if got := signInActivityOf(user); got != "2026-11-07T10:00:00Z" {
		t.Errorf("signInActivityOf = %q, want the last interactive sign-in", got)
	}
	if got := signInActivityOf(reportUser{}); got != "" {
		t.Errorf("signInActivityOf(no property) = %q, want empty", got)
	}
}

// TestIsGuestUser pins both shapes of a guest: the userType Graph answers today
// and the #EXT# principal name the n8n collector split on.
func TestIsGuestUser(t *testing.T) {
	cases := []struct {
		user reportUser
		want bool
	}{
		{reportUser{UserType: "Guest"}, true},
		{reportUser{UserType: "Member", UserPrincipalName: "guest_contoso.com#EXT#@contoso.onmicrosoft.com"}, true},
		{reportUser{UserType: "Member", UserPrincipalName: "ada@contoso.com"}, false},
	}
	for _, testCase := range cases {
		if got := isGuestUser(testCase.user); got != testCase.want {
			t.Errorf("isGuestUser(%+v) = %v, want %v", testCase.user, got, testCase.want)
		}
	}
}

// TestUnifiedLastLoginsReadsTheAuditWindow drives the fallback against a mocked
// management API: the app-only token, the AzureActiveDirectory subscription, a
// content blob and a UserLoggedIn record. It pins the address normalisation and
// the newest-record-wins rule.
func TestUnifiedLastLoginsReadsTheAuditWindow(t *testing.T) {
	const tenant = "00000000-0000-0000-0000-000000000001"
	const blob = "https://manage.office.com/api/v1.0/" + tenant + "/activity/feed/audit/blob-1"
	transport := &recordingTransport{
		bodies: map[string]string{
			"/" + tenant + "/oauth2/v2.0/token":                            `{"access_token":"app-token"}`,
			"/api/v1.0/" + tenant + "/activity/feed/subscriptions/start":   `{}`,
			"/api/v1.0/" + tenant + "/activity/feed/subscriptions/content": `[{"contentUri":"` + blob + `"}]`,
			"/api/v1.0/" + tenant + "/activity/feed/audit/blob-1": `[` +
				`{"Operation":"UserLoggedIn","CreationTime":"2026-11-07T09:00:00","UserId":"Ada@Contoso.com","ResultStatus":"success"},` +
				`{"Operation":"UserLoggedIn","CreationTime":"2026-11-08T09:00:00","UserId":"ada@contoso.com","ResultStatus":"success"},` +
				`{"Operation":"Add user.","CreationTime":"2026-11-08T10:00:00","UserId":"ada@contoso.com","ResultStatus":"success"}` +
				`]`,
		},
	}
	provider := New()
	provider.httpClient.Transport = transport

	logins, err := provider.unifiedLastLogins(context.Background(),
		providers.Credentials{ClientID: "client-id", ClientSecret: "client-secret", TenantID: tenant},
		&providers.Tokens{AccessToken: "report-token"})
	if err != nil {
		t.Fatalf("unifiedLastLogins = %v", err)
	}
	got, ok := logins["ada@contoso.com"]
	if !ok {
		t.Fatalf("logins = %v, want ada@contoso.com", logins)
	}
	want := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("last login = %s, want %s", got, want)
	}
	if _, ok := logins["grace@contoso.com"]; ok {
		t.Error("an address without a UserLoggedIn record was reported")
	}
	if len(transport.urls) == 0 {
		t.Error("the fallback issued no request")
	}
	// The subscription and the content listing are the app-only calls; the
	// delegated report token must never be sent to the management API.
	for _, raw := range transport.urls {
		if strings.HasPrefix(raw, baseURL) {
			t.Errorf("the fallback called Graph with the delegated token: %s", raw)
		}
	}
}
