package google

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/providers"
)

// userInfoTransport answers every request with one fixed userinfo document, so
// Account() can be exercised without reaching the network.
type userInfoTransport struct {
	body string
}

// RoundTrip implements http.RoundTripper.
func (t *userInfoTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(t.body)),
		ContentLength: int64(len(t.body)),
		Request:       request,
	}, nil
}

// TestAccountReadsTheHostedDomainAsTheOrganisation pins the organisation key the
// audit-log collector groups accounts by: the Workspace hosted domain (`hd`),
// lower-cased so one domain always answers one key, and empty for a personal
// Google account (which has no organisation to share a domain-wide feed with).
func TestAccountReadsTheHostedDomainAsTheOrganisation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "workspace account",
			body: `{"sub":"1234","email":"admin@contoso.com","name":"Admin","hd":"Contoso.COM"}`,
			want: "contoso.com",
		},
		{
			name: "workspace account with padding",
			body: `{"sub":"1234","email":"admin@contoso.com","hd":"  contoso.com  "}`,
			want: "contoso.com",
		},
		{
			name: "personal account",
			body: `{"sub":"1234","email":"someone@gmail.com","name":"Someone"}`,
			want: "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &Provider{httpClient: &http.Client{Transport: &userInfoTransport{body: testCase.body}}}
			account, err := provider.Account(context.Background(), providers.Credentials{}, &providers.Tokens{AccessToken: "token"})
			if err != nil {
				t.Fatalf("Account() = %v, want no error", err)
			}
			if account.OrgID != testCase.want {
				t.Fatalf("OrgID = %q, want %q", account.OrgID, testCase.want)
			}
		})
	}
}
