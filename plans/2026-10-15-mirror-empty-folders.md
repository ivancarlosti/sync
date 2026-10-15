# Empty folders never travelled, and a deletion left the folder behind

* **Date:** 2026-10-15
* **Repo:** `ivancarlosti/sync`
* **Status:** done (see the progress log at the bottom)
* **Related:** [`2026-10-06-personal-account-sync.md`](./2026-10-06-personal-account-sync.md), [`2026-10-13-billing-traffic-and-api-tokens.md`](./2026-10-13-billing-traffic-and-api-tokens.md), [`../docs/architecture.md`](../docs/architecture.md), [`../docs/api.md`](../docs/api.md)

## 1. Objective

A run only ever touched a folder while it was copying or updating a file inside
it, so the folder tree was reproduced as a *side effect* of the files:

* a folder that holds no file never reached the other side — an operator who
  organises a tree with empty placeholders gets one side that looks different from
  the other one, and no run ever fixes it (`folders_created` only counted the
  parents created on demand by an upload);
* `delete_missing` removed the file but left the folder it lived in on the other
  side, so the deletions propagated and the empty shells stayed behind forever.

The wanted end state: a per-job option that mirrors the folder tree itself
(default on, so the tree is what an operator expects), and folder removal as part
of `delete_missing` — narrow, so it can never discard content — with both halves
visible in the run history (`folder_created` / `folder_deleted` items and a
`folders_deleted` counter) and switchable from the job editor.

## 2. Root cause

| # | Symptom | Cause |
|---|---|---|
| 2.1 | an empty folder is never created on the other side | `engine.reconcilePaths` → `one` → `transfer` calls `ensureFolder` only for the parent of a file it is about to write, so a folder nothing is written into is never visited. The listing already knows the whole tree (`engine.walk` fills `endpoint.folders` for every folder it descends into), but nothing consumed that half |
| 2.2 | a deletion leaves the empty folder on the other side | `delete_missing` removes files through `remove`; the folder is not part of the plan, and `SyncRun` has no `FoldersDeleted` and `ItemAction` no `folder_deleted` |
| 2.3 | there was no place to store the choice | `sync_jobs` carries `delete_missing` and nothing about folders, and both the API payload and the editor form mirror the columns |
| 2.4 | the option cannot be a plain `bool` column | GORM replaces the zero value of a bool field that carries a column default and writes the replacement **back into the struct** (`gorm.io/gorm/callbacks/create.go`), so `SyncEmptyFolders` with `default:true` would turn a job saved with the switch off silently back on — reproduced with a throwaway test before the fix |


## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| 3.1 | Two independent options: mirroring (`sync_empty_folders`, creates) and `delete_missing` (removes). Neither implies the other | an operator who wants the tree mirrored does not necessarily want anything deleted, and the reverse; the two are separate decisions (`docs/api.md` §6) |
| 3.2 | Mirroring runs **before** the file reconciliation, and only ever creates | every upload then finds its parents already created (one code path for a folder, not two), and "create" cannot be mistaken for "delete" — removals stay in 3.3 |
| 3.3 | Removal runs **after** the file reconciliation | a folder whose last file this run deleted has to be seen as the empty folder it now is, otherwise its removal waits for the next run |
| 3.4 | A folder is removed only when the other side does not list it **and** it holds nothing: no file, no item V1 cannot copy (native document, shortcut) and no subfolder | the rule is narrow on purpose. Content that lives only on the deleted side — an upload by hand, a Google document the engine cannot transfer — protects the folder it sits in, so the operation can never discard data, only empty shells the other side no longer has |
| 3.5 | Deepest paths first (reverse lexicographic; a child always sorts after its own parent) | providers refuse to delete a folder that still contains a subfolder, so a bottom-up order makes the nested case work without a second pass |
| 3.6 | The set of folders that must stay is built ancestor-closed (every folder the other side lists, plus every folder that holds something) | one map decides everything; an ancestor of a kept folder is kept too, which is what makes 3.4 cheap to read |
| 3.7 | `sync_jobs.sync_empty_folders` is a `BOOLEAN` with `default:true`, migrated by `AutoMigrate` | the rows stored before the option existed inherit "mirror the tree": the previous behaviour is only *narrower* than the new default, so no existing job changes meaning except by gaining the folder half of it |
| 3.8 | `Store.SaveJob` captures the requested value, writes it again with an `UPDATE` when GORM substituted the default, and restores the struct field | 2.4, with the compensation kept in one place: the API, the scheduler and the tests all save through it. The write-back matters on drivers that do not return the updated row into the model |
| 3.9 | `jobInput.sync_empty_folders` is a `*bool`: an omitted value keeps the stored setting, and a new job is seeded with `true` | a payload written before the field existed (a script, an older client) must not turn mirroring off by accident, and `PUT /api/jobs/:id` replaces the job as a whole (`docs/api.md` §6) |
| 3.10 | New `sync_runs.folders_deleted` column and `folder_deleted` action; the run view shows the counter and the item rows label the action | the history has to explain what the new passes did, exactly like `folder_created` already did |
| 3.11 | The editor gets a "Mirror empty folders" switch, and `deleteMissingHint` now names the folder it removes | the operator sees both halves of the behaviour where the options are edited |
| 3.12 | A failed folder creation or deletion is one run error, retried once after a token renewal, never fatal | a folder is not a payload: one failing folder must not abort a tree of thousands, while an absolute refusal (a provider that cannot delete) still ends the run through `e.fail` |


## 4. Implementation

### 4.1 `internal/models/`

* `sync.go` — `SyncJob.SyncEmptyFolders` (`gorm:"default:true"`,
  `json:"sync_empty_folders"`, documented as mirrored by default) and
  `SyncRun.FoldersDeleted`;
* `enums.go` — `ActionFolderDeleted` (`folder_deleted`).

### 4.2 `internal/services/sync.go`

* `reconcile` — mirroring between the listings and the file reconciliation
  (3.2) and removal after it (3.3), each gated by its own job option;
* `mirrorFolders` / `createFolders` — one direction each, sorted so a parent is
  created before its children, never touching a path the other side holds as a
  file (one name cannot be a file and a folder at once: that path belongs to the
  file reconciliation);
* `removeEmptyFolders` / `deleteEmptyFolders` — the ancestor-closed `keep` set
  (3.6), the reverse order (3.5) and the narrow emptiness rule (3.4);
* `removeFolder` — deletes one folder, leaves `sync_files` alone (a folder is
  never an entry in the state table), and accounts the traffic entry, the
  `folders_deleted` counter and the `folder_deleted` item; `ensureFolder` already
  owned the `folder_created` half.

### 4.3 `internal/services/store.go`

* `SaveJob` — the compensation of 3.8, documented with the GORM behaviour it
  works around.

### 4.4 `internal/handlers/jobs.go`

* `jobInput.SyncEmptyFolders *bool` (3.9), applied by the shared `applyJobInput`
  (so create and update agree), and `handleCreateJob` seeding a fresh job with
  `true`.

### 4.5 `web/src/`

* `lib/api.ts` — `sync_empty_folders` on `SyncJob`/`JobPayload` and
  `folders_deleted` on `SyncRun`;
* `lib/jobs.ts` — both payload builders carry the field (`payloadFromJob`, and
  `newJobPayload` with `true`);

## 5. Tests

* `internal/services/sync_test.go` — `TestEngineMirrorsEmptyFolders` (the empty
  folder and the whole tree reach the other side, with the `folder_created`
  item), `TestEngineSkipsEmptyFoldersWhenDisabled`,
  `TestEngineMirrorsEmptyFoldersBack` (a bidirectional job mirrors both ways),
  `TestEngineKeepsFoldersWhenMirroringIsOff`,
  `TestEngineDeletesEmptiedFolders` (the folder whose last file this run deleted,
  deepest first, with `folder_deleted` items),
  `TestEngineKeepsFoldersHoldingSomething` (a native document and a subfolder
  each protect their parent), `TestEngineKeepsFoldersWithoutDeleteMissing` and
  `TestEngineFolderDeletionRenewsToken` (the renewal-and-retry path of 3.12).
* `internal/services/store_test.go` — `TestSaveJobKeepsEmptyFoldersOff`: the
  option stays off across a save (3.8), an update turns it back on, and a row
  inserted without the column inherits `true` (3.7).
* `internal/handlers/handlers_test.go` — `TestJobSyncEmptyFoldersRoundTrip`
  (3.9: off is stored and re-read, an update that omits the field keeps it, an
  update that names it applies it) plus the default of the create test.
* Mutation checks, run by hand and reverted: dropping the mirroring and removal
  passes fails the engine tests (which name the counters and the missing item
  actions), and reducing `SaveJob` to a plain `Save` fails the store test with
  *"saving the job changed the value the caller passed"*.
* Green: `gofmt -l ./internal` (empty), `go build ./...`, `go vet ./...`,
  `go test ./...`, and `cd web && npm run typecheck` (`check-i18n: ok — 7
  catalogs × 672 keys, 481 keys referenced from src/`, `vue-tsc` clean).

## 6. Progress log

* 2026-10-15 — reconnaissance: 2.1 traced through `reconcilePaths`/`transfer`
  (the listing already holds the tree, nothing consumes it), 2.2 through
  `remove` and the `ItemAction` table, 2.3 through the model, the handler payload
  and the editor form; 2.4 found by reading the GORM create callback
  (`field.DefaultValueInterface` → `field.Set`), then reproduced with a throwaway
  test before writing the fix.
* 2026-10-15 — agreed with the operator: two independent options, mirroring first
  and removal last, the narrow emptiness rule of 3.4, the column default of 3.7,
  and the snake_case action names for the badge.
* 2026-10-15 — implemented 4.1–4.5, the tests of §5 and the mutation checks, then
  the documentation (`docs/api.md` §6, `docs/database.md`,
  `docs/architecture.md` §3, the checker output in `docs/i18n.md` §6).

## 7. Follow-up

* **Only folders the two listings hold can be pruned.** Removal decides from the
  trees of the run, so a folder the deleted side does not list (a permission the
  account lost, a listing the walk could not descend into) is invisible and left
  alone: fail closed, never delete what was not seen.
* **Nothing enforces one folder rule across jobs.** Two jobs over overlapping
  trees can disagree (one mirrors, one prunes); the lease is per job, so they
  serialise per job only — the same is already true of `delete_missing` for
  files.
* **A provider that cannot delete fails the pruning pass folder by folder.** Each
  failure is one run error and the pass continues; should a provider ever refuse
  deletions structurally, the honest shape is a capability check before the pass
  rather than one error per folder.
* **The mirroring pass walks `endpoint.folders` once more.** That is cheap next to
  the provider calls, but folding it into the file reconciliation would save the
  loop if it ever shows up in a profile.

* `views/JobEditorView.vue` — the "Mirror empty folders" switch above the
  propagate-deletions one;
* `views/RunsView.vue` + `components/StatusBadge.vue` — the `folders_deleted`
  counter and the `folder_created` / `folder_deleted` labels (the API sends the
  snake_case action names, so those are the keys the badge looks up);
* the seven catalogs — `jobs.syncEmptyFolders`, `jobs.syncEmptyFoldersHint`, the
  updated `jobs.deleteMissingHint`, `runs.foldersDeleted`,
  `status.folder_created` and `status.folder_deleted`.
