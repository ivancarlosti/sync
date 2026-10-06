// Package google implements the providers.Provider interface on top of the
// Google Drive REST API v3 and the Google OAuth 2.0 endpoints.
//
// The calls are issued with a hand-rolled HTTP client instead of
// google.golang.org/api: Sync only needs nine endpoints, and keeping that
// dependency out of the binary keeps the image small and the behaviour
// (timeouts, error mapping, pagination) fully visible in this package.
package google

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/ivancarlosti/sync/internal/providers"
)

// endpoint is Google's OAuth 2.0 endpoint. It is declared here (instead of
// importing golang.org/x/oauth2/google) to avoid pulling the Google Cloud
// metadata package into the binary. Values come from Google's public OIDC
// discovery document.
var endpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

const (
	// apiBase is the Drive REST API v3 root.
	apiBase = "https://www.googleapis.com/drive/v3"
	// uploadBase is the Drive multipart upload root.
	uploadBase = "https://www.googleapis.com/upload/drive/v3"
	// userInfoURL returns the identity behind an access token.
	userInfoURL = "https://www.googleapis.com/oauth2/v3/userinfo"

	// MyDrive is the pseudo drive id standing for the user's own "My Drive".
	MyDrive = "my-drive"

	// requestTimeout bounds every single API call.
	requestTimeout = 5 * time.Minute
)

// itemFields is the projection used for every file/folder listing: only the
// fields Sync consumes, which keeps large listings cheap.
const itemFields = "id,name,mimeType,size,modifiedTime,md5Checksum,parents,trashed,shortcutDetails,driveId"

// apiError is the error shape returned by the Drive API.
type apiError struct {
	Status  int
	Code    int
	Message string
	Reason  string
}

// Error renders a diagnostic message that never contains the access token.
func (e *apiError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("google drive api: http %d (%s): %s", e.Status, e.Reason, e.Message)
	}
	return fmt.Sprintf("google drive api: http %d: %s", e.Status, e.Message)
}

// IsNotFound reports whether err is a 404/410 answered by the Drive API.
func (p *Provider) IsNotFound(err error) bool {
	var apiErr *apiError
	if !asAPIError(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusGone
}

// asAPIError unwraps an *apiError out of err.
func asAPIError(err error, target **apiError) bool {
	for err != nil {
		if converted, ok := err.(*apiError); ok {
			*target = converted
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// oauthConfig builds the OAuth 2.0 client configuration for one credential set.
func oauthConfig(creds providers.Credentials) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
		RedirectURL:  creds.RedirectURI,
		Scopes:       (&Provider{}).Scopes(),
		Endpoint:     endpoint,
	}
}

// client returns an HTTP client that injects the access token of the request.
// Token refresh is deliberately *not* delegated to oauth2: the services layer
// owns the token lifecycle so a refresh is persisted exactly once and a failed
// refresh marks the account as needing attention.
func (p *Provider) client(tokens *providers.Tokens) *http.Client {
	clone := *p.httpClient
	clone.Transport = &bearerTransport{base: p.httpClient.Transport, token: tokens.AccessToken}
	return &clone
}

// doJSON performs a request and decodes the JSON response into out. A nil body
// means "no request payload"; out may be nil when the response is irrelevant.
func (p *Provider) doJSON(ctx context.Context, client *http.Client, method, target string, body, out any) error {
	// Every request this provider issues goes through here, and the URL reaches
	// the request built below, so it is narrowed here, in the function that
	// sends it (see requestURLPattern). The per-identifier guards of the callers
	// refuse a bad id with a 400 well before this point; this layer keeps the
	// request on Google's API host even when a call site builds an URL of its
	// own.
	if !requestURLPattern.MatchString(target) {
		return unsafeRequestURLError("request URL", target)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("google drive: cannot encode request body: %w", err)
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("google drive: cannot build %s request: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("google drive: %s %s failed: %w", method, safeURL(target), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("google drive: cannot decode %s response: %w", method, err)
	}
	return nil
}

// decodeError turns a non-2xx response into an *apiError.
func decodeError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	message := strings.TrimSpace(string(raw))
	status := resp.StatusCode
	code := status
	reason := ""

	var envelope struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Errors  []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		code = envelope.Error.Code
		message = envelope.Error.Message
		if len(envelope.Error.Errors) > 0 {
			reason = envelope.Error.Errors[0].Reason
			if envelope.Error.Errors[0].Message != "" {
				message = envelope.Error.Errors[0].Message
			}
		}
	}
	return &apiError{Status: status, Code: code, Message: message, Reason: reason}
}

// safeURL trims a URL to a length that is safe to log.
func safeURL(raw string) string {
	if len(raw) > 160 {
		return raw[:160] + "..."
	}
	return raw
}

// withParam appends an encoded query parameter, skipping empty values.
func withParam(raw, key, value string) string {
	if value == "" {
		return raw
	}
	separator := "?"
	if strings.Contains(raw, "?") {
		separator = "&"
	}
	return raw + separator + key + "=" + url.QueryEscape(value)
}

// listURL builds a paginated list URL carrying the shared listing options
// (shared drive support plus the requested projection). It deliberately does not
// set includeItemsFromAllDrives: that legacy flag widens a listing to both My
// Drive and the shared drives, which contradicts the single corpus every caller
// selects through withDriveScope (corpora=user, or corpora=drive&driveId=…). Used
// against a shared drive it made the query answer the My Drive root instead of
// that drive, which is why the folder picker kept showing My Drive contents.
func listURL(path, fields string) string {
	target := apiBase + path
	target = withParam(target, "fields", fields)
	target = withParam(target, "supportsAllDrives", "true")
	return target
}

// splitScopes splits the OAuth scope string Google returns into a list.
func splitScopes(raw string) []string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// bearerTransport injects the access token into every request. It is private so
// a token can never leak through a shared client.
type bearerTransport struct {
	base  http.RoundTripper
	token string
}

// RoundTrip implements http.RoundTripper.
func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
