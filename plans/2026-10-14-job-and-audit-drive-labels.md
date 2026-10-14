# Job and audit locations: the lists showed a bare path

* **Date:** 2026-10-14
* **Repo:** `ivancarlosti/sync`
* **Status:** done (see the progress log at the bottom)
* **Related:** [`2026-10-05-provider-aware-folder-picker.md`](./2026-10-05-provider-aware-folder-picker.md), [`2026-10-05-sharepoint-library-picker.md`](./2026-10-05-sharepoint-library-picker.md), [`2026-10-07-audit-content-report.md`](./2026-10-07-audit-content-report.md)

## 1. Objective

Two locations on the same account are indistinguishable once they are stored. The
Jobs list shows its source and destination as *account + path*, and the Audit
history shows its root the same way, so:

* an OneDrive folder and a SharePoint library folder of one work account can read
  as the same location (`/Documents` on both sides, the account email identical);
* two SharePoint libraries of two sites both call their default library
  `Documents`, so their paths differ only below that name;
* a Google Shared Drive and My Drive are told apart by nothing at all.

The folder picker already knows the answer — it resolves `providers.Drive{id,
name, kind, owner}` for every root it lists — but the selection it hands back was
`{driveId, folderId, folderPath}`, so the drive family and the site were dropped
at the moment of choosing. The goal is to keep that context and render it next to
the path in the Jobs list, the job editor and the Audit history.

## 2. Root cause

| # | Symptom | Cause |
|---|---|---|
| 2.1 | A job row names no drive family | `SyncJob` persisted `source_drive_id`/`destination_drive_id` and the display paths only; the `name`/`kind`/`owner` the provider returned were never stored |
| 2.2 | An audit row names no drive family | `AuditRun` persisted `drive_id` + `root_path` only, for the same reason |
| 2.3 | The context was lost in the UI too | `FolderPicker.confirm()` emitted only the three fields above, and `FolderSelection` had no place for them |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| 3.1 | Store the display parts next to the id: `SourceDriveName/Kind/Owner` + `DestinationDriveName/Kind/Owner` on `sync_jobs`, `DriveName/Kind/Owner` on `audit_runs` (9 columns, sizes `191`/`32`/`191`) | mirrors `folder_path`: the label is what a list needs, and keeping it beside the id means no provider round trip and no second source of truth |
| 3.2 | The columns are **optional**; an empty set renders as today | rows written before the change (and clients that do not send them) keep working, and no back-fill is needed |
| 3.3 | Both lists render the family through the catalog (`browser.myDrive`, `browser.sharedDrive`, `browser.oneDrive`, `browser.sharePointLibrary`), not as a raw kind | the operator reads a language, not `document_library`; it is also the wording the picker already uses for its two Microsoft selects |
| 3.4 | New `web/src/lib/drives.ts` owns the vocabulary (`siteOf`, `isOneDriveKind`, `isSharedDrive`, `driveFamilyKey`, `driveLabel`, `driveFromJob`, `driveFromAudit`) | `locationOf`/`isOneDriveKind` lived inside `FolderPicker.vue`; three more screens now need the same rules, and a duplicated rule is a rule that drifts |
| 3.5 | `driveFamilyKey` resolves `personal`/`shared` against the **provider** | Google calls My Drive `personal` and Microsoft calls its own drive the same, so the kind alone cannot label a root; `AuditRun.provider` is stored and a job's provider is read from its account |
| 3.6 | The site segment of a library URL is shown after the library name | that is what `optionOf` already does in the picker ("Documents · marketing"), and it is the only thing that tells two default libraries apart |
| 3.7 | The label is **empty** when both the name and the kind are unknown | an id alone cannot tell an OneDrive from a library, and a guess is worse than the path-only rendering of today |
| 3.8 | Payload values are trimmed and clamped to the column width, by rune | the values come from a provider; a long library name must not turn a save into a 500, and a mid-rune cut would store invalid UTF-8 |
| 3.9 | Runs screen item paths stay untouched, and history is not back-filled | out of scope: the run log is per-file detail, and the complaint was about the two screens that name a location |


## 4. Changes

### 4.1 Models — `internal/models/sync.go`, `internal/models/audit.go`

`SyncJob` gains six columns next to the drive ids, `AuditRun` three. Both models
are already in `database.AutoMigrate`, so the new columns appear without a manual
migration.

### 4.2 API — `internal/handlers/jobs.go`, `internal/handlers/audits.go`, `internal/services/audit.go`

* `jobInput` accepts the six fields and `applyJobInput` writes them on the job;
* `auditInput` accepts three, forwarded into `services.AuditInput`;
* `AuditService.createRun` persists them on the `running` row it writes first;
* `driveLabel(value, max)` trims and clamps, with `driveNameWidth`/`driveKindWidth`/
  `driveOwnerWidth` naming the column widths once.

### 4.3 UI library — `web/src/lib/drives.ts`, `folders.ts`, `api.ts`, `jobs.ts`

* `drives.ts` is new and owns the label vocabulary above;
* `FolderSelection` gains `drive: DriveIdentity`, so the picker's context travels
  with the ids and the path as one value;
* `SyncJob`, `JobPayload`, `AuditRun` and `AuditPayload` gain the new fields, and
  `payloadFromJob`/`newJobPayload` carry them.

### 4.4 Picker — `web/src/components/FolderPicker.vue`

`locationOf`/`isOneDriveKind` move to `drives.ts` (`siteOf`/`isOneDriveKind`), and
`confirm()` resolves the current root out of `drives` + `sites` and emits its
`name`/`kind`/`owner` together with the selection.

### 4.5 Screens — `JobEditorView.vue`, `JobsView.vue`, `AuditView.vue`

* the editor stores the identity on the draft and shows the label under the folder
  path of each side;
* the job list shows it between the account and the path, for both ends;
* the audit screen keeps the identity of the picked root, sends it on start, shows
  it in the history's folder column and joins it into the report dialog
  description (`account · drive · path`).

### 4.6 Catalogs — two new keys in all seven locales

`browser.myDrive` and `browser.sharedDrive` (644 keys per catalog now).

### 4.7 Tests

* `internal/handlers/handlers_test.go`: `TestJobStoresDriveLabels` (create → read →
  update → payload without the fields), `TestCreateJobClampsDriveLabels` (rune-safe
  clamp), and the audit start assertion extended with the drive parts;
* `internal/services/audit_test.go`: `TestAuditStoresDriveLabels` (stored, trimmed,
  and empty when the caller sends nothing).

### 4.8 Docs

`docs/api.md` (§6 `jobView`/payload/optional-fields note, §8 payload/`auditView`),
`docs/database.md` (`sync_jobs` columns, `audit_runs` root), `docs/i18n.md` (key
count).

## 5. Verification

```bash
cd web && npm run typecheck
go build ./... && gofmt -l ./internal && go vet ./... && go test ./...
```

## 6. Progress log

* 2026-10-14 — reconnaissance: root-caused 2.1–2.3 (the picker resolves the drive
  and drops it at `confirm()`); plan written before the first edit.
* 2026-10-14 — implemented 4.1–4.8; typecheck (644 keys) and the full Go suite pass.
