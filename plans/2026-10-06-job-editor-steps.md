# Job editor: a step-aware footer and per-side account messages

* **Date:** 2026-10-06
* **Repo:** `ivancarlosti/sync`
* **Status:** done (see the progress log at the bottom)
* **Related:** [`2026-10-05-provider-aware-folder-picker.md`](./2026-10-05-provider-aware-folder-picker.md), [`../docs/i18n.md`](../docs/i18n.md)

## 1. Objective

Two defects in the job editor's footer and validation:

1. **The footer button always said *Create*/_Save_ and could not walk the
   tabs.** The one primary button called `submit()`, which on a failure
   silently re-targeted `tab`. The operator pressed it on *Details*, the screen
   jumped to *Source*, and nothing said why, so it read as "does nothing".
2. **An account message stuck to the wrong select.** `errors.accounts` was a
   single message for both sides and was never cleared when an account changed,
   so a message stayed visible under a select the operator had already fixed —
   and because the very same string was also the `<select>` placeholder, it
   looked like the placeholder had frozen.

## 2. Root cause

| # | Symptom | Cause |
|---|---|---|
| 2.1 | The footer cannot advance; pressing it jumps to *Source* | `submit()` is the only handler and it validates the whole form, then re-points `tab` at the first *tab* with a problem |
| 2.2 | The account message looks stuck | `errors` is only ever written wholesale (`errors.value = found`); nothing clears a key when its field is fixed |
| 2.3 | …and it never says which side is wrong | one key, `accounts`, covers both the source and the destination account |
| 2.4 | The message reads like a placeholder | `validation.selectAccount` is both the error text and the placeholder of both selects |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| 3.1 | The footer walks the four tabs: *Cancel*, *Previous*, a primary button that reads **Next** until the form is complete and *Create*/*Save* once it is | a wizard keeps the button under the operator's cursor instead of re-targeting a tab out from under them |
| 3.2 | A visible **"Step x of 4"** counter on the left of the footer | the tabs stay clickable, so the counter is what tells the operator where the primary button will take them |
| 3.3 | *Next* validates **only the current step** (`validateStep`), the last step falls back to the full `validate()` | the operator is only blamed for the field in front of them, but a tab clicked out of order still cannot smuggle an incomplete form past *Create* |
| 3.4 | One error key per field — `name`, `sourceAccount`, `source`, `destinationAccount`, `destination` — collected once in `collectErrors()` | the message renders under the exact field that owns it, on the exact side |
| 3.5 | `STEP_ERROR_KEYS` maps each step to the keys it owns | the *source* account error can never appear under the *destination* select |
| 3.6 | `watch`ers clear `name` on a name change and both account messages on an account-id change | a fixed field stops complaining immediately, mirroring how `applyFolders` clears the folder messages |
| 3.7 | `validation.selectAccount` is split into `selectSourceAccount` / `selectDestinationAccount` | a placeholder and a validation message are different sentences and stop colliding; each names the side |

## 4. Changes

### 4.1 `web/src/views/JobEditorView.vue`

* imports `ArrowRight` alongside `ArrowLeft`,
* new `STEP_ORDER` (`details`, `source`, `destination`, `options`) and
  `STEP_ERROR_KEYS` (step → the error keys it owns),
* new `stepIndex` (position of `tab`) and `complete` (whether the whole form
  passes `collectErrors`),
* `validate()` is split into `collectErrors()` (pure, returns the found keys) and
  `validate()` (writes `errors`, points `tab` at the first problem tab), which
  now knows the two account keys are not interchangeable,
* new `validateStep(step)`, `nextStep()`, `previousStep()`, plus `primaryLabel`,
  `stepLabel` and `primaryAction()` for the footer,
* two new `watch`ers clear `errors.name` / the two account messages when their
  field changes,
* the source and destination `<select>`s use the new placeholders and the new
  error keys,
* the footer is `justify-between`: the `stepLabel` counter on the left, then
  *Cancel* / *Previous* / the primary button; the primary button shows the
  *Save* icon once the form is complete and the *Next* arrow while it is not.

### 4.2 Catalogs — five new keys, one removed, in all seven locales

| Key | en-US |
|---|---|
| `common.next` | Next |
| `common.previous` | Previous |
| `jobs.stepOf` | Step {current} of {total} |
| `validation.selectSourceAccount` | Select a source account |
| `validation.selectDestinationAccount` | Select a destination account |
| ~~`validation.selectAccount`~~ | *removed* |

509 → **513** keys per catalog (the count in `docs/i18n.md` is updated).

### 4.3 Docs

* `docs/i18n.md` §1 — the key count (`508` → `513`),
* `docs/i18n.md` §6 — the sample `check-i18n` output.

## 5. Verification

```bash
cd web && npm run typecheck           # check-i18n + vue-tsc (both configs)
```

Green: 7 catalogs × 513 keys, 344 keys referenced from `src/`.

## 6. Progress log

* 2026-10-06 — reconnaissance: root-caused 2.1–2.4 from `validate()`/`submit()`
  and the shared `validation.selectAccount` string; confirmed the count with
  `web/scripts/check-i18n.mjs`. Plan agreed with the operator (step counter,
  Next→Create, per-side account errors).
* 2026-10-06 — implemented 4.1–4.3; `npm run typecheck` green.
