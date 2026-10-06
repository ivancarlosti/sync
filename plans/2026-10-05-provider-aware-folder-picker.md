# Provider-aware folder picker: Shared Drives and the SharePoint select

* **Date:** 2026-10-05
* **Repo:** `ivancarlosti/sync`
* **Status:** done (see the progress log at the bottom)
* **Related:** [`2026-10-05-sharepoint-library-picker.md`](./2026-10-05-sharepoint-library-picker.md), [`../docs/providers.md`](../docs/providers.md)

## 1. Objective

Two defects survived the SharePoint work of the same day:

1. **A Google Shared Drive listed the contents of My Drive.** Picking a Shared
   Drive in the folder browser showed the files of My Drive, so a job created
   against it would have synchronised the wrong tree.
2. **The picker was the same shape for every provider.** `FolderPicker.vue`
   always rendered the SharePoint search field and merged the drives and the
   sites into one select labelled *Drive*, even for a Google account, which has
   no libraries at all.

## 2. Root cause

| # | Symptom | Cause |
|---|---|---|
| 2.1 | A Shared Drive lists My Drive | `Children` sent `q="'root' in parents"` whenever `folderID == ''`. Google documents `parents.isRoot` as true for the **My Drive root alone**; `root` is never the top level of a shared drive. |
| 2.2 | …and the query was widened on top | `listURL` appended `includeItemsFromAllDrives=true` to every listing, a legacy flag that spans My Drive *and* the shared drives and contradicts the single corpus (`corpora=drive&driveId=…`) the caller had just selected. |
| 2.3 | A Shared Drive root had the same problem on writes | `CreateFolder` and the upload initiation sent `"parents": ["root"]`, which parents a new item in My Drive. |
| 2.4 | Google accounts saw a SharePoint feature | The site field and the merged select were rendered unconditionally, with no knowledge of the account's provider. |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| 3.1 | Address a shared drive's top level by the **drive id** | the drive id *is* the id of that drive's root folder, and it is what Drive accepts in a `parents` filter and in a `parents` metadata field |
| 3.2 | One helper, `rootParent(driveID, folderID)`, used by `Children`, `upload` and `CreateFolder` | the three call sites had the same alias problem; the rule ("only a real shared drive replaces `root`") lives in one place and is unit tested |
| 3.3 | Resolve the alias **before** the identifier guard at every call site | the value interpolated into the URL or the metadata document is then the one that is validated, which is what the scanning rules look for |
| 3.4 | Drop `includeItemsFromAllDrives` entirely rather than setting it conditionally | every caller already selects exactly one corpus through `withDriveScope`, so the flag could only widen a query; `supportsAllDrives=true` is what a shared drive actually needs |
| 3.5 | The picker receives a `provider` prop instead of asking the API | the job editor already holds the connected accounts, so the layout costs no request |
| 3.6 | Microsoft gets **two** selects — *OneDrive* (`personal`) and *SharePoint library* (`document_library` + `site`) — with the site field above the library one | the two kinds of root are not interchangeable and the search fills exactly one of them; the operator sees which one they are in |
| 3.7 | The two Microsoft selects share the one `driveId` state and the inactive one falls back to its placeholder | a single source of truth cannot disagree with the listing below; per-select local state would let the picker show a root it is not browsing |
| 3.8 | Every select is bound with `v-model` (a writable computed) instead of listening to `update:model-value` | the old drive select was uncontrolled: it showed its first option while `start()` had selected another root, and a later option-list change silently reset the browser |
| 3.9 | The site calls are only made for Microsoft | `SearchSites` answers `424` for a provider without the capability, so calling it for Google is a wasted round trip |

## 4. Changes

### 4.1 Google Drive — `internal/providers/google/drive.go`, `client.go`

* new `rootParent(driveID, folderID)`: `root`, `""` and `MyDrive` keep the alias;
  a real shared drive resolves to the drive id,
* `Children` resolves the alias before validating, so `'<drive id>' in parents`
  is what a shared drive gets (and My Drive keeps `'root' in parents`),
* `listURL` no longer sends `includeItemsFromAllDrives=true`.

### 4.2 Google writes — `internal/providers/google/upload.go`

* `upload` and `CreateFolder` parent a new item with
  `rootParent(driveID, parentID)`, so creating in the top level of a shared drive
  writes to the shared drive.

### 4.3 Tests — `internal/providers/google/identifiers_test.go`

* `TestChildrenOfASharedDriveRootAddressesTheDrive` — `corpora=drive`,
  `driveId=<id>`, `'<id>' in parents`, no `'root' in parents`, no
  `includeItemsFromAllDrives`, for both `""` and `root`,
* `TestWritesParentTheSharedDriveRoot` — the JSON metadata of `Upload` and
  `CreateFolder` carries `"parents":["<drive id>"]` (`recordingTransport` now
  records the request bodies),
* `TestRootParentLeavesTheOtherRootsAlone` — table test of the helper,
* the My Drive expectation (`corpora=user`) and the `allowedURLs` sample stay.

### 4.4 UI — `web/src/components/FolderPicker.vue`, `web/src/views/JobEditorView.vue`

* new `provider` prop, passed as `account('source'|'destination')?.provider`,
* `isMicrosoft` gates the site field *and* the layout: Google renders one
  *Drive* select (My Drive + Shared Drives, with the kind as the label suffix),
  Microsoft renders *OneDrive* and *SharePoint library* side by side, the second
  one under the field that fills it,
* `dedupe` / `optionOf` helpers keep the "never list one root twice" rule for
  both layouts; `driveModel`, `oneDriveModel` and `sharePointModel` are writable
  computeds over the single `driveId`,
* `openDrive` ignores an empty id (the placeholder of the inactive select),
* `start()` only loads the sites for Microsoft and opens a Microsoft account on
  its personal OneDrive.

### 4.5 Catalogs — three new keys in all seven locales

| Key | en-US |
|---|---|
| `browser.oneDrive` | OneDrive |
| `browser.sharePointLibrary` | SharePoint library |
| `browser.selectRoot` | Select a root |

505 → **508** keys per catalog (the count in `docs/i18n.md` is updated).

### 4.6 Docs

* `docs/providers.md` §5 — the job-editor bullet now describes the
  provider-aware layout and the shared-drive root,
* `docs/i18n.md` — the key count and the sample `check-i18n` output.

## 5. Verification

```bash
cd web && npm run typecheck           # check-i18n + vue-tsc (both configs)
go build ./... && gofmt -l ./internal && go vet ./... && go test ./...
```

Both green: 7 catalogs × 508 keys, 340 keys referenced from `src/`, every Go
package `ok`.

## 6. Progress log

* 2026-10-05 — reconnaissance: root-caused 2.1 from the Drive documentation
  (`parents.isRoot` is the My Drive root only) and 2.2 from `listURL`; read
  `identifiers.go` to confirm that reusing a validated drive id as a parent is
  safe. Plan agreed with the operator (two Microsoft selects).
* 2026-10-05 — implemented 4.1–4.4 and 4.6, catalogs in 4.5; `npm run typecheck`
  and `go test ./...` green.

