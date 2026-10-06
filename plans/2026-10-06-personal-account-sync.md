# Personal Microsoft accounts: `ObjectHandle is Invalid`, and one account on both ends

* **Date:** 2026-10-06
* **Repo:** `ivancarlosti/sync`
* **Status:** done (see the progress log at the bottom)
* **Related:** [`2026-10-05-sharepoint-library-picker.md`](./2026-10-05-sharepoint-library-picker.md), [`2026-10-05-provider-aware-folder-picker.md`](./2026-10-05-provider-aware-folder-picker.md), [`../docs/providers.md`](../docs/providers.md), [`../docs/api.md`](../docs/api.md)

## 1. Objective

Connecting a **personal** Microsoft account (an MSA, `@outlook.com`,
`@hotmail.com`, `@live.com`, …) leaves the folder picker unusable and makes a
Microsoft-only setup impossible:

1. Browsing a root answers **HTTP 400 `invalidRequest` / "ObjectHandle is
   Invalid"** instead of listing folders.
2. `/sites` answers a Graph error for such an account, which the API turned into
   a **502** — an empty SharePoint field looked like a broken instance.
3. A job whose source and destination are **the same account** was refused
   outright (`400 validation: the source and the destination must be different
   accounts`), even though the engine has always supported intra-provider jobs
   (`services.applyDirections`, README, `docs/providers.md`).

The wanted end state: a personal account syncs its OneDrive (into another folder
of the same OneDrive, or into a second account), and the picker never offers a
root it cannot open.

## 2. Root cause

| # | Symptom | Cause |
|---|---|---|
| 2.1 | `ObjectHandle is Invalid` on `/drives/{id}/items` or `/drives/{id}/root` | `Drives()` built the picker's list from `GET /me/drives` alone and took **the first drive tagged `driveType: personal`**. A recently created MSA lists internal **bookkeeping drives** there — `ODCMetadataArchive` and the `Bundles_*` drives — tagged `driveType: personal` exactly like the real OneDrive. Only the drive `GET /me/drive` (singular) names responds; the others answer 400 `invalidRequest` with *ObjectHandle is Invalid* |
| 2.2 | The picker opened the broken root even with a healthy account | `FolderPicker.vue` `start()` did `drives.find(isOneDriveKind) ?? drives[0]`, so whichever `personal` drive arrived first became the default |
| 2.3 | `GET /api/accounts/:id/sites` → **502** on a personal account | Graph refuses `/sites` for an MSA (*This API is not supported for MSA accounts*): a personal directory holds no `Microsoft.FileServices` address, so there is no tenant-wide site collection to enumerate. `SearchSites` let the Graph error out and the handler mapped it to 502 |
| 2.4 | Same account on both ends → `400 validation` | `internal/handlers/jobs.go` compared the two accounts (`source.ID == destination.ID`) and `JobEditorView.vue` mirrored it as `validation.differentAccounts` |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| 3.1 | Resolve the account's own drive with `GET /me/drive` (`ownDrive`) and list it **first** | the singular endpoint always names the one drive that answers; being first is also what makes the picker's default root correct without a special case |
| 3.2 | Drop every `driveType: personal` entry of `/me/drives` that is not that drive (and its duplicate) | the bookkeeping drives are indistinguishable by type alone; anything else tagged `personal` on the same account is not a folder tree. `documentLibrary` (and a missing type) still passes through, so a work account keeps working even if `/me/drive` is unreadable |
| 3.3 | `SearchSites` recognises a personal account through the same `ownDrive` probe and answers an **empty list**, best effort | Graph cannot be asked "are you an MSA?"; the own drive answers it. A failure of the probe is ignored so a SharePoint search on a work account is never blocked by it |
| 3.4 | The guard becomes a **location** guard: same account + same drive + same folder → `400 validation` | that is the only genuinely destructive pairing; two different folders of one account are a legitimate job |
| 3.5 | A **nesting** guard refuses a destination inside the source (in the direction the job copies) | copying a folder into a folder below itself grows the tree on every run; the check mirrors `applyDirections` (`forward`/`backward`) so a reverse-direction job is checked the other way round |
| 3.6 | The nesting guard compares the display `*_folder_path` values and reports `false` for an empty one | the ids encode no hierarchy, so the picker's paths are the only available evidence; an empty path simply disables the check instead of refusing a job the editor has not filled in yet |
| 3.7 | The picker hides the site field and the library select when there is no library to offer, and says why (`browser.noLibraryHint`) | an empty select reads as a bug; naming the limitation is the fix for the UI half of 2.3. The hint is worded for any account that exposes no library, so it is not wrong on a work account that has none shared with it either |
| 3.8 | `validation.differentAccounts` is removed from all seven catalogs | the message is now unreachable; `validation.differentFolders` (already present) replaces its only use |

## 4. Changes

### 4.1 Microsoft Graph — `internal/providers/microsoft/graph.go`

* new `ownDrive(ctx, client)` (`GET /me/drive`, with the shared `driveSelect`
  projection) and `isPersonalDrive(drive)` (reuses `driveFromGraph`, so
  `personal`/`Personal` behave the same here and in the picker),
* `Drives()` lists the own drive first, skips its duplicate in `/me/drives` and
  every other `personal` drive (only when the own drive could be identified),
* `SearchSites()` probes `ownDrive` and returns `[]providers.Drive{}` for a
  personal account before any `/sites` call.

### 4.2 API — `internal/handlers/jobs.go`

* the account rule is replaced by the location rule (3.4) plus the nesting rule
  (3.5/3.6); the drive/folder defaults are now computed once and reused when the
  job is written,
* new helpers `pathWithin(inner, outer)` and `folderPath(raw)`.

### 4.3 UI — `web/src/views/JobEditorView.vue`, `web/src/lib/folders.ts`

* `collectErrors()` reports `validation.differentFolders` on the destination field
  only when `sameLocation()` (same account, drive and folder, with the same
  `root` fallback the server applies) and the destination folder was actually
  picked,
* the account watcher also clears `errors.source`/`errors.destination`,
* `ROOT_ID` joins `ROOT_PATH` in `lib/folders.ts` to mirror `providers.DriveRoot`.

### 4.4 UI — `web/src/components/FolderPicker.vue`

* new `hasSharePoint` computed (Microsoft **and** at least one non-OneDrive root);
  it gates `loadSites()`, the site-search block and the library select,
* a hint under the selects when `isMicrosoft && !hasSharePoint`.

### 4.5 Catalogs — one key added, one removed, in all seven locales

| Key | en-US |
|---|---|
| `browser.noLibraryHint` | This account exposes no SharePoint libraries; its OneDrive is the only root. |
| ~~`validation.differentAccounts`~~ | removed (unreachable, 3.8) |

Still **513** keys per catalog (`docs/i18n.md` updated: 345 keys referenced from
`src/`).

### 4.6 Tests

* `internal/providers/microsoft/drives_test.go`:
  `TestDrivesDropTheBookkeepingDrivesOfAPersonalAccount` (new), and
  `TestDrivesCarryTheKindAndUrlOfEachRoot` now serves `/me/drive` too, so the
  duplicate in `/me/drives` is dropped and the own drive comes first,
* `internal/providers/microsoft/provider_test.go`:
  `TestSearchSitesIsEmptyForAPersonalAccount` (new), the URL-pipeline tests
  updated for the extra probe, `TestGraphRewritesTheMeSentinel` extended with
  `/me/drive`,
* `internal/handlers/handlers_test.go`: `TestCreateJobAllowsOneAccountOnBothEnds`
  (new, OneDrive → OneDrive and OneDrive → SharePoint), the renamed same-folder
  case, and two nesting cases.

### 4.7 Docs

* `docs/providers.md` (drive list row, the UI bullets, two troubleshooting rows),
* `docs/api.md` (`/drives`, `/sites`, the create/update validation paragraph),
* `README.md` (the intra-provider bullet), `docs/i18n.md` (the key report).

## 5. Verification

```bash
go build ./... && go test ./...
cd web && npm run typecheck        # check-i18n + vue-tsc (both configs)
```

Green: every Go package `ok`, `check-i18n: ok — 7 catalogs × 513 keys`. `go` is
not on `PATH` in this environment (the toolchain at
`~/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.0.linux-amd64/bin/go` is used
instead); `web/package.json` asks for Node `^20.19 || >=22.12` but Node 18.19 runs
`npm run typecheck` cleanly.

## 6. Progress log

* 2026-10-06 — reconnaissance: root-caused 2.1 (bookkeeping `personal` drives in
  `/me/drives`, only `/me/drive` answers), 2.2, 2.3 (Graph refuses `/sites` for an
  MSA) and 2.4 (the account guard in `jobs.go`/`JobEditorView.vue`, mirrored by a
  test case). Plan agreed with the operator.
* 2026-10-06 — implemented 4.1–4.7; `go test ./...` and `npm run typecheck` green.

## 7. Follow-up

* **Jobs already stored against a bookkeeping drive must be re-pointed.** The
  `drive_id` of such a job (a `b!…` id of `ODCMetadataArchive` or a `Bundles_*`
  drive) cannot be rewritten by the server, because the folder id it was picked
  with does not exist in the real OneDrive either: the operator has to open the
  job, pick the folder again in the folder browser and save. A new job is
  unaffected — the picker now only offers roots that answer.
* The nesting guard is path-based (3.6). A derived hierarchy (walking the parent
  chain) would be exact but costs a request per save; the display paths are what
  the editor already holds.
