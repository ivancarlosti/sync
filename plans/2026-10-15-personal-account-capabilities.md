# Personal Microsoft accounts: the directory permissions are not missing, they are not applicable

* **Date:** 2026-10-15
* **Repo:** `ivancarlosti/sync`
* **Status:** done (see the progress log at the bottom)
* **Related:** [`2026-10-06-personal-account-sync.md`](./2026-10-06-personal-account-sync.md), [`../docs/providers.md`](../docs/providers.md), [`../docs/app-registration.md`](../docs/app-registration.md), [`../docs/api.md`](../docs/api.md)

## 1. Objective

Connecting a **personal** Microsoft account (an MSA, `@outlook.com`) works, but the
Accounts screen immediately paints every capability badge red and prints
*"This account was connected before these permissions existed: reconnect it to grant
them."* — with a *Reconnect* button that cannot possibly deliver what it promises:
every reconnect returns exactly the same grant.

The wanted end state: the screen distinguishes **not granted yet** (actionable,
red) from **cannot be granted to this kind of account** (not actionable, greyed out
with one line of explanation), and a personal account is no longer nagged to
reconnect for something Entra ID will never give it.

## 2. Root cause

| # | Symptom | Cause |
|---|---|---|
| 2.1 | all eight capability badges red on an MSA, plus the reconnect alert | `GET /api/accounts` derived the badges from `providers.MissingCapabilities(providers.Catalog(microsoft), granted)`: the Microsoft table requests **11 directory scopes** (`User.Read.All`, `User.ReadWrite.All`, `Group.ReadWrite.All`, `Directory.ReadWrite.All`, `GroupMember.ReadWrite.All`, `Domain.Read.All`, `AdministrativeUnit.ReadWrite.All`, `RoleManagement.Read.All`, `Organization.Read.All`, `LicenseAssignment.ReadWrite.All`) plus `Sites.ReadWrite.All` (`internal/providers/microsoft/permissions.go`) |
| 2.2 | the reconnect never helps | an MSA is **not a member of a directory**: it is a tenant of its own (the well known *consumers* tenant `9188040d-6c67-4c5b-b112-36a304b66dad`). Entra ID never grants delegated directory scopes to it, so no consent screen offers them and every token response comes back without them |
| 2.3 | `CapabilityFiles` red too, although OneDrive works | that one capability is declared by *both* grantable scopes (`User.Read`, `Files.ReadWrite.All`) and by `Sites.ReadWrite.All`, which an MSA cannot hold — so the split has to be per **permission**, not per capability |
| 2.4 | the setup guide blamed the wrong thing | `admin.guide.warnings.work_accounts_only` said *"…so personal OneDrive accounts can no longer be connected"*, which was never true (and is even less true after this change) |


## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| 3.1 | Classify the account from the **tenant claim of the stored access token** (`tid`, else the tenant segment of `iss`) | it costs no Graph call, no refresh and no write, so an account connected before this release is classified in place; the claim is read, never signature-checked |
| 3.2 | Behind an optional `providers.AccountClassifier`, implemented by Microsoft alone | `internal/providers` stays provider-neutral and Google (or any future provider) needs nothing |
| 3.3 | `providers.AccountKind` with `""` (unknown), `personal` and `work`; unknown means **nothing is filtered out** | an account no signal could classify keeps the exact report it had before kinds existed; the column is never required |
| 3.4 | Fall back to `GET /me/drive` (`driveType`) when the token carries no readable claim | the same probe `SearchSites` already keeps for MSAs, and it costs one call only in the exotic case |
| 3.5 | `Permission.Kinds` + `providers.ApplicablePermissions` split the table **per permission** | 2.3: filtering per capability would take OneDrive away from an account that can use it |
| 3.6 | `NotApplicable` is reported for a capability the applicable half does **not declare at all** | a capability declared by both halves (`files`) is judged by its applicable half, so it is never listed twice — once green and once grey (`providers.ReportCapabilities`) |
| 3.7 | `needs_reconnect` counts only applicable capabilities | the bug in one sentence: a reconnect is only offered when it can change something |
| 3.8 | New optional `connected_accounts.kind` column (`VARCHAR(16)`), written **only** when the provider knows the kind (`Complete`, `verify`) and never cleared by an unknown | the token claim is read anyway; the column saves the read. Rows written before the column existed stay valid (`""` ⇒ nothing filtered) |
| 3.9 | The API reports `account_kind` and `not_applicable_capabilities`; the UI renders the third state with the `muted` badge tone plus one explanatory line | the operator sees *why* a badge is out of reach instead of a false alert |
| 3.10 | `Scopes()` keeps requesting the whole table for every account | the kind is unknown until the flow returns; the request is the same one that worked before, and Entra simply grants what it can |
| 3.11 | Fix the false `work_accounts_only` copy in all seven catalogs | 2.4: the promise it makes ("cannot be connected") is wrong |

## 4. Implementation

### 4.1 `internal/providers/provider.go`

* `AccountKind` (`AccountKindUnknown` / `AccountKindPersonal` / `AccountKindWork`),
  `Account.Kind`, `Permission.Kinds`, `AccountClassifier`,
* `AppliesToKind`, `ApplicablePermissions` (never nil slices),
* `HasCapability` refactored around the new `DeclaresCapability`,
* `CapabilityReport{Granted, Missing, NotApplicable}` + `ReportCapabilities`, which
  reads both halves of the table at once (3.6).

### 4.2 `internal/providers/microsoft/`

* `permissions.go` — `workAccounts` (`[]providers.AccountKind{work}`) tags the 11
  directory rows and `Sites.ReadWrite.All`;
* `account_kind.go` (new) — `consumerTenantID`, `AccountKindOf` (claim only),
  `accountKind` (claim, then `ownDrive`), `tokenTenant` / `jwtPayload` /
  `issuerTenant` (base64url with and without padding, answering "unknown" for
  anything unreadable), and the compile-time guard
  `var _ providers.AccountClassifier = (*Provider)(nil)`;
* `provider.go` — `Account()` fills `account.Kind` with that best-effort answer.

### 4.3 Storage and services

* `internal/models/account.go` — `Kind string` with `gorm:"size:16"` (optional
  column, auto-migrated by GORM like every other one);
* `internal/services/tokens.go` — `TokenManager.AccountKind(account)`: the second
  and last reader of `connected_accounts.access_token`, decrypting it to read
  claims only (no refresh, no write);
* `internal/services/oauth.go` — `Complete` copies `remote.Kind` onto the row when
  it is known, never clearing it otherwise.

### 4.4 `internal/handlers/accounts.go`

* `accountView` gains `account_kind` (omitted when unknown) and
  `not_applicable_capabilities[]`;
* `accountViewOf` resolves the kind (`accountKind`: the column, else the token),
  splits the table (`accountPermissions`) and builds the view — used by both the
  list and *Check the token*, so the two never disagree;
* `refreshIdentity` copies `remote.Kind`, never clearing it.

### 4.5 UI

* `web/src/lib/api.ts` — `account_kind?: 'personal' | 'work'` and
  `not_applicable_capabilities: Capability[]`;
* `web/src/components/CapabilityBadges.vue` — new `notApplicable` prop, the `muted`
  tone for it, and the badge order granted → not applicable → missing (the
  actionable red ones last);
* `web/src/views/AccountsView.vue` — passes the prop and shows
  `accounts.notApplicableCapabilities` when that list is not empty.

### 4.6 Catalogs — one key added, one rewritten, in all seven locales

| Key | en-US |
|---|---|
| `accounts.notApplicableCapabilities` | Not applicable: a personal Microsoft account cannot be granted the directory permissions. |
| `admin.guide.warnings.work_accounts_only` | Directory permissions cannot be granted to personal Microsoft accounts: a personal account connects with the file permissions only, and the directory screens stay unavailable to it. |

`npm run typecheck` reports **7 catalogs × 667 keys, 478 referenced from `src/`**,
189 referenced only dynamically; `docs/i18n.md` was updated to those figures. No
guide key changed, so `internal/services/i18n_test.go` needed no new entry.

### 4.7 Tests

* `internal/providers/provider_test.go` —
  `TestApplicablePermissionsSplitsByAccountKind` (the per-permission split and the
  report: `files` granted, `users` out of reach, undeclared capabilities still
  missing) and `TestApplicablePermissionsKeepsUnknownKindsWhole` (the unknown kind
  reproduces `CapabilitiesOf` + `MissingCapabilities` exactly);
* `internal/providers/microsoft/account_kind_test.go` (new) —
  `TestAccountKindOfReadsTheTenantClaim` (tid, both `iss` shapes, case tolerance,
  nil/empty/opaque/non-base64/non-JSON/tenant-less claims) and
  `TestProviderImplementsTheAccountClassifier`;
* `internal/providers/microsoft/permissions_test.go` —
  `TestPersonalAccountsKeepOnlyTheScopesTheyCanHold`: the MSA grant yields
  `files` granted, nothing missing, and exactly the seven directory capabilities as
  not applicable, while a work account keeps the report it always had;
* `internal/providers/microsoft/provider_test.go` —
  `TestGraphRewritesTheMeSentinel` now expects the extra `/me/drive` probe of an
  unreadable token and asserts the kind it produces;
* `internal/handlers/account_permissions_test.go` (new) —
  `TestPersonalAccountsNeverAskToReconnectForDirectoryPermissions` drives
  `GET /api/accounts` end to end over four fixtures (classified from the token,
  classified by the column, a work account, an opaque token) and
  `TestVerifyAccountStoresTheAccountKind` pins that *Check the token* writes the
  kind back and that an unclassifying refresh does not clear it.

### 4.8 Docs

`docs/api.md` §5 (`account_kind`, `not_applicable_capabilities`),
`docs/providers.md` §1 (the `AccountClassifier` interface, `Permission.Kinds`),
§4 (an "Account kind" row), §5 (the Accounts bullets) and §6 (a troubleshooting
row), `docs/app-registration.md` §1/§3.3 (the personal-account note) /§4/§5,
`docs/database.md` §2 (the `kind` column), `docs/oauth.md` §3 (the scope table
paragraphs) and `docs/i18n.md` (the key report).

## 5. Verification

```bash
export PATH=$HOME/.local/goroot/bin:$PATH
gofmt -l internal/            # no output
go build ./... && go vet ./...
go test ./...
cd web && npm run typecheck   # check-i18n + vue-tsc (both configs)
```

Green: every Go package `ok`, `check-i18n: ok — 7 catalogs × 667 keys, 478 keys
referenced from src/, 189 referenced only dynamically`. The end-to-end fixture is
the regression guard: a personal account listed with `account_kind: "personal"`,
`capabilities: ["files"]`, `missing_capabilities: []`, `needs_reconnect: false` and
`not_applicable_capabilities: [users, groups, members, domains, orgunits, roles,
licenses]`; the same grant on a work account still reports all eight capabilities
missing.

## 6. Progress log

* 2026-10-15 — reconnaissance: reproduced 2.1 by reading the handler path
  (`accounts.go` → `providers.MissingCapabilities` over the whole Microsoft table),
  confirmed 2.2 with the *consumers* tenant id, found 2.3 while checking whether a
  per-capability filter would do, and 2.4 in the catalogs. Decision A (the token
  claim, with the own-drive fallback, persisted as an optional column) agreed with
  the operator.
* 2026-10-15 — implemented 4.1–4.8; `gofmt`, `go build`, `go vet`, `go test ./...`
  and `npm run typecheck` all green.

## 7. Follow-up

* **A personal account is still asked for the directory scopes.** `Scopes()` cannot
  know the kind before the flow returns, so the authorization request carries the
  whole table for every account and keeps doing what it did before (Entra grants
  what it can). If Microsoft ever refuses such a request for an MSA instead of
  ignoring the extra scopes, the flow would need a two-step start (classify, then
  re-request) — deliberately not built now.
* **`kind` is a hint, not an authorization decision.** The claim is not
  signature-checked and nothing is enforced from it; every operation still goes
  through Graph with the stored token, where a missing scope surfaces as the usual
  403. Should the kind ever gate behaviour rather than a badge, the check belongs
  server side, behind a real token validation.
* **The grey badge names the capability, not the scope.** On a personal account the
  `files` badge stays green even though `Sites.ReadWrite.All` cannot be held, so the
  SharePoint part of the capability is not visible in the badge list (the folder
  picker already leaves the site field out entirely, see
  `2026-10-06-personal-account-sync.md`). A per-scope breakdown would be the honest
  next step if the nuance ever needs to be shown.

