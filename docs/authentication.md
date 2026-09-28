# Authentication

`AUTH_METHOD` selects one of three modes. Sync has a **single role**: an
authenticated operator is an administrator, so there is no separate permission
model (the Keycloak allow-list is what decides who may log in at all).

| Mode | Who gets in | Typical use |
|---|---|---|
| `none` | everybody, as an administrator | a private network, a demo instance, or behind an authenticating proxy |
| `account` | the single `ACCOUNT_LOGIN`/`ACCOUNT_PASSWORD` pair from the environment | a single operator instance |
| `keycloak` | a Keycloak (OIDC) realm, further restricted by `KEYCLOAK_ACCOUNTS` | teams with a central identity provider |

Defaults: `AUTH_METHOD=account`. Validation happens at boot
([configuration.md](configuration.md)), so an unusable mode never starts.

## 1. Session model

Sessions are **stateless**: nothing about a login is stored in the database.

* Cookie: `sync_session`, `HttpOnly`, `SameSite=Lax`, `Path=/`, `Secure` when
  `APP_URL` starts with `https://`, `Max-Age=604800` (7 days).
* Payload: `subject`, `email`, `name`, `picture`, `method`, `admin`, `exp` —
  base64url JSON plus an HMAC-SHA256 signature keyed with
  `crypto.DeriveKey(ENCRYPTION_KEY, "session")`. A tampered or expired cookie is
  simply treated as "anonymous" (no error page).
* `GET /api/auth/session` is public: it reports `mode`, `authenticated`, `open`,
  `captcha`, `keycloak`, `operator`, `defaults` and the build `version` — the SPA
  needs that before it can render anything.
* `POST /api/auth/logout` expires the cookie, is idempotent and always answers
  `204`, so the SPA can clear its state unconditionally.

The SPA stores nothing about the session itself: `api.ts` sends the cookie
(`withCredentials`), the auth store caches the session view for the page load and
`main.ts` installs a global 401 handler that drops the cached session and sends
the operator to `/login?redirect_to=…&auth_error=unauthorized`.

## 2. `none` mode

`AuthService.Open()` is true and every request carries the open identity
(`method: none`, `admin: true`). Nothing else changes: the same endpoints, the
same guards. Use it only where the network itself is the authentication
(VPN, private subnet) or behind a proxy that authenticates.

The login page recognises the mode (`open: true`) and offers an "Open the
dashboard" button instead of a form.

## 3. `account` mode

`POST /api/auth/login` with `{login, password, captcha_token?, redirect_to?}`:

1. **Throttling** — the *client address* (`c.ClientIP()`, i.e. the real address
   when `APP_TRUST_PROXY=true`) is counted. 10 failures (`LoginAttempts`) block it
   for 15 minutes (`LoginBlockWindow`); the map is pruned opportunistically and a
   successful login resets the counter. A blocked address gets
   `403 forbidden` (*"too many failed attempts, try again in a few minutes"*).
2. **Captcha** — when `RECAPTCHA_CLIENTID`+`RECAPTCHA_CLIENTSECRET` are set, the
   answer must carry `captcha_token`. Missing token → `400 validation`;
   a rejected answer → `403 forbidden`. The widget is rendered by the SPA only
   when the session view reports `captcha.enabled` with a `site_key`.
3. **Credentials** — `login` and `password` are compared with
   `crypto/subtle.ConstantTimeCompare`, so timing never leaks. A mismatch answers
   `401 unauthorized` (*"invalid credentials"*) and counts as a failure.
4. **Success** — the signed cookie is set and the response is the session view of
   the *new* session (no second round trip).

| Failure | HTTP | Code | SPA message key |
|---|---|---|---|
| empty user or password | (client side) | — | `auth.passwordRequired` |
| wrong credentials | 401 | `unauthorized` | `auth.error_invalid` (aliased in `LoginView.vue`) |
| too many attempts | 403 | `forbidden` | `auth.error_forbidden` + server detail |
| captcha rejected | 403 | `forbidden` | `auth.error_captcha` is the intended wording; the server detail is shown as technical text |
| captcha token missing | 400 | `validation` | `auth.error_captcha` |
| login endpoint in `none`/`keycloak` mode | 400 | `validation` | `auth.error_unknown` |

## 4. `keycloak` mode

Standard authorization-code flow with PKCE against
`<KEYCLOAK_BASE_URL>/realms/<KEYCLOAK_REALM>`:

```text
SPA ──GET /api/auth/keycloak?redirect_to=…──► API   (JSON: {url, state})
SPA ──navigate to url───────────────────────► realm (login + consent)
realm ──GET /api/auth/callback?code=&state=─► API   (exchange, validate, allow-list)
API ──302 / ────────────────────────────────► SPA   (cookie set; deep link preserved)
```

The SPA asks for the URL instead of being redirected, so a failure (realm down,
not configured → `424 not_configured`) is reported inside the login page.

Failures on the callback are **not** error pages: the API redirects to
`/login?auth_error=<code>&auth_detail=<message>` and the SPA translates the code.

| `auth_error` | Meaning | SPA key |
|---|---|---|
| `denied` | the realm answered `error=…` (consent denied, link expired) | `auth.error_denied` |
| `forbidden` | the account is authenticated but not on the allow-list | `auth.error_forbidden` |
| `failed` | the code exchange or the user-info call failed | `auth.error_failed` |
| `unauthorized` | the login link was already used or expired | `auth.error_unauthorized` |
| `internal` | unexpected server error (also logged with full detail) | `auth.error_internal` |

### Allow-list matching (`KEYCLOAK_ACCOUNTS`)

The e-mail returned by the realm's `userinfo` is lower-cased, trimmed and matched
against every entry (`allowedAccount`):

| Entry | Matches |
|---|---|
| `*` | every authenticated account |
| `you@example.com` | exactly that address |
| `example.com` or `@example.com` | every address of that domain |

An empty list fails the boot, so a `keycloak` instance can never be open by
accident. A refused account is logged with a masked address
(`a***@example.com`) and answers `403 forbidden`.

Identity mapping: `sub` → `subject`, `email` → `email`,
`name`/`preferred_username` → `name`, `picture` → `picture`, `method: keycloak`,
`admin: true` (the allow-list already decided who may enter).

## 5. Keycloak configuration checklist

1. Create a confidential client in the realm, standard flow enabled.
2. Valid redirect URI: `APP_URL/api/auth/callback` (or your `KEYCLOAK_REDIRECT_URI`).
3. Copy the client id/secret into `KEYCLOAK_CLIENT_ID`/`KEYCLOAK_CLIENT_SECRET`.
4. Set `KEYCLOAK_BASE_URL` + `KEYCLOAK_REALM`; the issuer becomes
   `<base>/realms/<realm>` and the discovery document is read from it.
5. Fill `KEYCLOAK_ACCOUNTS` (use `*` deliberately, never by default).
6. `AUTH_METHOD=keycloak`, restart, and check `GET /api/auth/session` reports
   `keycloak.enabled: true`.

## 6. Security notes

* The session cookie is signed, not encrypted: it contains an e-mail and a
  display name, never a token and never a password.
* Key rotation: rotating `ENCRYPTION_KEY` invalidates sessions (they are signed
  with a derived key) and makes stored tokens unreadable.
* Sessions cannot be revoked centrally without a restart; use a short
  `APP_URL`/realm session policy if that matters, or rely on Keycloak's own
  session lifetime (the realm ends the login, the cookie follows it).
* Every answer carries the CSP, `X-Frame-Options: DENY` and
  `Referrer-Policy: same-origin` (see [reverse-proxy.md](reverse-proxy.md)).
* No endpoint of the API is reachable without the session except `health`,
  `version`, `auth/session`, `auth/login`, `auth/keycloak`, `auth/callback` and
  the two provider callbacks (which are protected by the signed `state`).
