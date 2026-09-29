# Registering the provider applications

This is the long form of **Admin > Setup guide** (`GET /api/providers/:provider/guide`).
The screen and this document describe the same thing: the guide renders the
permission table of the compiled provider (see `internal/providers/*/permissions.go`),
so it cannot drift from what Sync actually requests.

Sync needs one OAuth client per provider. Nothing else: no service account, no
certificate, no shared secret beyond the client secret.

* [1. What the permissions are for](#1-what-the-permissions-are-for)
* [2. Google Workspace](#2-google-workspace)
* [3. Microsoft 365](#3-microsoft-365)
* [4. The in-app guide](#4-the-in-app-guide)
* [5. Reconnecting after an upgrade](#5-reconnecting-after-an-upgrade)
* [6. Troubleshooting](#6-troubleshooting)

## 1. What the permissions are for

Every permission Sync requests is requested from the first connection, in one
authorization request ("one always-on scope set"). A connection made today
therefore works after an upgrade instead of failing on an operation the token was
never allowed to perform. The cost is a longer consent screen; the benefit is
that `<provider> → Accounts → Check the token` is the only repair step you ever
need.

| Capability | Google scopes | Microsoft Graph scopes |
|---|---|---|
| `files` — browsing and transferring files | `drive`, `userinfo.email`, `userinfo.profile`, `openid` | `Files.ReadWrite.All`, `Sites.ReadWrite.All`, `User.Read`, `offline_access`, `openid`, `profile`, `email` |
| `users` — user accounts | `admin.directory.user`, `admin.directory.user.alias` | `User.Read.All`, `User.ReadWrite.All` |
| `groups` — groups and distribution lists | `admin.directory.group` | `Group.ReadWrite.All`, `Directory.ReadWrite.All` |
| `members` — group membership | `admin.directory.group.member` | `GroupMember.ReadWrite.All` |
| `domains` — verified domains | `admin.directory.domain.readonly` | `Domain.Read.All` |
| `orgunits` — organisational units | `admin.directory.orgunit` | `AdministrativeUnit.ReadWrite.All` |
| `roles` — administrative role catalogue (read-only) | `admin.directory.rolemanagement.readonly` | `RoleManagement.Read.All` |
| `licenses` — subscriptions and licence assignment | `apps.licensing` | `Organization.Read.All`, `LicenseAssignment.ReadWrite.All` |

`GET /api/accounts` reports, per connected account, which of these capabilities
the *stored* grant satisfies (`capabilities`) and which the current release asks
for on top of it (`missing_capabilities`, `needs_reconnect`), which is what the
Accounts screen renders as badges.

Two things a permission does **not** buy:

* **A role.** Granting an application the right to manage users does not make the
  connected identity an administrator: Google requires the connecting account to
  hold a Workspace administrator role, Microsoft requires a directory role
  (User Administrator, Groups Administrator, License Administrator).
* **An API.** Google scopes only work once the matching API is enabled in the
  project (Drive API, Admin SDK API, Enterprise License Manager API); a disabled
  API answers a permission error, not a "not enabled" error.

## 2. Google Workspace

### 2.1 Steps

1. **Project** — <https://console.cloud.google.com/projectcreate>. Create a
   project dedicated to Sync (for example `sync-files`).
2. **APIs** — enable the three APIs *in that project*:
   [Drive API](https://console.cloud.google.com/apis/library/drive.googleapis.com),
   [Admin SDK API](https://console.cloud.google.com/apis/library/admin.googleapis.com),
   [Enterprise License Manager API](https://console.cloud.google.com/apis/library/licensing.googleapis.com).
3. **OAuth consent screen** (APIs & Services → OAuth consent screen):
   * *Internal* for a Workspace domain: only the domain's accounts can authorise,
     and Google does not review the scopes. This is the recommended choice.
   * *External* works too, but while the app stays in *Testing* only the accounts
     published as test users can authorise it, and their refresh tokens expire
     after **seven days**. Publish the app to remove both limits.
4. **Client** — Credentials → Create credentials → OAuth client ID → *Web
   application*. Copy the **client id** and the **client secret** into
   Admin > Providers (or the `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET`
   environment variables).
5. **Redirect URI** — in the same client, add
   `APP_URL/api/oauth/google/callback` to *Authorised redirect URIs* (verbatim:
   the guide shows the exact value).
6. **Permissions** — on the consent screen, add every scope of the table above.
   Google classifies `admin.directory.*` and `apps.licensing` as **sensitive**, so
   an External app has to be verified to be used by more than a handful of
   accounts.
7. **API controls** — in the Admin console (Security → API controls → App access
   control) mark the client ID as *Trusted*, or add it to the allow-list of the
   organisational units that must be able to authorise it. Without this, a
   Workspace account can be refused the grant even though the consent screen was
   configured correctly.
8. **Connect** — Accounts → *Connect Google Drive*, signed in as an administrator
   (a super administrator for the directory permissions).

### 2.2 Notes

* Google has **no tenant-wide consent endpoint**: the grant always belongs to the
  account that authorised it. Use a dedicated administrator account for Sync, and
  revoke its access from Google's *Third-party apps* page when you retire the
  instance.
* `access_type=offline` + `prompt=consent` + `include_granted_scopes` are set by
  the provider, which is what makes a reconnection always return a refresh token.
* `include_granted_scopes` means a grant accumulates: if an operator authorises
  with a narrower set once (an old release, a manual URL), the union of both
  grants is what Google reports back — and Sync reports the capabilities of that
  union.

## 3. Microsoft 365

### 3.1 Steps

1. **Registration** — [Entra admin center](https://entra.microsoft.com) →
   Applications → App registrations → *New registration*, for example `Sync`.
   Choose *Accounts in this organizational directory only* for one directory, or
   *Accounts in any organizational directory (multi-tenant)* to let several
   directories connect to the same client — every directory administrator then
   grants the consent for their own directory (see the tenant step below). A
   multi-tenant registration also has to be publisher verified before other
   organisations can consent to it.
2. **Redirect URI** — Authentication → Add a platform → **Web** →
   `APP_URL/api/oauth/microsoft/callback`. Leave the implicit grant options
   unchecked: Sync uses the authorization-code flow with PKCE, which does not need
   them.
3. **Client secret** — Certificates & secrets → New client secret. Copy the
   **value** (shown once) into Admin > Providers, and note the expiry date: an
   expired secret breaks every job with `invalid_client`.
4. **Permissions** — API permissions → Add a permission → Microsoft Graph →
   *Delegated permissions* → add every Graph permission of the table above. They
   are all *admin consent required*.
5. **Admin consent** — press *Grant admin consent* for the tenant. Without it the
   authorization request fails with `AADSTS65001` (`consent_required` in the API
   answer) and Sync tells you to open the setup guide instead of retrying.
6. **Tenant** — `MICROSOFT_TENANT_ID` (or Admin > Providers → tenant id) decides
   the endpoint: `common` (any work/school account), `organizations`, or the
   tenant UUID/domain. Leave it on `common` (or empty) for a multi-tenant
   registration, so any directory can be connected; set the UUID or a verified
   domain to lock the client to a single directory.
7. **Role** — connect an account that holds the directory role matching the
   operations you plan (User Administrator for users, Groups Administrator for
   groups and distribution lists, License Administrator for licences). A Global
   Administrator works for everything.
8. **Connect** — Accounts → *Connect Microsoft 365*.

### 3.2 Admin consent from Sync

The setup guide offers the consent step directly: the button calls
`POST /api/oauth/microsoft/admin-consent`, which stores a single-use state and
returns the tenant consent URL
(`https://login.microsoftonline.com/{tenant}/v2.0/adminconsent`). The
administrator signs in, reviews the permissions, and Microsoft redirects back to
the registered callback with `admin_consent=True`. Sync then records
`provider.microsoft.admin_consent` (tenant, client id, timestamp — no secret) and
the guide shows *Granted for … on …*.

The record is compared against the **currently configured client id**: replacing
the app registration makes the guide report "not granted yet" again, which is the
truth — consent never carries over to another application.

The consent belongs to **one directory at a time**, so a multi-tenant instance
grants it once per connected directory: each administrator opens the same button
from the guide (or the link is sent to them) and consents for their own directory.
The stored record keeps the last directory that completed the flow, and a
connection to a directory that has not consented yet still fails with
`consent_required`, which is what tells the operator to repeat this step there.

### 3.3 Notes

* Personal Microsoft accounts cannot be granted directory permissions, so a
  personal OneDrive can no longer be connected once Sync requests the directory
  scopes. This is a consequence of the always-on scope set, not a limitation of
  the flow.
* Admin consent unlocks the *application*; the signed-in user still needs the
  role. A `403 Forbidden` from Graph after a successful consent almost always
  means "wrong role", not "missing consent".
* The sign-in endpoints are built from the configured tenant, so a tenant id that
  is not a valid segment falls back to `common` instead of escaping the endpoint
  (`internal/providers/microsoft/provider.go`, `tenantID`).
* The consent endpoint uses that same value except for the generic ones: Microsoft
  documents `common` as unsupported by `adminconsent` ("Do not use common"), so
  `common` and `consumers` — and therefore an empty tenant id — become
  `organizations` there (`consentTenant` in the same file). A concrete tenant id
  or verified domain is passed through unchanged, and only that one directory can
  consent.



## 4. The in-app guide

```text
GET /api/providers/:provider/guide
```

answers everything the screen renders:

| Field | Meaning |
|---|---|
| `configured` | an OAuth client is usable (environment or Admin > Providers) |
| `redirect_uri` | the exact URL to register |
| `console_urls` | the console deep links (`drive_api`, `admin_api`, `licensing_api`, `entra_apps`, `entra_authentication`, `entra_credentials`, `entra_api_permissions`, `graph_permissions`) |
| `permissions[]` | the permission table: `capability`, `title` (i18n suffix), `scope`, `admin_consent` |
| `scopes[]` | the same list space-separated, ready to paste |
| `capabilities[]` | the capabilities the table unlocks |
| `admin_consent_required` | the provider has a tenant-wide consent step (Microsoft) |
| `admin_consent` | `{tenant, client_id, at, granted}` of this instance |
| `steps[]` | the ordered walkthrough: `id`, `url`, `copy` (`redirect_uri` / `scopes`), `action` (`admin_consent`), `optional` |
| `warnings[]` | caveat codes the UI renders (`api_enablement`, `consent_screen_type`, `unverified_app`, `admin_role_required`, `license_required`, `tenant_scope`, `work_accounts_only`) |

`redirect_uri` is derived from `APP_URL` (`/api/oauth/<provider>/callback`), so the
walkthrough carries it **before** the OAuth client exists — that is the value to
register while following the steps. An override saved in **Admin > Providers**
wins over it.

The step text is not in the answer: the SPA translates
`admin.guide.steps.<provider>.<id>.title` / `.body`, so the walkthrough is
available in all seven languages. `internal/services/i18n_test.go` fails the build
if a step, warning or capability the API can return has no translation.

## 5. Reconnecting after an upgrade

When a release adds a permission:

1. `GET /api/accounts` reports it in `missing_capabilities`
   (`needs_reconnect: true`) and the Accounts screen marks the account.
2. The operator grants it in the console (or presses *Grant admin consent* for
   Microsoft) — the setup guide is one click away from the account row.
3. Pressing *Reconnect* runs the ordinary authorization flow for the same remote
   account: the row, its id and every job referencing it are preserved, only the
   tokens and `scopes` are replaced.

File synchronisation keeps working the whole time: the missing capabilities only
affect the operations that need them, and the engine never calls one.

## 6. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `code: "consent_required"` (412) when connecting | permissions refused: a denied prompt, a scope the app may not use, or a missing Microsoft admin consent | open the setup guide from the message, fix the console, retry |
| `redirect_uri_mismatch` (Google) / `AADSTS50011` (Microsoft) | the registered URI differs from `APP_URL/api/oauth/<provider>/callback` | register the exact URL: scheme, host, path, no trailing slash |
| Google: "This app is blocked" although the consent screen is configured | the client ID is not trusted in API controls | allow-list it for the organisational units (step 7) |
| Google: directory call fails with a permission error right after connecting | the API is not enabled in the project, or the account is not an administrator | enable Admin SDK / License Manager, connect an administrator |
| Google: the consent screen demands verification | the app is External and the scopes are sensitive | switch to Internal, or publish/verify the app |
| Microsoft: `AADSTS65001` / `consent_required` | admin consent was never granted (or was granted to another client id) | use the consent step of the guide; check that the client id still matches |
| Microsoft: Graph answers `403 Forbidden` after consent | the connected account lacks the directory role | connect an administrator, or assign the role |
| Microsoft: `invalid_client` after some weeks | the client secret expired | create a new secret and paste it in Admin > Providers (the stored tokens stay valid) |
| Account shows missing capability badges | connected before that permission was requested | reconnect the account |
