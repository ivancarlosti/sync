# Plan — Audit Logs (activity log) + rename current Audit to "Audit Storage"

Status: in progress (started 2026-11-07). Phase 0, 1 and 2 done; Phases 3–8 pending.

## 1. Context

The existing **Audit** tab (`AuditRun`, `AuditEntry`, `/api/audits`,
`web/src/views/AuditView.vue`, nav key `nav.audit`) is a **content/storage
report**: it walks a folder tree and reports sizes. It answers "what is stored
here", never "who deleted this file" or "who created or removed that user,
group, site or drive".

Operators repeatedly ask the second question. Both providers expose such a feed,
but they are **tenant/domain scoped**, not user scoped, and they are not
streaming-friendly for a single-binary, no-queue, no-public-ingress deployment:

| Feed | Scope | Retention / access |
|---|---|---|
| Microsoft Graph `auditLogs/directoryAudits` | Entra tenant | directory changes, ~30 days |
| Microsoft Graph `security/auditLog/queries` | tenant | unified audit log, up to 30 days live |
| Office 365 Management Activity API | tenant | content blobs expire **~7 days** |
| Google Workspace Reports `activities.list` | Workspace domain (customer) | ~6 months |

Because the vendor windows are short and the deployment has no queue, the only
design that fits is a **periodic polling collector** that stores normalised
events locally (see [2026-10-07-audit-content-report.md](2026-10-07-audit-content-report.md)
for the run/audit machinery it reuses).

### 1.1 Multi-tenant app reuse (confirmed)

One published OAuth app per provider serves **many** tenants/domains — that is
what the codebase already assumes:

- **Microsoft**: `MICROSOFT_TENANT_ID` defaults to `common`
  (`internal/config/config.go`), multi-tenant registration is documented in
  `docs/app-registration.md` §3.1, and `AdminConsentURL` uses `consentTenant`
  so `common`/empty become `organizations` (`internal/providers/microsoft/permissions.go`).
  **Each tenant's admin still consents once for their own directory**; a
  multi-tenant app must be **publisher-verified** before other tenants may
  consent. `provider.microsoft.admin_consent` currently records only the last
  directory (display-only limitation).
- **Google**: one `GOOGLE_CLIENT_ID` serves many Workspace domains; each domain
  super-admin authorises. Public distribution of the always-on admin scopes
  (`admin.directory.*`, `apps.licensing`, and the new
  `admin.reports.audit.readonly`) requires an **External** app with OAuth
  verification (restricted Drive scopes need a CASA assessment) and/or a Google
  Workspace Marketplace listing.
- **Consequence for this feature:** the feeds are tenant/domain wide, so N admin
  accounts in the same tenant would ingest the same events N times. The store
  must therefore dedup by **org id** (`tid` for Microsoft, customer id for
  Google) and run **one collector per org**, not per account. `ConnectedAccount`
  does not persist that id today (only a derived `Kind`), so capturing it is a
  prerequisite (see §3).

## 2. Goal

1. Rename the current Audit tab to **Audit Storage** — label only, keeping the
   `/audit` route and `/api/audits` API untouched.
2. Add an **Audit Logs** tab that lists normalised activity events —
   file deletes/moves, user create/delete, group create/delete, site/drive
   create/delete — who did it and when, filterable, CSV-exportable, purged by the
   existing retention policy.

Non-goals: real-time streaming, per-event push notifications, restoring deleted
objects.

## 3. Steps

Phases are independently shippable; each ends with `go test ./...` and
`npm run build` green.

### Phase 0 — Rename (label only)

- i18n only: `nav.audit` and `audit.title` become "Audit Storage" in all seven
  locales (`en-US`, `es-MX`, `fr-FR`, `pt-BR`, `ar-SA`, `hi-IN`, `h-CN`). The
  `auditLogs.*` namespace ships with the view itself (Phase 6), so no key exists
  before the screen that reads it.
- `AppHeader.vue`: keep `route: 'audit'`; the label is `nav.audit` and now reads
  "Audit Storage". The new `audit-logs` nav entry ships with Phase 6 (it needs
  the route and view, so adding it now would be a dead link).
- No route, handler, model or table rename. Deep links and the API stay stable.
- Done 2026-11-07: both keys renamed in all seven locales.

### Phase 1 — Org id + models + cursors

- `internal/models/account.go`: add `OrgID string` (`size:191;index`) to
  `ConnectedAccount`, populated from the provider identity (`tid` claim for
  Microsoft, Workspace hosted domain for Google) at connect/refresh; backfill
  lazily from the same signals when the column is empty.
- `internal/models/auditlog.go`:
  - `AuditLogEvent` (`audit_log_events`): `provider`, `org_id`, `source`
    (`directory`/`drive`/`unified`), `remote_id` (unique with provider+source),
    `action`, `actor_email`, `actor_id`, `target_kind`, `target_name`,
    `target_id`, `drive_id`, `site_id`, `occurred_at`, `raw` (JSON), `created_at`.
  - `AuditLogCursor` (`audit_log_cursors`): `provider`, `org_id`, `stream`,
    `cursor`/`last_event_at`, `last_run_at`, `last_error`, unique on
    `(provider, org_id, stream)`.
- `models/enums.go`: `AuditLogAction` (`create`,`delete`,`update`,`move`,
  `share`,`login`) and `AuditLogTarget` (`file`,`folder`,`user`,`group`,`site`,
  `drive`,`other`).
- Migration + backfill in `internal/database/database.go`; `Store` methods
  (`InsertAuditLogEvents` with `ON CONFLICT DO NOTHING`, cursor load/save, list
  with filters, purge older than N days).
- **Done 2026-11-07.** `models.AuditLogEvent`/`models.AuditLogCursor`,
  `AuditLogAction`/`AuditLogTarget`, `AuditLogEvent.OrgID`,
  `ConnectedAccount.OrgID` (also exposed as `org_id` in `accountView`),
  `providers.Account.OrgID` populated by both providers, the migration rows, and
  the store methods (`InsertAuditLogEvents`, `ListAuditLogEvents`,
  `AuditLogCursorFor`, `SaveAuditLogCursor`, `ListAuditLogCursors`,
  `PurgeAuditLogsBefore`). `InsertAuditLogEvents` returns no count on purpose: the
  SQLite driver over-reports `RowsAffected` when a batch mixes new and duplicate
  rows, so ingestion is treated as idempotent and the volume is logged as
  `len(events)`.
- **Deferred to Phase 4**: the `purge_audit_logs_days` setting and the
  `auditlogs` purge target, because both extend the tightly asserted
  settings/purge surface (`PurgeTargets`, `AppSettings`, handler tests) and are
  worthless before the collector writes events.

### Phase 2 — Optional provider capability

- `internal/providers/provider.go`: optional interface, implemented like
  `SiteBrowser`/`SiteResolver` so nothing breaks for providers that cannot:

  ```go
  type ActivityLogSource interface {
      ActivityLogScopes() []string  // extra scopes the collector needs
      ActivityLogs(ctx context.Context, creds Credentials, tokens *Tokens, cursor string) ([]ActivityEvent, string, error)
  }
  ```

- `ActivityEvent` mirrors the normalised fields of `AuditLogEvent`.
- `helpers` (`CapabilitiesOf`/`MissingCapabilities`) gain a `logs` capability so
  the Accounts screen flags accounts that need reconnecting.
- **Done 2026-11-07.** `providers.ActivityLogSource` (two methods:
  `ActivityLogScopes` and `ActivityLogs(ctx, creds, tokens, cursor)`), plus
  `providers.ActivityEvent` and `providers.OrgIdentifier` (`OrgIDOf(tokens)`),
  implemented by Microsoft from the `tid` claim. The organisation is not a
  parameter of `ActivityLogs` on purpose: one token set *is* one organisation.
- **Deferred to Phase 7**: `CapabilityLogs` and the `logs` row of
  `ProviderCapabilities`, because a capability no permission declares would make
  every existing account report it as missing (a red "needs reconnect" badge) the
  moment it ships. It must land in the same change as the scopes it derives from.

### Phase 3 — Collector service

- `internal/services/auditlog.go`: `AuditLogService` with `Collect(ctx)` that,
  for each org with a live account, resolves the cursor, calls
  `ActivityLogs`, normalises, inserts with dedup, and saves the cursor.
- Grouping: **one org = one collector pass**, picking the account with the
  widest grant; errors never abort the whole pass (per-org `last_error`).
- Rate/backoff: reuse the engine's token freshen + 401 reissue; a 429 backs the
  cursor off without advancing it.

### Phase 4 — Scheduler + maintenance

- `scheduler.go`: a `logs` loop (default every 5–15 min, configurable) calling
  `AuditLogService.Collect`; `RunRetention` also purges `audit_log_events` via
  `PurgeService` (add target `auditlogs`) and `SettingPurgeAuditLogsDays`.
- `handlers/maintenance.go`: add `POST /api/maintenance/auditlogs/collect`
  (force) and extend `handleMaintenancePurge` to accept `auditlogs`.

### Phase 5 — API

- `GET /api/audit-logs` — filters: `provider`, `org_id`, `action`, `target`,
  `actor`, `q`, `from`, `to`, `limit`, `offset`; returns rows + total.
- `GET /api/audit-logs/export` — CSV with BOM + `encoding/csv` (same pattern as
  `handleExportAudit`).
- `GET /api/audit-logs/cursors` — collector status per org (source, last run,
  cursor, error) for the UI banner.

### Phase 6 — Frontend

- `web/src/views/AuditLogsView.vue`: filter bar, virtualised/scrolling table,
  "who / what / when" columns, provider + action badges, empty/needs-consent
  states, CSV export, cursor banner.
- `api.ts`: typed client functions; router + `AppHeader` entry; i18n strings
  for all seven locales.

### Phase 7 — Provider implementations

- **Microsoft**: prefer Graph `security/auditLog/queries` (existing Graph token,
  `AuditLog.Read.All`) with `auditLogs/directoryAudits` for directory changes;
  map `deleted`/`added` members and directory objects to `AuditLogEvent`. Avoid
  the `manage.office.com` `ActivityFeed.Read` token unless unified-log fidelity
  is required (separate audience = separate token plumbing).
- **Google**: Reports API `activities.list` per application
  (`admin`, `drive`, `login`, `token`) with `admin.reports.audit.readonly`;
  paginate with `pageToken`, store the newest event id/time as the cursor.
- Both map to the same `AuditLogEvent`; anything unknown lands in `raw`.

### Phase 8 — Tests + docs

- Unit tests: normalisation (provider → event), dedup on `(provider, source,
  remote_id)`, cursor advance/no-advance on error, purge target.
- `docs/audit-logs.md` + `docs/api.md` additions, `docs/app-registration.md`
  scope rows (`AuditLog.Read.All`, `admin.reports.audit.readonly`), and the
  multi-tenant consent note from §1.1.

## 4. Risks

- **Consent gap.** Personal accounts and non-admin grants cannot read the feed;
  existing connections will show `needs_reconnect` and tenants/domains that have
  not granted the new scopes see HTTP 412 `consent_required`. Mitigation:
  capability badges + a clear banner, never a silent empty list.
- **Duplicate events across admin accounts in one tenant.** Mitigated by org
  grouping + the unique `(provider, source, remote_id)` index.
- **Vendor retention shorter than the polling interval** loses events. Mitigated
  by a short default interval (≤5 min for the 7-day Office 365 feed) and making
  it configurable.
- **Scopes change re-consent** for every tenant/domain (Microsoft admin consent,
  Google per-domain authorisation). This is unavoidable and must be called out
  in the release note.
- **Public distribution burden:** Google OAuth verification/CASA and Microsoft
  publisher verification are process steps outside the code; they gate *public*
  multi-tenant use, not local testing.
- **Volume.** Large tenants can emit many events; mitigated by pagination,
  incremental cursors, retention purge and indexes on
  `(provider, org_id, occurred_at)`.
