# Calendar synchronisation

A **calendar job** keeps two calendars aligned or mirrors an iCalendar (`.ics`)
feed into a writable provider calendar. It is the sibling of a file job, not a
mode of it: the file engine reconciles a tree of items by path, the calendar
engine reconciles events by iCalendar **UID**, and both share the same lifecycle
(reserve → run row → timeout → cancel → retention) so the UI presents them side
by side.

| Piece | Where |
|---|---|
| job, event link, run and run-item models | `internal/models/calendar.go` |
| calendar half of the provider interface | `internal/providers/calendar.go` |
| Google / Microsoft implementations | `internal/providers/{google,microsoft}/calendar.go` |
| `.ics` fetch + parser | `internal/ical` |
| pairing rule (pure, unit tested) | `internal/services/calendar_plan.go` |
| engine and run lifecycle | `internal/services/calendarsync.go` |
| endpoints | `internal/handlers/calendars.go` |
| screens | `web/src/views/CalendarsView.vue`, `web/src/views/CalendarEditorView.vue` |

## 1. What a job pairs

| Source kind | Reads from | Needs a connected account |
|---|---|---|
| `google` | a Google Calendar of a connected Google account | yes |
| `microsoft` | a Microsoft 365 / Outlook calendar of a connected Microsoft account | yes |
| `ics` | a public or private `.ics` URL over HTTP(S) | no |

The destination is **always** a calendar of a connected account: a feed has no
writable end, so it can only be a source. `direction` is `bidirectional` (both
calendars stay aligned) or `one_way` (the source is mirrored into the
destination and never written back); an `ics` job is forced to `one_way` instead
of being rejected, because the editor would otherwise have to special-case the
field.

The two ends are compared by account **and** calendar id: pointing a job at the
same calendar twice is a `400` (`the source and the destination cannot be the
same calendar`), since it would rewrite every event on every run.

Permissions the accounts need:

| Provider | Scope | Note |
|---|---|---|
| Google | `https://www.googleapis.com/auth/calendar` | read/write calendars **and** events; `calendar.events` alone would not let the picker list the calendars of the account |
| Microsoft | `Calendars.ReadWrite` | available to a personal account too, so the permission row carries no account-kind restriction |

Accounts connected before the calendar half existed are reported by the Accounts
screen as **missing the `calendars` capability**: reconnect them (the tokens of
the existing row are rotated, jobs and history are kept). See
[providers.md](providers.md) and [app-registration.md](app-registration.md) for
the console side of the same change.

A calendar the account may read but not write (a holiday calendar, a shared
calendar without write access) is listed by the picker and refused by the
engine (`… is read-only`), so a job can never be written to a calendar that
would reject every write.

## 2. Creating a job

**Calendars → New** opens the same four step wizard as a file job: *details*
(name, direction, conflict policy, enabled), *source*, *destination*, *options*
(the listing window and the interval). *Next* validates only the tab it leaves,
*Save* validates the whole form.

An `.ics` source adds three things to the *source* tab:

* **Check subscription** (`POST /api/calendars/feed-preview`) downloads the feed
  and shows the window it covers, how many events it holds, how many are
  recurring or cancelled, and a sample of ten — a typo in the URL turns into one
  request instead of a job that fails on its first run.
* **Username / password** for an HTTP Basic protected feed, and a **clear the
  stored password** switch. The password is write-only: the API never returns
  it, and an update that leaves the field empty keeps the stored secret, so the
  editor can round-trip a job without decrypting anything.
* The direction is fixed to *one way* and the field is disabled, with a hint
  saying why.

A calendar that is no longer listed by the account (deleted, unshared) stays
selectable under its **stored name**, so editing an unrelated field of a job
cannot silently re-point it at another calendar.

## 3. What a run does

1. **Window.** The listing range is `now - past_days` … `now + future_days`.
   `past_days` is clamped to `0–730` (default `30`, and `0` means the default),
   `future_days` to `1–1825` (default `365`). A feed is filtered to the same
   window while it is parsed, so the preview the editor shows and the first run
   count the same events.
2. **Resolve both sides.** The destination is resolved through the token
   manager (a revoked token ends the run as `412 reconnect`); an empty calendar
   selection falls back to the account's primary calendar. An `ics` source is
   not resolved at all — it is fetched.
3. **List.** One provider listing per side over the window; a feed download with
   the stored `ETag`/`Last-Modified` as validators.
4. **Pair by UID.** The candidate set is the union of both listings and the
   recorded state (`calendar_event_links`), so an event the engine already knows
   is still considered when one side stops listing it. The pairing key is the
   iCalendar UID (`iCalUID` on Google, `iCalUId` on Graph), falling back to the
   tracked event id.
5. **Decide and apply.** Every event gets one decision (§4, §5). A write goes to
   one side only, and what the provider answers is what is recorded.
6. **Persist.** `calendar_event_links` keeps, per UID: both provider event ids,
   both **content fingerprints** and both last-known starts, which is what makes
   "which side moved since the last run" answerable without storing a copy of
   the event. The run row and its per-event evidence are written next.

A run over an unchanged calendar costs two listings and **no writes**: nothing
is written unless a fingerprint moved.

The **fingerprint** (`providers.EventFingerprint`) covers summary, description,
location, start, end, all-day flag, the sorted attendee list and the recurring
flag. It deliberately ignores the provider id and the modification stamp, since
providers bump those on no-op writes.

Run bookkeeping:

| Aspect | Behaviour |
|---|---|
| concurrency | one run per job; a second start answers `409 busy`, and a scheduled slot that finds the job busy is skipped |
| timeout | the instance setting `sync_run_timeout_minutes` |
| cancel | `POST /api/calendars/:id/cancel` stops the run at the next event and still persists what it already wrote |
| statuses | `success`, `partial` (some events failed), `failed`, `cancelled`, `timeout` — the same vocabulary as a file run |
| counters | `events_scanned`, `events_created`, `events_updated`, `events_deleted`, `events_skipped`, `conflicts`, `errors`, `duration_ms` |
| evidence | at most `1000` run items per run; each holds action, UID, summary, start and the reason |
| retention | the newest `50` runs of a job survive; runs a restart interrupted are closed at boot |

## 4. Pairing rules

The rule is a pure function of both listings, the recorded link and the job
options (`decide` in `calendar_plan.go`). The action names are also the values
stored in `calendar_run_items.action`.

| Situation | Action | Evidence |
|---|---|---|
| both sides list the event, neither changed | `unchanged` | `neither side changed since the last run` |
| only the source changed | `update_destination` | `the source changed since the last run` |
| only the destination changed, bidirectional | `update_source` | `the destination changed since the last run` |
| only the destination changed, one-way | `unchanged` | `the destination changed; a one-way job does not write back` |
| both changed to the same content | `unchanged` | `both sides changed to the same content` |
| both changed differently | the policy decides (§5) | `… the source wins by policy`, `… the source is newer (by 42s)` |
| both sides list it, no link recorded | `unchanged`, link recorded | `both sides already hold this event` |
| source only, unknown event | `create_destination` | `the event is not in the destination calendar yet` |
| source only, destination copy deleted | `create_destination` or `delete_source` | `the destination copy was deleted; recreating it` / `… the policy removes the source too` |
| destination only, unknown event | `create_source` or `skip` | `the event is not in the source calendar yet` / `… a one-way job does not write back` |
| destination only, source copy deleted | `create_source`, `delete_destination` or `unchanged` | `the source copy was deleted; recreating it` / `… the policy removes the destination too` |
| a side stops listing an event whose recorded start is outside the window | `unchanged` | `the destination copy starts outside the listing window` |
| gone from both sides | `forget_link` | `the event is gone from both sides` |
| recurring series master or instance | `skip` | `the event belongs to a recurring series, which the engine does not copy` |
| cancelled event still listed | `skip` | `the source marks the event as cancelled` |

The **window drift rule** is what keeps a moved meeting from being read as a
deletion: a side that stops listing an event is only considered to have deleted
it when the start the link recorded is still inside the listing window. An event
that simply travelled into next year is left alone, and is picked up once the
window reaches it.

## 5. Conflicts, direction and deletions

`conflict_policy` is the same vocabulary as a file job:

| Policy | Both sides changed |
|---|---|
| `newest_wins` (default) | the side whose `updated` stamp is newer wins; equal stamps, or two sides that publish no stamp at all (Graph often does not), are reported as a `conflict` and **nothing is written** |
| `source_wins` | the destination is rewritten |
| `destination_wins` | the source is rewritten — unless the job is `one_way`, which then reports `unchanged` |
| `skip` | reported as a `conflict`, nothing written |

A one-way job never reverts a destination edit: it reports it as `unchanged`
rather than restoring the source version, so an annotation the user typed into
their own calendar is not thrown away.

`delete_missing` decides what happens to an event one side deleted:

* **off** (default) — the deleted copy is recreated from the side that still has
  it;
* **on** — the deletion propagates to the other side (`delete_destination` /
  `delete_source`). A one-way job keeps the destination copy when the source
  side says the event is gone, because there is no writable source to prune.

## 6. iCalendar subscriptions

The parser and the fetcher live in `internal/ical`; only the parts of RFC 5545
the engine needs are implemented.

**Fetching**

| Aspect | Behaviour |
|---|---|
| schemes | `http` and `https` only; the URL must be absolute and carry a host |
| url shape | an allow-list pattern pins the exact string handed to the HTTP client — absolute `http(s)`, a host name or a bracketed IPv6 literal, an optional port — so a non-http scheme, a relative path, credentials in the authority part, a space or a control character is refused before anything is dialled |
| credentials | the URL must **not** embed them — the dedicated username/password fields are sent as `Authorization: Basic …`, and only when a username is set |
| conditional GET | `If-None-Match` / `If-Modified-Since` from the last successful fetch; a `304` skips the parse and the writes |
| repair path | a `304` is only final when the destination still matches the recorded state. When an event the operator deleted in the destination needs to be recreated, the feed is fetched once more without validators, so the cheap path stays cheap and the repair path correct |
| size | at most **8 MiB** per download (`MaxFeedBytes`), one request bounded at 60 s |
| private targets | refused unless `CALENDAR_ALLOW_PRIVATE_TARGETS=true`: loopback, RFC 1918, link-local, carrier-grade NAT (Tailscale, `100.64.0.0/10`), NAT64, documentation/benchmark/reserved ranges and the cloud metadata endpoint. A host name is checked again at connect time, where the address actually dialled is known |
| secret at rest | the password is an AES-256-GCM payload (`SecretBox`) and is never serialised by the API |

**Parsing**

* folded lines are unfolded and `TEXT` escapes (`\n`, `\,`, `\;`, `\\`) decoded;
* `DTSTART`/`DTEND`, or `DTSTART` + `DURATION`; all-day `DATE` values stay
  all-day events (start at midnight, exclusive end, written back the same way so
  the round trip does not shift the day);
* a `TZID` is resolved against the runtime zone database; a `VTIMEZONE` block is
  not interpreted, and an unknown zone falls back to UTC instead of failing the
  feed. A floating time is read in the destination calendar's zone;
* `RRULE`, `RECURRENCE-ID` or a series master marks the event as recurring, and
  `STATUS:CANCELLED` as cancelled — both are counted and skipped, never copied;
* at most **20000** events per feed (`MaxEvents`): a bigger subscription is
  truncated, not refused, and the run reports how many events it read;
* a payload that is not a calendar at all is `ErrInvalidFeed` → `400 validation`.

## 7. Scheduling and history

`interval_minutes` follows the file-job rules: `0` means **manual only**, the
maximum is `10080` (seven days), and the next slot is `now + interval` for an
enabled job with an interval and `NULL` otherwise (`NextCalendarRunAt`, the
calendar twin of `NextRunAt`). Creating or updating a job recomputes the slot in
the same request, so a changed interval or a job that was just disabled takes
effect at once instead of at the next bootstrap.

The schedule loop runs both families in one tick: `RunDueCalendars` selects the
enabled jobs whose `next_run_at` has elapsed and starts each one through
`StartIfIdle`. A job that is still running is **not** queued — its slot is pushed
one interval forward, so a slow provider skips ticks instead of piling runs up
(`ErrBusy` → `409 busy` when the click comes from the API instead of the ticker).
When a run ends, `FinishCalendarRun` writes the outcome on the job in the same
transaction as the run row (`last_run_at`, `last_status`, `last_error` and the
next slot), so the Calendars screen and the following scheduling pass read the
same summary.

A restart closes what it interrupted: `Bootstrap` stores every calendar run left
`running` as `failed` (`the run was interrupted by a restart`) and re-arms each
enabled job whose slot is missing or in the past — the same treatment file jobs
get.

**Sync now** (`POST /api/calendars/:id/run`) starts the same engine in the
background and answers `202` with the run row, so the SPA can follow it.
`POST /api/calendars/:id/cancel` cancels the context of a run in flight and is
idempotent; `DELETE /api/calendars/:id` cancels first and then removes the job,
because a run nobody watches would otherwise keep writing to a calendar. The run
history of a deleted job stays in the database (the rows are the record of what
happened), but the endpoints resolve the job first, so they answer `404`.

Every run is bounded by the instance run timeout (**Admin > Settings**,
`1–1440` minutes, default `30`, the same setting file runs use): when it elapses
the run is stored as `timeout` and its message carries the last error, which is
where an open feed download or provider call shows up. The statuses are the
file-job vocabulary — `running`, `success`, `partial` (something was written and
at least one event failed), `failed`, `cancelled`, `timeout` — so the badge in
both screens behaves the same way.

The history of a job is `GET /api/calendars/:id/runs` (newest first); one run
with its per-event evidence is `GET /api/calendars/:id/runs/:runId`. *Preview*
(`GET /api/calendars/:id/preview`) runs the same engine in dry-run mode, so the
plan it returns cannot disagree with what *Sync now* would do. The newest `50`
runs of a job survive (`runKeepCount`, the file-job bound, applied after every
run), and each run keeps at most the `1000` item rows the endpoint will return.

## 8. API

| Method | Path | Answer |
|---|---|---|
| `GET` | `/api/accounts/:id/calendars` | `{calendars:[{id,name,primary,time_zone?,read_only}]}` |
| `GET` | `/api/calendars` | `{calendars:[calendarView]}` |
| `POST` | `/api/calendars` | **201** `calendarView` |
| `GET` | `/api/calendars/:id` | `calendarView` |
| `PUT` | `/api/calendars/:id` | replaces the job as a whole, recomputes `next_run_at` |
| `DELETE` | `/api/calendars/:id` | cancels a run in flight → `{id, cancelled}` |
| `POST` | `/api/calendars/:id/run` | **202** `{job_id, run}`, `409 busy` while a run is in flight |
| `POST` | `/api/calendars/:id/cancel` | `{job_id, cancelled}` — idempotent |
| `GET` | `/api/calendars/:id/preview` | dry run: `{job_id, window_start, window_end, source_listed, destination_listed, events_scanned, plan:[{uid,summary,start_at,action,evidence}]}` |
| `GET` | `/api/calendars/:id/runs?limit=` | `{runs:[calendarRun]}` (default 20, max 200) |
| `GET` | `/api/calendars/:id/runs/:runId` | `{run, items:[calendarRunItem]}` (default 200, max 1000) |
| `POST` | `/api/calendars/feed-preview` | `{url, username?, password?, past_days?, future_days?}` → `{etag?, last_modified?, window_start, window_end, events, recurring, cancelled, sample:[{uid,summary,start_at,end_at,all_day,recurring}]}` |

Payload fields, validation and error codes: [api.md](api.md) §7.

## 9. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| a run ends `failed` with `the provider token cannot be used any more` (`412 reconnect`) | the refresh token was revoked or expired | reconnect the account |
| `400 validation` — `… does not support calendars in this build` | the account's provider has no calendar half, or the provider credentials are missing | configure the provider, then reconnect the account |
| the Accounts screen lists `calendars` as missing | the account was connected before the calendar scopes existed | reconnect it |
| `400 validation` — `the subscription URL is not publicly routable` | the feed points at a loopback, private, link-local or reserved address | host the feed publicly, or set `CALENDAR_ALLOW_PRIVATE_TARGETS=true` for an internal server |
| `400 validation` — `the payload is not an iCalendar feed` | the URL answered HTML (a login page, a 404 page) | check the URL with *Check subscription*, and the credentials if the feed is protected |
| the preview shows `0 events` | the window does not overlap the feed | widen `past_days` / `future_days` |
| meetings are counted as `skipped` | the event belongs to a recurring series | expected, see §10 |
| `conflicts` grows without any write | both sides changed and `newest_wins` cannot pick a side (no modification stamps) | use `source_wins` (or `destination_wins` on a bidirectional job) |
| a run fails with `the feed password cannot be decrypted` | `ENCRYPTION_KEY` changed after the job was saved | re-enter the feed password |
| a long feed stops at 20000 events | `MaxEvents` | subscribe to a narrower feed, or split the calendar |

## 10. Limits and known gaps

* **Recurring events are not expanded.** A series master and its instances are
  skipped and counted (`events_skipped`), never deleted. Copying one occurrence
  would need an invented rule, and copying the master would duplicate the whole
  series next to the original.
* **Calendar runs do not notify.** The notification events (`sync.success`,
  `sync.run_failed`, …) cover file jobs; a calendar job writes its history to
  `calendar_runs` only.
* **No billing counters.** Calendar runs move no bytes, so the billing report
  does not include them.
* **Retention is per job, not by age.** The purge maintenance action and the
  `/api/maintenance/*` pruners touch file jobs, audits and billing; calendar
  runs are pruned to the newest `50` per job at the end of every run.
* **No guest invitations.** Attendees are part of the content fingerprint and are
  written when the provider accepts them, but the engine never asks a provider
  to notify guests (Google writes always send `sendUpdates=none` and Graph gets
  no `/send` call): a synced copy is a copy, not a meeting request.
* **`.ics` is read-only.** There is no export to a feed.
