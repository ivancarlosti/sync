// This file covers the HTTP surface of the Audit Logs feature (auditlogs.go): the
// filters and their validation, the pagination, the CSV export and the status of
// the feeds.

package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// seedAuditLog stores a handful of activity events spread over two providers and
// two tenants, which is enough to exercise every filter.
func seedAuditLog(t *testing.T, store interface {
	InsertAuditLogEvents(context.Context, []models.AuditLogEvent) error
}) {
	t.Helper()
	base := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	events := []models.AuditLogEvent{
		{
			Provider: string(models.ProviderMicrosoft), Source: "directory", RemoteID: "dir-1",
			OrgID: "tenant-a", Action: string(models.AuditLogCreate), Target: string(models.AuditLogTargetUser),
			ActorEmail: "admin@contoso.com", TargetName: "Bob New", OccurredAt: base,
		},
		{
			Provider: string(models.ProviderMicrosoft), Source: "unified", RemoteID: "uni-1",
			OrgID: "tenant-a", Action: string(models.AuditLogDelete), Target: string(models.AuditLogTargetFile),
			ActorEmail: "ada@contoso.com", TargetName: "Q4 Budget.xlsx",
			SiteID: "https://contoso.sharepoint.com/sites/finance", OccurredAt: base.Add(2 * time.Hour),
		},
		{
			Provider: string(models.ProviderGoogle), Source: "drive", RemoteID: "goo-1",
			OrgID: "contoso.com", Action: string(models.AuditLogDelete), Target: string(models.AuditLogTargetFile),
			ActorEmail: "ada@contoso.com", TargetName: "Board pack.pdf", OccurredAt: base.Add(4 * time.Hour),
		},
	}
	if err := store.InsertAuditLogEvents(context.Background(), events); err != nil {
		t.Fatalf("seeding the activity events: %v", err)
	}
}

// auditLogCursorAnswer is the shape of one cursor in the JSON answer. It is
// declared here (instead of importing the services type) so the assertions below
// also pin the field names the SPA reads.
type auditLogCursorAnswer struct {
	Provider     string     `json:"provider"`
	OrgID        string     `json:"org_id"`
	Stream       string     `json:"stream"`
	Cursor       string     `json:"cursor,omitempty"`
	LastEventAt  *time.Time `json:"last_event_at,omitempty"`
	LastRunAt    *time.Time `json:"last_run_at,omitempty"`
	Healthy      bool       `json:"healthy"`
	NeedsConsent bool       `json:"needs_consent"`
	LastError    string     `json:"last_error,omitempty"`
}

// auditLogPageAnswer is the JSON shape of GET /api/audit-logs.
type auditLogPageAnswer struct {
	Events  []auditLogView         `json:"events"`
	Total   int64                  `json:"total"`
	Limit   int                    `json:"limit"`
	Offset  int                    `json:"offset"`
	Cursors []auditLogCursorAnswer `json:"cursors"`
}

// TestListAuditLogsAnswersTheNewestFirst pins the list endpoint: the events, the
// total of the filtered set and the pagination echo.
func TestListAuditLogsAnswersTheNewestFirst(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	seedAuditLog(t, store)

	recorder := call(t, server, http.MethodGet, "/api/audit-logs?limit=2", nil)
	requireStatus(t, recorder, http.StatusOK)
	answer := decodeJSON[auditLogPageAnswer](t, recorder)
	if answer.Total != 3 || len(answer.Events) != 2 {
		t.Fatalf("total/len = %d/%d, want the first page of three events", answer.Total, len(answer.Events))
	}
	if answer.Limit != 2 || answer.Offset != 0 {
		t.Errorf("limit/offset = %d/%d, want the requested page", answer.Limit, answer.Offset)
	}
	if answer.Events[0].RemoteID != "goo-1" || answer.Events[1].RemoteID != "uni-1" {
		t.Errorf("order = %s, %s, want the newest event first",
			answer.Events[0].RemoteID, answer.Events[1].RemoteID)
	}
	if answer.Events[0].SiteID != "" || answer.Events[1].SiteID == "" {
		t.Errorf("site ids = %q/%q, want the SharePoint site on the file event alone",
			answer.Events[0].SiteID, answer.Events[1].SiteID)
	}

	// The second page carries the oldest event, and the vendor document is never
	// serialised.
	recorder = call(t, server, http.MethodGet, "/api/audit-logs?limit=2&offset=2", nil)
	requireStatus(t, recorder, http.StatusOK)
	page := decodeJSON[auditLogPageAnswer](t, recorder)
	if len(page.Events) != 1 || page.Events[0].RemoteID != "dir-1" {
		t.Fatalf("second page = %+v, want the oldest event", page.Events)
	}
	if strings.Contains(recorder.Body.String(), "\"raw\"") {
		t.Error("the answer carries the vendor document, which the API must not expose")
	}
}

// TestListAuditLogsFilters pins the filter vocabulary: each query parameter and
// each combination the screen sends narrows the set.
func TestListAuditLogsFilters(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	seedAuditLog(t, store)

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{name: "provider", query: "provider=google", want: []string{"goo-1"}},
		{name: "org", query: "org_id=tenant-a", want: []string{"uni-1", "dir-1"}},
		{name: "action", query: "action=delete", want: []string{"goo-1", "uni-1"}},
		{name: "target", query: "target=user", want: []string{"dir-1"}},
		{name: "actor", query: "actor=ada@contoso.com", want: []string{"goo-1", "uni-1"}},
		{name: "search", query: "search=budget", want: []string{"uni-1"}},
		{name: "combined", query: "provider=microsoft&action=delete", want: []string{"uni-1"}},
		{name: "from", query: "from=2026-11-05T10:00:00Z", want: []string{"goo-1", "uni-1"}},
		{name: "to a date", query: "to=2026-11-05", want: []string{"goo-1", "uni-1", "dir-1"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := call(t, server, http.MethodGet, "/api/audit-logs?"+testCase.query, nil)
			requireStatus(t, recorder, http.StatusOK)
			answer := decodeJSON[auditLogPageAnswer](t, recorder)
			got := make([]string, 0, len(answer.Events))
			for _, event := range answer.Events {
				got = append(got, event.RemoteID)
			}
			if strings.Join(got, ",") != strings.Join(testCase.want, ",") {
				t.Fatalf("%s = %v, want %v", testCase.query, got, testCase.want)
			}
			if answer.Total != int64(len(testCase.want)) {
				t.Fatalf("total = %d, want %d", answer.Total, len(testCase.want))
			}
		})
	}
}

// TestListAuditLogsRefusesAnUnknownFilter pins the validation: a value outside
// the stored vocabulary is a 400, never a silently empty page.
func TestListAuditLogsRefusesAnUnknownFilter(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)
	for name, query := range map[string]string{
		"provider": "provider=dropbox",
		"action":   "action=exploded",
		"target":   "target=every_secret",
		"from":     "from=yesterday",
		"range":    "from=2026-11-06&to=2026-11-05",
		"offset":   "offset=-1",
	} {
		recorder := call(t, server, http.MethodGet, "/api/audit-logs?"+query, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("the %s filter: status = %d, want 400 (body %s)", name, recorder.Code, recorder.Body.String())
			continue
		}
		requireErrorCode(t, recorder, codeValidation)
	}
}

// TestExportAuditLogsStreamsACSV pins the export: the filtered rows, the header
// of the columns the screen shows, the BOM a spreadsheet needs and the total of
// the filtered set in a header (so a truncated export is never silent).
func TestExportAuditLogsStreamsACSV(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	seedAuditLog(t, store)

	recorder := call(t, server, http.MethodGet, "/api/audit-logs/export?action=delete", nil)
	requireStatus(t, recorder, http.StatusOK)
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", contentType)
	}
	if disposition := recorder.Header().Get("Content-Disposition"); !strings.Contains(disposition, "audit-logs.csv") {
		t.Errorf("Content-Disposition = %q, want the download filename", disposition)
	}
	if total := recorder.Header().Get("X-Total-Count"); total != "2" {
		t.Errorf("X-Total-Count = %q, want the filtered total", total)
	}
	body := recorder.Body.Bytes()
	if len(body) < 3 || body[0] != 0xEF || body[1] != 0xBB || body[2] != 0xBF {
		t.Error("the CSV does not start with a UTF-8 BOM, so a spreadsheet will mangle accented names")
	}
	text := string(body[3:])
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 3 {
		t.Fatalf("the CSV has %d lines, want a header and the two deletions:\n%s", len(lines), text)
	}
	if !strings.HasPrefix(lines[0], "occurred_at,provider,org_id,stream,action,target,actor_kind,actor_email") {
		t.Errorf("header = %q, want the documented column order", lines[0])
	}
	if !strings.Contains(lines[0], "actor_on_behalf_of") {
		t.Errorf("header = %q, want the on-behalf-of column of a workload-made change", lines[0])
	}
	if !strings.Contains(text, "Q4 Budget.xlsx") || !strings.Contains(text, "Board pack.pdf") {
		t.Errorf("the export misses the filtered files:\n%s", text)
	}
	if strings.Contains(text, "Bob New") {
		t.Errorf("the export carried an event the filter excluded:\n%s", text)
	}
}

// TestAuditLogCursorsReportTheHealthOfEveryFeed pins the status banner: a feed
// that was read successfully is healthy, one waiting for a consent says so, and
// the opaque cursor is exposed as evidence of progress.
func TestAuditLogCursorsReportTheHealthOfEveryFeed(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	ctx := context.Background()
	ran := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	saved := []models.AuditLogCursor{
		{
			Provider: string(models.ProviderMicrosoft), OrgID: "tenant-a", Stream: "directory",
			Cursor: "2026-11-07T11:50:00Z", LastRunAt: &ran, LastEventAt: &ran,
		},
		{
			Provider: string(models.ProviderMicrosoft), OrgID: "tenant-a", Stream: "unified",
			LastRunAt: &ran,
			LastError: "microsoft: the office 365 activity feed needs an administrator to consent to ActivityFeed.Read",
		},
	}
	for i := range saved {
		if err := store.SaveAuditLogCursor(ctx, &saved[i]); err != nil {
			t.Fatalf("saving the cursor of %s: %v", saved[i].Stream, err)
		}
	}

	recorder := call(t, server, http.MethodGet, "/api/audit-logs/cursors", nil)
	requireStatus(t, recorder, http.StatusOK)
	answer := decodeJSON[struct {
		Cursors []auditLogCursorAnswer `json:"cursors"`
	}](t, recorder)
	if len(answer.Cursors) != 2 {
		t.Fatalf("cursors = %+v, want both feeds", answer.Cursors)
	}
	directory, unified := answer.Cursors[0], answer.Cursors[1]
	if !directory.Healthy || directory.NeedsConsent || directory.Cursor == "" {
		t.Errorf("directory cursor = %+v, want a healthy feed with a position", directory)
	}
	if unified.Healthy || !unified.NeedsConsent {
		t.Errorf("unified cursor = %+v, want a feed that needs a consent", unified)
	}
	if !strings.Contains(unified.LastError, "ActivityFeed.Read") {
		t.Errorf("last_error = %q, want the reason the feed is stale", unified.LastError)
	}

	// The list answer carries the same banner, so the screen needs one request.
	recorder = call(t, server, http.MethodGet, "/api/audit-logs", nil)
	requireStatus(t, recorder, http.StatusOK)
	page := decodeJSON[auditLogPageAnswer](t, recorder)
	if len(page.Cursors) != 2 {
		t.Fatalf("cursors in the list = %+v, want both feeds", page.Cursors)
	}
}

// TestMaintenanceAuditLogsCollectPollsTheFeeds pins the on-demand pass: it runs
// the collector and answers its counters, with no account connected in this
// fixture (so nothing is read and nothing fails).
func TestMaintenanceAuditLogsCollectPollsTheFeeds(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	recorder := call(t, server, http.MethodPost, "/api/maintenance/auditlogs/collect", nil)
	requireStatus(t, recorder, http.StatusOK)
	answer := decodeJSON[struct {
		Organisations int   `json:"organisations"`
		Streams       int   `json:"streams"`
		Failed        int   `json:"failed"`
		Events        int   `json:"events"`
		DurationMS    int64 `json:"duration_ms"`
	}](t, recorder)
	if answer.Organisations != 0 || answer.Streams != 0 || answer.Failed != 0 || answer.Events != 0 {
		t.Fatalf("pass = %+v, want nothing read with no account connected", answer)
	}
}

// TestListAuditLogsReportsWhoIsBehindTheEvent pins the answer of the actor
// attribution change: a workload keeps its name out of the address column, the
// person it acted for travels in its own field, and the device is its own target.
func TestListAuditLogsReportsWhoIsBehindTheEvent(t *testing.T) {
	server, store := newTestServer(t, models.AuthModeNone)
	base := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	events := []models.AuditLogEvent{
		{
			Provider: string(models.ProviderMicrosoft), Source: "directory", RemoteID: "dev-1",
			OrgID: "tenant-a", Action: string(models.AuditLogUpdate), Target: string(models.AuditLogTargetDevice),
			ActorKind: string(models.AuditLogActorApplication), ActorName: "Device Registration Service",
			ActorOnBehalfOf: "ivan@contoso.com", ActorOnBehalfOfName: "Ivan Almeida",
			TargetName: "DESKTOP-J979VG0", OccurredAt: base,
		},
		{
			Provider: string(models.ProviderMicrosoft), Source: "directory", RemoteID: "user-1",
			OrgID: "tenant-a", Action: string(models.AuditLogCreate), Target: string(models.AuditLogTargetUser),
			ActorKind: string(models.AuditLogActorUser), ActorEmail: "ada@contoso.com",
			TargetName: "Bob New", OccurredAt: base.Add(time.Hour),
		},
	}
	if err := store.InsertAuditLogEvents(context.Background(), events); err != nil {
		t.Fatalf("seeding the activity events: %v", err)
	}

	recorder := call(t, server, http.MethodGet, "/api/audit-logs?target=device", nil)
	requireStatus(t, recorder, http.StatusOK)
	answer := decodeJSON[auditLogPageAnswer](t, recorder)
	if answer.Total != 1 || len(answer.Events) != 1 {
		t.Fatalf("target=device answered %d/%d events, want the one device registration",
			len(answer.Events), answer.Total)
	}
	event := answer.Events[0]
	if event.ActorKind != string(models.AuditLogActorApplication) {
		t.Errorf("actor_kind = %q, want application", event.ActorKind)
	}
	if event.ActorEmail != "" {
		t.Errorf("actor_email = %q, want empty for a workload", event.ActorEmail)
	}
	if event.ActorName != "Device Registration Service" {
		t.Errorf("actor_name = %q, want the workload name", event.ActorName)
	}
	if event.ActorOnBehalfOf != "ivan@contoso.com" || event.ActorOnBehalfOfName != "Ivan Almeida" {
		t.Errorf("on behalf of = %q/%q, want the person behind the workload",
			event.ActorOnBehalfOf, event.ActorOnBehalfOfName)
	}

	// The person's own event keeps the address in the address column.
	recorder = call(t, server, http.MethodGet, "/api/audit-logs?target=user", nil)
	requireStatus(t, recorder, http.StatusOK)
	page := decodeJSON[auditLogPageAnswer](t, recorder)
	if len(page.Events) != 1 || page.Events[0].ActorEmail != "ada@contoso.com" ||
		page.Events[0].ActorKind != string(models.AuditLogActorUser) {
		t.Errorf("person event = %+v, want the address and the user kind", page.Events)
	}
}

// TestMaintenanceAuditLogsRenormaliseRepairsTheHistory pins the on-demand repair
// endpoint: it answers the counters of the pass and runs without an account (so
// there is nothing to repair in this fixture, and nothing fails either).
func TestMaintenanceAuditLogsRenormaliseRepairsTheHistory(t *testing.T) {
	server, _ := newTestServer(t, models.AuthModeNone)

	recorder := call(t, server, http.MethodPost, "/api/maintenance/auditlogs/renormalise", nil)
	requireStatus(t, recorder, http.StatusOK)
	answer := decodeJSON[struct {
		Scanned    int   `json:"scanned"`
		Updated    int   `json:"updated"`
		Attributed int   `json:"attributed"`
		Remaining  int64 `json:"remaining"`
		DurationMS int64 `json:"duration_ms"`
	}](t, recorder)
	if answer.Scanned != 0 || answer.Updated != 0 || answer.Attributed != 0 || answer.Remaining != 0 {
		t.Fatalf("repair = %+v, want nothing to do on an empty history", answer)
	}
}
