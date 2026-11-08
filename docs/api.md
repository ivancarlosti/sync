# API reference

Base path `/api`. Requests and answers are JSON (`Content-Type:
application/json`), except the SPA assets. Authentication is the session cookie
(see [authentication.md](authentication.md)); the two OAuth callbacks are public
and protected by the signed `state` parameter instead.

## 1. Conventions

* **Error shape** — every failure answers the same object:

  ```json
  { "code": "validation", "error": "the request body is not valid JSON: …" }
  ```

  `code` is the machine-readable value the SPA translates, `error` is an English
  message shown as technical detail. Server-side failures never expose internals:
  they answer `{"code":"internal","error":"unexpected error, check the server logs"}`
  and the real error goes to the process log.

* **Codes and statuses** (`internal/handlers/respond.go`):

  | Code | HTTP | Meaning |
  |---|---|---|
  | `validation` | 400 | malformed body, bad id, unknown provider, rejected value (including a remote identifier a provider refuses) |
  | `unauthorized` | 401 | no/expired session, invalid credentials, redeemed OAuth link |
  | `forbidden` | 403 | throttled login, captcha refused, account not on the allow-list |
  | `not_found` | 404 | unknown id, unknown endpoint under `/api` |
  | `busy` | 409 | a run for this job, or an audit for this account, is already in flight |
  | `rate_limited` | 429 | the provider throttled the call and the automatic retries did not clear it (wait and try again) |
  | `reconnect` | 412 | the provider token cannot be used any more (reconnect the account) |
  | `consent_required` | 412 | the provider permissions were refused or the tenant-wide admin consent is missing: the setup guide is the fix, not a retry |
  | `not_configured` | 424 | missing provider client, or a build without the SPA |
  | `delivery_failed` | 502 | the notification target refused the test message |
  | `internal` | 500 | unexpected error (details only in the log) |
  | — | 504 | a provider call exceeded its deadline |

* **IDs** — path parameters are unsigned integers; `0` or a non-numeric value is
  a `400`. Optional numeric query parameters (`job_id`) are validated the same
  way, optional integers (`days`, `limit`) fall back to their default instead.
* **Remote identifiers** — a provider id sent onwards (the `:drive` path
  parameter, `folder_id`, a job's stored folder/drive) must look like a token
  that provider hands out. Anything else is answered `400`/`validation` and no
  request leaves the server (see [providers.md](providers.md) §1). The same
  answer covers the second guard, on the request URL itself: if an id ever
  reaches a URL without being narrowed, the provider refuses to send it instead
  of failing with a `500`.
* **Lists are bounded** — `limit` is clamped (`runs` and `audits` 50/200, run
  items 500/1000, audit entries 10000, items 100/1000); `GET /api/runs` supports
  `job_id` and `status` filters, the status filter applies to the returned page.
* **Secrets** — no endpoint ever returns a token, a client secret, a channel
  secret (masked as `********`) or a SHA/HMAC. `/api/settings/raw` hides every
  `provider.*` key for the same reason.

## 2. Public endpoints

| Method | Path | Answer |
|---|---|---|
| `GET` | `/api/health` | `{status, database, version, auth_mode, running_jobs}`; `503` with `status: degraded` when the database ping fails |
| `GET` | `/api/version` | `{info:{name,version,commit,go_version}, auth_mode, providers[], locales[], themes[]}` |
| `GET` | `/api/auth/session` | session view: `{mode, authenticated, open, captcha:{enabled,site_key}, keycloak:{enabled}, operator, defaults, version}` |
| `POST` | `/api/auth/login` | `{login, password, captcha_token?, redirect_to?}` → session view + `Set-Cookie: sync_session=…` |
| `POST` | `/api/auth/logout` | `204`, expires the cookie |
| `GET` | `/api/auth/keycloak?redirect_to=` | `{url, state}` — where the SPA must navigate |
| `GET` | `/api/auth/callback?code=&state=` | `302 /` (success) or `302 /login?auth_error=…&auth_detail=…` |
| `GET` | `/api/oauth/:provider/callback?code=&state=` | `302 /accounts?connected=<provider>&detail=…` — or `302 /accounts?connect_error=<code>&provider=<provider>&detail=…`; a tenant-wide consent comes back here too (`admin_consent=True` → `302 /admin/guide/<provider>?consent=granted`) |

## 3. Sessions and settings

| Method | Path | Payload / answer |
|---|---|---|
| `GET` | `/api/stats?days=30` | `{days, since, stats:{accounts,jobs,enabled_jobs,runs,succeeded,partial,failed,cancelled,timed_out,conflicts,files_created,files_updated,files_deleted,bytes_transferred}, running_jobs, running_ids}` — `days=0` means *since the beginning* |
| `GET` | `/api/settings` | `{settings:{default_locale,default_theme,sync_default_interval_minutes,sync_run_timeout_minutes,billing_schema,billing_schema_custom,billing_rate_in_per_gib,billing_rate_out_per_gib,billing_currency,purge_enabled,purge_jobs_days,purge_audits_days,purge_billing_days,purge_auditlogs_days,auditlogs_enabled,auditlogs_interval_minutes,auditlogs_request_delay_seconds}, locales[], themes[], billing_schemas[], billing_schema_default}` |
| `PUT` | `/api/settings` | same `settings` object as body (`locale` ∈ the 7 codes, `theme` ∈ light/dark/system, interval `0–10080`, timeout `1–1440`, `billing_schema` ∈ `none`/`oci`/`aws`/`gcp`/`custom`, rate `0–100000` per GiB, currency a three letter code, `billing_schema_custom` a JSON document, retention `1–3650` days each, `auditlogs_interval_minutes` `1–1440`, `auditlogs_request_delay_seconds` `0–300`) → `{settings}` |
| `GET` | `/api/settings/raw` | `{values:{key:value}}` — every stored key except `provider.*` |

`billing_schemas` is the list the Admin > Settings form offers:
`{id, label, hint?, inbound_label, outbound_label, operations_label, cost_label,
unit, currency?, rate_per_gib, custom}`. The `custom` document accepts
`{label, inbound_label, outbound_label, operations_label, cost_label, unit}`;
every omitted field keeps the default. The pricing is the report's only cost
input: see §9 and [database.md](database.md) §8 for the queries behind it.

## 4. Providers

| Method | Path | Payload / answer |
|---|---|---|
| `GET` | `/api/providers` | `{providers:[ProviderCredentialsInfo], redirect_hint:{google,microsoft,note}}` |
| `GET` | `/api/providers/:provider` | one `ProviderCredentialsInfo` |
| `GET` | `/api/providers/:provider/guide` | the guided app registration: `{provider, configured, redirect_uri, console_urls, permissions[], scopes[], capabilities[], admin_consent_required, admin_consent, steps[], warnings[]}` (see [app-registration.md](app-registration.md)) |
| `PUT` | `/api/providers/:provider` | `{client_id, client_secret, redirect_uri, tenant_id}` (empty `client_secret` keeps the stored one) → refreshed info |
| `DELETE` | `/api/providers/:provider` | clears the override → refreshed info (the recorded `admin_consent` of the app registration is kept) |

`ProviderCredentialsInfo` = `{provider, client_id, secret_set, redirect_uri,
 tenant_id?, source: environment|database|none, configured,
 admin_consent?: {tenant, client_id, at, granted}}` — `admin_consent` is present
once a tenant-wide consent was recorded (Microsoft) and `granted` reflects the
client id configured right now.

## 5. Accounts

| Method | Path | Payload / answer |
|---|---|---|
| `GET` | `/api/oauth` | `{providers:[ProviderCredentialsInfo]}` — every compiled provider with the state of its OAuth client; the Connect screen offers the ones with `configured: true` |
| `POST` | `/api/oauth/:provider/start` | `{redirect_to?}` → `{url, state}` |
| `POST` | `/api/oauth/:provider/admin-consent` | `{redirect_to?}` → `{url, state}` — the tenant-wide consent URL (Microsoft only, `400` elsewhere) |
| `GET` | `/api/accounts` | `{accounts:[accountView]}` (tokens never included) |

`accountView` carries the derived permissions of the stored grant:
`capabilities[]` (satisfied by `scopes`), `missing_capabilities[]` (requested by
this release, not granted yet) and `needs_reconnect` (true when the second list is
not empty). `account_kind` is `personal` or `work` when the provider could tell the
two apart — Microsoft reads the tenant claim of the *stored* access token, so an
account connected before the field existed is classified without a Graph call —
and is omitted when no signal could classify it. `org_id` is the organisation the
account belongs to (the Entra tenant id, the Workspace hosted domain): it is read
from the column the connection wrote, or from the tenant claim of the stored access
token of an account connected before that column existed — the same fallback the
activity collector applies — and is omitted when neither names one (a personal
account, whose organisation has no administrator feed). The third list,
`not_applicable_capabilities[]`, holds the capabilities the account can never be
granted: on a personal Microsoft account (an MSA) the directory permissions are
outside what the account can hold, so they are neither satisfied nor missing, and
`needs_reconnect` ignores them — reconnecting could not deliver them. The
vocabulary itself is scoped per provider (`providers.CapabilitiesFor`): `teams` and
`planner` are Microsoft only, so a Workspace account is never offered them and they
never appear in its `missing_capabilities`. `status` is
`connected` while the stored token works and `error` once a
refresh was refused (with `last_error` holding the reason). The Accounts screen
offers the same reconnect action for a missing capability and for an `error`
status: reconnecting rotates the tokens of the existing row, so the jobs and their
history are kept (only `DELETE /api/accounts/:id` deletes them).
| `POST` | `/api/accounts/:id/verify` | validates the token against the provider → updated `accountView` |
| `DELETE` | `/api/accounts/:id` | `{id, deleted_jobs}` — the jobs using the account are deleted too |
| `GET` | `/api/accounts/:id/drives` | `{drives:[{id,name,kind,owner}]}` — the account's own drive first (the one `GET /me/drive` names), then the roots shared with it. A Microsoft personal account (MSA) also exposes internal bookkeeping drives in `/me/drives` for which Graph refuses the root (`400 ObjectHandle is Invalid`), so those are dropped |
| `GET` | `/api/accounts/:id/drives/:drive/items?folder_id=` | `{drive_id, folder_id, items:[{id,name,is_dir,size,modified_at,mime_type,hash,unsupported}]}` — folders first, then files, both alphabetical; `folder_id` defaults to the drive root |
| `GET` | `/api/accounts/:id/sites?q=` | `{sites:[{id,name,kind,owner}]}` — SharePoint libraries (empty list for providers without the capability). `q` is a keyword; omitted (or empty) it lists every site the account can reach, and a value the server cannot search falls back to the tenant root site. A Microsoft **personal** account owns no site collection and Graph refuses `/sites` for it, so the answer is an empty list instead of an error |
| `POST` | `/api/accounts/:id/sites/resolve` | body `{"url": "https://tenant.sharepoint.com/sites/marketing"}` → `{sites:[{id,name,kind,owner}]}` — the default library of that site, the manual fallback of the folder picker. `400` when the value is not a SharePoint URL/site id, `404` when the account cannot reach it, `424` for a provider without the capability |

## 6. Jobs

`jobView` = every `sync_jobs` column (`id, name, source_account_id,
destination_account_id, source_drive_id, source_drive_name, source_drive_kind,
source_drive_owner, source_folder_id, source_folder_path, destination_drive_id,
destination_drive_name, destination_drive_kind, destination_drive_owner,
destination_folder_id, destination_folder_path, direction, conflict_policy,
delete_missing, sync_empty_folders, interval_minutes, enabled, last_run_at,
next_run_at, last_status, last_error, created_at, updated_at`) plus
`exclude_patterns: string[]` and `running: boolean`.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/jobs` | `{jobs:[jobView]}` |
| `POST` | `/api/jobs` | body below → **201** `jobView` |
| `GET` | `/api/jobs/:id` | `jobView` |
| `PUT` | `/api/jobs/:id` | replaces the job as a whole (the editor always sends the full form), recomputes `next_run_at` |
| `DELETE` | `/api/jobs/:id` | cancels a run in flight → `{id, cancelled}` |
| `POST` | `/api/jobs/:id/run` | **202** `{job_id, run}` — `409 busy` while another run of the job is in flight |
| `POST` | `/api/jobs/:id/cancel` | `{job_id, cancelled}` — idempotent |
| `GET` | `/api/jobs/:id/schedule` | `{next_run_at, scheduled, preview:[5 timestamps]}` |

Create/update payload:

```json
{
  "name": "Nightly Google → OneDrive",
  "source_account_id": 1,
  "destination_account_id": 2,
  "source_drive_id": "root",
  "source_drive_name": "My Drive",
  "source_drive_kind": "personal",
  "source_drive_owner": "",
  "source_folder_id": "1AbCdEf",
  "source_folder_path": "My Drive / Photos",
  "destination_drive_id": "b!xyz",
  "destination_drive_name": "Documents",
  "destination_drive_kind": "document_library",
  "destination_drive_owner": "https://contoso.sharepoint.com/sites/marketing/Shared%20Documents",
  "destination_folder_id": "01ABCD",
  "destination_folder_path": "OneDrive / Backup",
  "direction": "google_to_microsoft",
  "conflict_policy": "newest_wins",
  "exclude_patterns": ["*.tmp", "cache/**"],
  "delete_missing": false,
  "sync_empty_folders": true,
  "interval_minutes": 1440,
  "enabled": true
}
```

Validation performed by the service: name required, both accounts must exist,
folders must be selected, `direction` ∈
`google_to_microsoft|microsoft_to_google|bidirectional`, `conflict_policy` ∈
`newest_wins|source_wins|destination_wins|skip`, `interval_minutes` `0–10080`,
every glob must compile (`path.Match`), blanks and duplicates are dropped.

Both ends may name the **same account** (OneDrive → OneDrive, OneDrive →
SharePoint of one work account); only the two locations are checked:
`400 validation` when they collapse to the same drive *and* folder, and when one
folder sits inside the other in the direction the job copies (which would copy the
tree into itself on every run). The nesting check compares the display
`*_folder_path` values, so a job whose paths the editor left empty is accepted and
only ever caught by the identical-location rule.

`*_drive_name`, `*_drive_kind` and `*_drive_owner` are the display parts of each
root, exactly as `GET /api/accounts/:id/drives` returned them
(`kind` ∈ `personal` \| `business` \| `shared` \| `document_library`, and `owner`
carries the drive URL). The API stores them next to the ids so the job list names
the location — My Drive, a shared drive, an OneDrive or a SharePoint library —
without a provider call. They are **optional**: a payload that omits them leaves
the columns empty, and a job stored before they existed keeps rendering as its
path alone. Values are clamped to the column widths (`191`/`32`/`191`).

`delete_missing` propagates a deletion instead of re-copying the file back, and
also deletes, on the side the job writes to, a folder the other side no longer
has once nothing is left under it (`folder_deleted` in the run items).

`sync_empty_folders` mirrors the folder tree itself, so a folder that holds no
file is created on the other side too (`folder_created`). It is on by default and
**optional** in the payload: a value that is omitted leaves the stored setting of
an update alone — an older client keeps working — and starts a new job with
`true`.

## 7. Runs

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/runs?job_id=&status=&limit=` | `{runs:[runView]}` (`limit` default 50, max 200) |
| `GET` | `/api/runs/:id` | `{run, items:[runItemView], traffic:[trafficEntry]}` (max 1000 items) |
| `GET` | `/api/runs/:id/items?limit=` | `{items, limit}` (default 100, max 1000) |

`runView` = the `sync_runs` row plus `running`; `runItemView` =
`{id, action, path, size, evidence?, created_at}`; `trafficEntry` is the traffic
one run moved through one account (see §9).

## 8. Audits

A content audit lists a provider folder tree to a chosen depth and stores the
resulting report. It is an **async job** like a sync run: the start answers
**202** and the report is persisted, so reading it (or exporting it) never
scans the provider again.

| Method | Path | Notes |
|---|---|---|
| `POST` | `/api/audits` | body below → **202** `{audit}`; **409 busy** while another audit of the same account is in flight |
| `GET` | `/api/audits?limit=` | `{audits:[auditView]}` (`limit` default 50, max 200, newest first) |
| `GET` | `/api/audits/:id` | `{audit, entries:[auditEntryView]}` (entries in DFS preorder, max 10000) |
| `GET` | `/api/audits/:id/export` | `text/csv; charset=utf-8` download (`Content-Disposition: attachment; filename="audit-<id>.csv"`) |
| `POST` | `/api/audits/:id/cancel` | `{id, cancelled}` — idempotent |

```json
{
  "account_id": 1,
  "drive_id": "b!xyz",
  "drive_name": "Documents",
  "drive_kind": "document_library",
  "drive_owner": "https://contoso.sharepoint.com/sites/marketing/Shared%20Documents",
  "folder_id": "01ABCD",
  "folder_path": "SharePoint / Marketing",
  "depth": 2
}
```

`drive_name`, `drive_kind` and `drive_owner` are optional display parts of the
root, as in §6: the run stores them so the history screen names the location
(My Drive, a shared drive, an OneDrive, a SharePoint library) without a provider
call. A run stored before the columns existed keeps rendering as its path alone.

`auditView` = the `audit_runs` row (`id, account_id, account_email, provider,
drive_id, drive_name, drive_kind, drive_owner, root_folder_id, root_path,
max_depth, status, trigger, started_at, finished_at, duration_ms, files, folders,
total_size, max_depth_reached, truncated, message, created_at`) plus `running`.
`auditEntryView` = `{id, kind
(folder|file), path, name, depth, size, total_size, files, folders, expanded,
listed, modified_at?, mime_type?}`.

Semantics:

* the **selected folder is depth 0**; `max_depth = 0` means **unlimited**, and
  the requested depth is clamped to `25`;
* the depth bounds the **report, not the walk**: a folder at
  `depth >= max_depth` is listed with the exact totals of its subtree (the walk
  descends into it and counts everything below) and says so through
  `listed:false` — only its children are missing from the report;
* `expanded` is false only for a folder the walk could not descend into (it was
  removed mid-audit, or a budget was hit first);
* `truncated` is set when a budget stopped the walk: the entry cap (`10000`
  entries) or the walk cap (`200000` visited nodes). A truncated report is the
  only incomplete one, and `max_depth_reached` can exceed the requested depth;
* `path` is relative to the audited root (empty for the root itself); a folder
  carries the aggregate of its subtree.

The CSV columns are
`kind,path,name,depth,size,total_size,files,folders,expanded,listed,modified_at,
mime_type`, prefixed with a UTF-8 BOM so a spreadsheet opens accented paths
correctly.

### 8.1 Activity logs

These are **not** the content audits above: they are the stored activity the
providers report (see [audit-logs.md](audit-logs.md)). The rows come from the
collector's local copy, so the list is one table over both clouds and survives
the retention of the vendors.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/audit-logs` | `{events:[auditLogView], total, limit, offset, cursors:[auditLogCursor], schedule:auditLogSchedule}` |
| `GET` | `/api/audit-logs/export` | `text/csv; charset=utf-8` download, same filters, capped at 5000 rows (`X-Total-Count` carries the filtered total) |
| `GET` | `/api/audit-logs/cursors` | `{cursors:[auditLogCursor], schedule:auditLogSchedule}` — the position and health of every feed |

Query parameters of the list and the export (every one optional):

| Parameter | Meaning |
|---|---|
| `provider` | `microsoft` \| `google` |
| `org_id` | the tenant (Microsoft) or Workspace domain (Google) |
| `action` | `create` \| `delete` \| `update` \| `move` \| `share` \| `login` \| `other` |
| `target` | `file` \| `folder` \| `user` \| `group` \| `device` \| `application` \| `site` \| `drive` \| `other` |
| `actor` | exact actor address, or the address of the person a workload acted for |
| `search` | substring of the actor address, the actor name, the person a workload acted for, or the object name |
| `from`, `to` | RFC3339 or `YYYY-MM-DD`; a date-only `to` means the end of that day |
| `limit`, `offset` | pagination of the list (`limit` default 50, max 500) |

An unknown value is a **400** (`validation`), never a silently empty page.

`auditLogView` = `{id, provider, source, remote_id, org_id, action, target,
actor_kind, actor_email, actor_name?, actor_id?, actor_on_behalf_of?,
actor_on_behalf_of_name?, target_name?, target_id?, drive_id?, site_id?,
occurred_at}`. `source` is the feed the record came from (`directory`, `unified`,
`drive`, `user_accounts`, `groups`, `admin`, `login`). `actor_kind` is `user`
(a person: `actor_email` is set), `application` (a workload: `actor_name` only,
`actor_email` empty) or `system` (no principal: `actor_name` is the service that
logged the event); `actor_on_behalf_of` names the person a workload acted for when
it is known (see [audit-logs.md](audit-logs.md) §3). The vendor document is stored
but **never serialised**.

`auditLogCursor` = `{provider, org_id, stream, cursor?, last_event_at?,
last_run_at?, healthy, needs_consent, last_error?}`: `healthy` is false when the
last pass failed, and `needs_consent` marks the one failure an operator can fix
with a grant.

`auditLogSchedule` = `{enabled, interval_minutes, request_delay_seconds,
last_run_at?, next_run_at?}` — the collector as configured in Admin > Settings
(§3): `enabled` is the `auditlogs_enabled` switch, and `next_run_at` is absent
while it is off, so the screen can say why the list stopped growing.
`last_run_at`/`next_run_at` describe the pass of the running process, not the
database: a restart polls immediately and reports from there.

### 8.2 Directory reports

A directory report is the read-only inventory of an organisation (see
[reports.md](reports.md)). It is async like an audit: the start answers **202**
and every section is persisted, so reading or exporting it never touches the
provider again.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/reports/sections?account_id=` | `{sections:[name], supported}` — what the account's provider can collect |
| `POST` | `/api/reports` | body below → **202** `{report}`; **409 busy** while another report of the same account runs |
| `GET` | `/api/reports?limit=` | `{reports:[reportView]}` (`limit` 50, max 200, newest first) |
| `GET` | `/api/reports/:id` | `{report, sections:[reportSectionView]}` — the table of contents, without the rows |
| `GET` | `/api/reports/:id/sections/:section?offset=&limit=` | `{section, columns, rows, total, offset, limit, truncated, error?}` — one page of one table |
| `GET` | `/api/reports/:id/export` | `application/zip` download of one CSV per section (`report-<id>.zip`) |
| `POST` | `/api/reports/:id/cancel` | `{id, cancelled}` — idempotent |
| `GET` | `/api/reports/schedules` | `{schedules:[reportScheduleView]}` |
| `PUT` | `/api/reports/schedules/:account_id` | body below → `{schedule}` |
| `DELETE` | `/api/reports/schedules/:account_id` | `{account_id, deleted}` — idempotent |

```json
{ "account_id": 1, "sections": ["users", "groups", "licenses"] }
```

```json
{ "enabled": true, "interval_minutes": 1440, "sections": [] }
```

`sections` is the closed vocabulary `organization`, `users`, `guests`, `groups`,
`distribution_lists`, `sites`, `licenses`, `domains`, `suspended_users`,
`group_members`, `org_units`, `roles`, `role_assignments`, `shared_drives`,
`teams`, `team_members`, `planner_plans`, `shared_drive_members`, `analytics`,
`youtube`; **an
empty list means every section the provider supports**, and an unknown name is a
**400**. A section the provider cannot collect is dropped from the run (the
answered `section_list` is what was actually requested), and `GET
/api/reports/sections` is what tells the UI which half of the vocabulary the
account's provider owns — `guests`, `distribution_lists`, `sites`, `teams`,
`team_members` and `planner_plans` belong to Microsoft, `suspended_users`,
`group_members`, `org_units`, `roles`, `role_assignments`, `shared_drives`,
`shared_drive_members`, `analytics` and `youtube` to Google (see
[reports.md §1](reports.md#1-what-a-report-contains)).

`reportView` = the `directory_reports` row (`id, account_id, account_email,
provider, org_id, sections, status, trigger, started_at, finished_at, duration_ms,
section_count, row_count, failed, truncated, message, created_at`) plus `running`
and `section_list` (the stored comma separated list as an array).
`reportSectionView` = `{section, columns:[{key,title}], row_count, truncated,
error?}`.

`status` is the lifecycle of the pass (`running | success | partial | failed |
cancelled | timeout`): **partial** means at least one section carries an `error` (a refused
endpoint, a personal account without `/sites`). Column keys and titles are
stable and English — a report is a data export, so the CSV header does not
follow the UI locale.

`reportScheduleView` = the `report_schedules` row (`id, account_id, enabled,
interval_minutes, sections, next_run_at?, last_run_at?, last_report_id?,
last_error?, created_at, updated_at`) plus `section_list`. `interval_minutes` is
clamped to `15 … 43200` (a value outside is a **400**) and defaults to `1440`; a
disabled schedule has no `next_run_at`.

### 8.3 Admin operations

Provider-backed administrative tasks (see [management.md](management.md) for the
full operation catalogue and the per-provider scope prerequisites).

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/api/management/operations?account_id=` | The operations the account can run; `account_id` is required (**400** without it). Each entry carries `supported` and `missing_capability`. |
| `GET` | `/api/management/tasks?account_id=&limit=` | History, newest first; `account_id` optional, `limit` clamped to `200`. |
| `POST` | `/api/management/tasks` | `{account_id, kind, params}` → **202** `{task}`; **400** for an unknown kind, a missing required parameter or an unconfirmed destructive operation; **409** while another task of the account runs; **412** when the grant does not cover the capability. |
| `GET` | `/api/management/tasks/:id` | `{task}` with `steps` (the journal). |
| `POST` | `/api/management/tasks/:id/cancel` | `{id, cancelled}`. |
| `GET` | `/api/management/tasks/:id/export` | The journal as `text/csv` (UTF-8 BOM). |

`managementTaskView` = the `management_tasks` row (`id, account_id,
account_email, provider, kind, title, status, trigger, params, result?, step_count,
failed, items_processed, bytes_moved, started_at, finished_at?, duration_ms,
message?, created_at`) plus `running` and `param_map` (the stored `params` as an
object). Destructive operations require `params.confirm = "true"`; the server
enforces it.

## 9. Notifications

| Method | Path | Payload / answer |
|---|---|---|
| `GET` | `/api/notifications` | `{channels:[channel], kinds:[{kind,label,fields:[{key,label,type,required?,secret?,hint?,options?,default?}]}], events:[…]}` |
| `POST` | `/api/notifications` | body below → **201** `channel` |
| `GET` | `/api/notifications/:id` | `channel` |
| `PUT` | `/api/notifications/:id` | body below → `channel` |
| `PUT` | `/api/notifications/:id/enabled` | `{"enabled": true}` → `channel` |
| `POST` | `/api/notifications/:id/test` | delivers a test message → `{id, delivered: true}` or `502 delivery_failed` |
| `DELETE` | `/api/notifications/:id` | `204 No Content` |

```json
{
  "name": "Ops e-mail",
  "type": "smtp",
  "config": { "host": "smtp.example.com", "port": 587, "from": "sync@example.com",
              "to": "ops@example.com", "password": "********" },
  "events": ["sync.success", "sync.run_failed"],
  "enabled": true
}
```

Field schemas, secrets and delivery semantics: [notifications.md](notifications.md).

## 9. Billing and API tokens

### Billing

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/billing?from=&to=&group_by=&account_id=&job_id=&limit=` | `{group_by, from, to, schema, rows:[billingRow], total}` |
| `GET` | `/api/billing/runs?from=&to=&account_id=&job_id=&limit=` | `{from, to, traffic:[trafficEntry]}` |
| `GET` | `/api/billing/export?...` | CSV of the report (same filters) |

* Dates are `YYYY-MM-DD` UTC days, both inclusive; **the default window is the
  last 90 days** and the default `limit` is 90 rows (1000 for the report, 500 for
  the per-run breakdown, 2000 maximum).
* `group_by` is `day` (default), `month`, `account` or `run`; anything else is a
  `validation` 400.
* `billingRow` = `{day?, month?, account_id?, account_email?, run_id?, job_id?,
  inbound, outbound, total}` where a counter is `{bytes, files, operations,
  runs}`. Only the fields of the requested grouping are set.
* `trafficEntry` = one `(run, account, direction)` row:
  `{id, run_id, job_id, account_id, account_email, provider, direction, bytes,
  files, operations, day, month, started_at}`. `direction` is `inbound` (read
  from the source account) or `outbound` (written to the destination);
  `operations` counts the payload-free write calls (deletions, folder creations).
* `schema` is the active billing vocabulary (§3): the report is **labelled** with
  it while the quantities stay bytes. The CSV applies it: `inbound`/`outbound`/
  `total` are rendered in its unit, plus a `cost` column when a rate is
  configured.

```text
kind: text/csv; charset=utf-8   filename: billing-<group_by>-<from>-to-<to>.csv
bucket,day,month,account_id,account_email,run_id,job_id,inbound,outbound,operations,total,cost,unit,currency
```

### API tokens

| Method | Path | Payload / answer |
|---|---|---|
| `GET` | `/api/tokens` | `{tokens:[apiTokenView], scopes:[…]}` |
| `POST` | `/api/tokens` | `{name, scopes?, expires_in_days?}` → **201** `{token, secret}` |
| `POST` | `/api/tokens/:id/regenerate` | → `{token, secret}` |
| `POST` | `/api/tokens/:id/revoke` | → `{token}` (idempotent) |
| `DELETE` | `/api/tokens/:id` | `204` |

* `apiTokenView` = `{id, name, prefix, scopes, created_by?, expires_at?,
  revoked_at?, last_used_at?, last_used_ip?, requests, created_at, state}` with
  `state` ∈ `active` \| `revoked` \| `expired`.
* `secret` is `sync_<prefix>.<secret>` and is **answered once**, by the create and
  the regenerate call; it is stored as a SHA-256 digest and cannot be read back.
* `scopes` is a subset of `read` (every `GET`) and `write` (everything else); the
  default is both, `expires_in_days` defaults to `0` (never) and is capped at 365.
* Authentication is `Authorization: Bearer sync_…` or `X-API-Key: sync_…`; see
  [authentication.md](authentication.md) §6.

## 10. Maintenance

| Method | Path | Payload / answer |
|---|---|---|
| `POST` | `/api/maintenance/schedule/run` | `{started}` — one scheduling pass right now |
| `POST` | `/api/maintenance/tokens/refresh` | `{refreshed}` |
| `POST` | `/api/maintenance/oauth/states/prune` | `{removed}` |
| `POST` | `/api/maintenance/runs/prune` | `{keep}` (default 200, 1…10000) → `{keep, pruned:true}` |
| `POST` | `/api/maintenance/audits/prune` | `{keep}` (default 50, 1…10000) → `{keep, pruned:true}` |
| `POST` | `/api/maintenance/auditlogs/collect` | `{organisations, streams, failed, events, repaired, pending_repairs, duration_ms, errors?}` — one activity-feed pass right now, including the repair of the stored history (see [audit-logs.md](audit-logs.md)) |
| `POST` | `/api/maintenance/auditlogs/renormalise` | `{scanned, updated, attributed, remaining, duration_ms}` — re-read the stored vendor documents and re-attribute the events a release before `actor_kind` mapped with a single actor slot |
| `POST` | `/api/maintenance/purge` | `{target}` ∈ `all` \| `jobs` \| `audits` \| `billing` \| `auditlogs`, optional `{days}` → `PurgeOutcome` |

## 11. Worked examples

```bash
# health (no session needed)
curl -s https://sync.example.com/api/health

# sign in and keep the cookie
curl -s -c jar.txt -H 'Content-Type: application/json' \
  -d '{"login":"admin@example.com","password":"secret"}' \
  https://sync.example.com/api/auth/login

# dashboard numbers of the last week
curl -s -b jar.txt 'https://sync.example.com/api/stats?days=7'

# create a job and run it
curl -s -b jar.txt -H 'Content-Type: application/json' -d @job.json \
  https://sync.example.com/api/jobs
curl -s -b jar.txt -X POST https://sync.example.com/api/jobs/1/run

# what happened to one path
curl -s -b jar.txt 'https://sync.example.com/api/runs/42/items?limit=200'

# audit a SharePoint library two levels deep, then download the report
curl -s -b jar.txt -H 'Content-Type: application/json' \
  -d '{"account_id":1,"folder_id":"01ABCD","folder_path":"SharePoint / Marketing","depth":2}' \
  https://sync.example.com/api/audits
curl -s -b jar.txt -o audit-1.csv https://sync.example.com/api/audits/1/export
```

## 12. Contract with the SPA

The SPA consumes exactly these shapes through `web/src/lib/api.ts`: typed
wrappers per endpoint, `codeOf(error)`/`messageOf(error)` to read `code`/`error`
from a failure. If an endpoint changes, change both sides and this file — the
catalog parity check of [i18n.md](i18n.md) does not cover payload names.
