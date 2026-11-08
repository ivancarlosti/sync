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
  [app-registration.md](app-registration.md) §3.4.

A tenant that never granted `ActivityFeed.Read` fails in one of two shapes: the
token exchange answers `AADSTS65001` (no consent at all), or the token is issued
with an empty role set and the subscription answers `AF10001` /
`permission set ()` (granted to the app, never consented in *that* directory).
Either one is recorded as the `unified` stream's `last_error`, the screen shows
the "waiting for a permission" banner, and the `directory` stream keeps working.
No reconnect can fix it — only an administrator grant, per directory, plus
unified audit logging turned on in Purview (otherwise the feed subscribes and
answers nothing).

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
deletions the screen exists for. The Reports API validates every name of that
filter against the published event manifest and answers
`400 … Event <name> not found in manifest` for the **whole request** when one is
unknown, so the list is the published Drive event set and nothing else (see
`driveEventNames`, pinned by `TestDriveEventNamesAreDocumented`). A name the API
starts rejecting is dropped for that page and the read retried, so a vendor
retirement costs one extra request instead of the stream.

## 3. The collector

`services.AuditLogService.Collect` (see `internal/services/auditlog.go`) runs:

* once at start-up, then on the interval of **Admin > Settings > Activity log
  capture** (`auditlogs_interval_minutes`, ten minutes by default, bounded to
  1–1440). The `logs` loop of `services.Scheduler` wakes once a minute and runs a
  pass only when the switch `auditlogs_enabled` is on **and** the previous pass
  finished at least one interval ago; and
* on demand through `POST /api/maintenance/auditlogs/collect` (the
  **Collect now** button), which ignores the switch and the interval — that is the
  documented way to verify a grant — but counts as the previous pass, so the
  schedule resumes one interval later instead of polling twice within a minute.

The switch stores *nothing* between passes and deletes nothing: with it off the
stored history keeps working (list, filters, export, retention) and only stops
growing. The Audit Logs screen says so above the table (`schedule.enabled` in
`GET /api/audit-logs`, §4), because a list that stops growing otherwise reads as a
broken grant.

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
   `move`, `share`, `login`, `other` × `file`, `folder`, `user`, `group`,
   `device`, `application`, `site`, `drive`, `other`), stores them and advances the
   cursor.

A failure is per stream, never per pass: a throttled feed stops the remaining
feeds of *that* organisation for the pass (the provider is asking for a backoff)
and every other organisation is still read.

Before a stream is given up on, the request itself is retried: every call of both
Microsoft streams goes through `providers.DoWithRetry` with `activityRetryPolicy`
(3 attempts, a `Retry-After` up to 90 s, inside the 5 minute `requestTimeout`),
because the audit endpoints answer a throttle with a minute or more and the
default 30 s cap would retry too early. Google requests retry through `doJSON`
already, and a rejected `eventName` is trimmed and retried (see §2).

### Pacing and throttling

`auditlogs_request_delay_seconds` (Admin > Settings > Activity log capture, 0–300,
0 by default) is the pause a pass takes **between two provider calls**. The first
call runs immediately; every later one waits first, and the wait is abandoned with
the pass when the context is cancelled. The pause exists because the audit endpoints
are counted per tenant: a pass that reads four streams of one organisation back to
back can spend the limit on the second one and answer `HTTP 429` even though
`DoWithRetry` would have absorbed a single refusal (see above).

When a tenant still throttles, raise the pause and widen the interval: the retry
policy wins against a one-off refusal, not against a pass that returns to the same
tenant within the minute. Both values apply to the next wake-up of the loop (at most
one minute later), so no restart is needed.

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

### Actor attribution

An event names a principal, and that principal is not always a person. The stored
`actor_kind` says which:

| `actor_kind` | Who | Stored in |
|---|---|---|
| `user` | a person | `actor_email` (indexed) + `actor_name` |
| `application` | a workload: a service principal, an app, a background job | `actor_name`, **never** `actor_email` |
| `system` | no principal at all (a scheduled or platform job) | `actor_name` = the service that logged it |

A workload often acts *for* somebody, and the clearest case is a device
registration: Microsoft logs `Add device` as the work of "Device Registration
Service" and names the enrolling human nowhere in `initiatedBy`. The collector
recovers the person in three places, in order:

1. **the record itself** — a sibling user `targetResource`, a
   `RegisteredOwner`/`RegisteredUsers` modified property, or an owner-like
   `additionalDetail`. Only a value that looks like an address is accepted: a
   display name alone is not attributed to anybody;
2. **the object** — `GET /v1.0/devices/{id}?$expand=registeredOwners`, one lookup
   per device per pass (memoised, at most 20 per pass, ~20 s each) in
   `internal/providers/microsoft/deviceowners.go`;
3. **nothing** — the row keeps the workload actor, and the screen says why it
   cannot name the person (see §4).

Both recovery steps write `actor_on_behalf_of` / `actor_on_behalf_of_name` and
never touch the workload actor: the row still says a service made the change, and
the person appears beside it.

The Graph read is authorised by `Device.Read.All`, `Device.ReadWrite.All`,
`Directory.Read.All` or `Directory.ReadWrite.All`. This application already holds
`Directory.ReadWrite.All` (see [app-registration.md](app-registration.md)), so the
enrichment needs **no new consent and no reconnect**, and a tenant that revokes the
grant simply keeps the honest service attribution. Anything else — a throttle, a
404 for a device deleted since the event, a refusal — is logged and skipped: an
unreachable enrichment never fails a pass.

Google's normaliser names `actor.email` from its own feed, so its rows are stored
with `actor_kind = user` and the two on-behalf-of columns stay empty.

### Repairing the history

A release that maps a field more precisely can re-read what it already stored: the
vendor document of every event is kept in `AuditLogEvent.Raw` for exactly this. At
the end of every pass `RepairActors` takes the rows whose `actor_kind` is empty
(stored by an older release), re-normalises them through the provider's optional
`ActivityLogNormaliser`, runs the optional `ActivityActorEnricher` on the device
rows of the batch, and writes back the actor columns and the target. It is bounded
(500 rows per batch, 20 batches and 2 minutes per pass), idempotent (a repaired row
stops being a candidate) and safe to interrupt; a row whose `raw` is missing is
repaired from the columns it already carries (a service principal name found in the
address column moves to `actor_name`). `POST /api/maintenance/auditlogs/renormalise`
runs the same pass on demand and answers `scanned`, `updated`, `attributed` and
`remaining`.

## 4. What the screen shows

`/audit-logs` (nav: **Audit Logs**) lists the stored events newest first, with the
filters `provider`, `org_id`, `action`, `target`, `actor` (the address of a person
or of the person a workload acted for), `search` (actor address, actor name,
on-behalf-of or object name), `from` and `to`. The CSV export applies the same
filters (`GET /api/audit-logs/export`, capped at 5000 rows, the filtered total in
`X-Total-Count`) and carries the same columns as the screen:
`occurred_at, provider, org_id, stream, action, target, actor_kind, actor_email,
actor_name, actor_on_behalf_of, actor_on_behalf_of_name, target_name, target_id,
site_id, remote_id`.

The form also offers an **account** selector (fed by `GET /api/accounts`, the same
list the Accounts screen shows). Picking an account applies the `provider` and the
`org_id` of that account, because the feed is read once per organisation: two
administrator accounts of one tenant list exactly the same events, and the hint
under the field names the organisation so the duplication reads as intended rather
than as a broken filter. An account without an organisation (a personal Microsoft
or Google account) has no administrator feed, so its option is offered greyed out
with the reason instead of producing a filter that could only ever be empty.

The **Organisation** column names the accounts that read each feed, prefixed with
`read by`, so a tenant id never has to be recognised on its own and the collector
identity can never be mistaken for the actor of the event.

The **Actor** column is rendered from `actor_kind` (§3), because the two are read
very differently:

* a **person** shows the address (and the display name under it);
* a **workload** shows its display name in the body and a `Service` chip, with no
  address at all, and the person it acted for under it (`on behalf of …`) when one
  was recovered;
* an event with no principal shows the name of the service that logged it and a
  `System` chip.

A workload-changed device whose owner could not be resolved carries one extra
line: the account reading that feed cannot look the owner up, which is what an
administrator has to grant a directory permission to fix. The hint is derived from
the `missing_capabilities` of the accounts of that organisation, so it appears only
when the screen knows the grant is missing.

Above the table, the schedule line answers "why is nothing new?":

* **automatic collection is off** — `schedule.enabled` is false: nothing is polled
  in the background, *Collect now* still works, and the link opens Admin >
  Settings;
* otherwise it names the next pass and the configured interval
  (`schedule.next_run_at`, `schedule.interval_minutes`), plus when the previous
  pass finished.

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

## 7. Troubleshooting

The health of every feed is in `GET /api/audit-logs/cursors`: `healthy`,
`needs_consent`, `last_error`, `last_run_at`, `last_event_at`. The three failures
seen in production:

| Symptom (`last_error`) | Cause | Fix |
|---|---|---|
| `… 400 … Event change_acl_scope not found in manifest` (`drive`) | a name of `driveEventNames` is not in Google's Drive event reference, and one unknown name fails the **whole** request | remove the name from `driveEventNames` and `classifyActivity`: the diagnostic names it. The trim in `activityPage` already drops it for the next pass, so the stream recovers before a release ships |
| `… AF10001 … permission set ()` (`unified`) | `ActivityFeed.Read` (application permission of *Office 365 Management APIs*) was never granted, or was granted without consent in **this** directory. Unified audit logging also has to be on | grant the permission, have an administrator of that directory consent, turn on unified audit logging in Purview — [app-registration.md](app-registration.md) §3.4 |
| `microsoft graph: … http 429` (`directory`) | Graph throttled the read; the `Retry-After` outlived the retry budget | nothing, unless it repeats on every pass: the call is retried twice with the delay Graph asked for, and a tenant that throttles a ten-minute poll needs a lower poll rate, not a bigger local copy |

`needs_consent` is what turns the banner into "waiting for a permission" instead
of "could not be read": it is set from the message, so a new vendor spelling has
to be added to the markers in `needsConsent` (`internal/services/auditlog.go`) —
`af10001` is there for exactly that reason.

