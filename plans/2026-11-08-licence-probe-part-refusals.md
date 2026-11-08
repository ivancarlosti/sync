# Report: the `licenses` section of a refused catalogue only carrying the first user's licences

## Context

A Workspace report of a resold tenant (`report-19`) came back with a `licenses`
section that holds two rows, both of them the licences of
`adriana.blanco@agencia.pub` (Business Standard + Cloud Identity Free), and a
`users` section whose `licenses` column is empty for every other user of the
directory. The tenant refuses the domain-wide catalogue
(`403 Unauthorized operation for the given domain`), so the section is the
per-user fallback of `probeLicenses` — one batch per user, 77 sub-requests of
`licenseAssignments.get`.

The fallback was ending the walk at the **first failed part**:
`acceptLicenseAnswers` returned as soon as a part answered `401`/`403`, treating it
as "the domain refused the caller", and `probeLicenses` then stopped after that
batch because `apiStatus(err) < 429`. A part that answers `403`, however, is an
answer about **one SKU** — in this tenant a subscription the reseller never
provisioned (`subscriptionNotFound`), or a product the API does not answer a `get`
for — and the parts after it carry the same user's other assignments. The result
was exactly what the report shows: a user whose first assignments arrive before the
first refused part is reported, everyone after them is not, and the walk never
reaches the second user of the directory.

Two secondary defects came with it:

* a `403` throttled answer (`errors[].reason` = `rateLimitExceeded` /
  `quotaExceeded`, which Google sends for a quota excess) was classified as a
  refusal, so one burst of parallel sub-requests could end the walk for the whole
  directory;
* `batchRefusal` dropped the `errors[].reason` of the part, so the report explained
  the refusal without saying what the API had said, and nothing could tell a
  throttle from a genuine permission error.

## Goal

* A part that fails is recorded as a failure of that SKU and the rest of the answer
  is still read: every assignment of every probed user lands in the section.
* What ends the walk is an answer about the **caller** — a batch whose parts were
  refused every one of them, or a batch the endpoint did not answer at all — plus
  the two caps that already existed (`reportLicenseProbeUsers`, the section's row
  cap).
* A throttle (`429`; `403` with a throttle reason; a `5xx`) is asked again for the
  next user instead of ending the walk.
* The error of a part carries the reason of the envelope, so an operator sees what
  the API said.

Non-goals: discovering the SKU catalogue at runtime (every listing is the same
refused call), changing the caps, retrying a refused part inside the same batch, or
reporting a per-part failure as a section failure when other rows were found.

## Steps

1. **`report.go` — a batch answer as a value.** New `licenseAnswer`
   (`Rows`, `Answered`, `Err`) with `fault()`, which answers `Err` only when *no*
   part of the batch answered anything. `rows` counts the assignments, `Answered`
   counts the parts that answered a question about a SKU (an assignment, or the
   `400`/`404` that says the user does not hold it) — that is the number that tells
   a failure of a SKU from a refusal of the caller.
2. **`acceptLicenseAnswers` reads every part.** No early return: `200` records the
   assignment, `400`/`404` counts as answered, anything else records the first
   `batchRefusal` and keeps going.
3. **`licenseProbeBatch` / `probeLicensesOf` answer `(licenseAnswer, error)`**, where
   the error is only the batch that could not be sent or read (transport, status of
   the endpoint, a document that is not a batch). The assignments of the batches
   before it stay part of the answer.
4. **`probeLicenses` walks on the classification**, not on the status number: it
   stops when `answer.fault()` / the batch error is not transient, where
   `transientLicenseFailure` means "throttled, or a `5xx`". `throttled` asks
   `apiError.rateLimited()`, the same classification `providers.DoWithRetry` acts
   on, because a part never travels through the retry policy.
5. **`batch.go` — `batchRefusal` keeps the reason** (`errorReason`), mirroring
   `decodeError`: status, code, reason and message of the part's envelope.
6. **Tests** `report_test.go`:
   `TestLicensesKeepReadingThePartsAfterARefusedOne` scripts the reported tenant —
   the first part of the batch refused, and one part refused between the two
   assignments of the first user — and pins four rows, one batch per user and the
   `licenses` column of all three users; both assertions fail on the old code.
   `TestLicensesKeepWalkingThroughAThrottledBatch` pins that an all-throttled batch
   is asked again for the next user and its reason reaches the section error. The
   two existing ends of the fallback (a directory answered part by part, a domain
   that refuses the per-user read too) keep passing unchanged.
7. **Docs** `docs/reports.md`: §1 `licenses` describes the part-level rule, the
   caller-level rule that replaced it, and the throttle that is asked again.

## Risks

* **A part-level failure is no longer visible as a section error.** A SKU the API
  refuses for every user costs one silent missing row instead of a failed section;
  the section still fails when nothing at all was found, which is the case an
  operator can act on.
* **A directory that throttles every batch** costs one call per user up to
  `reportLicenseProbeUsers` (500) instead of stopping at the first one. That is the
  price of not throwing away a directory over a quota window, and it is bounded by
  the same cap that already bounds the walk.
* **A domain that refuses the per-user read only for some users** (a partially
  delegated grant) now reports the users it can read instead of failing the
  section. The row count of the section is what tells the operator, which is the
  same signal the caps use (`truncated` covers the caps only).
