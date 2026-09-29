# Development

## 1. Toolchain

| Tool | Version used by this repository |
|---|---|
| Go | `go 1.27.1` (`go.mod`), the Dockerfile builds with `golang:1.27-alpine` |
| Node | `^20.19.0 || >=22.12.0` (`web/package.json` → `engines.node`), the floor Vite 8 declares; the Dockerfile builds with `node:22-alpine` |
| npm | ships with Node; `npm ci` in `web/` |
| Docker | any recent release with buildx (multi-arch `amd64` + `arm64` are built in CI) |
| Database | MySQL 8 / MariaDB 11 for local runs; the unit tests need none (see §4) |

If Go is not on the `PATH`, this checkout has been developed with a local SDK at
`$HOME/.local/goroot`: `export PATH=$HOME/.local/goroot/bin:$PATH`.

## 2. Layout

```text
cmd/server            the binary: config → database → services → HTTP → signals
internal/config        environment parsing + validation (the only os.Getenv caller)
internal/version       build metadata injected with -ldflags
internal/crypto        AES-256-GCM, HMAC-SHA256, PKCE (standard library only)
internal/database      GORM connection, DSN, AutoMigrate, settings repository
internal/models        GORM entities + every enum vocabulary (also the API contract)
internal/providers     Provider interface, registry, google/, microsoft/
internal/services      Store, AuthService, OAuthService, TokenManager, SettingsService,
                       SyncService (engine), Scheduler, Notifier, ProviderSettings, SecretBox
internal/notify        senders: smtp, webhook, shoutrrr (no storage knowledge)
internal/handlers      routing, session, validation, error → HTTP mapping, SPA fallback
web/                   Vue 3 SPA + embed.go (//go:embed all:dist)
web/src/i18n/locales   7 catalogs (en-US, pt-BR, es-MX, fr-FR, h-CN, hi-IN, ar-SA)
docs/                  this documentation
plans/                 the build plan and its progress log
```

Rules that keep the code reviewable:

* comments and identifiers in **English**; no commented-out code;
* no UI string in the frontend outside `web/src/i18n/locales/*.json`;
* no `os.Getenv` outside `internal/config`; no direct provider call outside
  `internal/services`/`internal/providers`; no token read outside `TokenManager`.

## 3. Commands

```bash
# backend
export PATH=$HOME/.local/goroot/bin:$PATH
go build ./...            # compile everything
go vet ./...              # static checks
go test ./...             # unit + handler tests (in-memory SQLite, no database needed)
go run ./cmd/server       # needs .env (or the environment) and a reachable database

# frontend (from web/)
npm ci                    # install exactly the lockfile
npm run dev               # Vite dev server on :5173, proxies /api to :3000
npm run typecheck         # vue-tsc --noEmit (both tsconfigs)
npm run build             # typecheck + vite build → web/dist
npm run preview           # serve the built SPA on :4173
```

### Two-step build (the SPA is embedded)

The Go binary embeds `web/dist` with `//go:embed all:dist`, so a release build is:

```bash
cd web && npm run build && cd ..          # 1. write web/dist
go build -o sync ./cmd/server             # 2. embed it
```

A binary built **without** step 1 contains only `web/dist/.gitkeep` and answers
`503 not_configured` at `/`; that is intentional, and it is also the classic
"my changes do not show up" cause — the Go binary was not rebuilt after the
frontend build.

`npm run dev` does not need the embed at all: Vite serves the SPA and proxies
`/api` (target `VITE_DEV_API`, default `http://127.0.0.1:3000`), which keeps the
session cookie on one origin.

## 4. Tests

```bash
go test ./...                 # ~45 s, everything
go test ./internal/services/  # the sync engine, tokens, notifications
go test ./internal/handlers/  # the API surface, with an in-memory database
go test ./internal/notify/    # senders against a local HTTP server
```

The suites are self-contained: `internal/services` and `internal/handlers` open
an in-memory SQLite database (`gorm.io/driver/sqlite`) and a fake provider, so no
MariaDB, no network and no credentials are required. `internal/handlers` drives
the real Gin router, which is why a routing change fails a test rather than a
review.

## 5. Adding things

### An endpoint

1. handler in the right `internal/handlers/*.go` file (validate, call one service
   method, `fail(c, err)` on error);
2. route in `internal/handlers/routes.go` (public group or `/secured`);
3. a test in `internal/handlers/handlers_test.go`;
4. document it in [api.md](api.md) and, if it returns text the UI shows, add the
   catalog key in all seven locales.

### A UI string

1. add the key to `web/src/i18n/locales/en-US.json` (namespaced: `common`, `nav`,
   `auth`, `status`, `validation`, `dashboard`, `accounts`, `browser`, `jobs`,
   `runs`, `notifications`, `admin`, `errors`, `theme`, `language`);
2. translate it in the six other files;
3. `node web/scripts/check-i18n.mjs` (also part of `npm run typecheck`);
4. use `t('namespace.key')` in the component — never a literal string.

### A locale

See [i18n.md](i18n.md): catalog file, `MESSAGES` + `LOCALES` in
`web/src/i18n/index.ts`, `config.SupportedLocales`, the alias table and the docs.

### A provider

Implement `providers.Provider` (see [providers.md](providers.md)), register it in
`build()` in `cmd/server/main.go`, add its name to `models.ProviderName.Valid()`
and to the admin wizard's copy.

## 6. Build pipeline (CI)

| Workflow | Trigger | What it does |
|---|---|---|
| `semantic-release.yml` | push to `main` | Conventional Commits → next version, tag, GitHub release, then dispatches `build.yml` |
| `build.yml` | release published / manual | `docker buildx` for `linux/amd64` + `linux/arm64` with `--build-arg VERSION=<x.y.z> COMMIT=<sha>`, pushes `ghcr.io/<owner>/<repo>:{latest,<version>}` |
| `update-readme.yml` | daily / manual | refreshes the buttons and footer block of `README.md` from the `ivancarlosti/.github` template |
| `keepalive.yml` | weekly | empty commit to keep the repository active |

The Dockerfile is a three-stage build (`node` → `golang` → `alpine`) that ends in
a non-root user with a `HEALTHCHECK` on `/api/health`; `VERSION`/`COMMIT` reach
`/api/version` and Admin > About through `-ldflags -X
internal/version.{Version,Commit}`.

## 7. Debugging

| Symptom | Check |
|---|---|
| blank page, `503 not_configured` | the Go binary was built before `web/dist` existed → rebuild both steps |
| the SPA shows old code | assets are immutable; the entry document is `no-cache`, so a hard reload is enough — but re-run the two-step build after a source change |
| `npm run dev` cannot reach the API | the Go server must listen on the `VITE_DEV_API` target (default 3000) |
| session lost on every request | cookie `Secure` requires HTTPS: `APP_URL` must match how you actually reach the instance |
| provider call hangs | `LOG_LEVEL=debug`; the provider clients use explicit request timeouts |
| a job never starts | `enabled`, `interval_minutes` and `next_run_at` (`GET /api/jobs/:id/schedule`), then `POST /api/maintenance/schedule/run` |
