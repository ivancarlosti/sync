# Plan — Keycloak single sign-on logout (RP-initiated)

- **Date:** 2026-11-09
- **Scope:** `AUTH_METHOD=keycloak`
- **Follows:** [`2026-09-27-first-building.md`](./2026-09-27-first-building.md) §1 (auth modes)

## Context

Signature of the problem reported by an operator:

> `services: access denied: i***@agencia.pub is not allowed to use this instance`

Sync refuses the account (the `KEYCLOAK_ACCOUNTS` allow-list), which is correct,
but it leaves the operator in a dead end:

1. The **realm** still holds its single sign-on session cookie. Clicking
   *Sign in with Keycloak* again returns the same account silently — the same
   refusal, forever, with no way to try another one.
2. *Sign out* in `UserMenu` only expires `sync_session`. The realm session
   survives, so the next visitor on that browser is signed in as the previous
   account.

The identity provider owns the session, so ending it is the realm's job: an
**RP-initiated logout** (`end_session_endpoint` of the realm) with

```text
https://{{ssodomain}}/realms/{{realm}}/protocol/openid-connect/logout
  ?client_id={{client_id}}
  &post_logout_redirect_uri={{APP_URL}}
```

Verified against the Keycloak implementation
(`protocol/oidc/endpoints/LogoutEndpoint.java`): `client_id` is accepted as a
substitute for `id_token_hint` when `post_logout_redirect_uri` is used (it answers
`400` only when *both* are missing), and the redirect must match the client's
**Valid post logout redirect URIs**. Without an `id_token_hint` Keycloak shows its
own "do you want to log out?" screen — unavoidable, and correct, because Sync
stores no token and the refused operator never had a session to hint with.

## Goal

* `POST /api/auth/logout` keeps clearing the cookie (idempotent, `204`).
* New public `GET /api/auth/keycloak/logout` → `{url}` (the realm end-session URL,
  built server side: the client id and `APP_URL` never leave the server).
* The SPA: *Sign out* (user menu) and a new *Sign in with another account* button
  on the login page (shown when the callback reported a failure) expire the cookie
  **and** send the browser to the realm; the realm comes back to `APP_URL` and the
  router guard lands on `/login`.
* If the realm cannot answer (not configured, outage, discovery down) the local
  session still ends — logging out must never trap the operator.

## Steps

1. `internal/services/auth.go` — `AuthService.KeycloakLogout(ctx)` builds the URL
   from the discovery document (falling back to the well-known path) with
   `client_id` + `post_logout_redirect_uri=APP_URL`.
2. `internal/handlers/auth.go` + `routes.go` — `handleKeycloakLogout`
   (`GET /api/auth/keycloak/logout`, public like the login start; `424
   not_configured` when no realm).
3. `web/src/lib/api.ts` — `auth.keycloakLogout()`.
4. `web/src/stores/auth.ts` — `signOut()` returns `true` when the browser is on
   its way to the realm; a realm failure falls back to the SPA.
5. `web/src/components/UserMenu.vue` — do not navigate when the realm redirect is
   in flight.
6. `web/src/views/LoginView.vue` — *Sign in with another account* in the Keycloak
   block whenever an `auth_error` is reported (the refused-account dead end).
7. `web/src/i18n/locales/*.json` — one new key, `auth.keycloakSwitchAccount`, in
   all seven catalogs (`check-i18n.mjs` enforces parity).
8. Tests: URL builder (services), endpoint wiring (handlers).
9. Docs: `api.md` (endpoint table), `authentication.md` (§4 logout flow + the
   Keycloak client setting), `configuration.md` (`APP_URL` / Keycloak note).

## Risks

| Risk | Mitigation |
|---|---|
| The realm redirects to an unregistered URI → Keycloak answers `400 invalid redirect uri` | Documented in `authentication.md`: add `APP_URL` to **Valid post logout redirect URIs** (or `+` to reuse the valid redirect URIs). |
| A Keycloak confirmation screen between *Sign out* and the redirect | Inherent to a logout without `id_token_hint`; documented. Storing the token to avoid it would defeat the stateless, token-free session. |
| A realm outage blocks the sign out | The SPA requests the URL first, tolerates a failure, clears the cookie anyway and only then navigates. |
| An authenticated visitor is sent to a public endpoint | `GET /api/auth/keycloak/logout` changes no state and carries no secret: it only names the configured client and `APP_URL`. |
| CSRF forced logout | The endpoint is a `GET` that changes nothing on the server; the cookie is still expired through the existing `POST /api/auth/logout` (SameSite=Lax keeps it off cross-site posts). |
