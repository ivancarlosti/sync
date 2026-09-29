# Plan — First build of **Sync** (CloudHQ-like minimal file sync)

- **Date:** 2026-09-27
- **Repo:** https://github.com/ivancarlosti/sync
- **Goal of this build:** a single, working, self-contained image `ghcr.io/ivancarlosti/sync:latest`
  (Go backend + embedded Vue 3 SPA) with OAuth providers (Google Drive / Microsoft Graph),
  encrypted tokens, 3 auth modes, notifications, 7 locales, light/dark theme (+RTL) and
  full AI-oriented documentation.
- **Follow-up:** [`2026-09-28-app-registration-guide.md`](./2026-09-28-app-registration-guide.md)
  — the guided app registration (`Admin > Setup guide`) and the always-on permission set
  (files, users, groups, distribution lists, roles, licences).

> Progress log rule: **update this file at the end of every work session / context loss point.**
> “Current status” must always describe exactly what is done and what is next.

---

## 0. Environment facts discovered

| Item | Value |
|---|---|
| Working dir | `/home/ivan/Documents/Git/sync` (empty repo: only `LICENSE`, `README.md`, `.github/`) |
| Git | branch `main`, remote `origin` = ivancarlosti/sync, 1 commit (`185d7d9 Initial commit`) |
| Go | not preinstalled → installed locally at `$HOME/.local/goroot` (go1.27.1). Export `PATH=$HOME/.local/goroot/bin:$PATH` |
| Node | v18.19.1 / npm 9.2.0 (so frontend deps must stay Node-18 friendly) |
| Docker | 29.8.1 with daemon available |

### CI constraints already present in the repo (must be respected)

* `.github/workflows/build.yml` builds a **root `./Dockerfile`**, multi-arch (amd64+arm64),
  pushes `ghcr.io/<owner>/<repo>:latest` + `:<version>` and passes
  `build-args: VERSION=<x.y.z>`, `COMMIT=<sha>` → the Dockerfile **must** accept `VERSION`/`COMMIT`
  and the binary must expose them at `/api/version` and in `Admin > About`.
* `semantic-release.yml` uses Conventional Commits (see `.github/COMMIT_CONVENTION.md`).

---

## 1. Fixed technical decisions (no open questions)

| Topic | Decision | Why |
|---|---|---|
| Go framework | **Gin** (`github.com/gin-gonic/gin`) | prompt allows Gin *or* Fiber; Gin has the most mature middleware/embed story |
| ORM | **GORM** + `gorm.io/driver/mysql` (MySQL/MariaDB) | required |
| Auto-migration | GORM `AutoMigrate` on boot | required |
| Go module | `github.com/ivancarlosti/sync`, `go 1.25` floor | matches repo owner |
| Frontend | Vue 3.5 + Vite 5 + TS 5.7 + Tailwind **3.4** + shadcn-vue-style components on **reka-ui** | local Node is 18; Tailwind 4 / Vite 8 need newer Node. shadcn-vue is a copy-into-repo component collection, so components live in `src/components/ui/*` with `components.json` kept so `npx shadcn-vue add …` keeps working |
| i18n | `vue-i18n` 11, 7 locales incl. **`h-CN`** (exact prompt name; `zh`/`zh-CN` accepted as alias when auto-detecting) | prompt lists `h-CN` twice |
| Embedding | `web/embed.go` (package `web`) with `//go:embed all:dist` and a committed `web/dist/.gitkeep` placeholder, so `go run ./cmd/server` works on a fresh clone | Go embed cannot reference parent dirs |
| Microsoft Graph | `msgraph-sdk-go` v1.103 + custom kiota `AccessTokenProvider` fed by our own AES-encrypted tokens (`golang.org/x/oauth2` for the token calls) | prompt mandates the SDK, but we must own token storage |
| Google | `google.golang.org/api/drive/v3` with `oauth2` (PKCE, `access_type=offline`) | required |
| Notifications | `shoutrrr` engine for URL-scheme services (SMTP `smtp://`), native HTTP sender for **Webhook** (needs arbitrary method/headers/body template, which `generic://` cannot express). Channel types exposed in UI = `smtp` + `webhook`; API also accepts `shoutrrr` (any shoutrrr URL) | prompt + practicality; documented in `docs/notifications.md` |
| Encryption | AES-256-GCM, `ENCRYPTION_KEY` = base64(32 bytes) *or* 64 hex chars *or* 32 raw chars; versioned `v1:` ciphertext prefix | required |
| Sessions | stateless signed cookie (HMAC-SHA256, key derived from `ENCRYPTION_KEY`), SameSite=Lax, 7 days | no extra table, works behind proxy |
| Auth modes | `none` (open, everyone is admin) / `account` (env credentials + optional reCAPTCHA) / `keycloak` (OIDC code+PKCE, allow-list of mails/domains → 403) | required |
| Sync engine | V1 = **files only** (Google native Docs/Sheets/Slides are skipped + logged). Full-scan reconciliation per run + `sync_files` state table (safe delete propagation, per-file diff). Directions: `google_to_microsoft`, `microsoft_to_google`, `bidirectional` (works intra-provider too). Conflict policies: `newest_wins` (default), `source_wins`, `destination_wins`, `skip`. Empty folders are not mirrored in V1 | scope of §1 of the prompt |
| Scheduler | in-process ticker (30 s) picking due jobs + token-refresh ticker (60 s, refresh 5 min before expiry) | no extra infra |
| Logging | structured `slog` (JSON), **never** logs token values | required |

---

## 2. Deliverable file map (target)

```
Dockerfile                       # stage1 node build → stage2 go build → stage3 alpine
.dockerignore  .gitignore  README.md  LICENSE
docker/ .env  .env.example  docker-compose.yml          (ONLY these three files)
docs/  architecture.md database.md authentication.md providers.md oauth.md
       notifications.md api.md development.md reverse-proxy.md i18n.md branding.md
plans/ 2026-09-27-first-building.md                     (this file)
cmd/server/main.go
internal/ config version crypto database models providers
          providers/google providers/microsoft services handlers notify
web/      embed.go package.json vite.config.ts tsconfig*.json tailwind.config.js
          postcss.config.js components.json index.html
          public/{logo.svg,logo.png,logo-1024.png,favicon.svg,favicon.ico,robots.txt}
          src/{main.ts,App.vue,style.css,assets/logo.svg,assets/logo-icon.svg,
               locales/*.json,lib,stores,router,i18n,components,views}
```

---

## 3. Execution checklist

### Phase A — scaffolding & assets
- [x] Inspect repo, CI workflows, environment
- [x] Install Go toolchain locally (`$HOME/.local/goroot`)
- [x] `plans/` document created
- [x] `.gitignore`, `.dockerignore`
- [x] Bidirectional logo: `web/public/logo.svg`, `logo.png` 1024², `logo-1024.png`, `favicon.svg`,
      `favicon.ico`, `src/assets/logo.svg`, `src/assets/logo-icon.svg` (`apple-touch-icon.png` too)
- [x] `Dockerfile` (multi-stage, ARG VERSION/COMMIT, non-root, healthcheck)
- [x] `docker/docker-compose.yml` + `docker/.env.example` + `docker/.env`

### Phase B — backend
- [x] `internal/version`, `internal/config` (full env validation + derived redirect URIs)
- [x] `internal/crypto` (AES-GCM + HMAC signing helpers)
- [x] `internal/models` (settings, connected_accounts, oauth_states, notification_channels,
      sync_jobs, sync_runs, sync_items, sync_files)
- [x] `internal/database` (connect, AutoMigrate, seed settings)
- [x] `internal/providers` (interface + registry) with `providers/google`, `providers/microsoft`
- [x] `internal/services` (state/PKCE, accounts, auth, keycloak OIDC, settings, notifications, sync engine, scheduler)
- [x] `internal/handlers` (router, middleware/session, auth, oauth, accounts, providers, notifications, settings, sync jobs, dashboard, health/version, SPA fallback)
- [x] `cmd/server/main.go`
- [x] `go mod tidy`, `go build`, `go vet` green
- [x] Smoke test against a live MySQL (`mariadb:11`) + Mailpit: every route group exercised

### Phase C — frontend
- [x] Logo/favicon/og assets: `web/public/*`, `web/src/assets/*`
- [x] Go side of the embed (`web/embed.go` + `web/dist/.gitkeep`); a binary built without a
      frontend answers `503 not_configured` on `/` instead of an empty page
- [x] Vite/Tailwind/shadcn-vue setup, theme tokens (light/dark), RTL support
- [x] 7 locale JSON files (en-US, pt-BR, es-MX, fr-FR, h-CN, hi-IN, ar-SA) — no hardcoded UI text
- [x] header: logo + 7-language selector + theme toggle (light/dark/system), persisted in localStorage
- [x] views: Login, Dashboard, Connected Accounts (+ folder browser), Sync Jobs (+ editor/run/runs),
      Notifications, Admin > Providers (wizard), Admin > Settings, Admin > About, 404
- [x] `npm install`, `npm run build`, `vue-tsc` green (verified through the two-step build)

### Phase D — docs & packaging
- [x] 12 docs under `/docs` (the 11 of the plan plus `configuration.md`, which the code
      references from `internal/database/database.go`)
- [x] `README.md` (features, quick start, env, docs index, security, footer/buttons markers
      for `.github/workflows/update-readme.yml`)
- [x] `web/scripts/check-i18n.mjs` (parity + key-usage + placeholder checker, wired into
      `npm run typecheck`, which is part of `npm run build`)
- [x] Validate: `go build ./...`, `npm run build`, `go test ./...`, smoke routes
- [x] Final review of acceptance criteria (section 15 of the prompt)

---

## 4. Acceptance-criteria tracker (prompt §15)

| # | Criterion | Status |
|---|---|---|
| 1 | `go run ./cmd/server` works locally reading `.env` | ✅ verified (smoke runs, `none` + `account` modes) |
| 2 | Root `Dockerfile` → `ghcr.io/ivancarlosti/sync:latest` | ✅ image builds locally (`docker build` green after fixing the Alpine `sync`-user clash); CI pushes it |
| 3 | `docker/docker-compose.yml` up with external DB via `host.docker.internal` | ✅ `docker compose config` valid; container smoke-tested against a MariaDB on the host |
| 4 | logo files exist & used (header, favicon, og:image) | ✅ header + login card use `/logo.svg`, favicon/ico/apple-touch + og/twitter meta in `index.html` |
| 5 | Admin > Providers shows Google/Microsoft configured or not | ✅ rendered in headless Chrome (`source`, `secret_set`, redirect hint) |
| 6 | Connect Google + Microsoft via OAuth and browse drives/folders | ◐ implemented, not exercised live (needs real OAuth clients) |
| 7 | Tokens stored encrypted + auto refresh works | ✅ ciphertext-only in DB, refresh path tested |
| 8 | SMTP + Webhook notifications work | ✅ verified live (Mailpit + HTTP target) |
| 9 | 3 auth modes (none/account/keycloak + allow-list) | ✅ `none` + `account` verified live (login, cookie, 401 without cookie); `keycloak` implemented |
| 10 | `APP_URL` + `APP_TRUST_PROXY` behind reverse proxy | ◐ implemented, documented in `docs/reverse-proxy.md`; no proxy run in this environment |
| 11 | Header: logo + 7 locales + theme toggle, RTL for ar-SA | ✅ headless render: 10/10 routes, `ar-SA` → `lang="ar-SA" dir="rtl"` + Arabic nav |
| 12 | Admin can change default locale/theme in Settings | ✅ verified live (PUT → session defaults → RTL render, then restored) |
| 13 | `/docs` complete & detailed for AI agents | ✅ 12 documents (1 814 lines) + README |
| 14 | `/docker` contains only `.env`, `.env.example`, `docker-compose.yml` | ✅ |
| 15 | Comment/boot-validation rules (§14): EN comments, boot env validation, no tokens in logs | ✅ |

Legend: ✅ done and verified · ◐ partially verified · ☐ not started.

---

## 5. Progress log

### 2026-09-27 — session 1
- Repo/CI analysis, CI contract extracted (Dockerfile at root, VERSION/COMMIT build args).
- Go 1.27.1 installed locally; dependency versions pinned and verified to exist
  (frontend: vue 3.5.43, vite 5.4.11, tailwindcss 3.4.17, reka-ui 2.10.5, vue-i18n 11.4.12, pinia 2.3.0,
  vue-router 4.5.0, axios 1.20.0, vue-tsc 2.2.0, typescript 5.7.2 — chosen for Node 18 compatibility;
  backend: gin v1.12.0, gorm v1.31.2 + mysql v1.6.0, x/oauth2 v0.37.0, google.golang.org/api v0.299.0,
  msgraph-sdk-go v1.103.0, kiota-abstractions-go v1.11.1, shoutrrr v0.21.1, godotenv v1.5.1).
- **Next:** Phase A files.

### 2026-09-27/28 — sessions 2-4 (phases B-D completed)
- Backend: config/validation, crypto, GORM schema + seeding, providers (Google + Graph),
  services (store, tokens, auth, oauth, settings, sync engine, scheduler, notifier),
  handlers (36 endpoints), embedded SPA fallback. `go build`, `go vet`, `go test ./...` green.
- Frontend: Vue 3 + Vite + Tailwind + reka-ui SPA, 7 locale catalogs (418 keys each),
  light/dark theme with a pre-paint script, RTL for `ar-SA`, 12 routes (login, dashboard,
  accounts + folder browser, jobs + editor, runs, notifications, admin providers/settings/about, 404).
- **Fixed during verification:**
  * `initLocale()` persisted the browser guess to `localStorage`, so the administrator
    `DEFAULT_LOCALE` could never apply → detection results are no longer persisted;
    verified live: settings `ar-SA` now renders `lang="ar-SA" dir="rtl"` with Arabic nav.
  * `LoginView` mapped `unauthorized` to the "session expired" copy; a wrong password now
    shows `auth.error_invalid` (`LOGIN_CODE_ALIASES`).
  * `Dockerfile` failed to build: `alpine:3.22` already ships a `sync` user, so
    `adduser -S sync` aborted the image → the runtime account is now `app` (verified by a
    green `docker build`; the image runs against an external MariaDB with a healthy `/api/health`).
- **Added:** `web/scripts/check-i18n.mjs` (catalog parity, key usage, placeholder check,
  wired into `npm run typecheck` → `npm run build`), 12 documents under `docs/` and a full
  `README.md`.
- **Verified:** two-step build (`npm run build` → `go build`), 10/10 SPA routes rendered in
  headless Chrome (correct i18n and theme), `ar-SA` RTL, `account`-mode login + session cookie
  + 401 without cookie, `docker compose config`, `docker build`, container healthcheck.
- **Next:** real Google/Microsoft OAuth smoke test (needs live clients); a reverse-proxy run
  for acceptance #10; tag a release so `semantic-release` + `build.yml` publish the image.
