# Directory reports

A **directory report** is a read-only inventory of the organisation behind a
connected account: the tenant record, its users, its groups, its licences and its
domains. It replaces the "Collector: Microsoft 365 using MS Graph" and
"Collector: Google Workspace using GAM" workflows of the n8n era with a
first-class screen, and it runs over the OAuth connection the account already
has — no application registration beyond the one in
[app-registration.md](app-registration.md), no GAM binary and no SSH.

This is *not* the content audit of [api.md §8](api.md) (which walks a folder
tree) and *not* the activity feed of [audit-logs.md](audit-logs.md) (which stores
what people did). A directory report describes the organisation itself.

## 1. What a report contains

The section vocabulary is shared; each provider fills the half its directory
actually has (`providers.ReportSectionsFor`). `GET /api/reports/sections` answers
the list for the account, which is what the start form offers.

**Microsoft 365** (`internal/providers/microsoft/report.go`):

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

**Google Workspace** (`internal/providers/google/report.go`):

| Section | Source | Calls |
|---|---|---|
| `organization` | `GET /customers/my_customer` (fallback: derived from the domain list) | 1 |
| `users` | `GET /users?projection=full` (paged, 500) | `ceil(users / 500)` |
| `suspended_users` | derived from the users pages (`suspended == true`) | 0 |
| `groups` | `GET /groups` (paged, 200) | `ceil(groups / 200)` |
| `group_members` | `GET /groups/{key}/members` per group (paged, 200) | one walk per group |
| `domains` | `GET /customer/my_customer/domains` | 1 |
| `org_units` | `GET /customer/my_customer/orgunits?type=all` | 1 |
| `roles` | `GET /customer/my_customer/roles` (paged, 200) | `ceil(roles / 200)` |
| `role_assignments` | `GET /customer/my_customer/roleassignments` (paged, 200) | `ceil(assignments / 200)` |
| `licenses` | `GET licensing/v1/product/{id}/users` per product of the catalogue | 11 |
| `shared_drives` | `GET drive/v3/drives` (the listing the folder picker uses) | `ceil(drives / 100)` |

The derived sections are the point of the design: the n8n flows asked Graph for
`/users/{id}/licenseDetails` once per user and re-listed the Google directory with
`isSuspended=true` and once per group for its members. Here the users page feeds
both `users` and `guests`/`suspended_users`, the groups page feeds both `groups`
and the walk of `group_members`, and the licence names of the users section come
from the catalogue the `licenses` section already reads.

`users` keeps every account (the `user_type`/`suspended` column tells them apart)
and `groups` keeps every group (the `kind`/`direct_members` column classifies it),
so a report that skips the derived sections still lists them.

`group_members` is the one section that costs a call per group — the API has no
tenant-wide membership listing, and neither has GAM. It is why the section is
opt-in: a report started from the screen picks it explicitly, and a scheduled
report that leaves it out pays nothing for it.

A report that asks for `users` also walks the licence catalogue (one call per
product, eleven of them), because `users.list` carries no licence: that is the
one place where a section pays for a column it does not own. A grant without
`apps.licensing` leaves the column empty instead of failing the table.

## 2. Permissions

**Microsoft 365** needs nothing new: every section is served by a scope the
account already granted when it was connected (`User.Read.All`,
`Group.ReadWrite.All`, `Directory.ReadWrite.All`, `Domain.Read.All`,
`Organization.Read.All`, `Sites.ReadWrite.All` — see
[app-registration.md](app-registration.md)).

**Google Workspace** is covered by the always-on Workspace scope set as well
(`admin.directory.user`, `.group`, `.group.member`, `.domain.readonly`,
`.orgunit`, `.rolemanagement.readonly`, `apps.licensing`, `drive`), with one
deliberate addition: `admin.directory.customer.readonly`, which is what answers
`GET /customers/my_customer` — the `organization` row (primary domain, address,
administrator). Everything else — the users, groups, members, domains,
organisational units, roles, role assignments, licence assignments and shared
drives — comes from the scopes the capability badges of the Accounts screen are
already derived from.

That one scope is additive and forgiving:

* a connection made **before** it was requested answers `403` on the customer
  record, and the `organization` section is then **derived from the domain list**
  (the `source` column reads `domains` instead of `customer`) — the row is
  degraded, the section does not fail;
* a connection made **today** gets the full record, because the scope is part of
  the authorization request from the first connection ("one always-on scope
  set", [app-registration.md §1](app-registration.md#1-what-the-permissions-are-for));
* Accounts → *Check the token* reports `needs_reconnect` for the older grant, so
  the operator can upgrade it deliberately.

What a GAM style export can still reach and Sync cannot is deliberately absent,
because the scope behind it is absent:

| Deferred collection | Why |
|---|---|
| Gmail delegates, per-mailbox settings | needs per-mailbox impersonation (`gmail.settings.sharing` + domain-wide delegation), not a user consent |
| YouTube CMS (content owner reports) | needs a content owner identity, not a Workspace account |
| Chrome management (browsers, profiles, telemetry) | needs the Chrome Management API and a separate admin role |
| Shared drive permissions (`teamdriveacls`) | the Drive API needs one call per file; deferred until a report can stream it |

The Microsoft side defers its own set (teams, channels, mailbox settings,
per-user sign-in activity), and both lists arrive the same way: a permission row
first, a section second, with `reconnect` for a grant that predates it.

## 3. Lifecycle

`ReportService.Start` (`internal/services/report.go`) mirrors `AuditService`: it
reserves the account (one report per account at a time), writes a `running` row,
and collects in the background, so the API answers **202** with an id the screen
polls.

```
POST /api/reports ──► reserve account ──► report row (running)
                          │
                          └─► providers.ReportSource.CollectReport
                                │  per section: paged read
                                │  Graph: same-host nextLink guard
                                │  Google: opaque nextPageToken, validated + escaped
                                ▼
                        store one table per section ──► finish row ──► prune (newest 20/account)
```

* **Per-section failure.** A refused endpoint (a personal Microsoft account has
  no `/sites`, a Google grant without `apps.licensing` cannot list licences) sets
  that section's `error`; the report becomes `partial` and the rest of the tables
  survive. The pass itself only fails on a cancelled or timed-out context.
* **Budgets.** 20 000 rows per section and 500 pages per walk; hitting either
  sets `truncated` on that section. The whole pass has a 15 minute budget
  (`ReportTimeout`) and ends as `timeout` instead of holding the slot forever.
* **Degradation, not failure.** A Google grant that predates the customer scope
  still produces an `organization` row (derived from the domain list), and a
  grant without `apps.licensing` still produces the `users` table with an empty
  `licenses` column. Losing one column is not worth losing the table.
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
| Vocabulary (`ReportSection`, `ReportSectionsFor`, `ReportTable`, `ReportSource`) | `internal/providers/report.go` |
| Microsoft collector (paged Graph reads, derived sections) | `internal/providers/microsoft/report.go` |
| Google collector (Admin SDK Directory, License Manager, shared drives) | `internal/providers/google/report.go` |
| Google scopes the collector relies on | `internal/providers/google/permissions.go` |
| Models (`directory_reports`, `report_sections`, `report_schedules`) | `internal/models/report.go` |
| Service (lifecycle, schedules, pruning) | `internal/services/report.go` |
| Endpoints | `internal/handlers/reports.go`, `internal/handlers/routes.go` |
| Screen | `web/src/views/ReportsView.vue` |
| Tests | `internal/providers/report_test.go`, `internal/providers/microsoft/report_test.go`, `internal/providers/google/report_test.go`, `internal/services/report_test.go` |

The screen needs no provider-specific code: it renders whatever
`GET /api/reports/sections` answers and translates `reports.section_<name>` from
the i18n catalogs, so a new section is a label and a collector, nothing else.

