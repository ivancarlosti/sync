# Calendar synchronisation between accounts and iCalendar subscriptions

## Context

Sync copies a folder tree between two connected accounts (Google Drive, Microsoft
Graph). Operators now need the same pairing for **calendars**: keep a Google
calendar and a Microsoft 365 calendar aligned, and mirror a public or private
iCalendar (`.ics`) feed — an Outlook published calendar, a Nextcloud export, a
team feed behind a token URL — into a writable provider calendar.

The existing design is provider neutral: `internal/providers.Provider` describes
files, `internal/services.SyncService` owns the run lifecycle and the per-file
state table (`sync_files`) that makes deletions and conflicts detectable without
re-reading history. Calendars have the same problem shape (two listings, a
remembered pairing, a conflict rule) but a different vocabulary (UID, start/end,
all-day, recurrence), so the calendar engine is a sibling of the file engine, not
a branch inside it.

## Goal

1. A **calendar job** pairs a source (Google calendar, Microsoft calendar or an
   `.ics` URL) with a writable destination calendar on a connected account.
2. The engine lists a bounded window (`past_days`..`future_days`), pairs events
   by **iCalendar UID** (falling back to the tracked event id), and applies the
   existing conflict vocabulary (`newest_wins`, `source_wins`,
   `destination_wins`, `skip`) with direction `bidirectional` or `one_way`.
3. An `.ics` source supports public and private feeds: HTTPS URL, optional HTTP
   Basic credentials or a bearer token, ETag/Last-Modified conditional GET, and a
   parser that unfolds folded lines, resolves `TZID`, all-day `DATE` values,
   `DURATION`, and flags recurring events.
4. Runs are auditable exactly like file runs: `calendar_runs` + per-event
   `calendar_run_items`, mirrored summary on the job (`last_status`,
   `next_run_at`), scheduler integration, notifications reuse.
5. The SPA gets a **Calendars** screen (list, editor, run/cancel, history) and
   the API gains `/api/calendars*` plus `GET /api/accounts/:id/calendars`.

## Steps

1. **Models** (`internal/models/calendar.go`): `CalendarJob`,
   `CalendarEventLink` (UID → both event ids + fingerprints + last-known
   starts), `CalendarRun`, `CalendarRunItem`. New enums in `enums.go`:
   `CalendarSourceKind`, `CalendarDirection`.
2. **Provider layer**: `providers.CapabilityCalendars`, the `CalendarProvider`
   interface (`Calendars`, `ListEvents`, `CreateEvent`, `UpdateEvent`,
   `DeleteEvent`), a normalized `CalendarEvent`/`CalendarEventInput` and
   `EventFingerprint` (content hash independent of the provider).
3. **Google** (`providers/google/calendar.go`): CalendarList + Events v3, with a
   dedicated identifier pattern for calendar ids (emails, `primary`), inline URL
   guards, deterministic `iCalUID` for cross-provider identity.
4. **Microsoft** (`providers/microsoft/calendar.go`): `Me().Calendars()`,
   `CalendarView` over the window, event create/patch/delete through the Graph
   SDK, `Calendars.ReadWrite` delegated scope.
5. **iCalendar** (`internal/ical`): `Fetch` (conditional GET, credentials,
   SSRF guard reusing the notification policy) and `Parse` (unfolding, escapes,
   time zones, all-day, duration, recurrence flags, size/event caps).
6. **Services**: `calendar_store.go` (state + runs), `calendar_plan.go` (pure
   `decide` function, unit tested), `calendarsync.go` (resolution, listing,
   apply, persistence, run lifecycle, cancellation, `StartIfIdle`).
7. **Scheduler**: a calendar pass sharing the sync loop interval, bootstrap of
   `next_run_at`, closing of interrupted calendar runs.
8. **API**: CRUD, `run`, `cancel`, `runs`, `runs/:id`, `preview` (fetch an ICS
   feed before saving), `GET /accounts/:id/calendars`.
9. **UI**: `CalendarsView.vue`, API client, router entry, header navigation and
   i18n keys in all seven catalogs.
10. **Docs/tests**: `docs/calendars.md`, `docs/api.md`, `docs/providers.md`;
    parser tests, decision-table tests, provider scope tests, handler tests.

## Risks

- **Scope widening forces reconnects.** Adding calendar scopes marks existing
  accounts as missing `calendars`; the Accounts screen already asks for a
  reconnect, and docs must say so.
- **UID quality varies.** Google returns `iCalUID`, Graph returns `iCalUId`
  (often empty on API-created events), ICS feeds are usually compliant but not
  always. Mitigation: always write a deterministic UID on create when the
  provider accepts one, and remember the pairing in `calendar_event_links` so a
  missing UID never duplicates an event.
- **Recurring events.** Expanding series cross-provider is out of scope for this
  iteration: masters and instances that declare recurrence are skipped and
  counted, never deleted.
- **Deletions vs window drift.** An event that moves outside the listing window
  looks deleted. The engine only treats a tracked event as deleted when its
  last-known start is still inside the window.
- **Private feeds.** Credentials are encrypted with `SecretBox`; the fetcher
  refuses private targets unless `CALENDAR_ALLOW_PRIVATE_TARGETS=true`, and the
  URL itself is treated as a secret in the UI (password field, never echoed).
- **Writes without a UID.** Graph ignores `iCalUId` on create and Google requires
  a valid-looking one; the state table is therefore authoritative, and a failed
  state write is reported as an error instead of silently duplicating.
