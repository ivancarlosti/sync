# Activity log capture: enable switch, interval and polling pace

## Context

The audit-log collector (`AuditLogService.Collect`, run by the logs loop of the
scheduler) polls every stream of every connected organisation once per
`DefaultAuditLogInterval` (10 minutes, a constant). Two problems follow:

1. **It cannot be switched off.** An instance that only wants the history, or one
   that is being throttled by a tenant, has no way to stop the background poll —
   the only control is to stop the whole container.
2. **The interval is a constant.** A tenant with many subscriptions, or one whose
   other tooling already reads the Office 365 management API, answers HTTP 429 to
   `directoryAuditLogs` because the pass reads every stream of every organisation
   back to back. The retry policy absorbs a single throttle
   (`internal/providers/retry.go`), but a pass that keeps hammering the same
   tenant inside one minute exhausts it. There is no knob for either the interval
   or the pace between the calls of one pass.

Everything else in this area is already an operator setting (retention, purge,
run timeout), read through `Store.Settings()` the way `PurgeService.Policy` does,
so the same pattern applies here: no new wiring, no new constructor argument.

## Goal

- **Admin > Settings** gains an *Activity log capture* card with a switch, an
  interval in minutes and a pause between provider calls in seconds.
- The scheduler polls only when the switch is on, and only after the configured
  interval has elapsed since the previous pass finished.
- A pass pauses the configured time between the provider calls it makes, which is
  what stops one organisation's streams from tripping the per-tenant throttle.
- `POST /api/maintenance/auditlogs/collect` (the *Collect* button) still forces a
  pass, switch off or not — that is the documented way to verify a grant.
- The Audit Logs screen says whether the schedule is off, and when the next pass
  is due, so a stale list is explained on the screen that shows it.

## Steps

1. **Settings keys** (`internal/models/enums.go`): `auditlogs_enabled`,
   `auditlogs_interval_minutes`, `auditlogs_request_delay_seconds`.
2. **Service settings** (`internal/services/settings.go`): three `AppSettings`
   fields, seeded in `Defaults()`, read and clamped in `Load()`, validated in
   `Update()` (interval `1–1440` minutes, pause `0–300` seconds), plus the bounds
   as constants and `clampAuditLogInterval` / `clampAuditLogDelay`.
3. **Collector policy** (`internal/services/auditlog.go`): `AuditLogPolicy` with
   `Policy(ctx)` (reads the three keys through `Store.Settings()`), and a paced
   pass: `Collect` reads the policy once and `collectGroup` waits the pause before
   each provider call except the first, aborting on a cancelled context.
4. **Scheduler** (`internal/services/scheduler.go`): the logs loop ticks at a
   fixed `DefaultAuditLogTick` (one minute) and runs the pass only when enabled and
   due (`RunScheduledAuditLogs`), recording the end of every pass — scheduled or
   forced — as the start of the next interval. `AuditLogSchedule(ctx)` reports the
   configuration and the next due pass for the API.
5. **API** (`internal/handlers/auditlogs.go`): `GET /api/audit-logs` and
   `GET /api/audit-logs/cursors` answer a `schedule` object next to the cursors.
6. **SPA**: `web/src/lib/api.ts` (settings fields, `AuditLogSchedule`, the
   `schedule` member of the page answer), the capture card in
   `web/src/views/admin/SettingsView.vue`, and the paused/next-pass line in
   `web/src/views/AuditLogsView.vue`, with keys in all seven catalogs.
7. **Docs**: `docs/audit-logs.md` (the schedule and the throttle advice),
   `docs/api.md` (settings payload and the `schedule` member) and
   `docs/configuration.md` (runtime changes table).
8. **Tests**: settings validation through the API, the policy clamping, and the
   scheduled pass honouring both the switch and the interval.

## Risks

- **A disabled collector stops growing the history.** The Audit Logs screen must
  say so, otherwise a quiet list looks like a broken grant (hence step 5 and 6).
- **A one minute tick is more wake-ups than a ten minute tick**, but each one is a
  settings read that says "not due"; the pass itself still runs at the operator's
  interval.
- **A long interval delays a new tenant's backfill.** The first pass of a process
  runs at start-up (as before) so a restart triggers a fresh read.
- **The pause lengthens a pass** by `delay × (calls − 1)`; the pass has its own
  request timeout per call and the loop never overlaps a pass with itself, so a
  large pause costs latency, not correctness.
- **A hand-edited database** could hold an absurd interval; both the write path
  and the read path clamp it, so the loop can never be told to poll every second.
