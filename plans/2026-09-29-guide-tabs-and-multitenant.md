# Setup guide: per-provider reload and multi-tenant Microsoft connections

* **Date:** 2026-09-29
* **Repo:** `ivancarlosti/sync`
* **Status:** in progress (see the progress log at the bottom)
* **Related:** [`2026-09-28-app-registration-guide.md`](./2026-09-28-app-registration-guide.md)

## 1. Objective

Three defects of the setup guide screen, reported from a real session:

1. **Untranslated keys.** The Microsoft 365 tab renders raw catalog keys such as
   `admin.guide.steps.microsoft.project.title` instead of a step title.
2. **Leaked provider copy.** The Google banner ("Google Drive and the Workspace
   directory share one Cloud project…") stays on screen after switching to the
   Microsoft 365 tab.
3. **Tenant id is unexplained and multi-tenant looks impossible.** The tenant id
   hint says only "Leave empty for the common tenant", the guide tells the
   operator to "keep the single-tenant option unless you really need
   multi-tenant", and the admin-consent URL is built from `common`, which
   Microsoft explicitly documents as unsupported for the `adminconsent` endpoint.
   Connecting accounts from several directories is the goal (a multi-tenant
   registration, public and publisher-verified).

## 2. Root causes

| # | Symptom | Cause |
|---|---|---|
| 2.1 | raw keys + Google banner on the Microsoft tab | `SetupGuideView.vue` loads the guide in `onMounted` only. `/admin/guide/google` → `/admin/guide/microsoft` is the **same route record**, so Vue Router reuses the component, `onMounted` never runs again and `guide` keeps the Google answer. `stepKey` then builds `admin.guide.steps.microsoft.<google-step-id>.title` (`project` is a Google step id) → a key no catalog has → the key string is rendered; the banner reads `admin.guide.intro.${guide.provider}` → `intro.google`. |
| 2.2 | admin consent cannot serve several directories | `AdminConsentURL` builds `https://login.microsoftonline.com/{tenant}/v2.0/adminconsent` from `tenantID`, whose default is `common`. Microsoft documents the `tenant` parameter of the admin-consent endpoint as *"GUID or friendly name format OR generically referenced with `organizations`… **Do not use `common`**"*. The sign-in and token endpoints are right to use `common`; the consent endpoint is not. |
| 2.3 | guidance discourages the supported mode | `single_tenant_recommended` warning (guide) + `docs/app-registration.md` §3.1 step 1 steer every operator to a single-tenant registration, and `admin.tenantIdHint` never mentions multi-tenant. |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| 3.1 | Reload on provider change with a `watch` on the `provider` computed (reset `guide` first so the stale copy cannot flash) | the screen is one route with a provider parameter; a watcher is the idiomatic Vue Router answer and keeps a single `load()` |
| 3.2 | Split the two tenant notions: `tenantID(creds)` keeps serving the **authorization/token** endpoints (`common` default, any work/school **or personal**), a new `consentTenant(creds)` serves the **adminconsent** path and maps the generic values (`""`, `common`, `consumers`) to the `organizationsTenant` constant | exactly what Microsoft documents; a concrete tenant id or verified domain is passed through unchanged |
| 3.3 | Keep the tenant id editable (no new setting, no migration) — `MICROSOFT_TENANT_ID` and Admin > Providers → tenant id already exist | multi-tenant only needs `common` (the default) plus a multi-tenant registration; nothing else in the code base keys off the tenant |
| 3.4 | Replace the `single_tenant_recommended` warning with `tenant_scope`, reword the Microsoft `app` and `consent` steps and the tenant hint | the warning is a code (`admin.guide.warnings.<code>`) the UI renders; a rename is one row in `guideWarnings` plus one key per catalog, and `single_tenant_recommended` would keep contradicting the shipped feature |
| 3.5 | Per-directory consent is documented, not tracked per directory | the stored record answers "does *this* instance have a consented directory"; a connection from a directory that has not consented yet still fails with `consent_required`, and a row per directory would need a new table for a status message |
| 3.6 | No silent fallback when a step translation is missing: a raw key keeps being rendered | the mismatch that produced one is gone (the component now reloads per provider) and `internal/services/i18n_test.go` fails the build when a step the API can return has no key; hiding the key behind an empty string would only make a real breakage invisible |

## 4. Changes

### 4.1 Frontend — `web/src/views/admin/SetupGuideView.vue`

* extract the outcome handling of `onMounted` into `showOutcome()`;
* `watch(provider, …)` → clear `guide`, `load()`, `showOutcome()`; `onMounted`
  keeps doing the same for the first paint.

### 4.2 Backend — `internal/providers/microsoft/provider.go`

* `adminConsentTenant(creds) string`: `tenantID(creds)` values that are generic
  (`common`, `consumers`) become `organizations`; everything else (uuid, verified
  domain) is untouched;
* `AdminConsentURL` uses `adminConsentTenant`;
* comments on both accessors so the difference (sign-in vs consent) is explicit.

### 4.3 Guide — `internal/services/guide.go`

* `single_tenant_recommended` → `tenant_scope` in the Microsoft warning list.

### 4.4 Catalogs — the same four edits in all seven locales

| Key | Change |
|---|---|
| `admin.tenantIdHint` | explain: empty/`common` = any directory (multi-tenant registration), tenant id/domain = one directory |
| `admin.guide.steps.microsoft.app.body` | offer *any organizational directory* (multi-tenant) next to single-tenant |
| `admin.guide.steps.microsoft.consent.body` | the consent is granted **per directory**; repeat the button for every directory |
| `admin.guide.warnings.single_tenant_recommended` → `admin.guide.warnings.tenant_scope` | how several directories are connected |

No key is added or removed: the catalogs stay at 499 keys and the placeholder
sets (`{tenant}`, `{date}`) are untouched.

### 4.5 Tests

* `internal/providers/microsoft/provider_test.go`: `TestConsentTenantKeepsASingleDirectory`
  (empty/`common`/`consumers` → `organizations`, uuid and verified domain
  passthrough).
* `internal/providers/microsoft/permissions_test.go`: `TestAdminConsentURLAcceptsEveryDirectory`
  plus the crafted-tenant fallback expectation of
  `TestAdminConsentURLRejectsATenantThatNavigates` moving from `common` to
  `organizations`.
* `internal/handlers/guide_test.go`: `TestAdminConsentEndpoint` expects the
  `/organizations/v2.0/adminconsent` segment — the regression this change is about.
* `internal/services/i18n_test.go` already fails the build when a step, warning or
  capability the API can return has no translation in any of the seven catalogs —
  it covers the rename and the updated values without a new test.

### 4.6 Docs

* `docs/app-registration.md` §3.1 (steps 1 and 6), §3.2 (per-directory consent),
  §3.3 (the `organizations` mapping) and §4 (the warning list);
* `docs/configuration.md` (`MICROSOFT_TENANT_ID` row) and `docs/oauth.md` §1.1
  (how `{tenant}` is chosen for the consent URL);
* `docker/.env.example` (what the default tenant means for each mode).

## 5. Verification

```bash
cd web && node scripts/check-i18n.mjs && npx vue-tsc --noEmit && npm run build
go build ./... && go vet ./internal/...
go test ./internal/providers/... ./internal/services/... ./internal/handlers/...
```

* seven catalogs in parity — 499 keys each, unchanged, and the changed values are
  the only lines in the `git diff` of `web/src/i18n/locales/` (4 per file);
* the SPA type-check and the production build succeed (so the embedded
  `web/dist` is refreshed);
* every Go package builds, `vet` is clean, and the provider, service and handler
  suites pass;
* `GET /api/providers/microsoft/guide` still returns the same step ids.

## 6. Progress log

* 2026-09-29 — reconnaissance: root-caused 2.1 (missing reload), 2.2 (`common` in
  the adminconsent path, against Microsoft's documented `organizations`) and 2.3.
  Plan written before the first edit.
* 2026-09-29 — implemented 4.1 (watcher plus `showOutcome()`, with the `load()`
  answer ignored when the tab changed while it was in flight), 4.2
  (`consentTenant` + `organizationsTenant`), 4.3 (`tenant_scope`), 4.4 (the four
  messages in the seven catalogs) and 4.6 (docs).
* 2026-09-29 — verification: `check-i18n.mjs` 7 × 499 keys, `vue-tsc` and
  `vite build` clean, `go build ./...` + `go vet ./internal/...` clean, and
  `go test` green for `internal/providers/...`, `internal/services/...` and
  `internal/handlers/...` — the last one needed the `organizations` expectation of
  `TestAdminConsentEndpoint` (4.5), which is the test that caught the change.
