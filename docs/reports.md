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
| `teams` | `GET /teams` (paged, 999) | `ceil(teams / 999)` |
| `team_members` | `GET /teams/{id}/members` per team | one walk per team |
| `planner_plans` | `GET /groups/{id}/planner/plans` per group | one walk per group |

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
| `shared_drive_members` | `GET /files/{id}/permissions` per shared drive (`useDomainAdminAccess`, paged, 100) | one walk per shared drive |

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

The same is true of the two product sections on the Microsoft side and of the
Google membership section, for the same reason: Graph has no tenant-wide team
membership (`/teams/{id}/members` is per team) and no tenant-wide Planner
collection at all (a plan hangs off the group that owns it), and a shared drive's
permissions are permissions *on a drive*, not group members of the domain. Each
reuses a collection the report already reads — `teams`/`team_members` share the
`/teams` page, `planner_plans` walks the `groups` page, and
`shared_drive_members` walks the listing `shared_drives` already reads — so the
listing itself is never paid for twice. `team_members` reads the team endpoint
rather than the group one because the role of a member there is the *conversation*
role of the team (owner of the team, not owner of the backing group).

A `shared_drive_members` walk that fails on one drive keeps the rows of the others
and reports the error only when it could not read a single drive; `team_members`
and `planner_plans` stop at the shared per-collection walk limit
(`reportGroupWalkLimit`, 5000 collections) and mark themselves truncated, exactly
like `group_members` does.

A report that asks for `users` also walks the licence catalogue (one call per
product, eleven of them), because `users.list` carries no licence: that is the
one place where a section pays for a column it does not own. A grant without
`apps.licensing` leaves the column empty instead of failing the table.

## 2. Permissions

**Microsoft 365** is served by the scopes the account grants at connection time
(`User.Read.All`, `Group.ReadWrite.All`, `Directory.ReadWrite.All`,
`Domain.Read.All`, `Organization.Read.All`, `Sites.ReadWrite.All` — see
[app-registration.md](app-registration.md)), with two product additions this
release introduced: `Team.ReadBasic.All` + `TeamMember.Read.All` for the `teams`
and `team_members` sections, and `Tasks.Read` for `planner_plans`
(`internal/providers/microsoft/permissions.go`). All three are directory scopes,
so an administrator grants them tenant-wide, and none of them is offered to a
personal Microsoft account (a team and a plan live in a directory, and an MSA has
none — see [app-registration.md §1](app-registration.md#1-what-the-permissions-are-for)).
`Tasks.Read` is the least privileged *delegated* scope of
`GET /groups/{id}/planner/plans`; the `Tasks.Read.All` next to it in the Graph
permission reference is an application permission only, so a delegated request
cannot carry it (§1), and `Group.ReadWrite.All` — already granted at connection
time — is the higher privileged delegated scope of the same read.

An account connected **before** those scopes joined the always-on set keeps
synchronising files exactly as it did; `GET /api/accounts` reports `teams` and
`planner` in `missing_capabilities` (`needs_reconnect`), and a report that asks
for the sections fails only those tables until the account is reconnected. The
capability badges are filtered per provider (`providers.CapabilitiesFor`), so a
Workspace account is never offered `teams` or `planner`: they are not missing
there, they do not exist.

**Google Workspace** is covered by the always-on Workspace scope set as well
(`admin.directory.user`, `.group`, `.group.member`, `.domain.readonly`,
`.orgunit`, `.rolemanagement.readonly`, `apps.licensing`, `drive`), with one
deliberate addition: `admin.directory.customer.readonly`, which is what answers
`GET /customers/my_customer` — the `organization` row (primary domain, address,
administrator). Everything else — the users, groups, members, domains,
organisational units, roles, role assignments, licence assignments, shared drives
and their members — comes from the scopes the capability badges of the Accounts
screen are already derived from. `shared_drive_members` needs no new scope: the
Drive scope that lists the shared drives also answers `/files/{id}/permissions`,
the *file* listing of a drive's members (a shared drive id is a file id to Drive;
the `drives` resource has no `permissions` method of its own), asked with
`useDomainAdminAccess=true` so an administrator can read the members of a drive
it does not belong to.

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
| Per-file permissions of a shared drive | `/files/{id}/permissions` answers the drive level (what `shared_drive_members` reports); the ACLs *inside* a drive need one call per file, deferred until a report can stream them |

The Microsoft side defers its own set (teams **channels**, mailbox settings,
per-user sign-in activity — teams, team membership and Planner plans are
collected since this release), and both lists arrive the same way: a permission
row first, a section second, with `reconnect` for a grant that predates it.

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
  Every failed section is logged the moment the pass closes
  (`report section failed`, with the report id, the account, the section and the
  reason the API gave), the closing `directory report finished` line carries
  `failed_sections`, the run's `message` names them
  (`completed with 3 unusable section(s): licenses, …`) and the screen shows that
  message under the status badge, marks the failed tab of the report detail with a
  warning and offers the API's reason as its tooltip. The container log is
  therefore enough to explain a `partial` row without opening the report, and the
  report is enough to explain it without the log.
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
| Microsoft collector (paged Graph reads, Teams and Planner walks, derived sections) | `internal/providers/microsoft/report.go` |
| Google collector (Admin SDK Directory, License Manager, shared drives and their permissions) | `internal/providers/google/report.go` |
| Google scopes the collector relies on | `internal/providers/google/permissions.go` |
| Models (`directory_reports`, `report_sections`, `report_schedules`) | `internal/models/report.go` |
| Service (lifecycle, schedules, pruning) | `internal/services/report.go` |
| Endpoints | `internal/handlers/reports.go`, `internal/handlers/routes.go` |
| Screen | `web/src/views/ReportsView.vue` |
| Tests | `internal/providers/report_test.go`, `internal/providers/microsoft/report_test.go`, `internal/providers/google/report_test.go`, `internal/services/report_test.go` |

The screen needs no provider-specific code: it renders whatever
`GET /api/reports/sections` answers and translates `reports.section_<name>` from
the i18n catalogs, so a new section is a label and a collector, nothing else.

