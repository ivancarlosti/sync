package microsoft

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// directoryAuditPageBody is one Graph directory audit page: a user creation by
// an administrator, with the identity the collector stores.
const directoryAuditPageBody = `{"value":[{"id":"Directory_abc123","category":"UserManagement",` +
	`"result":"success","activityDisplayName":"Add user","activityDateTime":"2026-11-04T08:15:30Z",` +
	`"loggedByService":"Core Directory","operationType":"Add",` +
	`"initiatedBy":{"user":{"id":"admin-id","displayName":"Ada Admin","userPrincipalName":"Ada@Contoso.com"}},` +
	`"targetResources":[{"id":"new-user-id","displayName":"Bob New","type":"User",` +
	`"userPrincipalName":"bob@contoso.com"}]}]}`

// TestDirectoryFeedNormalisesRecords pins the directory stream: the normalised
// verb, the target, the actor and the incremental window carried in the filter.
func TestDirectoryFeedNormalisesRecords(t *testing.T) {
	transport := &recordingTransport{bodies: map[string]string{"/v1.0/auditLogs/directoryAudits": directoryAuditPageBody}}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}
	cursor := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	events, next, err := provider.ActivityLogs(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "token"}, streamDirectory, cursor)
	if err != nil {
		t.Fatalf("ActivityLogs() = %v, want no error", err)
	}
	if len(events) != 1 {
		t.Fatalf("ActivityLogs() answered %d events, want 1", len(events))
	}
	event := events[0]
	if event.Source != streamDirectory || event.RemoteID != "Directory_abc123" {
		t.Errorf("identity = %s/%s, want directory/Directory_abc123", event.Source, event.RemoteID)
	}
	if event.Action != models.AuditLogCreate || event.Target != models.AuditLogTargetUser {
		t.Errorf("action/target = %s/%s, want create/user", event.Action, event.Target)
	}
	if event.ActorEmail != "ada@contoso.com" || event.ActorID != "admin-id" {
		t.Errorf("actor = %q/%q, want the administrator", event.ActorEmail, event.ActorID)
	}
	if event.TargetName != "Bob New" || event.TargetID != "new-user-id" {
		t.Errorf("target = %q/%q, want the created user", event.TargetName, event.TargetID)
	}
	want := time.Date(2026, 11, 4, 8, 15, 30, 0, time.UTC)
	if !event.OccurredAt.Equal(want) {
		t.Errorf("OccurredAt = %s, want %s", event.OccurredAt, want)
	}
	if !strings.Contains(string(event.Raw), `"activityDisplayName":"Add user"`) {
		t.Errorf("Raw = %s, want the Graph record verbatim", event.Raw)
	}
	if _, err := time.Parse(time.RFC3339, next); err != nil {
		t.Errorf("next cursor = %q, want an RFC3339 watermark", next)
	}

	if len(transport.urls) != 1 {
		t.Fatalf("the pass issued %d requests, want 1", len(transport.urls))
	}
	parsed, err := url.Parse(transport.urls[0])
	if err != nil {
		t.Fatalf("cannot parse %q: %v", transport.urls[0], err)
	}
	filter := parsed.Query().Get("$filter")
	if !strings.HasPrefix(filter, "activityDateTime ge ") {
		t.Fatalf("$filter = %q, want an activityDateTime watermark", filter)
	}
	wantStart, _ := time.Parse(time.RFC3339, cursor)
	wantStart = wantStart.Add(-activityOverlap)
	gotStart, err := time.Parse(time.RFC3339, strings.TrimPrefix(filter, "activityDateTime ge "))
	if err != nil {
		t.Fatalf("$filter = %q, want an RFC3339 watermark", filter)
	}
	if !gotStart.Equal(wantStart) {
		t.Errorf("$filter start = %s, want %s (the cursor minus the overlap)", gotStart, wantStart)
	}
	if auth := transport.headers[0].Get("Authorization"); auth != "Bearer token" {
		t.Errorf("Authorization = %q, want the stored access token", auth)
	}
}

// TestDirectoryFeedRefusesAPageLinkThatLeavesGraph pins the guard around
// @odata.nextLink: the link is followed with the account's bearer token, so it
// must stay on Graph.
func TestDirectoryFeedRefusesAPageLinkThatLeavesGraph(t *testing.T) {
	body := `{"value":[],"@odata.nextLink":"https://evil.example.com/v1.0/auditLogs/directoryAudits?$skiptoken=x"}`
	transport := &recordingTransport{bodies: map[string]string{"/v1.0/auditLogs/directoryAudits": body}}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}

	if _, _, err := provider.ActivityLogs(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "token"}, streamDirectory, ""); err == nil {
		t.Fatal("ActivityLogs() = nil, want the page link refused")
	}
	if len(transport.urls) != 1 {
		t.Errorf("the pass issued %d requests, want only the first page", len(transport.urls))
	}
}

// TestActivityLogStreamsAndScopes pins the contract of the two Microsoft feeds:
// the directory scope is the only delegated one, because the workload feed is
// app-only.
func TestActivityLogStreamsAndScopes(t *testing.T) {
	provider := New()
	if got := provider.ActivityLogStreams(); fmt.Sprint(got) != fmt.Sprint([]string{"directory", "unified"}) {
		t.Fatalf("ActivityLogStreams() = %v, want [directory unified]", got)
	}
	scopes := provider.ActivityLogScopes()
	if len(scopes) != 1 || scopes[0] != "https://graph.microsoft.com/AuditLog.Read.All" {
		t.Fatalf("ActivityLogScopes() = %v, want the delegated directory scope alone", scopes)
	}
	if !providers.HasCapability(provider.Permissions(), scopes, providers.CapabilityLogs) {
		t.Error("the directory scope must prove the logs capability")
	}
}

// TestActivityLogsRejectsAnUnknownStream pins the cursor identity: a stream this
// provider does not read never reaches the network.
func TestActivityLogsRejectsAnUnknownStream(t *testing.T) {
	transport := &recordingTransport{bodies: map[string]string{}}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}
	if _, _, err := provider.ActivityLogs(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "token"}, "drive", ""); err == nil {
		t.Fatal("ActivityLogs() = nil, want an unknown stream error")
	}
	if len(transport.urls) != 0 {
		t.Errorf("the pass issued %d requests, want none", len(transport.urls))
	}
}

// throttleTransport answers the first `throttled` calls with the 429 Graph sends a
// busy tenant, and the rest from the canned body of the request path. The
// Retry-After is zero seconds, so a retry does not make the test sleep.
type throttleTransport struct {
	urls      []string
	throttled int
	bodies    map[string]string
}

// RoundTrip implements http.RoundTripper.
func (t *throttleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.urls = append(t.urls, request.URL.String())
	status := http.StatusOK
	body := t.bodies[request.URL.Path]
	if body == "" {
		body = "{}"
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	if t.throttled > 0 {
		t.throttled--
		status = http.StatusTooManyRequests
		body = `{"error":{"code":"TooManyRequests","message":"Too many requests."}}`
		header.Set("Retry-After", "0")
	}
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}

// TestDirectoryFeedRetriesAThrottle pins the fix of the 429 on the directory
// stream: it was the only Microsoft call that abandoned a pass on the first
// throttle, and a failed stream also costs the unified feed of that organisation.
func TestDirectoryFeedRetriesAThrottle(t *testing.T) {
	transport := &throttleTransport{
		throttled: 1,
		bodies:    map[string]string{"/v1.0/auditLogs/directoryAudits": directoryAuditPageBody},
	}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}

	events, _, err := provider.ActivityLogs(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "token"}, streamDirectory, "")
	if err != nil {
		t.Fatalf("ActivityLogs() = %v, want the pass to survive one 429", err)
	}
	if len(transport.urls) != 2 {
		t.Fatalf("the pass issued %d requests, want 2 (the throttle then the retry)", len(transport.urls))
	}
	if len(events) != 1 {
		t.Errorf("ActivityLogs() answered %d events, want the record of the retried page", len(events))
	}
}

// TestDirectoryFeedDoesNotRetryARejectedRequest pins the other side of the retry: a
// 400 is a decision, so the pass fails at once instead of spending its budget.
func TestDirectoryFeedDoesNotRetryARejectedRequest(t *testing.T) {
	transport := &recordingTransport{
		statuses: map[string]int{"/v1.0/auditLogs/directoryAudits": http.StatusBadRequest},
		bodies:   map[string]string{"/v1.0/auditLogs/directoryAudits": `{"error":{"code":"BadRequest","message":"Invalid filter."}}`},
	}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}

	if _, _, err := provider.ActivityLogs(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "token"}, streamDirectory, ""); err == nil {
		t.Fatal("ActivityLogs() = nil, want the rejected filter")
	}
	if len(transport.urls) != 1 {
		t.Errorf("the pass issued %d requests, want exactly 1", len(transport.urls))
	}
}

// TestActivityFeedExplainsAF10001 pins the failure that looked like a broken token:
// the management API refuses the app-only subscription with AF10001 and an empty
// `permission set ()` when ActivityFeed.Read was never granted, and the stored
// message has to name the grant plus the two other things that fix it.
func TestActivityFeedExplainsAF10001(t *testing.T) {
	transport := &unifiedTransport{
		startStatus: http.StatusUnauthorized,
		startErr:    `{"error":{"code":"AF10001","message":"The permission set () sent in the request did not include the expected permission ActivityFeed.Read for the resource https:\/\/manage.office.com."}}`,
	}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}

	err := provider.startSubscription(context.Background(), "contoso.onmicrosoft.com", "app-token", "Audit.SharePoint")
	if err == nil {
		t.Fatal("startSubscription() = nil, want the AF10001 refusal")
	}
	for _, want := range []string{"AF10001", "ActivityFeed.Read", "application permission", "administrator", "Purview"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q in the message", err, want)
		}
	}
}

// TestUnifiedFeedRetriesAThrottledToken pins the retry of the token request and the
// form replay it needs: a consumed body would send an empty client-credentials
// request on the second attempt and turn a throttle into an invalid_client.
func TestUnifiedFeedRetriesAThrottledToken(t *testing.T) {
	transport := &unifiedTransport{blobs: unifiedBlobBody, throttleToken: 1}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}
	creds := providers.Credentials{ClientID: "app-id", ClientSecret: "app-secret", TenantID: "contoso.onmicrosoft.com"}

	if _, _, err := provider.ActivityLogs(context.Background(), creds,
		&providers.Tokens{AccessToken: "delegated"}, streamUnified, ""); err != nil {
		t.Fatalf("ActivityLogs() = %v, want the pass to survive one throttled token request", err)
	}
	forms := []string{}
	for index, raw := range transport.urls {
		if strings.HasSuffix(raw, "/oauth2/v2.0/token") {
			forms = append(forms, transport.forms[index])
		}
	}
	if len(forms) != 2 {
		t.Fatalf("the token request was issued %d times, want 2 (the throttle then the retry)", len(forms))
	}
	if forms[1] == "" || forms[1] != forms[0] {
		t.Errorf("retried form = %q, want the replay of %q", forms[1], forms[0])
	}
}

// TestUnifiedFeedNeedsATenant pins the precondition of the app-only feed: without

// a directory to read there is nothing to subscribe to, and the message says so
// instead of half-reading a feed.
func TestUnifiedFeedNeedsATenant(t *testing.T) {
	transport := &recordingTransport{bodies: map[string]string{}}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}
	creds := providers.Credentials{ClientID: "app", ClientSecret: "secret", TenantID: "common"}
	if _, _, err := provider.ActivityLogs(context.Background(), creds,
		&providers.Tokens{AccessToken: "token"}, streamUnified, ""); err == nil {
		t.Fatal("ActivityLogs() = nil, want the tenant precondition to fail")
	}
	if len(transport.urls) != 0 {
		t.Errorf("the pass issued %d requests, want none", len(transport.urls))
	}
}
