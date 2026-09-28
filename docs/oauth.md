# OAuth flows

Sync runs two authorization-code flows with PKCE (RFC 7636, `S256`):

| Flow | Purpose | Entry point | Documented in |
|---|---|---|---|
| `oauth` | connect a Google Drive / Microsoft Graph account | `POST /api/oauth/:provider/start`, `GET /api/oauth/:provider/callback` | this file |
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
| `error=` present in the query (consent denied, cancelled) | `/accounts?connect_error=denied&detail=<error_description>` |
| success | `/accounts?connected=<provider>&detail=<email or display name>` |
| any other failure | `/accounts?connect_error=<code>&detail=<message>` |

`<code>` is the ordinary API error code (`validation`, `unauthorized` — the link
was already used or expired —, `not_found`, `internal`); the accounts screen
translates it with the `accounts.error_*` catalog keys and shows `detail` as
technical text.

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

A background loop (`Scheduler.RefreshTokens`, every 60 s, `DefaultRefreshWindow`
= 5 min) refreshes tokens proactively, which is why a scheduled run at 3 AM
usually starts with a valid access token. `POST /api/maintenance/tokens/refresh`
forces the same pass and answers `{"refreshed": n}`.

## 3. Scopes

| Provider | Scopes requested |
|---|---|
| Google | `openid`, `https://www.googleapis.com/auth/drive`, `…/auth/userinfo.email`, `…/auth/userinfo.profile` (plus `access_type=offline` and `prompt=consent`, so a refresh token is always returned) |
| Microsoft | `offline_access`, `openid`, `profile`, `email`, `https://graph.microsoft.com/User.Read`, `…/Files.ReadWrite.All`, `…/Sites.ReadWrite.All` (plus `prompt=consent`, so a reconnection returns a refresh token) |

`Files.ReadWrite.All` is the Graph scope that covers personal OneDrive;
`Sites.ReadWrite.All` is what allows synchronising a SharePoint document library
(`GET /api/accounts/:id/sites`). The account's granted scopes are stored
space-separated in `connected_accounts.scopes` and shown in the accounts screen.

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
