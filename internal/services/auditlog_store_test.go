// This file covers the persistence of the Audit Logs feature (audit_log_events and
// audit_log_cursors in store.go): the identity that makes ingestion idempotent,
// the filters the screen uses, the cursor upsert and the retention delete.
//
// The collector and the provider normalisers are covered separately; here it is
// only what reaches the database.

package services

import (
	"context"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// activityEvent builds one stored activity event; only the fields the test reads
// are set.
func activityEvent(provider models.ProviderName, source, remoteID, orgID string, action models.AuditLogAction, actor string, occurredAt time.Time) models.AuditLogEvent {
	return models.AuditLogEvent{
		Provider:   string(provider),
		Source:     source,
		RemoteID:   remoteID,
		OrgID:      orgID,
		Action:     string(action),
		Target:     string(models.AuditLogTargetFile),
		ActorEmail: actor,
		OccurredAt: occurredAt,
	}
}

// TestInsertAuditLogEventsDeduplicates proves the identity of an event is
// (provider, source, remote id): re-reading an overlapping window stores nothing
// twice, while an event of another feed (same id, other source) still lands.
func TestInsertAuditLogEventsDeduplicates(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

	batch := []models.AuditLogEvent{
		activityEvent(models.ProviderMicrosoft, "directory", "evt-1", "tenant-a", models.AuditLogDelete, "admin@contoso.com", now),
		activityEvent(models.ProviderMicrosoft, "directory", "evt-2", "tenant-a", models.AuditLogCreate, "admin@contoso.com", now.Add(time.Minute)),
	}
	if err := store.InsertAuditLogEvents(ctx, batch); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	rows, total, err := store.ListAuditLogEvents(ctx, AuditLogFilter{})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("first insert stored %d/%d events, want 2", len(rows), total)
	}

	// The same window again, plus one genuinely new event and one whose id comes
	// from another feed: only the two new identities may be stored.
	repeat := []models.AuditLogEvent{
		batch[0],
		batch[1],
		activityEvent(models.ProviderMicrosoft, "directory", "evt-3", "tenant-a", models.AuditLogUpdate, "admin@contoso.com", now.Add(2*time.Minute)),
		activityEvent(models.ProviderMicrosoft, "drive", "evt-1", "tenant-a", models.AuditLogDelete, "admin@contoso.com", now.Add(3*time.Minute)),
	}
	if err := store.InsertAuditLogEvents(ctx, repeat); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	rows, total, err = store.ListAuditLogEvents(ctx, AuditLogFilter{})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if total != 4 || len(rows) != 4 {
		t.Fatalf("stored %d/%d events, want 4 (the duplicates must be ignored)", len(rows), total)
	}
}

// TestListAuditLogEventsFilters covers the filters the screen sends, the newest
// first order and the page bounds.
func TestListAuditLogEventsFilters(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)

	batch := []models.AuditLogEvent{
		activityEvent(models.ProviderMicrosoft, "directory", "a", "tenant-a", models.AuditLogDelete, "ana@contoso.com", base),
		activityEvent(models.ProviderMicrosoft, "directory", "b", "tenant-a", models.AuditLogCreate, "bruno@contoso.com", base.Add(time.Hour)),
		activityEvent(models.ProviderGoogle, "admin", "c", "contoso.com", models.AuditLogDelete, "ana@contoso.com", base.Add(2*time.Hour)),
	}
	batch[1].Target = string(models.AuditLogTargetUser)
	batch[1].TargetName = "Bruno Silva"
	batch[2].Target = string(models.AuditLogTargetUser)
	batch[2].TargetName = "Ana Souza"
	if err := store.InsertAuditLogEvents(ctx, batch); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	cases := []struct {
		name   string
		filter AuditLogFilter
		want   int64
	}{
		{"provider", AuditLogFilter{Provider: string(models.ProviderGoogle)}, 1},
		{"org", AuditLogFilter{OrgID: "tenant-a"}, 2},
		{"action", AuditLogFilter{Action: string(models.AuditLogDelete)}, 2},
		{"target", AuditLogFilter{Target: string(models.AuditLogTargetUser)}, 2},
		{"actor", AuditLogFilter{Actor: "ana@contoso.com"}, 2},
		{"search by target name", AuditLogFilter{Search: "bruno"}, 1},
		{"search by actor", AuditLogFilter{Search: "ana@"}, 2},
		{"from", AuditLogFilter{From: timePtr(base.Add(90 * time.Minute))}, 1},
		{"to", AuditLogFilter{To: timePtr(base.Add(30 * time.Minute))}, 1},
		{"combined", AuditLogFilter{Provider: string(models.ProviderGoogle), Action: string(models.AuditLogDelete)}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, total, err := store.ListAuditLogEvents(ctx, tc.filter)
			if err != nil {
				t.Fatalf("listing: %v", err)
			}
			if total != tc.want {
				t.Fatalf("filter %s matched %d events, want %d", tc.name, total, tc.want)
			}
		})
	}

	rows, _, err := store.ListAuditLogEvents(ctx, AuditLogFilter{})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(rows) != 3 || rows[0].RemoteID != "c" || rows[2].RemoteID != "a" {
		t.Fatalf("rows are not newest first: %+v", rows)
	}

	page, _, err := store.ListAuditLogEvents(ctx, AuditLogFilter{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("paging: %v", err)
	}
	if len(page) != 1 || page[0].RemoteID != "b" {
		t.Fatalf("page [1,1] = %+v, want the middle event", page)
	}
}

// TestAuditLogCursorUpsert proves a feed position is created on the first read
// and updated in place afterwards: one row per (provider, org, stream), never a
// growing pile of positions for the same feed.
func TestAuditLogCursorUpsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	fresh, err := store.AuditLogCursorFor(ctx, string(models.ProviderMicrosoft), "tenant-a", "directory")
	if err != nil {
		t.Fatalf("reading a missing cursor: %v", err)
	}
	if fresh.ID != 0 || fresh.Cursor != "" || fresh.OrgID != "tenant-a" {
		t.Fatalf("a missing cursor came back as %+v, want a zero cursor for the feed", fresh)
	}

	first := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	fresh.Cursor = "watermark-1"
	fresh.LastEventAt = &first
	fresh.LastRunAt = &first
	if err := store.SaveAuditLogCursor(ctx, &fresh); err != nil {
		t.Fatalf("creating the cursor: %v", err)
	}

	stored, err := store.AuditLogCursorFor(ctx, string(models.ProviderMicrosoft), "tenant-a", "directory")
	if err != nil {
		t.Fatalf("reading the cursor back: %v", err)
	}
	if stored.ID == 0 || stored.Cursor != "watermark-1" {
		t.Fatalf("stored cursor = %+v, want the created row", stored)
	}

	second := first.Add(time.Hour)
	stored.Cursor = "watermark-2"
	stored.LastEventAt = &second
	stored.LastRunAt = &second
	stored.LastError = ""
	if err := store.SaveAuditLogCursor(ctx, &stored); err != nil {
		t.Fatalf("updating the cursor: %v", err)
	}

	positions, err := store.ListAuditLogCursors(ctx)
	if err != nil {
		t.Fatalf("listing cursors: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("stored %d positions, want 1 (the feed must be updated in place)", len(positions))
	}
	if positions[0].Cursor != "watermark-2" {
		t.Fatalf("cursor = %q, want watermark-2", positions[0].Cursor)
	}
	if positions[0].LastEventAt == nil || !positions[0].LastEventAt.Equal(second) {
		t.Fatalf("last event at = %v, want %v", positions[0].LastEventAt, second)
	}
}

// TestPurgeAuditLogsBefore proves the retention delete is on OccurredAt: an event
// before the cutoff goes, one on or after it stays, whatever the order of their
// ids.
func TestPurgeAuditLogsBefore(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -30)

	if err := store.InsertAuditLogEvents(ctx, []models.AuditLogEvent{
		activityEvent(models.ProviderMicrosoft, "directory", "old", "tenant-a", models.AuditLogDelete, "a@contoso.com", cutoff.Add(-time.Hour)),
		activityEvent(models.ProviderMicrosoft, "directory", "edge", "tenant-a", models.AuditLogDelete, "a@contoso.com", cutoff),
		activityEvent(models.ProviderMicrosoft, "directory", "new", "tenant-a", models.AuditLogCreate, "a@contoso.com", now),
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	removed, err := store.PurgeAuditLogsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("purging: %v", err)
	}
	if removed != 1 {
		t.Fatalf("purged %d events, want 1 (the one strictly before the cutoff)", removed)
	}
	rows, total, err := store.ListAuditLogEvents(ctx, AuditLogFilter{})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if total != 2 {
		t.Fatalf("%d events survived, want 2", total)
	}
	for _, row := range rows {
		if row.RemoteID == "old" {
			t.Fatalf("the event before the cutoff survived: %+v", row)
		}
	}
}

// timePtr is a small helper for the optional date filters.
func timePtr(value time.Time) *time.Time { return &value }
