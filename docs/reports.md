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
| `users` | `GET /users` (paged), with optional `signInActivity`; `account_type` classifies member, guest, shared mailbox, room and equipment from `mailboxSettings.userPurpose`. Last login falls back to the unified audit log (`UserLoggedIn`, 7 days) when the tenant has no Entra ID P1/P2 | `ceil(users / 999)` + one `GET /users/{id}/mailboxSettings` per user |
| `shared_mailboxes` | derived from the users page (`mailboxSettings.userPurpose == shared`) | 0 (the purpose read is shared with `users`) |
| `guests` | legacy section, hidden from new reports: guests are classified by `account_type` in `users`; old reports keep their guests tab | 0 |
| `groups` | `GET /groups` (paged) | `ceil(groups / 999)` |
| `distribution_lists` | derived from the groups pages (mail-enabled, not security, no Teams) | 0 |
| `sites` | `GET /sites?search=*` (paged) | `ceil(sites / 200)` |
| `licenses` | `GET /subscribedSkus` | 1 |
| `domains` | `GET /domains` | 1 |
| `teams` | `GET /teams` (paged, 999) | `ceil(teams / 999)` |
| `team_members` | `GET /teams/{id}/members` per team (projected, then plain when the tenant refuses the projection) | one walk per team |
| `planner_plans` | `GET /groups/{id}/planner/plans` per Microsoft 365 group | one walk per Microsoft 365 group |

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
| `roles` | `GET /customer/my_customer/roles` (paged, 100 — the endpoint refuses more) | `ceil(roles / 100)` |
| `role_assignments` | `GET /customer/my_customer/roleassignments` (paged, 200) | `ceil(assignments / 200)` |
| `licenses` | `GET licensing/v1/product/{id}/users` per unique product of the SKU catalogue (20 today, Cloud Identity Free and Premium included), scoped to the primary domain (then the customer id, then `my_customer`); when every value is refused, `POST licensing/v1/batch` of `product/{product}/sku/{sku}/user/{email}` per user of the directory (one batch of the whole SKU catalogue per user) | 20 (+1), or `+1` per user when the catalogue is refused |
| `shared_drives` | `GET drive/v3/drives` (the listing the folder picker uses) | `ceil(drives / 100)` |
| `shared_drive_members` | `GET /files/{id}/permissions` per shared drive (`useDomainAdminAccess`, paged, 100) | one walk per shared drive |
| `analytics` | `GET analyticsadmin/v1beta/accounts` (paged, 200), then `GET analyticsadmin/v1beta/properties?filter=parent:{account}` per account (paged, 200) | `1 + accounts` walks |
| `youtube` | `GET youtube/v3/channels?mine=true` (paged, 50) | 1 |

The derived sections are the point of the design: the n8n flows asked Graph for
`/users/{id}/licenseDetails` once per user and re-listed the Google directory with
`isSuspended=true` and once per group for its members. Here the users page feeds
both `users` and `guests`/`suspended_users`, the groups page feeds both `groups`
and the walk of `group_members`, and the licence names of the users section come
from the catalogue the `licenses` section already reads.

`users` now keeps every account and classifies it in the `account_type` column:
`User`, `Guest`, `Shared mailbox`, `Room` or `Equipment` (and `Mailbox` for a
purpose Graph reports but the list above does not name). The classification comes
from `userType` and `mailboxSettings.userPurpose`; the purpose is one
`/users/{id}/mailboxSettings` call per user and needs the **application**
permission `MailboxSettings.Read` (app-only token, one consent per tenant). A
tenant without it still renders the users table, but shared mailboxes and
resources show as `User` and the `shared_mailboxes` section reports the missing
permission. The old `guests` section is hidden from new reports; guests are part of
`users` now, and a report collected before the merge keeps its guests tab.
`groups` keeps every group (the `kind`/`direct_members` column classifies it), so
a report that skips the derived section still lists them.

**Last login.** The `users`, `shared_mailboxes` and legacy `guests` tables carry a `last_login` column.
Graph's `signInActivity.lastSuccessfulSignInDateTime` is the precise source, but it
requires Microsoft Entra ID P1/P2: Microsoft 365 Business Standard (Entra ID Free)
refuses the property, and the report drops it without losing the users table. On
that tenant the collector falls back to the Microsoft 365 unified audit log, whose
`Audit.AzureActiveDirectory` content type records `UserLoggedIn` events, read with
the app-only `ActivityFeed.Read` permission the Audit Logs screen already uses.
The activity API serves the last **seven days**, so a user who has not signed in
during that window keeps an empty cell rather than a guessed date. The fallback is
best effort: a tenant without the audit subscription still gets the users table,
with the last-login column empty.

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

**`analytics` and `youtube` describe the connection, not the domain.** This is the
one place where a Google section is narrower than the section vocabulary suggests,
and the report says so instead of pretending otherwise:

* `analytics` lists the GA4 **properties** the granted identity can reach — the
  accounts of `analyticsadmin.googleapis.com/v1beta/accounts`, then the properties
  of each of them — with the account that holds the property, its type, industry
  category, time zone, currency and the dates Analytics recorded. Reaching the
  properties of the *domain* instead would need domain-wide delegation (Sync
  impersonates nobody: the provider holds one grant, of the account that connected)
  or a Google Cloud service account, which is a second credential an installation
  does not have.
* `youtube` lists the channels the granted identity **owns** (`channels.list` with
  `mine=true`). The Data API has no domain-wide channel listing at all: the
  content-owner reports of the YouTube CMS need a partner identity, so a Workspace
  domain's channels are only reachable one connected account at a time.
* Both sections therefore start from the account, and an empty table is a normal
  answer rather than a failure: an Analytics account is created by a person on
  analytics.google.com, and a Workspace identity may own no channel at all.
* Both tables lead with the **email of the connected account** (`account_email`,
  titled `Account`), read once per pass from the OpenID `userinfo` endpoint the
  accounts screen already calls. A table of properties or of channels that does not
  say whose they are is not an inventory, and the domain of a report is the tenant
  while the rows are the connection's. The read is cached for the pass and its
  failure is **not** a section failure: the cell stays empty and the rows stand.
* `analytics` also degrades **per account**, like `shared_drive_members` per drive:
  an Analytics account the grant cannot read (deleted between the two calls, or a
  `403` on its properties) costs its own rows and nothing else, and the first error
  is only surfaced when not one property could be read.
* The account name is validated against `^accounts/[0-9]{1,20}$` before it is
  interpolated into the `filter=parent:{account}` of the property listing, because
  the filter is a small query language of its own: a value the API echoes back must
  never decide where the walk goes.

The two Microsoft walks also degrade per collection, because a per-collection
refusal almost never means the report is broken:

* **`team_members` and the `email` projection.** The listing asks for
  `$select=id,displayName,email,userId,roles`, and some tenants resolve that
  projection against the base `microsoft.graph.conversationMember` type instead of
  the team's own one, answering `400 Could not find a property named 'email'`. The
  section then reads the collection again as the plain listing — which still
  carries `email`, so no column is lost, unlike the `users` licence fallback — and
  remembers the refusal, so the remaining teams are read plainly the first time
  instead of paying a doomed call each. The retry happens only when the projected
  read returned no row at all, so a mid-listing failure is never masked.
* **`planner_plans` and the groups that cannot own a plan.** Only a Microsoft 365
  group (`groupTypes` holds `Unified`) has a Planner workload, so a security group
  or a distribution list is never asked for plans: a directory that is mostly
  security groups would otherwise pay a refused call per group. A group Graph
  answers `403` for (the signed-in user cannot see its plans) or `404` for (it has
  none) is skipped as well, and the section keeps the plans of the groups that did
  answer. It fails only when every Microsoft 365 group refused it, which is the
  shape of a missing `Tasks.Read` grant rather than of an empty tenant.

The two page sizes of the role collections are **not** interchangeable: `roles`
caps `maxResults` at 100 and answers a `400 invalidParameter`
(`Invalid value '200'. Values must be within range [1, 100]`) to anything above
it, while `roleassignments` accepts 200. They are two constants
(`reportRolePageSize`, `reportAssignmentPageSize`) for that reason.

A report that asks for `users` also walks the licence catalogue (one call per
product, eleven of them), because `users.list` carries no licence: that is the
one place where a section pays for a column it does not own. A grant without
`apps.licensing` leaves the column empty instead of failing the table.

The product list is derived from the SKU catalogue of `licenseskus.go` rather
than written out beside it: a licence the per-user fallback can see — Cloud
Identity Free (`101001`) and Premium (`101005`) included — is also asked for by
the domain-wide listing. The two walks cannot drift apart again.

The catalogue is scoped with `customerId`, tried in order of likelihood: the
**primary domain** (`agencia.pub`), the Admin-console id (`C045l8wpy`) and the
`my_customer` alias the rest of a report uses. The License Manager API guide
documents the primary domain; the id is accepted by some domains and refused by
others — a resold tenant answered the id with `403 Unauthorized operation for the
given domain` and the domain with its whole catalogue. Trying the next value costs
one call, and it is what keeps a tenant whose catalogue answers from being read one
batch per user instead (below); 77 licensing queries per user is enough to exhaust
the "Queries per minute per user" quota after a handful of users. The
`my_customer` alias is the last candidate — and the only one a grant that cannot
read the customer record has (one made before
`admin.directory.customer.readonly`); the call it costs the section is the one the
`organization` section already pays for when it is part of the same report.

A `403` from the catalogue is a decision about the *value* it was asked with, not
about the product — that value is not a customer the caller may read (a resold
tenant's Admin-console id is the live example), or the grant predates the scope.
The walk therefore stops at the first refusal instead of repeating the same 403
for every product, tries the next customer value, and only when every value is
refused falls back to the **per-user** read of the same API (below). When that read
finds nothing either, the `licenses` section carries the reason while the `users`
table keeps its rows with an empty `licenses` column.

**The per-user fallback (`probeLicenses`).** The `get` of the Enterprise License
Manager API takes no customer id at all, so it answers for a licence the caller can
see even when the domain-wide listing of that domain is refused. It is the walk GAM
calls `Info_User_Licenses`: the SKU catalogue is probed per user, and the section is
whatever those reads answer. Every lookup is a sub-request of a **batch**
(`POST licensing.googleapis.com/batch`, the `batchPath` of the API), so the whole
catalogue of a user costs one call instead of one call per SKU — a directory of 300
users is 300 calls, not 23 100. What bounds it:

* `reportLicenseProbeUsers` (500 users) and the row cap of the section bound the
  walk; hitting either marks the section `truncated` rather than reporting a
  directory that was only half read. A report that wants every user of a bigger
  directory needs GAM, not a fallback.
* The batch answers `200` when the *batch* was accepted, not when its parts were:
  the status of a part is what decides a row. `200` carries an assignment; `404`
  (and `400`, for a SKU Google retired or the tenant cannot own) means the user
  simply does not hold that SKU and is not an error. Every part is read whatever
  the parts before it answered: a failed part is an answer about *that SKU* — the
  subscription the resold tenant never had, the sub-request a quota happened to hit
  — and the rest of the answer still carries the same user's other assignments.
* What ends the walk is an answer about the *caller*, not about a SKU: a batch whose
  parts were refused **every one of them** (the domain refuses the per-user read as
  well), or a batch the endpoint did not answer at all. A refusal that says "slow
  down" — `429`, or the `403` a quota excess arrives as, told apart by
  `errors[].reason` — is asked again for the next user instead, and a `5xx` too.
  Reading the answer up to the first failed part instead is what reported the
  licences of the first user of a directory and nobody else's.
* A part that says "slow down" is the one failure that answered **nothing**, so it is
  not read as "this user holds no such licence": the batch of that user is asked again,
  up to `reportLicenseProbeAttempts` (3) with a doubling pause (`licenseProbeBackoff`,
  2 s) between attempts, and an assignment a retry repeats is not a second row. A user
  whose parts never answer keeps **no known licence set**: the `licenses` and
  `license count` cells of that user stay *empty* instead of reading `0`, both tables
  are flagged `truncated`, and one `slog.Warn` line (`the per-user licence lookup left
  users unread`, with the number of users and the reason the API gave) says so in the
  log. A cell that says `0` is a user the API answered, and a blank one is a user it
  did not — a report of a throttled tenant that spelled the difference as `0` is what
  claimed 15 users of a paid domain hold no licence.
* A part a domain *refuses* (`403 subscriptionNotFound`, the subscription a resold
  tenant never had) stays what it always was: an answer about that SKU, so the user's
  other assignments are exact and the cells stay filled. It is only reported, by a
  second `slog.Warn` line (`the per-user licence lookup was refused part of the
  catalogue`) carrying the reason — that reason is the one to hand to Google, and it
  is the difference between a licence this API does not show and a caller that was
  simply too fast.
* Parts are **not** correlated by position or `Content-ID`: a part that carries a
  licence carries its own product id, SKU id and user id, so an answer Google
  reorders cannot mislabel a row. A part without a `userId` is attributed to the
  address the lookup was made with.
* The catalogue is the SKU list of `licenseskus.go` (the paid editions, the storage
  add-ons and their add-ons). A SKU Google adds later is not probed until that list
  grows, and a SKU the tenant cannot own costs a part of a batch that was paid for
  anyway. Names come from the answer when it carries one (`skuName`) and from the
  list otherwise.

A report whose licence cells came back short is read with `cmd/reportprobe`
([development.md](development.md) §3): the same collector, against the real
tenant, with the counters of every user in the log and the three readings of the
licence columns in `summary.txt`.


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
time — is the higher privileged delegated scope of the same read. A `Tasks.Read`
grant the tenant nevertheless refuses per group (a group whose plans the signed-in
user cannot see) costs that group's plans and not the section, as §1 describes.

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

**Analytics and YouTube add a scope each.** The `analytics` section reads the
Analytics Admin API and the `youtube` section the YouTube Data API, so the grant
grows by two read-only user scopes: `analytics.readonly` and `youtube.readonly`
(`CapabilityAnalytics`, `CapabilityYouTube`). Both are granted with administrator
consent like the rest, which is exactly why the two sections describe the
connection rather than the domain (§1). Neither API is enabled by default in a
Cloud project, so the guide links their enablement pages too
(`analyticsadmin.googleapis.com`, `youtube.googleapis.com`).

That one customer scope is additive and forgiving:

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
| YouTube CMS (content owner reports) | needs a content owner identity, not a Workspace account; the `youtube` section reports the channels the connection owns instead (§1) |
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
  survive. A section the **database** refuses (a value it cannot store, a
  constraint it enforces) is reported exactly the same way: each section is stored
  with its own INSERT, so the refusal costs that one table and not the pass. The
  failed write is logged with the driver's error and the section (`storing a report
  section failed`, with the report id, the section, the row count and the size of
  the document), the closing line names it in `failed_sections` and the run's
  `message` counts it. The pass itself only fails on a cancelled or timed-out
  context, or when not one section could be stored at all.
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
  A section's JSON document is capped at 8 MiB as well
  (`services.reportSectionBytes`): the row cap is a budget of rows and not of
  bytes, so 20 000 team memberships are a few megabytes while 20 000 domain rows
  are a few hundred kilobytes. A document over the cap keeps the first rows that
  fit — the walk order, so the table still starts where the directory does — and
  the section is flagged `truncated` like one that hit the row cap, never dropped.
* **Degradation, not failure.** A Google grant that predates the customer scope
  still produces an `organization` row (derived from the domain list), and a
  grant without `apps.licensing` still produces the `users` table with an empty
  `licenses` column. Losing one column is not worth losing the table. The same
  holds for a domain the Enterprise License Manager API refuses outright
  (`403 Unauthorized operation for the given domain`, which is the answer of a
  subscription no paid edition backs): the section falls back to the per-user batch
  read (§1), reports the refusal only when that read answers nothing either, stops
  the catalogue walk there instead of paying ten more calls, and every other table
  is unaffected.
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
| Google collector (Admin SDK Directory, License Manager, shared drives and their permissions, Analytics Admin API, YouTube Data API) | `internal/providers/google/report.go` |
| Google scopes the collector relies on | `internal/providers/google/permissions.go` |
| Models (`directory_reports`, `report_sections`, `report_schedules`) | `internal/models/report.go` |
| Service (lifecycle, schedules, pruning) | `internal/services/report.go` |
| Endpoints | `internal/handlers/reports.go`, `internal/handlers/routes.go` |
| Screen | `web/src/views/ReportsView.vue` |
| Tests | `internal/providers/report_test.go`, `internal/providers/microsoft/report_test.go`, `internal/providers/google/report_test.go`, `internal/services/report_test.go` |

The screen needs no provider-specific code: it renders whatever
`GET /api/reports/sections` answers and translates `reports.section_<name>` from
the i18n catalogs, so a new section is a label and a collector, nothing else.

