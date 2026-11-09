# Licence probe: reproduce it against the real tenant before changing it again

## Context

`report-22` reported `License count = 0` for 15 of the 23 users of a resold tenant
whose own export lists a paid edition for 19 of them. `db9f33e` made a *throttled*
part of a per-user licence batch ask to be asked again and made a read that never
answered say nothing (empty cells, section `truncated`) instead of `0`. The re-run
is still short: the paid-edition users still do not all show up.

Two things are missing to fix it properly:

1. **The statuses.** The code path depends on what the Enterprise License Manager
   API actually answers per part — `429`/quota `403` (throttle, retryable),
   `subscriptionNotFound` (an answer about a SKU), a `5xx`, or an empty batch. The
   report stores none of them, so every guess about the cause is a guess.
2. **A loop.** Reproducing through the instance means a database, a connected
   account, an OAuth consent and a scheduled run, per attempt — and the run row only
   keeps counters, so the run cannot be read.

## Goal

1. A local harness that runs **the collector itself** (`google.Provider.CollectReport`,
   the call `ReportService` makes) against a real Workspace tenant, given nothing but
   a credential, and writes the sections as the CSVs the ZIP carries.
2. The three readings of the licence columns made explicit — holds a licence,
   answered with none, never answered — so "still short" becomes a number.
3. The per-part status of every lookup reachable at debug level, so the fix that
   follows is a reading of the API's answers and not another hypothesis.
4. The behaviour of the last change stays covered by unit tests, because the harness
   needs credentials and CI does not have them.

## Steps

1. `cmd/reportprobe`: flags for the three credentials a run can use (a
   service-account key with domain-wide delegation, the OAuth client plus refresh
   token of a connection, or an access token minted elsewhere), `-sections`
   (default `users,licenses`), `-out`, `-debug`, `-scopes` (the scope list a DWD
   grant must authorise) and `-timeout`.
2. Token minting with the standard library only: RS256 assertion (`iss`, `sub`,
   `scope`, `aud`, `iat`, `exp`) for the delegation grant, a form POST for the
   refresh token, and the endpoint's own refusal text when the grant is refused.
3. Output: one CSV per section with the BOM and the column titles of the ZIP
   (`<section>.error.txt` for a section that answered with a reason instead of a
   table) plus `summary.txt`, which counts the licence cells.
4. Two `slog.Debug` lines in the collector — the counters of each user's lookup and
   the status of each part that failed — so `-debug` says throttled vs refused.
5. `.local/` gitignored: the key and the CSVs of a local run never enter the tree.
6. `docs/development.md` §2/§3/§7 and `docs/reports.md` §1 point at the tool.
7. Tests: the assertion (signature verifies, `sub`/`scope`/`aud` correct), the token
   exchange (answered and refused), the section vocabulary, the three readings of
   the licence columns and the two shapes written to disk.
8. Then: run it against the tenant, read the statuses, and fix with what they say —
   pacing the walk if the parts are throttled, per-user retry if they are not, and
   Google/support for the SKUs the domain refuses.

## Risks

* **Drift from the service.** The harness calls the collector, not `ReportService`:
  it does not exercise the status mapping, the stored tables or the truncation
  budget. Anything that only shows up through the service still has to be re-checked
  on a scheduled run.
* **A credential on disk.** `.local/` is gitignored, the tool never logs or writes a
  token, but the key it reads is a domain-wide delegation key: it belongs on a
  development machine, not in the repository, an image or a CI secret.
* **Delegation scope.** The grant only works if the service account's client id is
  authorised in Admin console for the scopes of `-scopes`; a missing one answers
  `403` on that API only, which the section reports as its reason.
* **The harness costs API quota**, and quota is exactly what is being measured: a
  run reads the directory and one batch per user, so back-to-back runs can make a
  throttled tenant look worse than it is. Space them out and compare like with like.
* **No tenant here.** Without a key the harness cannot run in CI; the parts that
  can be tested without one are, and the rest stays a manual step.
