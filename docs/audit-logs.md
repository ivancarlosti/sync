# Audit logs

> **Where this sits in the product.** *Audit storage* (see [api.md](api.md) §8)
> analyses the **content** of a folder tree and stores a report of what is inside
> it. *Audit logs* — this page — stores the **activity** the providers report:
> who created, deleted, moved or shared what, and who signed in. The two share
> the word "audit" and nothing else.

## 1. Why a local copy exists

Neither Microsoft nor Google offers a push subscription an operator can register
without extra infrastructure, and both expire the data:

| Feed | Vendor | Retention at the vendor |
|---|---|---|
| Directory changes | Microsoft Graph `auditLogs/directoryAudits` | 30 days |
| File and workload activity | Office 365 management activity API (`Audit.SharePoint`, `Audit.General`) | ~7 days |
| Drive, user, group, admin, sign-in | Google Workspace Reports API | 6 months (180 days) |

Sync therefore **polls** each feed and keeps what it read in `audit_log_events`
(see [database.md](database.md) §3). The screen reads that table, so the history
survives the vendor window, and one query covers both clouds.

## 2. What is read

### Microsoft 365 — two streams

| Stream | Endpoint | Authorization |
|---|---|---|
| `directory` | Graph v1.0 `/auditLogs/directoryAudits` | delegated `AuditLog.Read.All` (admin consent) |
| `unified` | `https://manage.office.com/api/v1.0/{tenant}/activity/feed` | **application** permission `ActivityFeed.Read` (admin consent) |

`directory` answers the changes to the directory itself: users, groups, roles,
policies, administrative units. `unified` answers what happens *inside* the
workloads — a file deleted from OneDrive or a SharePoint library is an
`Audit.SharePoint` record there, and Graph v1.0 has no equivalent (the
file-activity endpoints only exist in `/beta`, as an asynchronous query job).

The two differ in authorization too, and that is why they are separate streams
with separate cursors and separate last errors:

* `directory` is read **with the delegated token of the connected account**, so
  it works as soon as an administrator connects;
* `unified` takes an **app-only token** built from the instance's OAuth client
  (`client_credentials` for `https://manage.office.com/.default`) for the tenant
  of that account. The permission cannot be requested in an authorization URL,
  so it is not part of the permission table: see
  [app-registration.md](app-registration.md) §5.

A tenant that never granted `ActivityFeed.Read` answers 401/403 on the token
exchange. That is recorded as the `unified` stream's `last_error`, the screen
shows the "waiting for a permission" banner, and the `directory` stream keeps
working. No reconnect can fix it — only an administrator grant.

### Google Workspace — five streams

One stream per Reports API application (`admin.googleapis.com`):

| Stream | Content |
|---|---|
| `drive` | creations, uploads, edits, moves, renames, deletions, sharing changes |
| `user_accounts` | create, delete, suspend, undelete, rename, password and admin-privilege changes |
| `groups` | group lifecycle and membership |
| `admin` | Admin console settings changes |
| `login` | sign-ins, failures, challenges and the suspicious-sign-in warnings |

All five are read with one delegated scope, `admin.reports.audit.readonly`
(admin consent). The `drive` stream sends an `eventName` filter: without it every
view, preview and download would come back too, and that volume buries the
deletions the screen exists for.

## 3. The collector

`services.AuditLogService.Collect` (see `internal/services/auditlog.go`) runs:

* once at start-up, then **every ten minutes** (the `logs` loop of
  `services.Scheduler`), and
* on demand through `POST /api/maintenance/auditlogs/collect` (the
  **Collect now** button).

One pass:

1. groups the connected accounts by `(provider, organisation)`, where the
   organisation is `connected_accounts.org_id` (the Entra tenant, the Workspace
   hosted domain) or — for a row connected before that column existed — the
   organisation the provider reads from the token (Microsoft's `tid` claim). An
   account that cannot name its organisation is skipped: guessing one from an
   e-mail domain would read a stranger's tenant;
2. picks the first healthy account of the group: any administrator of a tenant or
   domain can read its whole feed, so the feed is read **once per organisation**,
   not once per account;
3. calls every stream of the provider (`ActivityLogStreams()`), handing each the
   cursor stored for `(provider, organisation, stream)`;
4. normalises the records onto one vocabulary (`create`, `delete`, `update`,
   `move`, `share`, `login`, `other` × `file`, `folder`, `user`, `group`, `site`,
   `drive`, `other`), stores them and advances the cursor.

A failure is per stream, never per pass: a throttled feed stops the remaining
feeds of *that* organisation for the pass (the provider is asking for a backoff)
and every other organisation is still read.

### Cursors

The cursor is opaque to the store — an RFC3339 UTC watermark for every stream. It
only moves after the events were stored, so a failed write re-reads the same
window on the next pass. Ingestion is idempotent, which is what makes the overlap
safe:

* Google: the pass starts at `cursor − 5 min`, because the Reports API files an
  event when it processes it, not exactly when it happened;
* Microsoft `directory`: the same five minute overlap in the `$filter`;
* Microsoft `unified`: the content API is walked in 24-hour windows from the
  cursor, each listing at most 200 blobs.

The identity of a stored event is `(provider, source, remote_id)`, so a re-read
can never duplicate a row. The cursor also keeps `last_event_at`, `last_run_at`
and `last_error`, which is what the screen renders as the health of each feed.

## 4. What the screen shows

`/audit-logs` (nav: **Audit Logs**) lists the stored events newest first, with the
filters `provider`, `org_id`, `action`, `target`, `actor`, `search` (actor e-mail
or object name), `from` and `to`. The CSV export applies the same filters
(`GET /api/audit-logs/export`, capped at 5000 rows, the filtered total in
`X-Total-Count`).

Above the table, the feed banner explains a stale list:

* **waiting for a permission** — at least one feed carries a `needs_consent`
  error: grant `ActivityFeed.Read` (Microsoft) in the app registration, or
  reconnect the Workspace account so it picks up
  `admin.reports.audit.readonly`;
* **could not be read** — any other failure, with its message.

## 5. Retention

Stored events are erased by the retention pass like every other record:

* `purge_auditlogs_days` (Admin > Settings > Purge, default 180 days) is the age
  past which an event is deleted;
* the same pass can be forced from the same screen (*Purge now* → **Activity
  events**) or restricted to that single target.

A retention shorter than the vendor window is allowed and simply trims the local
copy sooner; a longer one is the reason to keep the copy at all.

## 6. Adding a provider

A provider opts in by implementing `providers.ActivityLogSource`
(`internal/providers/provider.go`):

```go
type ActivityLogSource interface {
    ActivityLogStreams() []string
    ActivityLogScopes() []string
    ActivityLogs(ctx context.Context, creds Credentials, tokens *Tokens, stream, cursor string) ([]ActivityEvent, string, error)
}
```

The collector skips a provider that does not implement it, so the feature is
additive. `ActivityLogScopes()` is the delegated contract — what the guide and the
Accounts screen ask an operator to grant. An app-only permission is documented
instead, never declared there: it can neither be requested by an authorization
URL nor proven by the `scope` of a token response, so declaring it would mark
every existing account as missing a permission a reconnect cannot add.

