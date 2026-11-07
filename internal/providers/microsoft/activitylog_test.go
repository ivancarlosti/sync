package microsoft

import (
	"context"
	"fmt"
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
