# Configuration

Every value Sync needs is an environment variable. `internal/config` is the only
package that reads the environment: it parses `.env` (current working directory,
then `docker/.env`), applies the process environment on top, validates everything
and either returns a usable `*config.Config` or one error listing **all** problems
at once.

## 1. Where the values come from

| Source | When it applies |
|---|---|
| `docker/.env` (copied from `.env.example`) | `docker compose up` passes it through `env_file:` |
| `.env` in the working directory | local `go run ./cmd/server` (loaded with `godotenv`) |
| real environment variables | container orchestrators; **always win** over the files |
| Admin > Settings (database) | the four editable settings: default locale, default theme, default interval, run timeout — they override the environment for the SPA, never for the boot checks |
| Admin > Providers (database, encrypted) | provider OAuth clients; override the environment values when present |

## 2. Boot sequence

```text
config.Load()            parse .env + environment, derive redirect URIs, Validate()
   │  failure → exit 1 with every problem listed (no half-started container)
slog.SetDefault(LOG_LEVEL)
database.Open()          connect + ping (10 s budget), 25/5 connection pool, UTC
   │  failure → exit 1
database.Migrate()       AutoMigrate the 8 tables (idempotent)
database.Seed()          insert the settings that do not exist yet
database.PruneOAuthStates()  best effort cleanup of abandoned flows
build()                  services + providers + notifier (wiring only)
Scheduler.Bootstrap()    close interrupted runs, recompute next_run_at
handlers.New()           Gin engine, routes, security headers, proxies
Scheduler.Start()        the two tickers begin
ListenAndServe()         APP_PORT on 0.0.0.0
```

`SIGINT`/`SIGTERM` (docker stop) drains in-flight HTTP requests for up to 20 s,
stops the scheduler and lets running sync jobs reach their final state.

## 3. Application and reverse proxy

| Variable | Default | Notes |
|---|---|---|
| `APP_URL` | — (**required**) | Public URL of the instance, no trailing slash. Must be absolute (`https://sync.example.com`). Used for OAuth/OIDC redirect URIs, absolute links in notifications and the `Secure` flag of the session cookie (HTTPS scheme → `Secure`). |
| `APP_PORT` | `3000` | Listening port inside the container. Must match the published port. |
| `APP_TRUST_PROXY` | `false` | `true` reads `X-Forwarded-For`/`X-Forwarded-Proto`/`X-Forwarded-Host`. Required behind a reverse proxy, otherwise the login throttling counts the proxy address. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. `debug` also switches GORM to `Info` logging. |
| `APP_DEV` | `false` | Enables Gin debug mode (verbose routing output) for `npm run dev`. Never enable it in production. |

## 4. Database (always external)

| Variable | Default | Notes |
|---|---|---|
| `DB_HOST` | — (**required**) | MariaDB/MySQL host. In docker-compose use `host.docker.internal`. |
| `DB_PORT` | `3306` | |
| `DB_DATABASE` | — (**required**) | Database name; it must exist and be writable. |
| `DB_USERNAME` | — | Can be empty only if the server allows it. |
| `DB_PASSWORD` | — | |
| `DB_SSL` | `false` | `true` adds `tls=skip-verify` to the DSN (typical for managed services). |

The DSN always sets `charset=utf8mb4`, `parseTime=True`, `loc=UTC`,
`interpolateParams=true` and 10/60/60 s timeouts. The pool is 25 open / 5 idle
connections, one hour lifetime, ten minutes idle timeout.

## 5. Security and authentication

| Variable | Default | Notes |
|---|---|---|
| `ENCRYPTION_KEY` | — (**required**) | Master key for AES-256-GCM. Accepted notations, all equal to 32 bytes: base64 (standard / raw / URL, 44 or 43 chars), 64 hex chars, or exactly 32 raw ASCII characters. Generate with `openssl rand -base64 32`. Changing it makes the stored tokens and channel secrets unreadable → reconnect the accounts and re-enter the secrets. |
| `AUTH_METHOD` | `account` | `none` (open instance, every visitor is an administrator), `account` (one login/password from the environment), `keycloak` (OIDC). See [authentication.md](authentication.md). |
| `ACCOUNT_LOGIN` | — | Required in `account` mode. |
| `ACCOUNT_PASSWORD` | — | Required in `account` mode. Compared in constant time. |
| `RECAPTCHA_CLIENTID` | — | Optional; with `RECAPTCHA_CLIENTSECRET` it enables Google reCAPTCHA on the login form. |
| `RECAPTCHA_CLIENTSECRET` | — | Optional. |
| `KEYCLOAK_BASE_URL` | — | Required in `keycloak` mode, e.g. `https://sso.example.com`. |
| `KEYCLOAK_REALM` | — | Required in `keycloak` mode. Issuer = `<base>/realms/<realm>`. |
| `KEYCLOAK_CLIENT_ID` | — | Required in `keycloak` mode. |
| `KEYCLOAK_CLIENT_SECRET` | — | Required in `keycloak` mode. |
| `KEYCLOAK_REDIRECT_URI` | `APP_URL/api/auth/callback` | Derived when empty. Register it as a valid redirect URI in Keycloak. |
| `KEYCLOAK_ACCOUNTS` | — | Allow-list, space or comma separated: `*` (everyone), `you@example.com`, `example.com` or `@example.com` (whole domain). An empty list denies every login. |

## 6. OAuth providers

| Variable | Default | Notes |
|---|---|---|
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | — | Empty means the provider is not offered in the UI; it can be entered later in Admin > Providers. |
| `GOOGLE_REDIRECT_URI` | `APP_URL/api/oauth/google/callback` | Derived when empty. |
| `MICROSOFT_CLIENT_ID` / `MICROSOFT_CLIENT_SECRET` | — | Same behaviour as Google. |
| `MICROSOFT_TENANT_ID` | `common` | `common`, `organizations`, `consumers` or a tenant UUID. Empty and the generic values accept any directory: the consent step then uses `organizations`, because Microsoft does not support `common` for `adminconsent`. A UUID or verified domain locks the client and the consent to that one directory. |
| `MICROSOFT_REDIRECT_URI` | `APP_URL/api/oauth/microsoft/callback` | Derived when empty. |

## 7. Interface defaults

| Variable | Default | Notes |
|---|---|---|
| `DEFAULT_LOCALE` | `en-US` | One of `en-US`, `pt-BR`, `es-MX`, `fr-FR`, `h-CN`, `hi-IN`, `ar-SA`. Used for a visitor who never chose a language. |
| `DEFAULT_THEME` | `system` | `light`, `dark` or `system`. |

The two values above are seeded into the `settings` table at first boot and can
be changed afterwards in **Admin > Settings**; the seed never overwrites an
existing row.

## 8. Validation rules (boot fails with every problem at once)

| Rule | Real message |
|---|---|
| `APP_URL` missing | `APP_URL is required: public URL of this instance (e.g. https://sync.example.com)` |
| `APP_URL` not absolute | `APP_URL must be an absolute URL including the scheme (got "…")` |
| `LOG_LEVEL` unknown | `LOG_LEVEL must be one of "debug", "info", "warn", "error" (got "…")` |
| `ENCRYPTION_KEY` missing/unusable | `ENCRYPTION_KEY is empty; generate one with 'openssl rand -base64 32'` / `… key must be 32 bytes` |
| `DB_HOST` missing | `DB_HOST is required (the database is external; docker users usually set host.docker.internal)` |
| `DB_DATABASE` / `DB_USERNAME` / `DB_PASSWORD` missing | `DB_DATABASE is required` / `DB_USERNAME is required` / `DB_PASSWORD is required (use a real password, even for a local database)` |
| `DB_PORT` out of range | `DB_PORT must be between 1 and 65535 (got 0)` |
| `AUTH_METHOD` unknown | `AUTH_METHOD must be one of "none", "account" or "keycloak" (got "…")` |
| `account` mode | `ACCOUNT_LOGIN is required when AUTH_METHOD=account` · `ACCOUNT_PASSWORD must be at least 8 characters when AUTH_METHOD=account (got N)` · `RECAPTCHA_CLIENTID and RECAPTCHA_CLIENTSECRET must be set together (or both empty to disable the captcha)` |
| `keycloak` mode | one line per missing value: `KEYCLOAK_BASE_URL is required when AUTH_METHOD=keycloak` (same for `KEYCLOAK_REALM`, `KEYCLOAK_CLIENT_ID`, `KEYCLOAK_CLIENT_SECRET`) plus `KEYCLOAK_ACCOUNTS must list at least one e-mail or domain allowed to log in (use * to allow every authenticated user)` |
| provider client half set | `GOOGLE_CLIENT_SECRET is required because GOOGLE_CLIENT_ID is set` (same for Microsoft) |
| `DEFAULT_LOCALE` / `DEFAULT_THEME` unknown | `DEFAULT_LOCALE must be one of en-US, pt-BR, es-MX, fr-FR, h-CN, hi-IN, ar-SA (got "xx")` · `DEFAULT_THEME must be one of light, dark, system (got "…")` |
| database unreachable | `database: cannot open <host>:<port>/<name>: …` or `database: ping failed (check DB_HOST/DB_PORT/DB_USERNAME/DB_PASSWORD)` |

When at least one problem is found the boot stops before the database is touched:

```text
invalid configuration (2 problem(s)):
  - DB_PASSWORD is required (use a real password, even for a local database)
  - DEFAULT_THEME must be one of light, dark, system (got "night")
```

A malformed **optional** value (e.g. `DB_SSL=yes`) never aborts the boot: the
helper falls back to the default and the validation pass reports the real
problems.

## 9. Minimal `.env`

```dotenv
APP_URL=https://sync.example.com
ENCRYPTION_KEY=<openssl rand -base64 32>
DB_HOST=host.docker.internal
DB_DATABASE=sync
DB_USERNAME=sync
DB_PASSWORD=<password>
AUTH_METHOD=account
ACCOUNT_LOGIN=admin@example.com
ACCOUNT_PASSWORD=<password>
```

Everything else has a default. Adding `GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET`
(or configuring them in Admin > Providers) is what makes the Connect page offer
Google Drive; the same applies to Microsoft 365.

## 10. Runtime changes

| What | Where | Effect |
|---|---|---|
| default locale / theme, default interval, run timeout | Admin > Settings (`PUT /api/settings`) | validated (`0–10080` min interval, `1–1440` min timeout) and stored in `settings`; applied to visitors without their own choice |
| provider OAuth client (client id, secret, redirect URI, tenant) | Admin > Providers (`PUT /api/providers/:provider`) | the secret is encrypted (`v1:`) and overrides the environment; `DELETE` clears the override |
| job schedule, direction, conflict policy, excludes, deletions | job editor (`PUT /api/jobs/:id`) | stored on the job; `next_run_at` is recomputed immediately |

## 11. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| container exits immediately, log lists several lines | validation | fix every listed variable; the message names the variable |
| `database: ping failed` | wrong host/port/credentials, or the DB is not reachable from the container | check `DB_*`, and that the host is reachable (`host.docker.internal` on Linux needs the `extra_hosts` entry that `docker/docker-compose.yml` already sets) |
| login redirects to `/?auth_error=…` | cookie rejected (HTTPS/`Secure` mismatch, or a proxy that rewrites the host) | see [reverse-proxy.md](reverse-proxy.md) |
| tokens stopped working after a restart | `ENCRYPTION_KEY` changed | restore the key or reconnect the accounts |
| `503 not_configured` at `/` | the binary was built without `web/dist` | run `npm run build` in `web/` and rebuild the binary (`docs/development.md`) |
