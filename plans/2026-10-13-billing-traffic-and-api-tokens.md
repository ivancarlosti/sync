# Plan — Billing traffic, API tokens and the audit depth fix

Status: implemented (2026-10-13).

## 1. Goal

Four additions and one bug fix:

1. **Billing** — per-account inbound/outbound traffic per run, readable by day and
   by month, exposed through the API (so a cost tool can pull it) and shown in a
   new Billing screen.
2. **API tokens** — a real programmatic credential: several named tokens,
   regenerate, revoke/delete, and a secret that is shown **once**.
3. **API tab** under Administration to manage those tokens from the SPA.
4. **Billing schemas** — the Billing report can be labelled/exported with the
   line-item vocabulary of OCI, AWS, GCP, none (bare bytes) or a custom mapping.
5. **Audit fix** — a depth-limited audit under-reported the totals of deep
   folders (and of every ancestor, up to the run total).

## 2. Architecture

### Billing

`SyncRun.BytesTransferred` only ever counted the *uploaded* bytes of a copy, with
no account and no direction. The engine knows both sides at the copy site
(`endpoint.account`), so the accounting is done there:

* **inbound** — bytes *read* from an account (`Download`),
* **outbound** — bytes *written* to an account (`Upload`),
* **operations** — write calls that move no payload (delete, folder creation).

A run accumulates **absolute** per-`(account, direction)` counters and flushes
them (throttled, then once at the end) as rows keyed by
`(run_id, account_id, direction)`. Absolute values keep a retried flush
idempotent; a crash or a cancel keeps whatever the last flush stored, which is the
honest answer for traffic that really did move.

Buckets are **UTC days/months** derived from the run's `started_at` and stored as
text, so the summary query is a plain portable `GROUP BY` on MySQL/MariaDB and on
the SQLite of the tests.

### API tokens

A token is `sync_<prefix>.<secret>`: the 8 hex character `prefix` is public and
indexed (the lookup is a single indexed `SELECT`), the 32 random bytes of `secret`
are only ever stored as SHA-256. Creating or regenerating a token is the **only**
answer that carries the secret; every later view shows the prefix and the metadata.
Authentication is resolved in the same middleware that resolves the session
cookie, ahead of it, and the identity carries the scopes of the token, so a
read-only token is refused on a mutating route by a second guard.


## 3. Model (new tables)

`internal/models/billing.go`

* `TrafficEntry` (`traffic_entries`): `run_id`, `account_id`, `job_id`,
  `direction` (`inbound`/`outbound`), `bytes`, `files`, `operations`, `day`,
  `month`, timestamps. Unique index on `(run_id, account_id, direction)`, plus
  `(account_id, day)` for the report.

`internal/models/token.go`

* `APIToken` (`api_tokens`): `name`, `prefix` (unique), `hash` (never
  serialised), `scopes`, `created_by`, `last_used_at`, `last_used_ip`,
  `expires_at`, `revoked_at`, `requests`, timestamps.

Both are added to the `AutoMigrate` list in `internal/database/database.go`.

## 4. Semantics

* A run records one inbound row per source account and one outbound row per
  destination account; an account that is both (an intra-provider job) gets both.
* `operations` counts deletes and folder creations, so a provider that bills per
  request can be reconciled too.
* Only a **successful** transfer is counted: a download that fails mid-flight
  records nothing (its upload never happened either).
* Traffic rows are pruned with their run (`PruneRuns`).
* An API token is an **administrator** credential: scopes only narrow it
  (`read` = every `GET`, `write` = everything).
* Regenerating a token replaces prefix and secret in place, so the old secret
  stops working immediately while the row (and its audit fields) survives.
* Revoking is soft (the row stays, `revoked_at` is set, authentication fails);
  deleting removes the row.

## 5. Bounds

| Constant | Value | Purpose |
|---|---|---|
| `runKeepCount` | (existing) | runs kept per job by the pruner |
| `trafficProgressEvery` | 5s | throttle for the traffic flush |
| `tokenMaxLifetime` | 365d | expiry clamp |
| `AuditNodeLimit` | 10000 | entries one report may hold |
| `AuditVisitLimit` | 200000 | nodes one walk may visit |
| `AuditMaxDepth` | 25 | requested depth clamp |


## 6. Audit depth — the fix

Before: a folder at `depth >= max_depth` was written with `expanded:false` and
`0` bytes and its children were not listed, so every ancestor (and the
`total_size` of the run) silently under-reported the whole subtree below the cut,
with `truncated:false` and no warning in the UI.

Now: **the depth limits the report, not the walk.**

* The walk always visits and counts the whole subtree; `Depth` only decides which
  nodes are *emitted*, so `total_size`, `files` and `folders` are exact at every
  level, including the boundary folders and the run total.
* `listed` (new on `AuditEntry`, the JSON view and the CSV) says whether a
  folder's children are present in the report; `expanded` keeps its meaning
  ("the walk descended into it", now true for a boundary folder too).
* The report can only be incomplete when a hard budget is hit: `AuditNodeLimit`
  (entries) or the new `AuditVisitLimit` (visited nodes), both of which set
  `truncated:true` — which the UI already badges.
* `max_depth_reached` is the depth really walked, so it can exceed the request.

## 7. Endpoints

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/billing?from=&to=&group_by=&account_id=` | `day` (default), `month`, `account`, `run` |
| `GET` | `/api/billing/runs?from=&to=&account_id=&limit=` | one row per run, both directions |
| `GET` | `/api/billing/export?...` | CSV of the same table, schema-aware |
| `GET` | `/api/tokens` | token views, never the secret |
| `POST` | `/api/tokens` | `{name, scopes?, expires_at?}` → `{token, secret}` (**once**) |
| `POST` | `/api/tokens/:id/regenerate` | → `{token, secret}` (**once**) |
| `POST` | `/api/tokens/:id/revoke` | → `{token}` |
| `DELETE` | `/api/tokens/:id` | `204` |

`GET /api/runs/:id` gains `traffic:[…]`; `GET /api/settings` gains
`billing_schemas:[…]`.

## 8. Defaults

Billing schema `none` (bare bytes), rate `0` (no cost column unless a rate is
set), currency `USD`, token scopes `read` + `write`, token lifetime 365 days,
custom schema document empty, traffic flush every 5s.

## 9. Files

Backend: `internal/models/billing.go` (new), `internal/models/token.go` (new),
`internal/models/audit.go`, `internal/models/enums.go`,
`internal/database/database.go`, `internal/services/billing_schema.go` (new),
`internal/services/apitokens.go` (new), `internal/services/audit.go`,
`internal/services/sync.go`, `internal/services/store.go`,
`internal/services/settings.go`, `internal/handlers/billing.go` (new),
`internal/handlers/tokens.go` (new), `internal/handlers/audits.go`,
`internal/handlers/runs.go`, `internal/handlers/settings.go`,
`internal/handlers/middleware.go`, `internal/handlers/routes.go`,
`internal/handlers/handlers.go` (Deps), `cmd/server/main.go`.

Frontend: `web/src/lib/api.ts`, `web/src/views/BillingView.vue` (new),
`web/src/views/RunsView.vue`, `web/src/views/AuditView.vue`,
`web/src/views/admin/ApiView.vue` (new), `web/src/views/admin/SettingsView.vue`,
`web/src/layouts/AdminLayout.vue`, `web/src/components/AppHeader.vue`,
`web/src/router/index.ts`, seven catalogs in `web/src/i18n/locales/`.

Docs: `docs/api.md`, `docs/authentication.md`, `docs/database.md`,
`docs/architecture.md`, `docs/i18n.md`.

Tests: `internal/services/billing_test.go` (new),
`internal/services/apitokens_test.go` (new), `internal/services/audit_test.go`,
`internal/services/sync_test.go`, additions to
`internal/handlers/handlers_test.go`.

## 10. Verification

```bash
cd web && npm run typecheck
export PATH=$HOME/.local/goroot/bin:$PATH
go build ./... && gofmt -l ./internal && go vet ./... && go test ./...
```

Plus a live check of the new surface:

```bash
go run ./cmd/server                       # then, with a freshly minted token
curl -H "Authorization: Bearer sync_…" 'http://127.0.0.1:3000/api/billing?group_by=day'
```

## 11. Progress log

* 2026-10-13 — plan written; audit depth semantics settled (depth limits the
  listing, not the accounting).
* 2026-10-13 — **A (audit fix)** done: `expand` now walks and counts the whole
  subtree while the depth only gates `emit`; `AuditEntry.Listed` (JSON + CSV) says
  whether a folder's children are part of the report; `AuditVisitLimit` (200 000
  visited nodes) joined `AuditNodeLimit` as the only source of `truncated`.
  `TestAuditDepthBoundary` rewritten (exact totals below the cut), a `visit`
  budget test added, and the handler lifecycle test seeds a real tree below the
  boundary (`inside.txt` is counted but not listed).
* 2026-10-13 — **C (API tokens)** done: `models/token.go`,
  `services/apitokens.go`, `api_tokens` in the migration, bearer/`X-API-Key` in
  `handlers/middleware.go`, `requireScope` on the whole `/secured` tree,
  `handlers/tokens.go` + routes + `Deps.APITokens`, `ApiView.vue` behind the new
  `admin-api` tab, seven catalogs, docs (`api.md` §9, `authentication.md` §6).
* 2026-10-13 — **B (billing)** done: `models/billing.go`, traffic accounting in
  `services/billing.go` hooked into `copy`/`remove`/`ensureFolder` (+ one forced
  flush per run), `Store.SaveTraffic`/`BillingSummary`/`ListTrafficRuns`/
  `GetRunTraffic`, `PruneRuns` takes the rows with their run, `handlers/billing.go`
  (report, per-run breakdown, CSV) + `traffic[]` on the run detail, `BillingView.vue`
  and the traffic table of a run in `RunsView.vue`.
* 2026-10-13 — **E (schemas)** done: `services/billing_schema.go` (none/OCI/AWS/GCP/
  custom, unit, price per GiB, currency), the four settings keys seeded and
  validated, `billing_schemas[]` in `GET /api/settings`, the billing card in
  Admin > Settings.
* 2026-10-13 — incidental fix found by the suite: `SyncService.Cancel` could lose a
  cancellation that arrived between the 202 answer and the goroutine reaching
  `execute` (`runState.cancelled`, applied by `attach`), which left a run in
  flight forever and hung `TestSyncServiceRunLifecycle` (pre-existing on `HEAD`).
* 2026-10-13 — verification: `gofmt -l`, `go vet ./...`, `go test ./...` and
  `npm run typecheck && npm run build` all clean.
* 2026-10-13 — live check against a real `mariadb:11` (Docker, throwaway):
  the binary boots, migrates (`traffic_entries`, `api_tokens`), then
  `POST /api/tokens` (read-only) → `GET /api/billing` 200 with the OCI schema,
  `POST /api/maintenance/oauth/states/prune` with that token → **403
  `forbidden`**, `X-API-Key` accepted, a wrong secret → **401**, a traffic row
  inserted in SQL appears in the day/month report with `runs`/`operations` and in
  the CSV as `1.000000,1.000000,3,2.000000,0.020000,GiB,USD`, and revoke →
  401 / delete → 204.
