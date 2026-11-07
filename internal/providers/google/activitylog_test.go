package google

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// activityTransport answers the Reports API with one canned document per
// application while recording the URLs, so the window, the filter and the
// pagination a pass issues can be asserted without reaching Google.
type activityTransport struct {
	urls     []string
	headers  []http.Header
	bodies   map[string]string // keyed by the application name
	statuses map[string]int    // keyed by the application name
}

// RoundTrip implements http.RoundTripper.
func (t *activityTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.urls = append(t.urls, request.URL.String())
	t.headers = append(t.headers, request.Header.Clone())
	application := request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
	status := http.StatusOK
	if code, ok := t.statuses[application]; ok {
		status = code
	}
	body := t.bodies[application]
	if body == "" {
		body = `{"items":[]}`
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}

// driveActivityBody is one drive deletion as the Reports API reports it.
const driveActivityBody = `{"items":[{"id":{"time":"2026-11-05T09:30:00.000Z","uniqueQualifier":"1234567890",` +
	`"applicationName":"drive","customerId":"C01abc"},"actor":{"callerType":"USER","email":"Ada@Contoso.com",` +
	`"profileId":"10987654321"},"ipAddress":"203.0.113.7","events":[{"type":"access","name":"delete",` +
	`"parameters":[{"name":"primary_event","boolValue":true},{"name":"doc_id","value":"file-42"},` +
	`{"name":"doc_title","value":"Q4 Budget.xlsx"},{"name":"doc_type","value":"file"}]}]}]}`

// TestActivityLogsNormalisesDriveDeletion pins what the Audit Logs screen shows
// for the headline event of the feature: who deleted which file, when, and the
// identity that makes a re-read idempotent.
func TestActivityLogsNormalisesDriveDeletion(t *testing.T) {
	transport := &activityTransport{bodies: map[string]string{streamDrive: driveActivityBody}}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}

	events, next, err := provider.ActivityLogs(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "token"}, streamDrive, "")
	if err != nil {
		t.Fatalf("ActivityLogs() = %v, want no error", err)
	}
	if len(events) != 1 {
		t.Fatalf("ActivityLogs() answered %d events, want 1", len(events))
	}
	event := events[0]
	if event.Action != models.AuditLogDelete || event.Target != models.AuditLogTargetFile {
		t.Errorf("action/target = %s/%s, want delete/file", event.Action, event.Target)
	}
	if event.ActorEmail != "ada@contoso.com" {
		t.Errorf("ActorEmail = %q, want the lower-cased address", event.ActorEmail)
	}
	if event.TargetName != "Q4 Budget.xlsx" || event.TargetID != "file-42" {
		t.Errorf("target = %q/%q, want the title and the id", event.TargetName, event.TargetID)
	}
	want := time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)
	if !event.OccurredAt.Equal(want) {
		t.Errorf("OccurredAt = %s, want %s", event.OccurredAt, want)
	}
	if event.Source != streamDrive {
		t.Errorf("Source = %q, want the stream name %q", event.Source, streamDrive)
	}
	if event.RemoteID != "C01abc/drive/1234567890/0" {
		t.Errorf("RemoteID = %q, want the customer/application/qualifier/index identity", event.RemoteID)
	}
	if len(event.Raw) == 0 || !strings.Contains(string(event.Raw), `"doc_title"`) {
		t.Errorf("Raw = %s, want the vendor record", event.Raw)
	}
	if _, err := time.Parse(time.RFC3339, next); err != nil {
		t.Errorf("next cursor = %q, want an RFC3339 timestamp", next)
	}
}

// TestActivityLogsResumesFromTheCursorWithOverlap pins the incremental read: the
// window starts a few minutes before the watermark, because the Reports API
// files an event when it processes it rather than exactly when it happened.
func TestActivityLogsResumesFromTheCursorWithOverlap(t *testing.T) {
	transport := &activityTransport{}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}
	cursor := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	if _, _, err := provider.ActivityLogs(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "token"}, streamLogin, cursor); err != nil {
		t.Fatalf("ActivityLogs() = %v, want no error", err)
	}
	if len(transport.urls) != 1 {
		t.Fatalf("the pass issued %d requests, want 1", len(transport.urls))
	}
	parsed, err := url.Parse(transport.urls[0])
	if err != nil {
		t.Fatalf("cannot parse %q: %v", transport.urls[0], err)
	}
	query := parsed.Query()
	if got := parsed.Path; got != "/admin/reports/v1/activity/users/all/applications/login" {
		t.Errorf("path = %q, want the login application", got)
	}
	wantStart, _ := time.Parse(time.RFC3339, cursor)
	wantStart = wantStart.Add(-activityOverlap)
	gotStart, err := time.Parse(time.RFC3339, query.Get("startTime"))
	if err != nil {
		t.Fatalf("startTime = %q, want RFC3339", query.Get("startTime"))
	}
	if !gotStart.Equal(wantStart) {
		t.Errorf("startTime = %s, want %s (the cursor minus the overlap)", gotStart, wantStart)
	}
	if query.Get("eventName") != "" {
		t.Errorf("login must not filter event names, got %v", query["eventName"])
	}
	if auth := transport.headers[0].Get("Authorization"); auth != "Bearer token" {
		t.Errorf("Authorization = %q, want the stored access token", auth)
	}
}

// TestActivityURLFiltersDriveEvents pins the narrowing of the drive feed: the
// access events (view, preview, download) are what would bury the changes.
func TestActivityURLFiltersDriveEvents(t *testing.T) {
	raw := activityURL(streamDrive, time.Unix(0, 0), time.Unix(3600, 0), "")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("cannot parse %q: %v", raw, err)
	}
	names := parsed.Query()["eventName"]
	if len(names) != len(driveEventNames) {
		t.Fatalf("eventName = %v, want the %d curated names", names, len(driveEventNames))
	}
	for _, unwanted := range []string{"view", "preview", "download"} {
		for _, name := range names {
			if name == unwanted {
				t.Errorf("eventName carries %q, which the drive feed must leave out", unwanted)
			}
		}
	}
	if got := parsed.Query().Get("maxResults"); got != "1000" {
		t.Errorf("maxResults = %q, want 1000", got)
	}
}

// TestActivityLogsFollowsPageTokens pins the pagination: a nextPageToken is
// followed, and the cursor of a fully read window is the end of it.
func TestActivityLogsFollowsPageTokens(t *testing.T) {
	transport := &pagedActivityTransport{}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}

	events, next, err := provider.ActivityLogs(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "token"}, streamUserAccounts, "")
	if err != nil {
		t.Fatalf("ActivityLogs() = %v, want no error", err)
	}
	if len(transport.urls) != 2 {
		t.Fatalf("the pass issued %d requests, want 2 (first page plus nextPageToken)", len(transport.urls))
	}
	if !strings.Contains(transport.urls[1], "pageToken=page-2") {
		t.Errorf("second request = %q, want the page token", transport.urls[1])
	}
	if len(events) != 2 {
		t.Fatalf("ActivityLogs() answered %d events, want 2", len(events))
	}
	if events[0].Action != models.AuditLogCreate || events[0].Target != models.AuditLogTargetUser {
		t.Errorf("first event = %s/%s, want create/user", events[0].Action, events[0].Target)
	}
	if events[1].Action != models.AuditLogDelete {
		t.Errorf("second event = %s, want a delete", events[1].Action)
	}
	if _, err := time.Parse(time.RFC3339, next); err != nil {
		t.Errorf("next cursor = %q, want an RFC3339 timestamp", next)
	}
}

// pagedActivityTransport answers the first page with a nextPageToken and the
// second one without, which is the shape of a listing that spans two pages.
type pagedActivityTransport struct {
	urls []string
}

// RoundTrip implements http.RoundTripper.
func (t *pagedActivityTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.urls = append(t.urls, request.URL.String())
	body := `{"items":[{"id":{"time":"2026-11-05T10:00:00.000Z","uniqueQualifier":"1",` +
		`"applicationName":"user_accounts","customerId":"C01"},"actor":{"callerType":"USER","email":"admin@contoso.com"},` +
		`"events":[{"type":"USER_SETTINGS","name":"CREATE_USER","parameters":[{"name":"USER_EMAIL","value":"new@contoso.com"}]}]}],` +
		`"nextPageToken":"page-2"}`
	if strings.Contains(request.URL.RawQuery, "pageToken=page-2") {
		body = `{"items":[{"id":{"time":"2026-11-05T11:00:00.000Z","uniqueQualifier":"2",` +
			`"applicationName":"user_accounts","customerId":"C01"},"actor":{"callerType":"USER","email":"admin@contoso.com"},` +
			`"events":[{"type":"USER_SETTINGS","name":"DELETE_USER","parameters":[{"name":"USER_EMAIL","value":"gone@contoso.com"}]}]}]}`
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}

// TestActivityLogsReportsAForbiddenGrant pins the failure the screen has to
// explain: an account granted before the Reports scope existed answers 403, and
// the collector stores that message as the feed's last error.
func TestActivityLogsReportsAForbiddenGrant(t *testing.T) {
	transport := &activityTransport{statuses: map[string]int{streamDrive: http.StatusForbidden}}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}

	_, _, err := provider.ActivityLogs(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "token"}, streamDrive, "")
	if err == nil {
		t.Fatal("ActivityLogs() = nil, want the API failure")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want the status of the refusal", err)
	}
}

// TestActivityLogsRejectsAnUnknownStream pins the cursor identity: a stream this
// provider does not read never reaches the network.
func TestActivityLogsRejectsAnUnknownStream(t *testing.T) {
	transport := &activityTransport{}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}
	if _, _, err := provider.ActivityLogs(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "token"}, "unified", ""); err == nil {
		t.Fatal("ActivityLogs() = nil, want an unknown stream error")
	}
	if len(transport.urls) != 0 {
		t.Errorf("the pass issued %d requests, want none", len(transport.urls))
	}
}

// TestActivityLogStreamsAndScopes pins the contract the collector and the guide
// read: the five Reports applications and the single read-only scope.
func TestActivityLogStreamsAndScopes(t *testing.T) {
	provider := &Provider{}
	streams := provider.ActivityLogStreams()
	want := []string{"drive", "user_accounts", "groups", "admin", "login"}
	if len(streams) != len(want) {
		t.Fatalf("ActivityLogStreams() = %v, want %v", streams, want)
	}
	for i, name := range want {
		if streams[i] != name {
			t.Errorf("stream %d = %q, want %q", i, streams[i], name)
		}
	}
	scopes := provider.ActivityLogScopes()
	if len(scopes) != 1 || scopes[0] != reportsScope {
		t.Fatalf("ActivityLogScopes() = %v, want the Reports scope", scopes)
	}
	if !providers.HasCapability(provider.Permissions(), scopes, providers.CapabilityLogs) {
		t.Error("the Reports scope must prove the logs capability")
	}
	// The scope is requested by the connection itself, so an account connected
	// today already holds it.
	requested := false
	for _, scope := range provider.Scopes() {
		if scope == reportsScope {
			requested = true
		}
	}
	if !requested {
		t.Error("Scopes() must request the Reports scope")
	}
}
