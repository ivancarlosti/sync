# Directory reports (Microsoft 365 audit workbook)

## Context

Operators of Sync already hold delegated Microsoft Graph permissions for the whole
directory (`User.Read.All`, `Group.ReadWrite.All`, `Directory.ReadWrite.All`,
`Domain.Read.All`, `Organization.Read.All`, `Sites.ReadWrite.All`). Today those
permissions are only *declared* (the capability badges of an account) — nothing
reads them.

The n8n "Collector: Microsoft 365 using MS Graph" flow replicated an audit
workbook: users, guests, groups, distribution lists, sites, licences and domains,
exported to a spreadsheet. This plan brings that workflow into Sync as a first
class, read-only **directory report**, built from the permissions the instance
already has.

Phase 1 deliberately changes **no permission**: every section below is served by
a scope Sync already requests. Sections that need a scope Sync does not hold
(`team_members`, `mailbox_purpose`, `channel_members`, per-user sign-in activity)
are deferred to phase 2, where the UI offers "reconnect the account" instead of
failing.

## Goal

- A `Reports` screen that starts a directory report against an account, polls it
  while it runs, reads every section as a table and downloads the whole workbook
  as a zip of CSVs.
- A per-account opt-in schedule (interval in minutes) driven by the existing
  in-process scheduler.
- Zero new provider permissions; zero new Go dependencies (stdlib `archive/zip`).
- ~7 Graph calls per report plus paging, instead of the per-user
  `/users/{id}/licenseDetails` loop of the n8n flow (which costs one call per
  user in the tenant).

## Sections of phase 1

| Section | Scope already held | Source |
| --- | --- | --- |
| `organization` | `Organization.Read.All` | `GET /organization` |
| `users` | `User.Read.All` | `GET /users` (paged) |
| `guests` | `User.Read.All` | derived from the users page (`userType == Guest`) |
| `groups` | `Group.ReadWrite.All` | `GET /groups` (paged) |
| `distribution_lists` | `Group.ReadWrite.All` | derived from the groups page |
| `sites` | `Sites.ReadWrite.All` | `GET /sites?search=*` (paged) |
| `licenses` | `Organization.Read.All` | `GET /subscribedSkus` |
| `domains` | `Domain.Read.All` | `GET /domains` |

Deferred to phase 2: `teams`, `channels`, `team_members`, `mailboxes`,
`mailbox_purpose`, `channel_members`, `user_activity`, `sites_without_teams`.

## Steps

1. **Vocabulary** — `internal/providers/report.go`: `ReportSection`,
   `ReportColumn`, `ReportRow`, `ReportTable`, `ReportProgress` and the optional
   `ReportSource` interface (type-asserted like `ScopeCatalog`/`SiteBrowser`, so
   the mandatory `Provider` contract does not change).
2. **Models** — `internal/models/report.go`: `DirectoryReport` (one execution),
   `ReportSectionData` (one stored table, columns and rows as JSON) and
   `ReportSchedule` (per account opt-in). Registered in
   `internal/database/database.go`.
3. **Collector** — `internal/providers/microsoft/report.go`: paged Graph reads
   with the same-host `@odata.nextLink` guard and `DoWithRetry` of
   `activitylog.go`; per-section caps, per-section error reporting (a refused
   endpoint never fails the whole report) and the two derived sections.
4. **Service** — `internal/services/report.go`: `ReportService` with the exact
   lifecycle of `AuditService` (reserve → attach → run → release, `Wait`,
   `Cancel`, one report per account), plus `RunDue` for the scheduler.
5. **Store** — `internal/services/store.go`: create/finish/progress/list/get,
   section rows, interrupted-run recovery, per-account pruning and the schedule
   table.
6. **Scheduler** — a reports pass inside the existing schedule loop
   (`RunDueReports`), so no extra goroutine and no new timing constant beyond one
   minute of granularity.
7. **API** — `internal/handlers/reports.go`:
   `GET /api/reports/sections`, `POST /api/reports`, `GET /api/reports`,
   `GET /api/reports/:id`, `GET /api/reports/:id/sections/:section`,
   `POST /api/reports/:id/cancel`, `GET /api/reports/:id/export` (zip of CSVs),
   `GET|PUT|DELETE /api/reports/schedules[/:account_id]`.
8. **Web** — `web/src/views/ReportsView.vue` (start form, schedule card, history
   table, report dialog with a section switcher), `api.ts` types and client,
   router entry, navigation link and the seven i18n catalogs.
9. **Docs & tests** — `docs/reports.md`, `docs/api.md`, `docs/architecture.md`,
   `docs/testing.md`; provider and service tests with the existing
   `recordingTransport` / fake provider harnesses.

## Risks

- **Tenant size.** A 100k-user directory is a 100k-row table; sections are capped
  at 20 000 rows and the cap is reported as `truncated` on the section, not
  silently. Reports are pruned to the newest 20 per account.
- **A section failing must not fail the report.** Every section carries its own
  `error` (a personal account has no `/sites`, a tenant may refuse `/domains`);
  the run is `partial` and the UI shows the reason next to the section.
- **`assignedLicenses` in `$select`.** Some tenants refuse the navigation
  property; the users reader retries the page with the plain select and leaves
  the licence column empty rather than losing the section.
- **Throttling.** `/users` and `/groups` are the hot endpoints; the collector
  reuses the Graph retry policy and honours the request context (15 minute
  budget), so a throttled tenant ends as `timeout` and stays resumable by hand.
- **Naming.** `reports` and `/api/reports` are new; the existing `audits`
  (content audit) and `audit-logs` (activity feed) keep their routes, so no
  client breaks.
