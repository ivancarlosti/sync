# Directory reports

A **directory report** is a read-only inventory of the organisation behind a
connected account: the tenant record, its users and guests, its groups and
distribution lists, its SharePoint sites, its licence consumption and its
domains. It replaces the "Collector: Microsoft 365 using MS Graph" workflow of
the n8n era with a first-class screen, and it needs **no permission Sync does not
already hold**.

This is *not* the content audit of [api.md §8](api.md) (which walks a folder
tree) and *not* the activity feed of [audit-logs.md](audit-logs.md) (which stores
what people did). A directory report describes the organisation itself.

## 1. What a report contains

| Section | Source | Calls |
|---|---|---|
| `organization` | `GET /organization` | 1 |
| `users` | `GET /users` (paged) | `ceil(users / 999)` |
| `guests` | derived from the users pages (`userType == Guest`) | 0 |
| `groups` | `GET /groups` (paged) | `ceil(groups / 999)` |
| `distribution_lists` | derived from the groups pages (mail-enabled, not security, no Teams) | 0 |
| `sites` | `GET /sites?search=*` (paged) | `ceil(sites / 200)` |
| `licenses` | `GET /subscribedSkus` | 1 |
| `domains` | `GET /domains` | 1 |

The two derived sections are the point of the design: the n8n flow asked Graph
for `/users/{id}/licenseDetails` once per user (one call for every account in the
tenant) and listed groups twice. Here the users page feeds both `users` and
`guests`, the groups page feeds both `groups` and `distribution_lists`, and the
licence names of the users section come from the catalogue the `licenses` section
already reads — so a 5 000-user tenant costs a handful of calls instead of 5 000.

`users` keeps every account (the `user_type` column tells a guest from a member)
and `groups` keeps every group (the `kind` column classifies it), so a report that
skips the derived sections still lists them.

## 2. Permissions

Every section is served by a scope the account already granted when it was
connected (`User.Read.All`, `Group.ReadWrite.All`, `Directory.ReadWrite.All`,
`Domain.Read.All`, `Organization.Read.All`, `Sites.ReadWrite.All` — see
[app-registration.md](app-registration.md)). Starting a report therefore never
asks the operator to reconnect an account.

The sections that would need a scope Sync does **not** request are deliberately
absent:

| Deferred section | Scope it needs |
|---|---|
| `teams`, `channels` | `Team.ReadBasic.All`, `Channel.ReadBasic.All` |
| `team_members`, `channel_members` | `TeamMember.Read.All`, `ChannelMember.Read.All` |
| `mailboxes`, `mailbox_purpose` | `MailboxSettings.Read` |
| `user_activity` (last sign-in) | `AuditLog.Read.All` + Entra ID P1/P2, and it is ~24 h stale |

They arrive together with their permission row, and the API then answers
`reconnect` for an account that connected before the scope existed instead of
failing a section nobody can fix.

## 3. Lifecycle

`ReportService.Start` (`internal/services/report.go`) mirrors `AuditService`: it
reserves the account (one report per account at a time), writes a `running` row,
and collects in the background, so the API answers **202** with an id the screen
polls.

```
POST /api/reports ──► reserve account ──► report row (running)
                          │
                          └─► providers.ReportSource.CollectReport
                                │  per section: paged Graph read (same-host nextLink guard)
                                ▼
                        store one table per section ──► finish row ──► prune (newest 20/account)
```

* **Per-section failure.** A refused endpoint (a personal Microsoft account has
  no `/sites`) sets that section's `error`; the report becomes `partial` and the
  rest of the tables survive. The pass itself only fails on a cancelled or
  timed-out context.
* **Budgets.** 20 000 rows per section and 500 pages per section; hitting either
  sets `truncated` on that section. The whole pass has a 15 minute budget
  (`ReportTimeout`) and ends as `timeout` instead of holding the slot forever.
* **Cancellation** is cooperative (`ReportService.Cancel`), and the scheduler's
  `Bootstrap` closes the reports a restart interrupted.
* **Retention.** The newest 20 reports per account are kept (`PruneReports`);
  there is no separate age-based purge target.

## 4. Schedules

The automatic report is **opt in per account**: a `report_schedules` row is what
says "report this account every N minutes", and no row means "never". The
interval is bounded to 15 minutes … 30 days (default one day) and the sections
are the same vocabulary as a manual run (empty = all).

`ReportService.RunDue` runs inside the scheduler's existing schedule loop (no
extra goroutine): it pushes the slot forward *before* starting, so a failing
account retries once per interval, and it records the report id and the last
error on the row so the screen can explain what happened.

## 5. Export

`GET /api/reports/:id/export` streams a **ZIP holding one CSV per section**
(`users.csv`, `domains.csv`, …), each with a UTF-8 BOM and a human readable
header. A CSV per table is what every spreadsheet opens without a column-splicing
step, and it keeps the export dependency free (standard library only).

## 6. Where things live

| Piece | File |
|---|---|
| Vocabulary (`ReportSection`, `ReportTable`, `ReportSource`) | `internal/providers/report.go` |
| Microsoft collector (paged Graph reads, derived sections) | `internal/providers/microsoft/report.go` |
| Models (`directory_reports`, `report_sections`, `report_schedules`) | `internal/models/report.go` |
| Service (lifecycle, schedules, pruning) | `internal/services/report.go` |
| Endpoints | `internal/handlers/reports.go`, `internal/handlers/routes.go` |
| Screen | `web/src/views/ReportsView.vue` |
| Tests | `internal/providers/microsoft/report_test.go`, `internal/services/report_test.go` |
