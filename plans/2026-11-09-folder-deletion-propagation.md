# A folder deleted on one side was re-created instead of being deleted

* **Date:** 2026-11-09
* **Repo:** `ivancarlosti/sync`
* **Status:** done (see the progress log at the bottom)
* **Related:** [`2026-10-15-mirror-empty-folders.md`](./2026-10-15-mirror-empty-folders.md), [`../docs/architecture.md`](../docs/architecture.md), [`../docs/api.md`](../docs/api.md), [`../docs/database.md`](../docs/database.md)

## 1. Context

With **Mirror empty folders** (`sync_empty_folders`) on, deleting a folder on one
side of a **bidirectional** job does not delete it on the other side: the next run
brings it back.

The run does three folder-relevant passes (`internal/services/sync.go`,
`reconcile`):

1. `walk` lists both trees;
2. `mirrorFolders` → `createFolders` **creates**, in both directions, every
   folder the other side lists and this one lacks;
3. `reconcilePaths` transfers the files;
4. `removeEmptyFolders` → `deleteEmptyFolders` removes, on the side the job
   writes to, the folders the *other* side no longer lists.

A deletion on side A is observed as "A does not list the folder, B still does".
The mirroring pass reads exactly the same observation as "B has a folder A is
missing" and re-creates it, so by the time the pruning pass runs both sides list
it again (`keep[relative] = true`) and nothing is removed.

**The observation alone is ambiguous**, and a bidirectional job with
`delete_missing` allows both readings:

* "the folder is new on B and has not travelled to A yet" → create on A;
* "the folder was deleted on A and B still has it" → delete on B.

Files already solve this with the state table `sync_files` (`decide`,
`decideMissingSource`, `decideMissingTarget`): *tracked + missing on one side +
`delete_missing`* means deleted, *untracked* means new. Folders had no state at
all, so mirroring always won. Reordering the passes cannot fix it: the surviving
side still lists the folder, and a brand-new folder is indistinguishable from a
deleted one without memory.

## 2. Goal

Propagate a folder deletion in the direction the job writes, and never resurrect
it, while keeping the properties the feature already promises:

* an empty folder that is genuinely new still travels (the point of
  `sync_empty_folders`);
* a folder is only ever deleted when nothing is left in it — the narrow rule of
  `2026-10-15-mirror-empty-folders.md` §3.4 (no file, no item V1 cannot copy, no
  subfolder);
* the one-way behaviours stay as they are: `delete_missing` still prunes the
  write side, and a folder deleted on the write side is still restored when the
  deletion cannot be propagated;
* one-way jobs, jobs with both options off, and the existing test suite keep
  passing unchanged.

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| 3.1 | A new state table `sync_folders` (`job_id` + `path`, unique, timestamps) records the folders **both** sides listed at the end of the last run | it is the same shape as `sync_files` and the only way to tell the two readings of one listing apart |
| 3.2 | Rows are inserted for new shared paths only (`ON CONFLICT DO NOTHING`) and dropped for paths neither side lists any more | the delta is tiny on a steady tree; rewriting the whole tree every run would be wasteful |
| 3.3 | The state is written even when both options are off | the memory must exist the moment an operator enables either option, and a steady tree costs zero writes |
| 3.4 | A one-sided folder has two candidate actions, gated like a file: `canDelete = delete_missing && (the side that still has it is a side the job writes)` and `canCreate = sync_empty_folders && (the side missing it is a side the job writes)` | the options stay independent and per-direction, exactly as they are for files |
| 3.5 | When only one candidate is possible it applies unchanged (today's behaviour); when **both** are possible — the bidirectional + `delete_missing` cell — the state decides: tracked ⇒ delete on the surviving side, untracked ⇒ create on the missing side | this is the reported bug, and the untracked half is what keeps a genuinely new folder travelling both ways |
| 3.6 | Emptiness is evaluated **at deletion time** from the run's snapshot, with a per-parent count of files, unsupported items and surviving subfolders, deepest first | the existing rule, but a child whose own deletion was blocked or failed now also protects its parent instead of failing the run |
| 3.7 | The pass order is unchanged: creates → files → deletes | every upload still finds its parents, and a folder whose last file this run deleted is seen empty |
| 3.8 | A blocked deletion is a `skipped` item with evidence, not a run error, and it does **not** bump `files_skipped` | nothing failed, and the folder counters stay about folders |
| 3.9 | No API, UI or i18n change | `folder_created`, `folder_deleted`, `folders_deleted` and `skipped` already exist and render already |
| 3.10 | The first run after the upgrade has no state, so a one-sided folder counts as new and is mirrored once; deletions propagate from the next run on | honest bootstrap: there is nothing to remember yet. Documented in `docs/api.md` §6 |

## 4. Steps

1. `internal/models/sync.go` — `SyncFolder` (`ID`, `JobID`, `Path`,
   `CreatedAt`, `UpdatedAt`; unique index `idx_syncfolder_job_path`).
2. `internal/database/database.go` — register `&models.SyncFolder{}` in `Migrate`.
3. `internal/services/store.go` — `FolderState`, `SaveFolders` (chunked bulk
   insert, `DO NOTHING`), `DeleteFolders`; `DeleteJob` and the orphan purge of
   the account deletion drop `sync_folders` rows too.
4. `internal/services/sync.go` — load the state in `reconcile`; replace
   `mirrorFolders`/`createFolders`/`removeEmptyFolders`/`deleteEmptyFolders` with
   `planFolders` + `createFolders(plan.creates)` + `deleteFolders(plan.deletes)`
   and `rememberFolders`; `removeFolder` takes the evidence string of the plan.
5. Tests in `internal/services/sync_test.go` (§5).
6. Documentation: `docs/database.md` (table + retention row), `docs/architecture.md`
   §3, `docs/api.md` §6.

## 5. Verification

* `TestEnginePropagatesFolderDeletion` — bidirectional, mirroring and deletions
  on: run 1 mirrors `empty` and `docs`; deleting `empty` on the source deletes it
  on the destination (`FoldersDeleted == 1`, `folder_deleted`, state row dropped).
* `TestEnginePropagatesFolderDeletionFromTheDestination` — the same deletion made
  on the destination side.
* `TestEngineDoesNotDeleteANewFolder` — an untracked folder is created on the
  other side, never deleted, whichever side it appeared on (the failure mode of
  an order-only or state-less rule).
* `TestEngineRestoresAFolderWithoutDeleteMissing` — with `delete_missing` off a
  bidirectional job restores the folder (`folder_created`, state row kept), the
  same way it restores a file.
* `TestEngineKeepsAFolderHoldingContentWhenDeletedElsewhere` — a native document
  protects the folder it sits in; the run records `skipped` and stays green.
* `TestEnginePrunesUntrackedDestinationFolders` — a one-way job keeps pruning a
  stray empty destination folder.
* Mutation check: dropping the `tracked` branch and letting the create branch win
  turns the two propagation tests red; letting the delete branch win for untracked
  folders turns `TestEngineDoesNotDeleteANewFolder` and both propagation tests red
  (`TestEngineMirrorsEmptyFoldersBack` stays green under both, because its
  `delete_missing` is off — which is the point of the two-option gating).
* Green: `gofmt -l ./internal`, `go build ./...`, `go vet ./...`,
  `go test ./...`, `cd web && npm run typecheck`.

## 6. Risks

* **Bootstrap run** (3.10): a deletion made before the upgrade is mirrored once
  and then propagates; the alternative (deleting on the strength of an empty
  state) could discard a folder that never travelled.
* **Root changes**: the state paths are relative to the job's roots and
  `Store.SaveJob` cannot tell a root change from an edit, so a job pointed at a
  different folder keeps stale rows — the same latent issue `sync_files` already
  has. Not addressed here; the state is bounded by the tree it tracks.
* **Renames** now surface as delete + create when `delete_missing` is on (the old
  path is tracked and one-sided). Documented, and the safer of the two readings.
* **First-run write volume**: a job whose tree both sides share inserts one row
  per shared folder the first time. Chunked at 500 rows per statement; afterwards
  only the delta.
* **Two jobs over overlapping trees** still take independent decisions (the lease
  is per job). Unchanged, already documented in the mirroring plan's follow-up.

## 7. Progress log

* 2026-11-09 — reproduced by reading the three passes: `mirrorFolders` creates on
  both sides before `reconcilePaths`, so the deletion never reaches
  `deleteEmptyFolders`; agreed with the operator on the `sync_folders` state
  table (over a JSON column on `sync_jobs`, a timestamp heuristic, or an
  order-only change).
* 2026-11-09 — implemented the table, the store methods, the folder plan, the
  deletion guard and the tests of §5; updated the documentation.
* 2026-11-09 — two corrections found by the new tests and by review:
  `deleteFolders` no longer drops the state row of the folder it removed from the
  in-memory map — `rememberFolders` decides that from the listings, and a row
  removed early was never seen as "gone from both sides", so it lingered in the
  database and the folder would have been read as tracked on a later run. A folder
  the emptiness guard keeps is now recorded as `skipped` (with the evidence) and
  counted in `files_skipped`, like every other item that is left alone; without
  the item an operator saw a folder the run had planned to delete and never
  explained why it survived. `go test -race ./internal/services` is clean too.
