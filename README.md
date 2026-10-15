<!-- buttons -->
[![Stars](https://img.shields.io/github/stars/ivancarlosti/sync?label=⭐%20Stars&color=gold&style=flat)](https://github.com/ivancarlosti/sync/stargazers)
[![Watchers](https://img.shields.io/github/watchers/ivancarlosti/sync?label=Watchers&style=flat&color=red)](https://github.com/sponsors/ivancarlosti)
[![Forks](https://img.shields.io/github/forks/ivancarlosti/sync?label=Forks&style=flat&color=ff69b4)](https://github.com/sponsors/ivancarlosti)
[![Downloads](https://img.shields.io/github/downloads/ivancarlosti/sync/total?label=Downloads&color=success)](https://github.com/ivancarlosti/sync/releases)
[![GitHub commit activity](https://img.shields.io/github/commit-activity/m/ivancarlosti/sync?label=Activity)](https://github.com/ivancarlosti/sync/pulse)
[![GitHub Issues](https://img.shields.io/github/issues/ivancarlosti/sync?label=Issues&color=orange)](https://github.com/ivancarlosti/sync/issues)  
[![License](https://img.shields.io/github/license/ivancarlosti/sync?label=License)](LICENSE)
[![GitHub last commit](https://img.shields.io/github/last-commit/ivancarlosti/sync?label=Last%20Commit)](https://github.com/ivancarlosti/sync/commits)
[![Security](https://img.shields.io/badge/Security-View%20Here-purple)](https://github.com/ivancarlosti/sync/security)
[![Code of Conduct](https://img.shields.io/badge/Code%20of%20Conduct-2.1-4baaaa)](https://github.com/ivancarlosti/sync?tab=coc-ov-file)
<!-- endbuttons -->

<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="web/public/logo.svg">
    <img src="web/public/logo.svg" alt="Sync" width="96" height="96">
  </picture>
  <br>
  Sync
</h1>

<p align="center">
  Self-hosted file synchronisation between <strong>Google Drive</strong> and
  <strong>Microsoft 365</strong> (OneDrive / SharePoint) — one Go binary with an
  embedded Vue&nbsp;3 interface, encrypted tokens, notifications, seven languages
  and light/dark themes.
</p>

---

## Features

* **Two providers, any direction** — Google Drive → OneDrive/SharePoint,
  Microsoft → Google, or bidirectional; intra-provider jobs work too
  (Google → Google, OneDrive → OneDrive, or OneDrive → SharePoint of the same
  work account).
* **Per-job control** — source and destination folders picked in a browser,
  conflict policy (`newest_wins`, `source_wins`, `destination_wins`, `skip`),
  exclude globs (`*.tmp`, `cache/**`), optional deletion propagation and a
  schedule (from manual-only up to weekly).
* **Full audit trail** — every run stores counters and a per-file decision with
  the reason (`evidence`), which is what the Runs screen and
  `GET /api/runs/:id/items` show.
* **Encrypted at rest** — OAuth access/refresh tokens and notification secrets
  are AES-256-GCM ciphertexts (`v1:`); the database never holds a plaintext
  secret.
* **Three authentication modes** — `none`, `account` (single credential pair) and
  `keycloak` (OIDC + e-mail/domain allow-list), with signed stateless session
  cookies, login throttling and optional reCAPTCHA.
* **Notifications** — SMTP, webhooks (method, headers, body template, HMAC) and
  any [shoutrrr](https://containrrr.dev/shoutrrr/) URL (Discord, Telegram,
  Gotify, ntfy, …) for `sync.success`, `sync.run_failed`, `sync.error`,
  `account.connected`, `account.error`.
* **Seven locales incl. RTL** — `en-US`, `pt-BR`, `es-MX`, `fr-FR`, `h-CN`,
  `hi-IN`, `ar-SA` (the Arabic catalog mirrors the whole document).
* **Guided app registration** — `Admin > Setup guide` walks an operator through
  the Google Cloud console or Microsoft Entra with the exact redirect URI, the
  exact permission list and a one-click tenant-wide admin consent; the Accounts
  screen then shows which capability each connected account holds and what a
  reconnection would add.
* **Admin screens** — provider credentials (with an encrypted override over the
  environment), instance defaults (locale, theme, interval, run timeout), raw
  settings, maintenance actions and build information.
* **One container** — the SPA is embedded in the Go binary; the only external
  dependency is MySQL/MariaDB.

## Interface

Every screen below is one route of the Vue SPA the Go binary serves: no second
process, no asset server. They were captured on a running instance in the default
light theme and in `en-US` (each screen also ships in dark mode and in the other
six languages, Arabic included), filled with a small demo dataset — three
connected accounts, four jobs, a week of runs and a week of traffic — so the
numbers are illustrative, not a benchmark.

### Sign in and overview

**Sign in** — `AUTH_METHOD=account` asks for the credential pair from the
environment; `none` opens the dashboard directly and `keycloak` replaces the form
with the OIDC button. Google reCAPTCHA, when configured, appears on this form.

![Sign in](docs/screenshots/01-login.png)

**Dashboard** — the counters of the selected window (24 hours, 7 days, 30 days or
the whole history), the most recent runs, and the health of the instance.

![Dashboard](docs/screenshots/02-dashboard.png)

### Accounts and jobs

**Accounts** — one row per connected account: the permissions the stored grant
actually satisfies, what a reconnection would add (a personal Microsoft account
can never hold the directory ones, so they are reported separately), the token
expiry and the last synchronisation.

![Accounts](docs/screenshots/03-accounts.png)

**Jobs** — the source and destination of every job, its direction and conflict
policy, a schedule preview, the outcome of the last run and the next slot;
running, enabling, editing and deleting happen in place.

![Jobs](docs/screenshots/04-jobs.png)

**New job** — a four step wizard (details, source, destination, options) where
the direction follows the provider pair the operator picks.

![New job](docs/screenshots/05-job-new.png)

**Job editor** — the same wizard for an existing job, with the picked drives and
folders, the exclude patterns and the interval the job was saved with.

![Job editor](docs/screenshots/06-job-editor.png)

### Runs, audit and billing

**Runs** — the log of everything the engine copied, filterable by job, with a
banner while a run is in flight and the counters of each execution (scanned,
created, updated, deleted, skipped, conflicts, errors and the bytes moved).

![Runs](docs/screenshots/07-runs.png)

**Run detail** — the counters of one run, the traffic it moved per account, and
every file decision with the reason the engine recorded: the `evidence` field the
API returns, so "why did this file move?" has an answer months later.

![Run detail](docs/screenshots/08-run-detail.png)

**Audit** — point at an account, a folder and a depth, and walk the tree to see
what it really holds; the completed reports stay in the history and download as
CSV.

![Audit](docs/screenshots/09-audit.png)

**Audit report** — the tree with the exact size of every folder, the folders
whose children were not listed, and the totals the report was computed from (a
depth limit trims the listing, never the totals).

![Audit report](docs/screenshots/10-audit-report.png)

**Billing** — the traffic the instance moved, by day, month, account or run,
priced with the line-item vocabulary of an invoice (OCI here; AWS, GCP and a
custom mapping are available) and both rates from *Administration → Settings*.

![Billing](docs/screenshots/11-billing.png)

**Notifications** — one card per channel (SMTP, webhook, or any
[shoutrrr](https://containrrr.dev/shoutrrr/) URL) with the events it subscribes
to, the outcome of the last delivery and a button that sends a test through it.

![Notifications](docs/screenshots/12-notifications.png)

### Administration

**Providers** — the OAuth client of each provider, either typed here (stored
encrypted, overriding the environment) or inherited from it, with the redirect
URI that has to be registered in the provider console.

![Providers](docs/screenshots/13-providers.png)

**Setup guide** — the walkthrough that produces that client: the console steps,
the exact redirect URI and the exact permission list with copy buttons, plus what
each permission unlocks and why an administrator decision is needed.

![Setup guide](docs/screenshots/14-setup-guide.png)

**API** — personal access tokens for the REST API (`read` or `read` + `write`),
with their expiry, the last address that used them and the request count; the
secret itself is shown once, at creation.

![API](docs/screenshots/15-api-tokens.png)

**Settings** — the defaults proposed to every operator (language, theme,
schedule, run timeout), the billing vocabulary and prices, and how long runs,
audit reports and traffic days are kept.

![Settings](docs/screenshots/16-settings.png)

**About** — build metadata, the providers compiled into this binary, the shipped
languages and a live health check of the database.

![About](docs/screenshots/17-about.png)

**Not found** — an unknown route (and every API error) is answered with a message
and a way back, rather than a blank page.

![Not found](docs/screenshots/18-not-found.png)

The set is regenerated with `npm run screenshots` from `web/`, against a running
instance (see [docs/development.md](docs/development.md)).

<!-- NEXT -->
## Quick start (Docker Compose)

You need a reachable MySQL/MariaDB (Sync never ships a database container — the
README's compose file assumes `host.docker.internal`) and one secret key.

```bash
cd docker
cp .env.example .env          # then edit it
openssl rand -base64 32       # paste the result into ENCRYPTION_KEY

docker compose up -d
docker compose logs -f
```

Open `http://<host>:3000` (or the `APP_URL` you configured). With
`AUTH_METHOD=account` sign in with `ACCOUNT_LOGIN`/`ACCOUNT_PASSWORD`; with
`none` the dashboard opens directly.

The image is published as `ghcr.io/ivancarlosti/sync:latest` (and
`:<version>`); to build it locally instead, uncomment the `build:` block in
`docker/docker-compose.yml` and run `docker compose up -d --build`.

### Minimum `.env`

```dotenv
APP_URL=https://sync.example.com
ENCRYPTION_KEY=<openssl rand -base64 32>
DB_HOST=host.docker.internal
DB_DATABASE=sync
DB_USERNAME=sync
DB_PASSWORD=<password>
AUTH_METHOD=account
ACCOUNT_LOGIN=admin@example.com
ACCOUNT_PASSWORD=<at least 8 characters>
```

Everything else has a default. Adding `GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET`
(or creating the application later with **Admin > Setup guide**, and pasting the
values in **Admin > Providers**) is what enables Google Drive;
`MICROSOFT_CLIENT_ID`/`MICROSOFT_CLIENT_SECRET` do the same for Microsoft 365.
Every variable, its default and its validation message:
[docs/configuration.md](docs/configuration.md).

## First run

1. **Accounts** — connect a Google and/or Microsoft account (the OAuth client must
   be configured first; the screen links to the providers page otherwise, and
   **Admin > Setup guide** has the whole registration walkthrough).
2. **Jobs → New job** — pick the accounts and folders, choose a direction, a
   conflict policy, optional excludes, an interval, then save.
3. *Synchronise now* runs it immediately; the run, its counters and its per-file
   decisions appear under **Runs**, and the dashboard aggregates them.
4. **Notifications** — add a channel (e-mail, webhook, shoutrrr) and subscribe it
   to the events you care about; the *Send a test notification* button validates
   it.
5. **Administration → Settings** — set the default language, theme, interval and
   run timeout for operators who never chose their own.

## Local development

```bash
export PATH=$HOME/.local/goroot/bin:$PATH   # if Go is installed locally
go test ./...                               # self-contained: in-memory SQLite
go run ./cmd/server                         # reads .env / the environment

cd web
npm ci
npm run dev                                 # Vite on :5173, proxies /api → :3000
```

The Go binary embeds `web/dist`, so a release build is two steps:

```bash
cd web && npm run build && cd ..
go build -o sync ./cmd/server
```

Details, conventions and the test layout: [docs/development.md](docs/development.md).

<!-- NEXT -->
## Documentation

| Document | Content |
|---|---|
| [architecture.md](docs/architecture.md) | process shape, request lifecycle, sync pipeline, invariants |
| [configuration.md](docs/configuration.md) | every environment variable, boot sequence, validation, troubleshooting |
| [database.md](docs/database.md) | tables, indexes, relationships, retention, backup, useful queries |
| [authentication.md](docs/authentication.md) | the three modes, session cookie, throttling, captcha, Keycloak allow-list |
| [oauth.md](docs/oauth.md) | provider flows, PKCE, token storage/refresh, redirect URIs, tenant-wide consent |
| [app-registration.md](docs/app-registration.md) | guided app registration for Google Workspace and Microsoft Entra, permissions, consent, troubleshooting |
| [providers.md](docs/providers.md) | provider interface, capabilities, app registration, troubleshooting |
| [notifications.md](docs/notifications.md) | channel schemas, events, placeholders, secrets, delivery semantics |
| [api.md](docs/api.md) | endpoint reference with payloads, error codes and examples |
| [development.md](docs/development.md) | toolchain, commands, tests, how to extend, CI pipeline |
| [reverse-proxy.md](docs/reverse-proxy.md) | nginx/Caddy/Traefik, headers, health checks, pitfalls |
| [i18n.md](docs/i18n.md) | catalogs, resolution order, RTL, formatting, parity checker |
| [branding.md](docs/branding.md) | assets, theme tokens, rebranding checklist |

## Security notes

* Secrets are encrypted with AES-256-GCM; keep `ENCRYPTION_KEY` out of the image
  and out of the repository (use a secret manager or an env file with restricted
  permissions). Changing it invalidates stored tokens.
* Sessions are signed, stateless cookies (7 days, `HttpOnly`, `SameSite=Lax`,
  `Secure` when `APP_URL` is HTTPS); every answer carries a Content-Security
  Policy, `X-Frame-Options: DENY` and `Referrer-Policy: same-origin`.
* The API is only reachable with a session (plus the documented public
  endpoints); OAuth callbacks are protected by a single-use signed `state`.
* Login throttling counts the client address, so set `APP_TRUST_PROXY=true` when
  a reverse proxy is in front (see [reverse-proxy.md](docs/reverse-proxy.md)).

## License

See [LICENSE](LICENSE).

<!-- footer -->
---

## 🧑‍💻 Consulting and technical support
* For personal support and queries, please submit a new issue to have it addressed.
* For commercial related questions, please [**contact me**][ivancarlos] for consulting costs.

[cc]: https://docs.github.com/en/communities/setting-up-your-project-for-healthy-contributions/adding-a-code-of-conduct-to-your-project
[contributing]: https://docs.github.com/en/articles/setting-guidelines-for-repository-contributors
[security]: https://docs.github.com/en/code-security/getting-started/adding-a-security-policy-to-your-repository
[support]: https://docs.github.com/en/articles/adding-support-resources-to-your-project
[it]: https://docs.github.com/en/communities/using-templates-to-encourage-useful-issues-and-pull-requests/configuring-issue-templates-for-your-repository#configuring-the-template-chooser
[prt]: https://docs.github.com/en/communities/using-templates-to-encourage-useful-issues-and-pull-requests/creating-a-pull-request-template-for-your-repository
[funding]: https://docs.github.com/en/articles/displaying-a-sponsor-button-in-your-repository
[ivancarlos]: https://ivancarlos.me
