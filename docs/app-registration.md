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
| `teams` — Teams teams and their members ([reports.md](reports.md)) | — | `Team.ReadBasic.All`, `TeamMember.Read.All` |
| `planner` — Planner plans of the tenant's groups ([reports.md](reports.md)) | — | `Tasks.Read` |
| `domains` — verified domains and the tenant record | `admin.directory.domain.readonly`, `admin.directory.customer.readonly` | `Domain.Read.All` |
| `orgunits` — organisational units | `admin.directory.orgunit` | `AdministrativeUnit.ReadWrite.All` |
| `roles` — administrative role catalogue (read-only) | `admin.directory.rolemanagement.readonly` | `RoleManagement.Read.All` |
| `licenses` — subscriptions and licence assignment | `apps.licensing` | `Organization.Read.All`, `LicenseAssignment.ReadWrite.All` |
| `logs` — the organisation-wide activity feed ([audit-logs.md](audit-logs.md)) | `admin.reports.audit.readonly` | `AuditLog.Read.All` |
| `analytics` — the GA4 properties the connection can reach ([reports.md](reports.md)) | `analytics.readonly` | — |
| `youtube` — the channels the connection owns ([reports.md](reports.md)) | `youtube.readonly` | — |

The **directory report** ([reports.md](reports.md)) adds three rows of its own on
the Google side: `admin.directory.customer.readonly` under `domains`, which is what
answers `GET /customers/my_customer` — the `organization` section of a Google
report — plus the two rows `analytics` and `youtube` above for the GA4 property and
YouTube channel sections. Every other Google section is built from `users`,
`groups`, `members`, `domains`, `orgunits`, `roles`, `licenses` and `files` above,
shared drive members included: the Drive scope that lists a shared drive also
answers its permissions (`/files/{id}/permissions`, the file listing with
`useDomainAdminAccess=true`). The `analytics` and `youtube` scopes are the two
sections that describe the **connection** rather than the domain: the Analytics
Admin API and the YouTube Data API answer for the granted identity alone, because a
domain-wide listing needs domain-wide delegation (an impersonation Sync does not
perform) or a content-owner identity (a YouTube partner, not a Workspace account).
The Microsoft vocabulary is covered by the table
plus the two rows `teams` and `planner` above, which this release added for the
Teams and Planner sections — both directory scopes, granted tenant-wide with the
same admin consent as the rest. A grant made before the customer scope existed
still reports: the section is then derived from the domain list (see
[reports.md §2](reports.md#2-permissions)), and Accounts → *Check the
token* offers the upgrade. Collections that need a scope Sync does not request
at all (Chrome management, per-user sign-in activity on the Microsoft side, Teams
channels) are not offered until that row exists. Gmail delegates, Google
cross-mailbox copy and Google calendar transfer are not offered even with the
scope: Google restricts them to a service account with domain-wide delegation,
which an OAuth connection cannot provide (see [management.md](management.md)).
The YouTube **CMS** (content-owner reports) stays out even though the `youtube`
row does: the two are different identities (a partner account vs. the connected
one,
[reports.md §1](reports.md#1-what-a-report-contains)).

Every Graph scope in that column is a **delegated** permission, because the table
*is* the authorization request: Entra refuses a request that names a permission
the resource exposes as an application permission only, and it refuses the whole
request, not the one scope (`AADSTS650053`, *"asked for scope 'X' that doesn't
exist on the resource 00000003-0000-0000-c000-000000000000"*). That is why the
`planner` row asks for `Tasks.Read` rather than the `Tasks.Read.All` next to it in
the [permission reference](https://learn.microsoft.com/en-us/graph/permissions-reference),
which exists in the *Application* column alone. The app-only permissions the
product does need (`ActivityFeed.Read`, §3.4) are therefore not in this table:
they are granted in the console and used with a client-credentials token, never
requested in a consent URL.

`GET /api/accounts` reports, per connected account, which of these capabilities
the *stored* grant satisfies (`capabilities`) and which the current release asks
for on top of it (`missing_capabilities`, `needs_reconnect`), which is what the
Accounts screen renders as badges.

A **personal** Microsoft account (an MSA, `@outlook.com`) can never hold the
directory permissions at all: it is a tenant of its own, not a member of a
directory, so Entra ID has no grant to give and no consent screen offers them.
`GET /api/accounts` therefore reports those capabilities in
`not_applicable_capabilities` for such an account — out of reach, not missing —
next to `account_kind: "personal"`, which says why. The account itself is fully
usable: it connects, syncs its own OneDrive and browses its folders.

Two things a permission does **not** buy:

* **A role.** Granting an application the right to manage users does not make the
  connected identity an administrator: Google requires the connecting account to
  hold a Workspace administrator role, Microsoft requires a directory role
  (User Administrator, Groups Administrator, License Administrator).
* **An API.** Google scopes only work once the matching API is enabled in the
  project (Drive API, Admin SDK API, Enterprise License Manager API); a disabled
  API answers a permission error, not a "not enabled" error.

One permission is deliberately **not** in that table: the Office 365 management
activity API — the feed of file and workload activity shown on the Audit Logs
screen — takes an *application* permission, `ActivityFeed.Read`, granted with
tenant-wide admin consent (§3.4). It cannot be requested in an authorization
URL and it is never reported in the `scope` of a token response, so declaring it
here would mark every connected account as missing a permission that a reconnect
cannot add.

The same rule keeps `Device.Read.All` out of the table. The Audit Logs screen
names the person behind a workload-made device change by reading
`/devices/{id}?$expand=registeredOwners` ([audit-logs.md](audit-logs.md) §3), and
Graph accepts any of `Directory.ReadWrite.All`, `Directory.Read.All`,
`Device.Read.All` or `Device.ReadWrite.All` for it. `Directory.ReadWrite.All` is
already in the `groups` row above, so the enrichment rides on a grant every
connected account carries: no new consent, no reconnect, and nothing to fix if a
tenant refuses the read (the row simply names the service). Asking for the
narrower `Device.Read.All` would be an improvement in least privilege, and it is
recorded as a follow-up rather than smuggled into an upgrade: a new scope in this
table makes every refresh ask for it, and a refresh token issued before it existed
fails with `AADSTS65001 consent_required` — which would take files, users and logs
down together for every account.

## 2. Google Workspace

### 2.1 Steps

1. **Project** — <https://console.cloud.google.com/projectcreate>. Create a
   project dedicated to Sync (for example `sync-files`).
2. **APIs** — enable the five APIs *in that project*:
   [Drive API](https://console.cloud.google.com/apis/library/drive.googleapis.com),
   [Admin SDK API](https://console.cloud.google.com/apis/library/admin.googleapis.com),
   [Enterprise License Manager API](https://console.cloud.google.com/apis/library/licensing.googleapis.com),
   [Google Analytics Admin API](https://console.cloud.google.com/apis/library/analyticsadmin.googleapis.com),
   [YouTube Data API v3](https://console.cloud.google.com/apis/library/youtube.googleapis.com).
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
6. **Permissions** — on the consent screen, open *Manually add scopes* and paste
   the comma-separated list the guide offers (step 6 of **Admin > Setup guide**).
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
* `analytics.readonly` and `youtube.readonly` are **user** scopes, granted with
  administrator consent like the directory ones: they answer for the connected
  identity alone, so the Analytics and YouTube report sections describe the
  connection rather than the domain (see
  [reports.md §1](reports.md#1-what-a-report-contains)). Domain-wide analytics or
  channel inventory would need domain-wide delegation, which this provider does not
  use. Both scopes are read-only: the sections never write an Analytics or YouTube
  resource. Whether an External app has to be reviewed for them depends on Google's
  classification of each scope, which is why step 3 above keeps *Internal* the
  recommended consent-screen type.
* **A personal Google account (`@gmail.com`) cannot be connected.** The
  always-on scope set asks for `admin.directory.*` and `apps.licensing`, which
  only exist for a Workspace domain, so Google refuses the authorization request
  of an account that has no domain to administer (`invalid_scope` /
  `admin_consent_required` in the authorization answer). This is a consequence of
  the always-on scope set, not a limitation of the flow — the Drive capability
  alone would work, but a stored grant that cannot satisfy the directory
  capabilities would have to be reconnected as soon as one is used. Use a
  Workspace account instead (a dedicated administrator account, see the note
  above).

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

* Personal Microsoft accounts (MSAs) cannot be granted directory permissions, so
  Sync receives the file scopes alone for them (`User.Read`,
  `Files.ReadWrite.All` and the four OpenID Connect ones). The account connects and
  syncs its OneDrive normally: the directory capabilities are reported as
  `not_applicable_capabilities` — out of reach, with no reconnect offered for them
  (§1, §5) — and `Sites.ReadWrite.All` belongs to that group too, because Graph
  refuses `/sites` for an MSA.
* Admin consent unlocks the *application*; the signed-in user still needs the
  role. A `403 Forbidden` from Graph after a successful consent almost always
  means "wrong role", not "missing consent".
* The sign-in endpoints are built from the configured tenant, so a tenant id that
  is not a valid segment falls back to `common` instead of escaping the endpoint
  (`internal/providers/microsoft/provider.go`, `tenantID`).
* The four OpenID Connect scopes (`openid`, `profile`, `email`, `offline_access`)
  are requested and listed by the guide, but Entra ID does not report them back:
  the token response `scope` value only carries the Graph resource scopes, and it
  is percent-encoded (`"scope": "https%3A%2F%2Fgraph.microsoft.com%2Fmail.read"`).
  They are therefore declared `informational` — requested, documented, and never
  used to prove a capability — and the stored grant is decoded before it is
  compared (`providers.SplitScopes`).
* The consent endpoint uses that same value except for the generic ones: Microsoft
  documents `common` as unsupported by `adminconsent` ("Do not use common"), so
  `common` and `consumers` — and therefore an empty tenant id — become
  `organizations` there (`consentTenant` in the same file). A concrete tenant id
  or verified domain is passed through unchanged, and only that one directory can
  consent.

### 3.4 The activity feed (app-only permission)

The Audit Logs screen reads two Microsoft feeds (see [audit-logs.md](audit-logs.md)
§2): the directory feed with the delegated `AuditLog.Read.All` of §1, and the file
and workload feed from the **Office 365 management activity API**, which only
exists as an application permission.

1. Entra > **App registrations** > your application > **API permissions**.
2. **Add a permission** > **APIs my organization uses** > search for *Office 365
   Management APIs* > **Application permissions** > `ActivityFeed.Read`.
3. **Grant admin consent** for the directory.
4. Turn on **unified audit logging** in the Purview compliance portal (Audit >
   Audit log). Without it the subscription succeeds and the content listings stay
   empty, which looks like a broken feed rather than a disabled one.

That grant is per directory and cannot be carried by the delegated token, so
Sync builds an app-only token from the configured client id and secret
(`client_credentials` for `https://manage.office.com/.default`) for the tenant of
the connected account. It fails in two shapes, and they mean different things:

* `AADSTS65001` on the token exchange — the application permission was never
  granted, or the consent was given to another client id;
* `401 AF10001` (`permission set ()`) on the subscription — the permission exists
  on the app registration, but this directory never consented to it. The token is
  issued with no application roles, so no retry and no reconnect can help.

Either way the Audit Logs screen shows the "waiting for a permission" banner for
that stream (the message names `ActivityFeed.Read` and, for `AF10001`, the three
things to fix), the directory stream keeps working, and nothing else in the
product is affected. A second tenant needs its own step 3.



### 3.5 Mailbox purpose (app-only permission)

The Microsoft directory report classifies every user as member, guest, shared
mailbox, room or equipment. The classification reads
`mailboxSettings.userPurpose`, and Graph only answers another user's settings with
the **application** permission `MailboxSettings.Read` — the delegated
`MailboxSettings.Read` reads only the signed-in administrator's own mailbox.

1. Entra > **App registrations** > your application > **API permissions**.
2. **Add a permission** > **Microsoft Graph** > **Application permissions** >
   `MailboxSettings.Read` (display text *Read all user mailbox settings*).
3. **Grant admin consent** for the directory.

Do **not** add `MailboxSettings.Read.All` to the delegated list: that scope does
not exist on the Graph resource and Entra refuses the whole authorization request
with `AADSTS650053`. The app-only grant is per tenant; without it the Users table
still renders but shared mailboxes and resources appear as `User`, and the
`shared_mailboxes` section reports the Graph refusal.

## 4. The in-app guide

```text
GET /api/providers/:provider/guide
```

answers everything the screen renders:

| Field | Meaning |
|---|---|
| `configured` | an OAuth client is usable (environment or Admin > Providers) |
| `redirect_uri` | the exact URL to register |
| `console_urls` | the console deep links (`drive_api`, `admin_api`, `licensing_api`, `analytics_api`, `youtube_api`, `entra_apps`, `entra_authentication`, `entra_credentials`, `entra_api_permissions`, `graph_permissions`) |
| `permissions[]` | the permission table: `capability`, `title` (i18n suffix), `scope`, `admin_consent`, `informational` (requested for the protocol, proves no capability — see §3.3), `kinds` (the account kinds that can hold the scope: absent means every kind, `["work"]` on the directory scopes and `Sites.ReadWrite.All`, which a personal Microsoft account can never be granted — see §1) |
| `scopes[]` | the permission scopes in request order — the OAuth request joins them with a space (`scope` parameter); the guide renders them comma-separated, which is what the Google consent screen's *Manually add scopes* box expects |
| `capabilities[]` | the capabilities the table unlocks, scoped to the provider (`providers.CapabilitiesFor`): `teams` and `planner` are Microsoft only, `analytics` and `youtube` Google only, so no guide ever offers a capability its directory cannot hold |
| `admin_consent_required` | the provider has a tenant-wide consent step (Microsoft) |
| `admin_consent` | `{tenant, client_id, at, granted}` of this instance |
| `steps[]` | the ordered walkthrough: `id`, `url`, `copy` (`redirect_uri` / `scopes` / `permissions`), `action` (`admin_consent`), `optional` |
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

This release adds one such upgrade on the Microsoft side: `teams` and `planner`
(`Team.ReadBasic.All`, `TeamMember.Read.All`, `Tasks.Read`, §1). The app
registration has to carry them before the reconnect is useful — Entra refuses to
authorise a scope the registration does not declare (`AADSTS65001`) — so the order
is: add the three permissions under **Delegated permissions** in the console,
*Grant admin consent*, then reconnect. They must be the delegated rows: Entra
lists a `Tasks.Read.All` too, but as an **application** permission, and asking for
it in the authorization request fails the whole flow with `AADSTS650053` (§6) even
though the app registration carries it. Connections that skip it keep syncing
files and simply do not offer the Teams and Planner report sections. Google needs
no equivalent step for `shared_drive_members`: it rides on the `drive` scope every
connection already grants.

The Google side has two upgrades of that kind this release: `analytics` and
`youtube` (`analytics.readonly`, `youtube.readonly`, §1). A connection made before
them reports both capabilities in `missing_capabilities`, and the two report
sections fail alone (the rest of the report is unaffected) until the account is
reconnected. Two things have to be in place first: the two APIs enabled in the
Cloud project (§2.1 step 2), and the client trusted in the Admin console (step 7) —
a scope the project does not enable answers a permission error at call time, which
is the shape of a missing grant even when the grant is fine.

The caveat of the Microsoft table belongs here too: a permission an account can
never hold is **not** a missing permission. On a personal Microsoft account the
directory scopes fall in that group, so `GET /api/accounts` reports them in
`not_applicable_capabilities` (greyed out, with a one-line explanation) rather than
in `missing_capabilities`, and *Reconnect* is not offered for them — the reconnect
would come back with exactly the same grant. `admin.guide.warnings.work_accounts_only`
is the setup-guide wording of that limit.

The four OpenID Connect scopes never appear here: they are declared
`informational`, because Entra ID grants them without echoing them in the token
response `scope` value, and a scope that is never reported cannot prove a
capability.

## 6. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `code: "consent_required"` (412) when connecting | permissions refused: a denied prompt, a scope the app may not use, or a missing Microsoft admin consent | open the setup guide from the message, fix the console, retry |
| `redirect_uri_mismatch` (Google) / `AADSTS50011` (Microsoft) | the registered URI differs from `APP_URL/api/oauth/<provider>/callback` | register the exact URL: scheme, host, path, no trailing slash |
| Google: "This app is blocked" although the consent screen is configured | the client ID is not trusted in API controls | allow-list it for the organisational units (step 7) |
| Google: directory call fails with a permission error right after connecting | the API is not enabled in the project, or the account is not an administrator | enable Admin SDK / License Manager (and the Analytics Admin / YouTube Data API for the two report sections), connect an administrator |
| Google: the consent screen demands verification | the app is External and the scopes are sensitive | switch to Internal, or publish/verify the app |
| Microsoft: `AADSTS65001` / `consent_required` | admin consent was never granted (or was granted to another client id) | use the consent step of the guide; check that the client id still matches |
| Microsoft: `AADSTS650053` — *"The application asked for scope 'Tasks.Read.All' that doesn't exist on the resource `00000003-0000-0000-c000-000000000000`"*, although the registration lists it | the permission was added under **Application permissions**, and the authorization request asks for it as a delegated scope. An application permission has no delegated counterpart, so Entra refuses the whole request — the grant on the app registration does not help | add the delegated row instead (`Tasks.Read` for `planner`, §1) under **Delegated permissions**, *Grant admin consent*, then reconnect. Remove the application permission unless an app-only call needs it (§3.4) |
| Microsoft: `AF10001` / `permission set ()` on the activity feed | `ActivityFeed.Read` was granted to the app but never consented in **this** directory (the grant is per tenant), or the app registration lost the permission | redo §3.4 for that directory; the Audit Logs screen names the stream that failed |
| Microsoft: the activity feed subscribes but stores nothing | unified audit logging is off in the tenant, so the content listings are empty | turn it on in the Purview compliance portal (Audit > Audit log) and wait for the first records |
| Microsoft: Graph answers `403 Forbidden` after consent | the connected account lacks the directory role | connect an administrator, or assign the role |
| Microsoft: `invalid_client` after some weeks | the client secret expired | create a new secret and paste it in Admin > Providers (the stored tokens stay valid) |
| Account shows missing capability badges | connected before that permission was requested | reconnect the account |
