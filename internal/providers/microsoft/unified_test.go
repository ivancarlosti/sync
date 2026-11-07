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

// unifiedBlobBody is one content blob of the management activity API: a
// SharePoint file deletion.
const unifiedBlobBody = `[{"Id":"blob-record-1","Operation":"FileDeleted","Workload":"SharePoint",` +
	`"RecordType":6,"ResultStatus":"Succeeded","CreationTime":"2026-11-05T14:02:11",` +
	`"UserId":"ada@contoso.com","UserKey":"i:0h.f|membership|1003","ObjectId":"file-object-id",` +
	`"ItemType":"File","SiteUrl":"https://contoso.sharepoint.com/sites/finance",` +
	`"SourceFileName":"Q4 Budget.xlsx","CorrelationId":"corr-1"}]`

// unifiedTransport answers the management activity API end to end: the app-only
// token, the (idempotent) subscription, the content listing and the blob it
// points at.
type unifiedTransport struct {
	urls     []string
	methods  []string
	forms    []string
	headers  []http.Header
	blobs    string
	tokenErr string
	startErr string
}

// RoundTrip implements http.RoundTripper.
func (t *unifiedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.urls = append(t.urls, request.URL.String())
	t.methods = append(t.methods, request.Method)
	t.headers = append(t.headers, request.Header.Clone())
	form := ""
	if request.Body != nil {
		raw, _ := io.ReadAll(request.Body)
		form = string(raw)
	}
	t.forms = append(t.forms, form)

	status := http.StatusOK
	body := "{}"
	switch {
	case strings.HasSuffix(request.URL.Path, "/oauth2/v2.0/token"):
		if t.tokenErr != "" {
			status = http.StatusBadRequest
			body = t.tokenErr
			break
		}
		body = `{"access_token":"app-token","token_type":"Bearer","expires_in":3599}`
	case strings.HasSuffix(request.URL.Path, "/subscriptions/start"):
		if t.startErr != "" {
			status = http.StatusBadRequest
			body = t.startErr
			break
		}
		body = `{"contentType":"Audit.SharePoint","status":"enabled"}`
	case strings.HasSuffix(request.URL.Path, "/subscriptions/content"):
		body = "[]"
		if request.URL.Query().Get("contentType") == "Audit.SharePoint" {
			body = `[{"contentType":"Audit.SharePoint","contentId":"content-1",` +
				`"contentUri":"https://manage.office.com/blobs/content-1",` +
				`"contentCreated":"2026-11-05T14:03:00","contentExpiration":"2026-11-12T14:03:00"}]`
		}
	case strings.HasSuffix(request.URL.Path, "/content-1"):
		body = t.blobs
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}

// TestUnifiedFeedWalksTokenSubscriptionContentAndBlob pins the whole app-only
// flow: the client-credentials token for manage.office.com, the subscription of
// each content type, the incremental content window and the normalised record of
// the blob it points at.
func TestUnifiedFeedWalksTokenSubscriptionContentAndBlob(t *testing.T) {
	transport := &unifiedTransport{blobs: unifiedBlobBody}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}
	creds := providers.Credentials{
		ClientID:     "app-id",
		ClientSecret: "app-secret",
		TenantID:     "11111111-2222-3333-4444-555555555555",
	}

	events, next, err := provider.ActivityLogs(context.Background(), creds,
		&providers.Tokens{AccessToken: "delegated"}, streamUnified, "")
	if err != nil {
		t.Fatalf("ActivityLogs() = %v, want no error", err)
	}
	if len(events) != 1 {
		t.Fatalf("ActivityLogs() answered %d events, want 1", len(events))
	}
	event := events[0]
	if event.Source != streamUnified || event.RemoteID != "blob-record-1" {
		t.Errorf("identity = %s/%s, want unified/blob-record-1", event.Source, event.RemoteID)
	}
	if event.Action != models.AuditLogDelete || event.Target != models.AuditLogTargetFile {
		t.Errorf("action/target = %s/%s, want delete/file", event.Action, event.Target)
	}
	if event.ActorEmail != "ada@contoso.com" || event.ActorID != "i:0h.f|membership|1003" {
		t.Errorf("actor = %q/%q, want the SharePoint user", event.ActorEmail, event.ActorID)
	}
	if event.TargetName != "Q4 Budget.xlsx" || event.TargetID != "file-object-id" {
		t.Errorf("target = %q/%q, want the deleted file", event.TargetName, event.TargetID)
	}
	if event.SiteID != "https://contoso.sharepoint.com/sites/finance" {
		t.Errorf("SiteID = %q, want the site that locates the row", event.SiteID)
	}
	want := time.Date(2026, 11, 5, 14, 2, 11, 0, time.UTC)
	if !event.OccurredAt.Equal(want) {
		t.Errorf("OccurredAt = %s, want %s", event.OccurredAt, want)
	}
	if !strings.Contains(string(event.Raw), `"Operation":"FileDeleted"`) {
		t.Errorf("Raw = %s, want the workload record verbatim", event.Raw)
	}
	if _, err := time.Parse(time.RFC3339, next); err != nil {
		t.Errorf("next cursor = %q, want an RFC3339 watermark", next)
	}

	// The token is exchanged once, app-only, for the management API resource.
	if transport.methods[0] != http.MethodPost || !strings.HasSuffix(transport.urls[0], "/oauth2/v2.0/token") {
		t.Fatalf("first request = %s %s, want the app-only token exchange", transport.methods[0], transport.urls[0])
	}
	if !strings.Contains(transport.forms[0], "grant_type=client_credentials") ||
		!strings.Contains(transport.forms[0], "scope=https%3A%2F%2Fmanage.office.com%2F.default") {
		t.Errorf("token form = %q, want a client-credentials request for manage.office.com", transport.forms[0])
	}
	if !strings.Contains(transport.urls[0], creds.TenantID) {
		t.Errorf("token URL = %q, want the connected tenant", transport.urls[0])
	}

	subscribed := map[string]bool{}
	listed := 0
	for index, raw := range transport.urls {
		if strings.Contains(raw, "/subscriptions/start") && transport.methods[index] == http.MethodPost {
			for _, contentType := range unifiedContentTypes {
				if strings.Contains(raw, "contentType="+url.QueryEscape(contentType)) {
					subscribed[contentType] = true
				}
			}
		}
		if strings.Contains(raw, "/subscriptions/content") {
			listed++
			if !strings.Contains(raw, "startTime=") || !strings.Contains(raw, "endTime=") {
				t.Errorf("content request = %q, want the incremental window", raw)
			}
		}
	}
	if len(subscribed) != len(unifiedContentTypes) {
		t.Errorf("subscribed to %v, want every content type of the feed", subscribed)
	}
	if listed != len(unifiedContentTypes) {
		t.Errorf("listed %d content windows, want one per content type", listed)
	}

	blobAuthed := false
	for index, raw := range transport.urls {
		if strings.HasSuffix(raw, "/blobs/content-1") {
			blobAuthed = transport.headers[index].Get("Authorization") == "Bearer app-token"
		}
	}
	if !blobAuthed {
		t.Error("the blob was not read with the app-only token")
	}
}

// TestUnifiedFeedExplainsAMissingConsent pins the degradation the screen shows:
// a tenant that did not grant ActivityFeed.Read answers AADSTS65001, and the
// message names the permission so the operator knows what to fix, while the
// directory stream keeps working.
func TestUnifiedFeedExplainsAMissingConsent(t *testing.T) {
	transport := &unifiedTransport{tokenErr: `{"error":"invalid_grant",` +
		`"error_description":"AADSTS65001: The user or administrator has not consented to use the application.` +
		`\r\nTrace ID: abc"}`}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}
	creds := providers.Credentials{ClientID: "app", ClientSecret: "secret", TenantID: "contoso.onmicrosoft.com"}

	events, next, err := provider.ActivityLogs(context.Background(), creds,
		&providers.Tokens{AccessToken: "delegated"}, streamUnified, "")
	if err == nil {
		t.Fatal("ActivityLogs() = nil, want the consent failure")
	}
	if len(events) != 0 || next != "" {
		t.Errorf("a failed pass answered %d events and cursor %q, want neither", len(events), next)
	}
	if !strings.Contains(err.Error(), "ActivityFeed.Read") || !strings.Contains(err.Error(), "AADSTS65001") {
		t.Errorf("err = %v, want the permission and the Entra diagnostic", err)
	}
	if strings.Contains(err.Error(), "app-secret") {
		t.Errorf("the error leaked the client secret: %v", err)
	}
}

// TestStartSubscriptionAcceptsAnAlreadyEnabledType pins the idempotency of the
// subscription: enabling an enabled content type answers 400/AF20024, which
// every pass after the first one hits and must not report as a failure.
func TestStartSubscriptionAcceptsAnAlreadyEnabledType(t *testing.T) {
	transport := &unifiedTransport{startErr: `{"error":{"code":"AF20024","message":"The subscription is already enabled."}}`}
	provider := &Provider{httpClient: &http.Client{Transport: transport}}
	if err := provider.startSubscription(context.Background(), "contoso.onmicrosoft.com", "app-token", "Audit.SharePoint"); err != nil {
		t.Fatalf("startSubscription() = %v, want the already-enabled answer accepted", err)
	}
}
