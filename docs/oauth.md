# OAuth flows

Sync runs two authorization-code flows with PKCE (RFC 7636, `S256`):

| Flow | Purpose | Entry point | Documented in |
|---|---|---|---|
| `oauth` | connect a Google Drive / Microsoft Graph account | `POST /api/oauth/:provider/start`, `GET /api/oauth/:provider/callback` | this file |
| `admin_consent` | grant the provider's permissions for a whole tenant (Microsoft) | `POST /api/oauth/:provider/admin-consent`, same callback | this file |
| `keycloak` | log an operator in against a realm | `GET /api/auth/keycloak`, `GET /api/auth/callback` | [authentication.md](authentication.md) |

Both use the same building blocks: a `state` value signed with
`crypto.DeriveKey(ENCRYPTION_KEY, "state")`, a PKCE verifier that never leaves the
server, one row in `oauth_states` and a 10 minute validity (`stateTTL`).

## 1. Connecting a provider account

```text
SPA  POST /api/oauth/google/start {redirect_to?}      → 200 {"url":"…","state":"…"}
SPA  navigates the browser to url                    → Google consent screen
Google → GET /api/oauth/google/callback?code=&state= → exchange + store + 302 /accounts?connected=google
```

`POST /api/oauth/:provider/start` (authenticated):

1. `:provider` must be `google` or `microsoft` and compiled into the binary,
   otherwise `400` / `404`.
2. The OAuth client must be usable (environment or Admin > Providers), otherwise
   `424 not_configured` (*"the … provider is not configured"*) — the SPA links to
   Admin > Providers in that case.
3. A `state` row is created: 32 random bytes hex-encoded, HMAC-signed,
   `flow=oauth`, `provider`, `code_verifier`, `redirect_to`, `expires_at = now + 10 min`.
4. The answer is `{url, state}`; the SPA navigates instead of relying on a server
   redirect, so it can stay on the page when something fails.

`GET /api/oauth/:provider/callback` is **public** (a top-level browser
navigation may carry an expired session cookie): the signed `state` row is the
authentication. Every outcome is a `302` back to `/accounts` with a translatable
query string (`connected`, `connect_error`, `detail`) — no message text travels
through the redirect.

| Condition | Redirect |
|---|---|
| `error=` present in the query (consent denied, cancelled) | `/accounts?connect_error=<code>&provider=<provider>&detail=<error_description>` |
| success | `/accounts?connected=<provider>&detail=<email or display name>` |
| any other failure | `/accounts?connect_error=<code>&provider=<provider>&detail=<message>` |

`<code>` is the ordinary API error code (`validation`, `unauthorized` — the link
was already used or expired —, `not_found`, `internal`) or one of the OAuth codes
the provider answered with, mapped by `connectErrorCode`: `access_denied` →
`denied`, and `consent_required` / `admin_consent_required` /
`interaction_required` / `invalid_scope` / `unauthorized_client` →
`consent_required` (which tells the screen to offer the setup guide instead of a
retry). The accounts screen translates it with the `accounts.error_*` catalog keys
and shows `detail` as technical text.

### 1.1 Tenant-wide admin consent (Microsoft)

`POST /api/oauth/:provider/admin-consent {redirect_to?}` answers `{url, state}`
where `url` is `https://login.microsoftonline.com/{tenant}/v2.0/adminconsent`
carrying the client id, the registered redirect URI, the same scope list as the
authorization request and the state. `{tenant}` is the configured tenant id, or
`organizations` when it is empty or one of the generic values (`common`,
`consumers`) that accept personal accounts — Microsoft does not support `common`
on this endpoint, and each directory administrator consents for their own
directory. The `admin_consent` flow row is identical to
an `oauth` row minus the PKCE verifier (there is no code exchange).

Microsoft comes back on the same callback with `admin_consent=True&tenant=…` — or
with `error=access_denied` when the administrator refused. The handler tells the
two flows apart by the `flow` column of the state row (`OAuthService.FlowOf`, a
read that does not consume it):

* success → `OAuthService.CompleteAdminConsent` consumes the state and records
  `provider.microsoft.admin_consent` (`{tenant, client_id, at}` in the `settings`
  table) → `302 /admin/guide/microsoft?consent=granted`;
* refusal → the state is consumed all the same (`AbandonAdminConsent`, so a
  replayed link cannot resurrect the flow) → `302 /admin/guide/microsoft?consent_error=…&detail=…`.

Providers without a consent step (Google) answer `400 validation`.

States are **single use**: `Store.ConsumeOAuthState` deletes the row in the same
statement that reads it, so a replayed callback is rejected.

## 2. Token storage and lifecycle

`TokenManager` (`internal/services/tokens.go`) is the only code that touches
`connected_accounts.access_token` / `refresh_token`:

1. **Resolve** — load the account, decrypt both tokens with the `SecretBox`.
2. **Refresh when needed** — an access token expiring within two minutes is
   renewed before the provider call, so a run never refreshes on its critical
   path. If the provider does not return a new refresh token, the stored one is
   kept (Microsoft and Google both rotate opportunistically).
3. **Persist** — the new token set is encrypted again (`v1:` AES-256-GCM),
   `expires_at`/`refreshed_at` are updated and `status` returns to `connected`.
4. **Hand over** — the callback receives a `ProviderSession` (account, provider
   implementation, credentials, valid tokens) and nothing else.

| Situation | Result |
|---|---|
| refresh token rejected or missing | the account is marked `status=error` with `last_error`, the call returns `ErrReconnect` → HTTP **412** with `code=reconnect`; the SPA shows "reconnect this account" |
| provider answered 401 mid-run | same as above (the run records the file as `failed`) |
| provider answered 404 for an item | `providers.ErrNotFound` → the engine treats it as a deletion, never an error |
| authorization server refused the permissions (`access_denied`, `invalid_scope`, `consent_required`) | `providers.AuthError` → `services.ErrConsent` → HTTP **412** with `code=consent_required`; the SPA offers the setup guide |

A background loop (`Scheduler.RefreshTokens`, every 60 s, `DefaultRefreshWindow`
= 5 min) refreshes tokens proactively, which is why a scheduled run at 3 AM
usually starts with a valid access token. `POST /api/maintenance/tokens/refresh`
forces the same pass and answers `{"refreshed": n}`.

## 3. Scopes

Every permission a provider needs is requested **from the first connection**, in
one authorization request. A connection made today therefore keeps working after
an upgrade instead of failing on an operation its grant never covered; the price
is a longer consent screen. The complete list, the capability each scope unlocks
and the registration instructions are in
[app-registration.md](app-registration.md); the code side is one table per
provider (`internal/providers/google/permissions.go`,
`internal/providers/microsoft/permissions.go`), which is also what
`GET /api/providers/:provider/guide` renders.

| Provider | Capabilities | Request options |
|---|---|---|
| Google | files (`drive`, `userinfo.email`, `userinfo.profile`, `openid`), users (`admin.directory.user`, `.user.alias`), groups (`admin.directory.group`), members (`admin.directory.group.member`), domains (`admin.directory.domain.readonly`), orgunits (`admin.directory.orgunit`), roles (`admin.directory.rolemanagement.readonly`), licenses (`apps.licensing`) | `access_type=offline`, `prompt=consent`, `include_granted_scopes=true`, PKCE `S256` |
| Microsoft | files (`Files.ReadWrite.All`, `Sites.ReadWrite.All`, `User.Read`, `offline_access`, `openid`, `profile`, `email`), users (`User.Read.All`, `User.ReadWrite.All`), groups (`Group.ReadWrite.All`, `Directory.ReadWrite.All`), members (`GroupMember.ReadWrite.All`), domains (`Domain.Read.All`), orgunits (`AdministrativeUnit.ReadWrite.All`), roles (`RoleManagement.Read.All`), licenses (`Organization.Read.All`, `LicenseAssignment.ReadWrite.All`) | `prompt=consent`, `response_mode=query`, PKCE `S256`; every directory scope needs the tenant-wide admin consent (§1.1) |

`Files.ReadWrite.All` is the Graph scope that covers personal OneDrive;
`Sites.ReadWrite.All` is what allows synchronising a SharePoint document library
(`GET /api/accounts/:id/sites`). The account's granted scopes are stored
space-separated in `connected_accounts.scopes` and shown in the accounts screen,
which also derives the capability badges: `capabilities` (satisfied by the stored
grant) and `missing_capabilities` / `needs_reconnect` (requested by this release,
not yet granted → reconnect).

The four OpenID Connect scopes (`openid`, `profile`, `email`, `offline_access`)
are requested but never used to **prove** a capability. Microsoft Entra ID grants
them without reporting them back — the token response `scope` value carries the
Graph resource scopes only, percent-encoded
(`"scope": "https%3A%2F%2Fgraph.microsoft.com%2Fmail.read"`) — so the table marks
them `informational` (`providers.Permission.Informational`) and the splitter
tolerates that encoding (`providers.SplitScopes`). Without both, an account
connected with the current permission set would be reported as needing a
reconnect it cannot fix. A red *files* badge on a Microsoft account therefore
always means `User.Read`, `Files.ReadWrite.All` or `Sites.ReadWrite.All` is
missing from the grant.

## 4. Redirect URIs

| Flow | Default (derived from `APP_URL`) | Where to register it |
|---|---|---|
| Google | `APP_URL/api/oauth/google/callback` | Google Cloud console → OAuth client → Authorized redirect URIs |
| Microsoft | `APP_URL/api/oauth/microsoft/callback` | Entra ID → App registration → Authentication → Redirect URIs (Web) |
| Keycloak | `APP_URL/api/auth/callback` | Keycloak client → Valid redirect URIs |

They can be overridden with `GOOGLE_REDIRECT_URI`, `MICROSOFT_REDIRECT_URI` and
`KEYCLOAK_REDIRECT_URI`; `GET /api/providers` returns a `redirect_hint` with the
two provider paths so the admin screen can print the exact URL to paste.

## 5. Reconnecting an account

When the refresh token is gone (revoked in the provider console, password change,
admin removal), the account turns `error` and every request that needs it answers
`412 reconnect`. The fix is always the same: open **Accounts**, press the connect
button of the same provider and authorise the same remote account — the upsert
keeps the row (and therefore all the jobs that reference it) and replaces the
tokens.

`POST /api/accounts/:id/verify` runs the same provider call the engine would and
reports whether the token still works, which is what the "Check the token"
button uses before a test run of a large job.
