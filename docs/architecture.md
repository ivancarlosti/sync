# Architecture

Sync is a single Go binary that serves both a JSON API and an embedded Vue 3
single-page application. There is no separate frontend container, no Node.js at
runtime and no queue or external worker: one process, one MySQL/MariaDB
database, one HTTP port.

## 1. Process shape

```text
                       ┌──────────────────────────── browser ────────────────────────────┐
                       │  Vue 3 SPA (served by the same binary, embedded in the Go build)│
                       └───────────────┬─────────────────────────────────────────────────┘
                                       │  /api/*  (JSON, session cookie)
┌──────────────────────────────────────▼──────────────────────────────────────────────────┐
│ cmd/server (main.go) — configuration, database, wiring, HTTP server, signal handling     │
│                                                                                         │
│  internal/handlers  ── routing, session, validation, error → HTTP status mapping          │
│  internal/services  ── Store, AuthService, OAuthService, TokenManager, SettingsService,   │
│                        SyncService (engine), Scheduler, Notifier, ProviderSettings        │
│  internal/providers ── provider interface + google + microsoft implementations            │
│  internal/notify    ── SMTP / webhook / shoutrrr senders                                  │
│  internal/database  ── GORM connection, AutoMigrate, settings repository                  │
│  internal/models    ── GORM entities and every enum vocabulary                            │
│  internal/crypto    ── AES-256-GCM, HMAC-SHA256, PKCE                                     │
│  internal/config    ── environment parsing + validation (the only os.Getenv caller)       │
└──────────────────────────────────────┬──────────────────────────────────────────────────┘
                                       │ GORM (go-sql-driver/mysql, UTC, utf8mb4)
                                       ▼
                          MySQL / MariaDB  (always external)
```

Three background loops run inside the same process (`internal/services/scheduler.go`):

| Loop | Period | What it does |
|---|---|---|
| schedule | 30 s (`DefaultScheduleInterval`) | starts every enabled job whose `next_run_at` elapsed |
| tokens | 60 s (`DefaultTokenInterval`) | renews access tokens that expire inside the 5 min window (`DefaultRefreshWindow`) |
| (on boot) bootstrap | once | closes runs a restart interrupted, recomputes `next_run_at` for every scheduled job |

Sync runs are goroutines owned by `SyncService`; a per-job lease guarantees that
one job never runs twice, even when the binary is started twice against the same
database.

## 2. Request lifecycle

Every request passes through the same chain (`internal/handlers/handlers.go`):

1. `gin.LoggerWithConfig` (skips `/api/health`) + `gin.Recovery`.
2. `securityHeaders()` — `X-Content-Type-Options`, `X-Frame-Options: DENY`,
   `Referrer-Policy: same-origin` and the Content-Security-Policy.
3. `trustProxies()` — `APP_TRUST_PROXY=true` makes Gin read `X-Forwarded-For`
   (login throttling and logs depend on the real client address).
4. `session()` — resolves the operator once per request. In `none` mode every
   request carries the open administrator identity; otherwise the signed
   `sync_session` cookie is verified (an invalid/expired cookie simply leaves the
   request anonymous).
5. `requireSession()` on the `/api` tree below the public endpoints — answers
   `401 {"code":"unauthorized"}` when the request is anonymous.
6. The handler decodes its payload, calls exactly one service method and maps the
   error with `fail()`/`abort()` (`internal/handlers/respond.go`).

Anything that is not `/api/**` is served by the SPA handler:

| Request | Answer |
|---|---|
| an existing file in the embedded `dist` (e.g. `/assets/index-abc.js`) | the file; `Cache-Control: public, max-age=31536000, immutable` under `assets/`, `no-cache` otherwise |
| any other path (`/`, `/jobs/12`, …) | `index.html` (`no-cache`) so the client router resolves the deep link |
| `/api/...` with no route | JSON `404` — the SPA never masks an API typo |
| a binary built without a frontend | `503 {"code":"not_configured"}` |

## 3. Sync run pipeline

`SyncService.Start` (`internal/services/sync.go`) claims the per-job lease,
creates the `sync_runs` row (`status=running`) and runs one pass under
`context.WithTimeout(run timeout)`:

```text
list source tree ──► list destination tree ──► plan per path ──► transfer ──► update sync_files ──► finalise run
   (provider A)          (provider B)        (state + policy)   (stream)      (state table)        (status, notify)
```

* **Listing** skips exclude patterns (counted as `skipped`) and collects the
  items V1 cannot transfer (Google native documents, shortcuts) as `unsupported`.
* **Planning** compares the two sides against `sync_files` (the state table) and
  the conflict policy; every decision is stored with a short `evidence` string
  (e.g. `destination newer by 42s`).
* **Transfer** streams a download and an upload; folders are created on demand
  (`folder_created`).
* **Finalising** sets `success`, `partial` (some files failed) or `failed`, writes
  the counters, updates `last_run_at`/`next_run_at`, touches the accounts'
  `last_synced_at` and publishes `sync.success` / `sync.run_failed` / `sync.error`
  to the notifier.

Cancellation is cooperative (`SyncService.Cancel`), and a restart closes runs that
were left `running` (the audit history never shows a run that no longer exists).

## 4. Invariants every change must keep

1. **Secrets never leave the process in clear text.** `internal/crypto.Encrypt`
   produces `v1:<base64>` AES-256-GCM payloads; the models declare token columns
   with `json:"-"`, the notification channels mask every secret on the way out
   (`********`) and no log line prints a token (`internal/services/*` log only
   account ids, masked e-mails and error strings).
2. **No hardcoded UI text.** Every string the SPA renders comes from
   `web/src/i18n/locales/*.json`; the API answers a machine-readable `code`
   (`validation`, `not_found`, `unauthorized`, `forbidden`, `busy`, `reconnect`,
   `not_configured`, `internal`, `delivery_failed`) beside the English message, and
   the SPA translates the code (see [i18n.md](i18n.md)).
3. **The database schema is created by `database.Migrate`**, never by hand: an
   upgrade is "pull the image and start it".
4. **Only `internal/config` reads the environment.** Every other package receives
   values through a constructor, which is what makes the boot contract testable.
5. **The API is thin.** A handler validates input, calls one service method and
   maps the error; business rules live in `internal/services`.
6. **The SPA is embedded.** `web/embed.go` (`//go:embed all:dist`) means a release
   image contains exactly one artifact; `web/dist/.gitkeep` keeps a fresh clone
   buildable without Node.

## 5. Where to change what

| Change | Touch |
|---|---|
| new endpoint | `internal/handlers/routes.go` + handler file + `docs/api.md` |
| new setting | `internal/models/enums.go` (`Setting*`), `services.Defaults`, `SettingsService.Update`, Admin > Settings view + catalogs |
| new channel kind | `internal/notify` sender + `channelSchemas` in `internal/handlers/notifications.go` + `docs/notifications.md` |
| new provider | implement `providers.Provider`, register in `cmd/server/main.go` (`build`), document in `providers.md` |
| new UI language | catalog file + `web/src/i18n/index.ts` (`MESSAGES`, `LOCALES`) + `config.SupportedLocales` + `docs/i18n.md` |
| new screen | `web/src/views/**`, route in `web/src/router/index.ts`, nav entry + catalog keys |

## 6. Related documents

* [configuration.md](configuration.md) — every environment variable and its validation
* [database.md](database.md) — tables, indexes, retention, upgrade path
* [authentication.md](authentication.md) — the three auth modes and the session cookie
* [oauth.md](oauth.md) — provider flows, PKCE, token refresh
* [providers.md](providers.md) — provider capabilities and registration
* [api.md](api.md) — endpoint reference
* [notifications.md](notifications.md) — channel schemas and events
* [development.md](development.md) — toolchain, commands, tests
* [reverse-proxy.md](reverse-proxy.md) — HTTPS, cookies, headers in front of Sync
* [i18n.md](i18n.md) — catalogs, parity, RTL
* [branding.md](branding.md) — logo assets, theme tokens

