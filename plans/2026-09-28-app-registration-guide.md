# Guided provider app registration + always-on directory permissions

* **Date:** 2026-09-28
* **Repo:** `ivancarlosti/sync`
* **Status:** in progress (see the progress log at the bottom)
* **Related:** [`2026-09-27-first-building.md`](./2026-09-27-first-building.md)

## 1. Objective

An operator who has never opened the Google Cloud console or the Entra admin
center must be able to create the OAuth client this instance needs, from inside
Sync, and end up with a connection that already holds every permission the
roadmap needs (user management, Microsoft distribution lists, Google groups,
creation, migration, exclusion).

Two deliverables:

1. **A guided registration flow** (`Admin > Setup guide` + `GET /api/providers/:provider/guide`):
   ordered steps per provider, the exact redirect URI, the exact permission list,
   console deep links, and the tenant-wide admin consent action with its real
   status. Today this knowledge lives in `docs/providers.md` §3 (11 lines) and is
   partially missing (no consent-screen types, no verification warning, no API
   controls allowlist, no admin-consent step for Microsoft).
2. **The permission groundwork:** both providers request the complete scope set
   from now on, so a connection made today is immediately ready for the roadmap
   and never needs another reconnect.

## 2. Decisions taken (2026-09-28)

| # | Decision | Rationale |
|---|---|---|
| 2.1 | **One always-on scope set** — no capability "profiles", no `oauth_states.profile` column, no `Credentials.Scopes` field | approved explicitly; keeps `models` and the credential resolver untouched, and makes "connect an account" a single, predictable action |
| 2.2 | **The broadest directory set**, including role and licence scopes (`Directory.ReadWrite.All`, `RoleManagement.Read.All`, `admin.directory.rolemanagement.readonly`, `apps.licensing`, `LicenseAssignment.ReadWrite.All`) | approved explicitly: future reporting/licence features must not force another reconnect |
| 2.3 | **Admin consent becomes mandatory for Microsoft** (every directory permission is `AdminConsentRequired = Yes`), so it becomes a first-class feature: a one-click tenant `adminconsent` flow, a stored consent status, and a real `consent_required` error code | today an un-consented tenant answers `code: "validation"` → *"The provider answered with an unexpected payload"*, which is misleading and unactionable |
| 2.4 | **One permission table per provider** is the single source of truth for `Scopes()`, the guide, the capability badges and the docs | code and instructions can never drift; adding a permission is one table row |
| 2.5 | **Capabilities are derived from the granted scope string** already stored in `connected_accounts.scopes` | no migration, no new column, and a stale account is detected automatically (`missing_capabilities`) |
| 2.6 | **Guide content is server-side and structured** (i18n keys + copy targets), with the long-form narrative in English in `docs/app-registration.md` | seven catalogs stay small and honest; the parity checker never forces translating prose |
| 2.7 | **Google uses 3LO with an admin identity in this phase**; service-account + domain-wide delegation is the documented follow-up | no new secret type (an encrypted service-account key) in this iteration |

### Consequences to document

* Widening the scope set does **not** break existing installs: a stored grant stays
  as it is. File sync keeps working; reconnecting an account is what applies the
  wider grant, and the Accounts screen reports what an account is missing.
* After this change a **Microsoft connection requires the tenant-wide admin
  consent**; personal Microsoft accounts can no longer be connected (directory
  permissions cannot be granted to them).
* Admin consent unlocks the scopes for every user of the tenant, but a directory
  *operation* still requires the acting identity to hold the Entra role
  (User Administrator / Groups Administrator) — the grant is not the privilege.

---

## 3. Permission matrix (verbatim scope strings)

The tables below are the contract; the code mirrors them row for row
(`internal/providers/google/permissions.go`,
`internal/providers/microsoft/permissions.go`).

### 3.1 Google (OAuth 2.0 / 3LO, admin identity)

| Capability | Scope | Admin identity |
|---|---|---|
| `files` | `openid` | no |
| `files` | `https://www.googleapis.com/auth/userinfo.email` | no |
| `files` | `https://www.googleapis.com/auth/userinfo.profile` | no |
| `files` | `https://www.googleapis.com/auth/drive` | no |
| `users` | `https://www.googleapis.com/auth/admin.directory.user` | yes |
| `users` | `https://www.googleapis.com/auth/admin.directory.user.alias` | yes |
| `groups` | `https://www.googleapis.com/auth/admin.directory.group` | yes |
| `members` | `https://www.googleapis.com/auth/admin.directory.group.member` | yes |
| `domains` | `https://www.googleapis.com/auth/admin.directory.domain.readonly` | yes |
| `orgunits` | `https://www.googleapis.com/auth/admin.directory.orgunit` | yes |
| `roles` | `https://www.googleapis.com/auth/admin.directory.rolemanagement.readonly` | yes |
| `licenses` | `https://www.googleapis.com/auth/apps.licensing` | yes |

APIs to enable in the same project: Drive API (`drive.googleapis.com`), Admin SDK
API (`admin.googleapis.com`), Enterprise License Manager API
(`licensing.googleapis.com`). Google has no tenant-consent endpoint: the guide
explains the administrator identity requirement and the Admin console API-controls
allow-list instead.

### 3.2 Microsoft (Graph delegated, all admin consent required)

| Capability | Scope |
|---|---|
| `files` | `offline_access`, `openid`, `profile`, `email` |
| `files` | `https://graph.microsoft.com/User.Read` |
| `files` | `https://graph.microsoft.com/Files.ReadWrite.All` |
| `files` | `https://graph.microsoft.com/Sites.ReadWrite.All` |
| `users` | `https://graph.microsoft.com/User.Read.All` |
| `users` | `https://graph.microsoft.com/User.ReadWrite.All` |
| `groups` | `https://graph.microsoft.com/Group.ReadWrite.All` |
| `groups` | `https://graph.microsoft.com/Directory.ReadWrite.All` |
| `members` | `https://graph.microsoft.com/GroupMember.ReadWrite.All` |
| `domains` | `https://graph.microsoft.com/Domain.Read.All` |
| `orgunits` | `https://graph.microsoft.com/AdministrativeUnit.ReadWrite.All` |
| `roles` | `https://graph.microsoft.com/RoleManagement.Read.All` |
| `licenses` | `https://graph.microsoft.com/Organization.Read.All` |
| `licenses` | `https://graph.microsoft.com/LicenseAssignment.ReadWrite.All` |

Scope matching is prefix tolerant (`providers.normalizeScope`), because the
identity platform reports a Graph scope either qualified or short.

### 3.3 Capability names

`files`, `users`, `groups`, `members`, `domains`, `orgunits`, `roles`,
`licenses` (`providers.Capability`, `providers.ProviderCapabilities`) — the UI
vocabulary of the Accounts screen and the guide; labels live in
`admin.capability.*`.

---

## 4. Phases

### Phase 1 — Permission tables (single source of truth)
- [x] `providers.Capability` + `providers.Permission` + `ScopeCatalog` +
      `ConsentGranter` + `AuthError`/`IsConsentRequired` in `provider.go`.
- [x] `ScopesOf`, `CapabilitiesOf`, `MissingCapabilities`, `HasCapability`,
      `Catalog`, `ConsoleURLs`, prefix-tolerant `containsScope`.
- [x] `internal/providers/google/permissions.go` and
      `internal/providers/microsoft/permissions.go` (tables + `ConsoleURLs`,
      Microsoft also `AdminConsentURL`).
- [x] `Scopes()` of both providers delegates to the table (no literal list left).
- [x] Tests: exact scope lists pinned, capability coverage, `AdminConsentURL`
      contract and tenant escaping, `AuthError`/consent classification.

### Phase 2 — `consent_required` instead of `validation`
- [x] `services.ErrConsent` (+ `IsConsentRequired`), 412 +
      `code: "consent_required"` in `handlers/respond.go`.
- [x] Both providers map an authorization-server refusal to `providers.AuthError`
      during the code exchange; `OAuthService.Complete` wraps a consent refusal
      in `ErrConsent`.
- [x] The callback maps the OAuth `error` values (`connectErrorCode`) and passes
      the provider in the query string, so the SPA can offer the guide.

### Phase 3 — Tenant-wide admin consent (Microsoft)
- [x] `models.FlowAdminConsent` + `models.SettingAdminConsentSuffix`.
- [x] `OAuthService.BeginAdminConsent`, `CompleteAdminConsent`, `FlowOf`,
      `AbandonAdminConsent`; `Store.FindOAuthState` (peek, no consume).
- [x] `POST /api/oauth/:provider/admin-consent`, callback branch on
      `admin_consent=True` → `302 /admin/guide/<provider>?consent=granted`.
- [x] `ProviderSettings.SetAdminConsent`/`AdminConsent` (`{tenant, client_id, at}`,
      `granted` computed against the current client id), exposed in
      `ProviderCredentialsInfo.admin_consent`.

### Phase 4 — Guide service and endpoint
- [x] `services.GuideService` (`internal/services/guide.go`): steps, console
      links, permission table, capabilities, consent state, warnings.
- [x] `GET /api/providers/:provider/guide` (`handlers/providers.go`), wired
      through `Deps.Guide` (mandatory dependency).
- [x] Tests: shape for both providers, unknown provider → `ErrNotFound`,
      consent record round trip, `Clear` keeps the record.
- [x] Endpoint tests (`internal/handlers/guide_test.go`): the guide answer for
      both providers, `400` for an unknown one, both admin-consent callback
      outcomes, and the capability derivation of `GET /api/accounts`.

### Phase 5 — Accounts capabilities
- [x] `accountView.capabilities` / `missing_capabilities` / `needs_reconnect`
      derived from `connected_accounts.scopes` (no migration).
- [x] Units on the API, not on the engine: file synchronisation is untouched.


### Phase 6 — SPA
- [x] `api.ts`: `Capability`, `AdminConsentStatus`, `GuidePermission`,
      `GuideStep`, `ProviderGuide`, `providers.guide()`, `accounts.adminConsent()`,
      account capability fields, `ProviderInfo.admin_consent`.
- [x] `views/admin/SetupGuideView.vue` (route `admin-guide`, `/admin/guide/:provider?`):
      provider switch, numbered steps with console links, copy fields for the
      redirect URI and the scope list, consent button + state, permission table
      with capability labels, caveats, capability badges, outcome banner.
- [x] `components/CopyField.vue` (clipboard with a legacy fallback),
      `components/CapabilityBadges.vue`.
- [x] `AdminLayout` tab + `AppHeader` match + provider cards link to the guide.
- [x] `AccountsView`: capability badges, missing-permission hint, *Reconnect*
      action, and a `consent_required` banner that links to the guide.

### Phase 7 — i18n, documentation, validation
- [x] 7 catalogs × ~81 new keys (`nav.adminGuide`, `accounts.*` ×5,
      `admin.capability.*` ×8, `admin.guide.*`), step/warning/capability text
      included, `ar-SA` mirrored.
- [x] `internal/services/i18n_test.go`: every key the guide API can make the UI
      render exists in every catalog; the catalogs are in parity; capability
      labels are distinct (the Go side of `web/scripts/check-i18n.mjs`).
- [x] `docs/app-registration.md` (long form, English) + updates to
      `providers.md`, `oauth.md`, `api.md`, `README.md`.
- [x] Validation: `gofmt`, `go build ./...`, `go vet ./...`, `go test ./...`,
      `node web/scripts/check-i18n.mjs`, `npx vue-tsc --noEmit`.

---

## 5. Planned file map (all implemented)

```text
internal/providers/provider.go                     Capability/Permission/Catalog/ConsentGranter/AuthError
internal/providers/{google,microsoft}/permissions.go   permission tables (+ ConsoleURLs, AdminConsentURL)
internal/providers/provider_test.go                capability/consent classification
internal/providers/{google,microsoft}/permissions_test.go  pinned scope lists
internal/models/enums.go                           FlowAdminConsent, SettingAdminConsentSuffix
internal/services/errors.go                        ErrConsent
internal/services/credentials.go                   AdminConsentStatus + SetAdminConsent/AdminConsent
internal/services/oauth.go                         consent mapping, Begin/CompleteAdminConsent, FlowOf
internal/services/store.go                         FindOAuthState (peek, no consume)
internal/services/guide.go                         GuideService
internal/services/guide_test.go, i18n_test.go      guide + translation drift tests
internal/handlers/{handlers,providers,oauth,accounts,routes}.go  dependency, endpoints, view fields
internal/handlers/guide_test.go                    endpoint tests of the new routes
cmd/server/main.go                                 wiring
web/src/lib/api.ts                                 types + guide/admin-consent calls
web/src/views/admin/SetupGuideView.vue             the screen
web/src/components/{CopyField,CapabilityBadges}.vue
web/src/views/{AccountsView,admin/ProvidersView}.vue, layouts/AdminLayout.vue,
web/src/components/AppHeader.vue, web/src/router/index.ts
web/src/i18n/locales/*.json                        7 catalogs
docs/app-registration.md (+ providers/oauth/api/README)

## 6. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Widening the scope set is a breaking change for an existing install | the grant is only replaced on reconnect; the Accounts screen reports `missing_capabilities` instead of failing later; the plan and the docs say so explicitly |
| Personal Microsoft accounts can no longer connect | documented (guide warning `work_accounts_only`, `app-registration.md` §3.3); single-tenant registration recommended |
| Google sensitive scopes require verification for an External app | the guide recommends an Internal consent screen and warns about the 7-day refresh tokens of a Testing app (`consent_screen_type`) |
| An operator grants consent but the client id changed | `AdminConsentStatus.Granted` compares the stored client id with the configured one, so the guide reports "not granted" again (test `TestAdminConsentBelongsToTheClientItWasGrantedTo`) |
| Guide text and code drift apart | steps/warnings/capabilities come from the binary; `i18n_test.go` fails the build on a missing translation, and the scope lists are pinned by unit tests |
| A translated key is forgotten in one locale | `go test ./internal/services/...` (parity) plus `npm run build` (`check-i18n.mjs`) |
| The consent callback could be forged | the state row is single use, signed, flow-scoped and validated before anything is recorded; a refusal consumes it too |

## 7. Acceptance criteria

1. `Admin > Setup guide` lists, per provider, every step with a working console
   link, the exact redirect URI and the exact permission list, both copyable.
2. `GET /api/providers/:provider/guide` matches `Scopes()` of the running binary
   (asserted by tests) and reports the redirect URI that is actually registered.
3. A Microsoft connection without tenant consent answers **412
   `consent_required`** (not `validation`), and the screen offers the setup guide;
   the guide's consent button performs the tenant-wide consent and the status is
   then shown (and stored).
4. `GET /api/accounts` reports `capabilities`, `missing_capabilities` and
   `needs_reconnect` per account, and the Accounts screen renders them plus a
   *Reconnect* action.
5. File synchronisation, jobs and runs behave exactly as before: no schema
   change, no engine change, `go test ./...` green.
6. Every new string exists in all 7 catalogs (`check-i18n.mjs` + the Go parity
   test) and `npx vue-tsc --noEmit` is clean.

## 8. Progress log

Rule: update this section at the end of every work session.

### 2026-09-28 — session 1 (implementation, phases 1-7)
- Backend: permission tables for both providers, `Capability`/`Permission` API,
  `AuthError`/`ErrConsent` → 412 `consent_required`, Microsoft tenant-wide
  admin-consent flow (start + callback + stored status), `GuideService` and
  `GET /api/providers/:provider/guide`, account capability derivation.
- Frontend: `admin-guide` route and screen, `CopyField`, `CapabilityBadges`,
  Providers/Accounts integration, 7 catalogs × 499 keys (81 new).
- Docs: `docs/app-registration.md` (new), `providers.md`, `oauth.md`, `api.md`,
  `README.md`.
- Verified: `gofmt`, `go build ./...`, `go vet ./...`, `go test ./...` (all
  packages green, incl. the new provider/guide/i18n tests),
  `node web/scripts/check-i18n.mjs` (7 × 499 keys in parity),
  `npx vue-tsc --noEmit` (exit 0).
- Next: live smoke test with real Google/Microsoft clients (the only step that
  needs credentials), a visual pass on the guide screen (dark theme, `ar-SA` RTL),
  and — if the directories are to be written to — the actual user/group/licence
  operations these permissions were requested for.

plans/2026-09-28-app-registration-guide.md         this file
```

