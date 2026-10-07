# Plan — Audit content report

Status: implemented (2026-10-07).

## 1. Goal

Add an **Audit** tab to the Sync SPA. An audit walks a SharePoint / shared-drive
(provider) folder tree to a chosen depth, produces a *content report* (folders and
files with sizes and subtree totals) and offers a **server-side CSV download** of
that report.

The work mirrors the existing Runs machinery end to end (models → service →
handlers → Store → migration → wiring → tests → frontend → i18n → docs) so a new
contributor recognises every moving part.

## 2. Architecture

An audit is an **async job**, exactly like a sync run:

1. `POST /api/audits` authenticates, reserves the account, writes an `audit_runs`
   row in the `running` state and launches a goroutine on `context.Background()`.
   The handler answers **202** with the new `auditView` the UI polls.
2. The goroutine lists the tree (reusing the engine's token freshen/reissue-on-401
   behaviour), buffers the entries in memory, then batch-inserts them (their `id`
   order is the DFS preorder of the walk) and finalises the run row.
3. `GET /api/audits/:id` returns the run plus its entries; the report is
   **persisted**, so the CSV download and every reload read the database and never
   re-scan the provider.
4. `GET /api/audits/:id/export` streams the persisted report as CSV.

Live progress is written to the row (throttled) so the UI shows counters while the
walk is in flight — a small addition over `SyncRun`.

## 3. Model (new tables)

`internal/models/audit.go`

- `AuditRun` (`audit_runs`): account/provider/drive/root, `max_depth`, status,
  trigger, timing, summary counters (`files`, `folders`, `total_size`,
  `max_depth_reached`), `truncated`, `message`.
- `AuditEntry` (`audit_entries`): one node, `kind` (`folder`/`file`), `path`,
  `name`, `depth`, `size`, `total_size`, `files`, `folders`, `expanded`,
  `modified_at`, `mime_type`.

`AuditKind` (`folder`, `file`) joins the enum vocabulary in `models/enums.go`.

## 4. Semantics

- The **selected folder is depth 0**; its children are depth 1, and so on.
- A folder at `depth >= max_depth` is recorded but **not expanded**
  (`expanded:false`, subtree sizes `0`).
- `max_depth = 0` means **unlimited**.
- Entries are emitted in **DFS preorder**, folders before files, alphabetical.
- A folder entry carries the aggregate of its subtree (`total_size`, `files`,
  `folders`).

## 5. Bounds

| Constant | Value | Purpose |
|---|---|---|
| `AuditNodeLimit` | 10000 | entry cap; `Truncated` + stop when hit |
| `AuditMaxDepth` | 25 | requested depth clamp |
| `auditKeepCount` | 50 | audits per account kept by the pruner |
| `auditProgressEvery` | 2s | throttle for the live-counter writes |

One audit per account at a time (`ErrBusy` → **409**).

## 6. CSV contract

`Content-Type: text/csv; charset=utf-8`,
`Content-Disposition: attachment; filename="audit-<id>.csv"`, UTF-8 BOM +
`encoding/csv`, English headers:
`kind,path,name,depth,size,total_size,files,folders,expanded,modified_at,mime_type`.

## 7. Endpoints

| Method | Path | Notes |
|---|---|---|
| `POST` | `/api/audits` | body below → **202** `{audit}`; **409 busy** |
| `GET` | `/api/audits?limit=` | `{audits:[auditView]}` |
| `GET` | `/api/audits/:id` | `{audit, entries:[auditEntryView]}` |
| `GET` | `/api/audits/:id/export` | CSV |
| `POST` | `/api/audits/:id/cancel` | `{id, cancelled}` |
| `POST` | `/api/maintenance/audits/prune` | `{keep}` → `{keep, pruned:true}` |

## 8. Defaults

Retention 50, progress ~2s, filename `audit-<id>.csv`, node cap 10000, depth clamp
25, one concurrent audit per account, top-level `/api/audits`.

## 9. Files

Backend: `internal/models/audit.go`, `internal/models/enums.go`,
`internal/database/database.go` (AutoMigrate), `internal/services/audit.go`,
`internal/services/store.go`, `internal/handlers/audits.go`,
`internal/handlers/routes.go`, `internal/handlers/handlers.go` (Deps),
`internal/handlers/maintenance.go`, `internal/services/scheduler.go`,
`cmd/server/main.go`.

Frontend: `web/src/lib/api.ts`, `web/src/views/AuditView.vue`,
`web/src/router/index.ts`, `web/src/components/AppHeader.vue`, seven catalogs in
`web/src/i18n/locales/`.

Docs: `docs/api.md`, `docs/architecture.md`, `docs/i18n.md`,
`docs/development.md`.

Tests: `internal/services/audit_test.go`, additions to
`internal/handlers/handlers_test.go`.

## 10. Verification

```bash
cd web && npm run typecheck
export PATH=$HOME/.local/goroot/bin:$PATH
go build ./... && gofmt -l ./internal && go vet ./... && go test ./...
```
