# Architecture

Sync is a single Go binary that serves both a JSON API and an embedded Vue 3
single-page application. There is no separate frontend container, no Node.js at
runtime and no queue or external worker: one process, one MySQL/MariaDB
database, one HTTP port.

## 1. Process shape

```text
                       ┌──────────────────────────── browser ────────────────────────────┐
                       │  Vue 3 SPA (served by the same binary, embedded in the Go build)│
                       └───────────────┬─────────────────────────────────────────────────┘
                                       │  /api/*  (JSON, session cookie)
┌──────────────────────────────────────▼──────────────────────────────────────────────────┐
│ cmd/server (main.go) — configuration, database, wiring, HTTP server, signal handling     │
│                                                                                         │
│  internal/handlers  ── routing, session, validation, error → HTTP status mapping          │
│  internal/services  ── Store, AuthService, OAuthService, TokenManager, SettingsService,   │
│                        SyncService (engine), CalendarService (engine), AuditService,      │
│                        Scheduler, Notifier, ProviderSettings                              │
│  internal/providers ── provider interface (files + calendars) + google + microsoft         │
│  internal/ical      ── .ics fetcher, validator and RFC 5545 parser                        │
│  internal/notify    ── SMTP / webhook / shoutrrr senders                                  │
│  internal/database  ── GORM connection, AutoMigrate, settings repository                  │
│  internal/models    ── GORM entities and every enum vocabulary                            │
│  internal/crypto    ── AES-256-GCM, HMAC-SHA256, PKCE                                     │
│  internal/config    ── environment parsing + validation (the only os.Getenv caller)       │
└──────────────────────────────────────┬──────────────────────────────────────────────────┘
                                       │ GORM (go-sql-driver/mysql, UTC, utf8mb4)
                                       ▼
                          MySQL / MariaDB  (always external)
```

Four background loops run inside the same process (`internal/services/scheduler.go`):

| Loop | Period | What it does |
|---|---|---|
| schedule | 30 s (`DefaultScheduleInterval`) | starts every enabled job, file or calendar, whose `next_run_at` elapsed |
| tokens | 60 s (`DefaultTokenInterval`) | renews access tokens that expire inside the 5 min window (`DefaultRefreshWindow`) |
| retention | 6 h (`DefaultPurgeInterval`) | applies the retention of Admin > Settings > Purge (runs, audit reports, traffic days) |
| (on boot) bootstrap | once | closes runs, calendar runs and audits a restart interrupted, recomputes `next_run_at` for every scheduled job |

Sync runs are goroutines owned by `SyncService`; a per-job lease guarantees that
one job never runs twice, even when the binary is started twice against the same
database.

## 2. Request lifecycle

Every request passes through the same chain (`internal/handlers/handlers.go`):

1. `gin.LoggerWithConfig` (skips `/api/health`) + `gin.Recovery`.
2. `securityHeaders()` — `X-Content-Type-Options`, `X-Frame-Options: DENY`,
   `Referrer-Policy: same-origin` and the Content-Security-Policy.
3. `trustProxies()` — `APP_TRUST_PROXY=true` makes Gin read `X-Forwarded-For`
   (login throttling and logs depend on the real client address).
4. `session()` — resolves the operator once per request. In `none` mode every
   request carries the open administrator identity; otherwise the signed
   `sync_session` cookie is verified (an invalid/expired cookie simply leaves the
   request anonymous).
5. `requireSession()` on the `/api` tree below the public endpoints — answers
   `401 {"code":"unauthorized"}` when the request is anonymous.
6. The handler decodes its payload, calls exactly one service method and maps the
   error with `fail()`/`abort()` (`internal/handlers/respond.go`).

Anything that is not `/api/**` is served by the SPA handler:

| Request | Answer |
|---|---|
| an existing file in the embedded `dist` (e.g. `/assets/index-abc.js`) | the file; `Cache-Control: public, max-age=31536000, immutable` under `assets/`, `no-cache` otherwise |
| any other path (`/`, `/jobs/12`, …) | `index.html` (`no-cache`) so the client router resolves the deep link |
| `/api/...` with no route | JSON `404` — the SPA never masks an API typo |
| a binary built without a frontend | `503 {"code":"not_configured"}` |

## 3. Sync run pipeline

`SyncService.Start` (`internal/services/sync.go`) claims the per-job lease,
creates the `sync_runs` row (`status=running`) and runs one pass under
`context.WithTimeout(run timeout)`:

```text
list source tree ──► list destination tree ──► plan per path ──► transfer ──► update sync_files ──► finalise run
   (provider A)          (provider B)        (state + policy)   (stream)      (state table)        (status, notify)
```

* **Listing** skips exclude patterns (counted as `skipped`) and collects the
  items V1 cannot transfer (Google native documents, shortcuts) as `unsupported`.
* **Planning** compares the two sides against `sync_files` (the state table) and
  the conflict policy; every decision is stored with a short `evidence` string
  (e.g. `destination newer by 42s`).
* **Folder plan** is taken on the two listings, before the transfer: a folder only
  one side lists is either new there or deleted on the other side, and
  `sync_folders` (the folder half of the state) is what tells them apart. The plan
  yields the creates, which run first so every upload finds its parents, and the
  deletes, which run last.
* **Transfer** streams a download and an upload; folders are created on demand
  (`folder_created`).
* **Folder creation** (`sync_empty_folders`, on by default) creates on a side every
  folder the plan calls new on the other one, so a folder that holds no file still
  travels.
* **Folder deletion** (`delete_missing`) runs after the transfer and removes the
  folders the plan says were deleted elsewhere once nothing is left under them — no
  file, no item V1 cannot copy, no subfolder that survived (`folder_deleted`; a
  folder the guard kept is recorded as `skipped`). Only a bidirectional job with
  both options on has a real choice, and then the state decides: tracked in
  `sync_folders` means deleted, untracked means new. A one-way job keeps its
  behaviour, where `delete_missing` prunes the write side and `sync_empty_folders`
  restores it.
* **State** is written when the run ends: `sync_files` per transferred path and
  `sync_folders` per folder both sides listed, so the next run can tell a deletion
  from a folder that has not travelled yet.
* **Finalising** sets `success`, `partial` (some files failed), `failed`, or
  `timeout` when the run exhausts its time budget (a throttled provider is the
  usual cause; the run continues on the next slot) or `cancelled` when an operator
  stops it, writes the counters, updates `last_run_at`/`next_run_at`, touches the
  accounts' `last_synced_at` and publishes `sync.success` / `sync.run_failed` /
  `sync.error` to the notifier.

Cancellation is cooperative (`SyncService.Cancel`), and a restart closes runs that
were left `running` (the audit history never shows a run that no longer exists).

## 3b. Audit pipeline

`AuditService.Start` (`internal/services/audit.go`) is the read-only sibling of the
run pipeline: it claims a per-account lease, creates the `audit_runs` row
(`status=running`) and walks one tree in the background.

```text
list folder tree to a depth ──► buffer entries (DFS preorder) ──► batch insert audit_entries ──► finalise run
        (provider)                    (subtree totals)               (one InsertInBatches)          (status, prune)
```

* The walk reuses the engine's token handling: an expired access token is renewed
  and a provider 401 mid-walk is retried once (`TokenManager.Reissue`).
* The selected folder is **depth 0**; `max_depth = 0` is unlimited (clamped to
  25). The depth bounds the **report, not the walk**: a folder at the boundary is
  listed with the exact totals of its subtree — the walk descends into it and
  counts everything below (`listed:false` marks the folders whose children are
  left out), so an ancestor never under-reports what is below the cut.
* The walk is bounded by two budgets: `AuditNodeLimit = 10000` entries and
  `AuditVisitLimit = 200000` visited nodes, either of which sets `truncated`. The
  live counters reach the row every ~2s so the polling UI shows progress.
* `GET /api/audits/:id/export` streams the stored report as CSV without touching
  the provider again; `PruneAudits` keeps the newest audits per account.

Cancellation is cooperative too (`AuditService.Cancel`), and the scheduler
`Bootstrap` closes the audits a restart interrupted.

## 3c. Traffic accounting (billing)

`internal/services/billing.go` sits beside the engine: the three places that touch
a provider (`copy`, `remove`, `ensureFolder`) report what they moved, and the run
flushes the totals.

```text
download ─► inbound bytes  (source account)
upload   ─► outbound bytes (destination account)      ─► flush (throttled 5s) ─► traffic_entries
delete / folder created ─► one operation (the account it acts on)                 (upsert on run+account+direction)
```

* The counters are **absolute** `(account, direction)` totals of the run, never
  increments: the flush is an upsert on
  `(run_id, account_id, direction)`, so repeating it (throttled while the run is in
  flight, forced once at the end) can neither double count nor lose a retry.
* `inbound` is the account read from, `outbound` the account written to; an
  operation that moves no payload is attributed to the account it acts on with the
  same split. A job whose two endpoints are the same account therefore keeps two
  rows.
* The UTC `day`/`month` buckets come from the run start, so a run never spans a
  bucket and the report is a plain `GROUP BY` (portable to MySQL/MariaDB and to the
  SQLite of the tests).
* A cancelled or crashed run keeps whatever the last throttled flush stored, which
  is the traffic that really moved; `PruneRuns` deletes the rows with their run.
* `internal/services/billing_schema.go` only decides the *vocabulary* of a report
  (`none`, `oci`, `aws`, `gcp`, `custom`): the labels, the unit and the price per
  GiB. The quantities stay bytes in the database.

## 3d. Calendar pipeline

`CalendarService` (`internal/services/calendarsync.go`) is the sibling of the file
engine: same reserve/run-row/timeout/cancel/retention lifecycle, a different unit
of reconciliation. It pairs **events by iCalendar UID** instead of items by path.

```text
window (now ± past/future days) ──► list source ─┐
                                   list destination ┴──► pair by UID ──► decide ──► write one side ──► upsert link
                                     (provider or .ics)   (calendar_event_links)  (§4, §5 of calendars.md)   (fingerprints)
```

* **One run per job.** `reserve` claims an in-process lease (`running[job.ID]`);
  the store also refuses a job that already has a row marked `running`, so a
  second binary against the same database cannot double-run a job (`ErrBusy` →
  `409 busy`). A slot that finds the job busy is pushed one interval forward
  instead of queued.
* **The plan is a pure function.** `calendar_plan.go` (`decide`) maps both
  listings, the recorded link and the job options onto one action per UID
  (`create_destination`, `update_source`, `delete_*`, `skip`, `unchanged`,
  `forget_link`), so *Preview* (dry run) and a real run cannot disagree.
* **Fingerprints, not stamps, decide "who changed".** `providers.EventFingerprint`
  hashes the content (summary, description, location, start, end, all-day flag,
  sorted attendees, recurring flag) and deliberately ignores the provider id and
  the modification stamp; `calendar_event_links` stores both fingerprints, so an
  unchanged calendar costs two listings and no writes. `newest_wins` can only
  compare stamps — Graph often publishes none — and reports a `conflict` instead
  of guessing.
* **An `.ics` source is a third source kind**, not a provider: `internal/ical`
  fetches the feed with `ETag`/`Last-Modified` validators (a `304` skips both the
  parse and the writes), refuses non-public destinations unless
  `CALENDAR_ALLOW_PRIVATE_TARGETS=true` (checked on the host name and again on the
  dialled address), and parses only the RFC 5545 subset the engine uses. A feed is
  read-only, so its job is forced to `one_way`.
* **Recurring series are never expanded.** A series master and its instances
  carry `RRULE`/`RECURRENCE-ID`/a recurrence block; they are counted in
  `events_skipped` and left alone rather than copied with an invented rule.
* **Calendar runs move no bytes**, so `billing` is not involved and the
  notification events stay with the file engine; the evidence lives in
  `calendar_runs` and `calendar_run_items` (newest 50 runs per job).
  `FinishCalendarRun` mirrors the outcome on the job row in the same transaction
  as the run, which is what advances the next slot.

## 3e. API tokens

`internal/services/apitokens.go` mints `sync_<prefix>.<secret>`: 4 random bytes as
the public, unique-indexed prefix and 32 random bytes as the secret, of which only
the SHA-256 digest is stored. `handlers/middleware.go` resolves a presented
`Authorization: Bearer` / `X-API-Key` credential before the cookie (a presented
token is the only credential considered) and `requireScope` maps the HTTP method
onto the scope a request needs (`GET`/`HEAD`/`OPTIONS` → `read`, everything else →
`write`). A browser session carries no scopes, which means "everything an
administrator may do", so the guard only ever narrows a token.

## 4. Invariants every change must keep

1. **Secrets never leave the process in clear text.** `internal/crypto.Encrypt`
   produces `v1:<base64>` AES-256-GCM payloads; the models declare token columns
   with `json:"-"`, the notification channels mask every secret on the way out
   (`********`) and no log line prints a token (`internal/services/*` log only
   account ids, masked e-mails and error strings).
2. **No hardcoded UI text.** Every string the SPA renders comes from
   `web/src/i18n/locales/*.json`; the API answers a machine-readable `code`
   (`validation`, `not_found`, `unauthorized`, `forbidden`, `busy`, `reconnect`,
   `not_configured`, `internal`, `delivery_failed`) beside the English message, and
   the SPA translates the code (see [i18n.md](i18n.md)).
3. **The database schema is created by `database.Migrate`**, never by hand: an
   upgrade is "pull the image and start it".
4. **Only `internal/config` reads the environment.** Every other package receives
   values through a constructor, which is what makes the boot contract testable.
5. **The API is thin.** A handler validates input, calls one service method and
   maps the error; business rules live in `internal/services`.
6. **The SPA is embedded.** `web/embed.go` (`//go:embed all:dist`) means a release
   image contains exactly one artifact; `web/dist/.gitkeep` keeps a fresh clone
   buildable without Node.

## 5. Where to change what

| Change | Touch |
|---|---|
| new endpoint | `internal/handlers/routes.go` + handler file + `docs/api.md` |
| new setting | `internal/models/enums.go` (`Setting*`), `services.Defaults`, `SettingsService.Update`, Admin > Settings view + catalogs |
| new channel kind | `internal/notify` sender + `channelSchemas` in `internal/handlers/notifications.go` + `docs/notifications.md` |
| new provider | implement `providers.Provider` (and optionally `providers.CalendarProvider`), register in `cmd/server/main.go` (`build`), document in `providers.md` |
| new pairing rule | `internal/services/calendar_plan.go` (with a table test) + evidence text in `calendars.md` §4 |
| new UI language | catalog file + `web/src/i18n/index.ts` (`MESSAGES`, `LOCALES`) + `config.SupportedLocales` + `docs/i18n.md` |
| new screen | `web/src/views/**`, route in `web/src/router/index.ts`, nav entry + catalog keys |

## 6. Related documents

* [configuration.md](configuration.md) — every environment variable and its validation
* [database.md](database.md) — tables, indexes, retention, upgrade path
* [authentication.md](authentication.md) — the three auth modes and the session cookie
* [oauth.md](oauth.md) — provider flows, PKCE, token refresh
* [providers.md](providers.md) — provider capabilities and registration
* [calendars.md](calendars.md) — calendar jobs, pairing rules and `.ics` subscriptions
* [api.md](api.md) — endpoint reference
* [notifications.md](notifications.md) — channel schemas and events
* [development.md](development.md) — toolchain, commands, tests
* [reverse-proxy.md](reverse-proxy.md) — HTTPS, cookies, headers in front of Sync
* [i18n.md](i18n.md) — catalogs, parity, RTL
* [branding.md](branding.md) — logo assets, theme tokens

