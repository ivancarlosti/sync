# Licence probe: a part that asked to be asked again is not an answer

## Context

`report-22` (a Workspace report of the resold tenant `agencia.pub`) came back with the
`licenses` column of `users.csv` filled for 5 of its 23 users. The tenant's own
`User_Download` export lists a paid edition (`1010020028`, Google Workspace Business
Standard) for 19 of them, so 15 users were reported with `License count = 0`, i.e. the
report claims they hold nothing.

What the fallback read that run is visible in the exported `licenses.csv`:

| row set | users |
| --- | --- |
| `1010010001` Cloud Identity Free | adriana, ai, rogerio.parrilla, rogeriop, thiago |
| `1010020028` Business Standard | adriana, ai, rogeriop, thiago |

The users with an assignment are the **first two and the last three of the walk**
(`users.csv` is in directory order), and `rogerio.parrilla` is suspended and holds the
free edition only — exactly what the Admin console says. So each of the five *was*
answered in full, and the 18 users in between were not answered at all. That is not an
early stop (the walk reached the end of the directory) and it is not a per-SKU refusal
(a refused part would still leave the other 76 parts of the same answer, which is what
`b29f008` fixed).

It is the shape of a **throttled part**: the parts of the middle batches were refused
with a 429 or with the 403 a quota excess arrives as, the walk classified that as "not
a decision about the caller", moved on — and the unanswered SKUs were read as "this user
holds nothing". `licenseAnswer` counts a 200/400/404 as an answer, but the code threw
the unanswered parts away instead of treating them as an open question, so a caller over
its quota got a report that was quietly wrong. Nothing in the run could show it: the
section kept its rows, so it was neither failed nor truncated, and no line was logged.

## Goal

1. A part that asked to be retried is **asked again** (bounded, with backoff) before the
   user's licence set is accepted.
2. A user whose licence read is still open — throttled after the retries, answered by
   nothing, never reached because the walk stopped, or past the probe cap — is reported
   as **unknown**, not as "no licences": the licence cells of that user stay empty and
   the sections are marked `truncated`.
3. A partial licence inventory is **visible**: one `slog.Warn` per report with how many
   users were left unanswered, and the reason the API gave (throttle vs refusal). That
   line is what tells a resold tenant's throttling apart from an assignment the caller
   genuinely cannot see.
4. `docs/reports.md` §1 says all of this, so the next reader does not have to re-derive
   it from a CSV.

## Steps

1. `licenseAnswer` grows three counters — `Answered` (200/400/404), `Retried` (429, the
   quota 403, 5xx) and `Refused` (anything else) — plus `RetryErr`, `complete()` and
   `merge()`. `fault()` (no part answered at all ⇒ a decision about the caller) keeps
   its meaning and falls back to `RetryErr`.
2. `acceptLicenseAnswers` classifies a failed part with `transientLicenseFailure`: a
   part that asks to be asked again is neither a row nor a refusal.
3. `probeLicensesOf` asks the catalogue of one user up to `reportLicenseProbeAttempts`
   times while parts keep asking to be retried, pausing `licenseProbeBackoff` (doubling,
   context aware) between attempts. The per-batch walk moves to `probeLicensesOnce`.
4. `addLicense` drops an assignment the index already holds, so a retried batch cannot
   double a row.
5. `probeLicenses` fills `licensesKnown` (the users whose read is complete), counts the
   ones it could not finish, marks the section `truncated`, and logs the outcome.
6. `licensesOf` answers whether a user's licence read is complete, and `usersTable`
   leaves `Licenses`/`License count` empty for a user it is not.
7. Tests: a throttled part is retried and its assignment lands; a user whose parts stay
   throttled shows empty cells (not `0`) while the user next to it keeps its own; the
   existing part-refusal and walk tests keep passing.
8. `docs/reports.md` §1 `licenses` documents the retry, the unknown cells and the log line.

## Risks

* **A retried batch costs time.** Two extra batches per throttled user, up to 14 s of
  backoff each, inside a report that already walks one batch per user. Bounded by
  `reportLicenseProbeAttempts`, and only paid when the API asked for it.
* **`truncated` becomes common for a resold tenant** whose walk stops early — that is the
  point (the table no longer claims to be the directory's whole inventory), but an
  operator who reads `truncated` as "the row cap bit" needs the new wording in the docs.
* **An empty cell is not a number.** A consumer that sums `License count` must skip
  blanks; the CSV exported today can no longer be read as "0 = no licence", which is
  precisely what it wrongly said.
* **A refusal part stays a SKU-level answer.** A tenant that refuses `get` for a SKU it
  cannot own keeps its exact rows and is *not* marked unknown, so a licence hidden by the
  API (rather than throttled) is still reported as absent — the log line is the only
  signal, and no code path can recover an assignment the API does not answer.
* Not fixed here: `reportLicenseProbeUsers` (500) still bounds the probe, so a directory
  larger than that is partly unknown — now visibly so.
