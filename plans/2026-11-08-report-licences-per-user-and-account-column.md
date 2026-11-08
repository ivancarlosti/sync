# Report: the `licenses` section of a domain the catalogue refuses, and the account column of `analytics` / `youtube`

## Context

Two complaints about one report:

1. **`licenses` is empty on some domains.** `loadLicenses` walks
   `licenseAssignments.listForProduct`, one call per product of
   `reportLicenseProducts`, and every one of them carries `customerId`. A domain
   whose paid edition the caller does not manage directly — a resold tenant, a
   domain behind a distributor — answers that call with
   `403 Unauthorized operation for the given domain`. `requestURLPattern` marks
   the entry optional, so the section does not fail the report: it reports the
   refusal and stays empty, and the `licenses` column of the `users` section goes
   with it (the column is filled from the same walk). GAM answers the same 403 by
   not asking the domain at all: `Info_User_Licenses` calls `getUserLicenses`,
   which reads `licenseAssignments.get` per user and per SKU — a read that takes
   **no customer id** and is therefore answered for a licence the caller can see.

2. **`analytics` and `youtube` do not say whose they are.** Both sections list
   what the *connected identity* reaches (the Analytics Admin account list, the
   `mine=true` channel list), and neither table carries that identity. The
   operator has to remember which account the row was collected with.

## Goal

* `licenses` (and therefore the `licenses` column of `users`) is populated on a
  domain that refuses the catalogue: after the refusal the collector probes the
  per-user read of the same API, one batch per user against the SKU catalogue,
  and reports the assignments it finds. A per-user refusal still leaves the
  section degraded, never the report.
* The `analytics` and `youtube` tables carry the connected account's address as
  their **first** column.

Non-goals: domain-wide delegation, a change of the licensing scope, writing
licences, discovering SKUs at runtime (every catalogue listing is the same
refused call).

## Steps

1. **SKU catalogue** — new `internal/providers/google/licenseskus.go`: the
   `reportLicenseSku` shape (`Product`, `Sku`, `Name`), the 77-entry table copied
   from Google's public catalogue (the one GAM ships as `glskus.py`, grouped by
   `reportLicenseProducts`), and `licenseSkuName`, the fallback name of an answer
   that carries none.
2. **Batch plumbing** `internal/providers/google/report.go`: `licensingPath`
   (`/apps/licensing/v1`, shared with `licensingBase`) and `licensingBatchURL`
   (`https://licensing.googleapis.com/batch`, the `batchPath` of the API's
   discovery document). `licenseProbePath` builds
   `/product/{product}/sku/{sku}/user/{address}` behind the package's identifier
   patterns; `licenseProbeBody` renders the `multipart/mixed` request (one
   `application/http` part per SKU, generated boundary); `readBatchAnswers` /
   `parseBatchAnswer` read the answer (a 200 document whose *parts* carry the
   statuses). Nothing is correlated by `Content-ID`: a part that carries an
   assignment carries its `productId`, `skuId` and `userId`, so a 200 needs no
   index and a 404 is the normal "not assigned" answer.
3. **Fallback** `loadLicenses`: the refusal branch calls `probeLicenses`, which
   lists the directory, walks up to `reportLicenseProbeUsers` (500) users one
   batch per user (chunks of `reportLicenseProbeBatch` = 100 sub-requests), and
   records the assignments. A failure that cannot improve for the next user — a
   401/403 in a part (the domain refused the caller), a 400/404 of the batch, or a
   batch that could not be read at all — stops the walk; a `429`/`5xx` keeps it
   going. The section reports the refusal only when not one assignment was
   found. Rows land in the same per-user index as the catalogue walk, so both the
   section and the `users` column read them unchanged.
4. **Account column**: `reportData.accountEmail()` reads the OpenID userinfo
   email once (`Provider.Account`, the call the accounts screen makes), caches it,
   and both tables prepend `column("account_email", "Account")`. Its failure is
   not a section failure: the cell stays empty and the rows stay.
5. **Tests** `internal/providers/google/report_test.go`: the transport grows a
   `batch` hook that renders a real `multipart/mixed` answer, a userinfo fixture
   for the email column, the probe (one batch per user, the assignment of one SKU
   recorded, the refusal of the catalogue no longer failing the section), the
   probe refused too (the existing refusal test, extended to prove the probe ran),
   and the `account_email` cell of both new sections.
6. **Docs** `docs/reports.md`: the per-user fallback under §1 `licenses` (cost,
   cap, what is invisible), the two-section bullets for the account column, the
   degradation paragraph that describes the 403.

## Risks

* **Cost of the fallback.** One batch per user, so a refused catalogue turns the
  section into `1 + min(users, 500)` calls of 77 sub-requests each. Bounded and
  only on the refusal path, but a 500-user tenant pays ~500 calls; the section's
  progress reports the rows as they land, and both caps are named constants.
* **A stale catalogue.** SKUs Google adds later are not probed and their licences
  are invisible to the fallback. The table says so, and the catalogue walk (when
  the domain answers it) is unaffected.
* **Batch correlation.** A batch answers out of order; nothing is correlated, so
  a legitimate reorder cannot mislabel a row. A part that fails to parse is
  skipped rather than failing the user.
* **Extra call per section.** `analytics` and `youtube` now pay one userinfo call
  each (cached across both) to name the account. Read-only, no new scope.
