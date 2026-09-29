# API reference

Base path `/api`. Requests and answers are JSON (`Content-Type:
application/json`), except the SPA assets. Authentication is the session cookie
(see [authentication.md](authentication.md)); the two OAuth callbacks are public
and protected by the signed `state` parameter instead.

## 1. Conventions

* **Error shape** — every failure answers the same object:

  ```json
  { "code": "validation", "error": "the request body is not valid JSON: …" }
  ```

  `code` is the machine-readable value the SPA translates, `error` is an English
  message shown as technical detail. Server-side failures never expose internals:
  they answer `{"code":"internal","error":"unexpected error, check the server logs"}`
  and the real error goes to the process log.

* **Codes and statuses** (`internal/handlers/respond.go`):

  | Code | HTTP | Meaning |
  |---|---|---|
  | `validation` | 400 | malformed body, bad id, unknown provider, rejected value |
  | `unauthorized` | 401 | no/expired session, invalid credentials, redeemed OAuth link |
  | `forbidden` | 403 | throttled login, captcha refused, account not on the allow-list |
  | `not_found` | 404 | unknown id, unknown endpoint under `/api` |
  | `busy` | 409 | a run for this job is already in flight |
  | `reconnect` | 412 | the provider token cannot be used any more (reconnect the account) |
  | `consent_required` | 412 | the provider permissions were refused or the tenant-wide admin consent is missing: the setup guide is the fix, not a retry |
  | `not_configured` | 424 | missing provider client, or a build without the SPA |
  | `delivery_failed` | 502 | the notification target refused the test message |
  | `internal` | 500 | unexpected error (details only in the log) |
  | — | 504 | a provider call exceeded its deadline |

* **IDs** — path parameters are unsigned integers; `0` or a non-numeric value is
  a `400`. Optional numeric query parameters (`job_id`) are validated the same
  way, optional integers (`days`, `limit`) fall back to their default instead.
* **Lists are bounded** — `limit` is clamped (`runs` 50/200, run items 500/1000,
  items 100/1000); `GET /api/runs` supports `job_id` and `status` filters, the
  status filter applies to the returned page.
* **Secrets** — no endpoint ever returns a token, a client secret, a channel
  secret (masked as `********`) or a SHA/HMAC. `/api/settings/raw` hides every
  `provider.*` key for the same reason.

## 2. Public endpoints

| Method | Path | Answer |
|---|---|---|
| `GET` | `/api/health` | `{status, database, version, auth_mode, running_jobs}`; `503` with `status: degraded` when the database ping fails |
| `GET` | `/api/version` | `{info:{name,version,commit,go_version}, auth_mode, providers[], locales[], themes[]}` |
| `GET` | `/api/auth/session` | session view: `{mode, authenticated, open, captcha:{enabled,site_key}, keycloak:{enabled}, operator, defaults, version}` |
| `POST` | `/api/auth/login` | `{login, password, captcha_token?, redirect_to?}` → session view + `Set-Cookie: sync_session=…` |
| `POST` | `/api/auth/logout` | `204`, expires the cookie |
| `GET` | `/api/auth/keycloak?redirect_to=` | `{url, state}` — where the SPA must navigate |
| `GET` | `/api/auth/callback?code=&state=` | `302 /` (success) or `302 /login?auth_error=…&auth_detail=…` |
| `GET` | `/api/oauth/:provider/callback?code=&state=` | `302 /accounts?connected=<provider>&detail=…` — or `302 /accounts?connect_error=<code>&provider=<provider>&detail=…`; a tenant-wide consent comes back here too (`admin_consent=True` → `302 /admin/guide/<provider>?consent=granted`) |

## 3. Sessions and settings

| Method | Path | Payload / answer |
|---|---|---|
| `GET` | `/api/stats?days=30` | `{days, since, stats:{accounts,jobs,enabled_jobs,runs,succeeded,partial,failed,cancelled,conflicts,files_created,files_updated,files_deleted,bytes_transferred}, running_jobs, running_ids}` — `days=0` means *since the beginning* |
| `GET` | `/api/settings` | `{settings:{default_locale,default_theme,sync_default_interval_minutes,sync_run_timeout_minutes}, locales[], themes[]}` |
| `PUT` | `/api/settings` | same `settings` object as body (`locale` ∈ the 7 codes, `theme` ∈ light/dark/system, interval `0–10080`, timeout `1–1440`) → `{settings}` |
| `GET` | `/api/settings/raw` | `{values:{key:value}}` — every stored key except `provider.*` |

## 4. Providers

| Method | Path | Payload / answer |
|---|---|---|
| `GET` | `/api/providers` | `{providers:[ProviderCredentialsInfo], redirect_hint:{google,microsoft,note}}` |
| `GET` | `/api/providers/:provider` | one `ProviderCredentialsInfo` |
| `GET` | `/api/providers/:provider/guide` | the guided app registration: `{provider, configured, redirect_uri, console_urls, permissions[], scopes[], capabilities[], admin_consent_required, admin_consent, steps[], warnings[]}` (see [app-registration.md](app-registration.md)) |
| `PUT` | `/api/providers/:provider` | `{client_id, client_secret, redirect_uri, tenant_id}` (empty `client_secret` keeps the stored one) → refreshed info |
| `DELETE` | `/api/providers/:provider` | clears the override → refreshed info (the recorded `admin_consent` of the app registration is kept) |

`ProviderCredentialsInfo` = `{provider, client_id, secret_set, redirect_uri,
 tenant_id?, source: environment|database|none, configured,
 admin_consent?: {tenant, client_id, at, granted}}` — `admin_consent` is present
once a tenant-wide consent was recorded (Microsoft) and `granted` reflects the
client id configured right now.

## 5. Accounts

| Method | Path | Payload / answer |
|---|---|---|
| `GET` | `/api/oauth` | `{providers:[ProviderCredentialsInfo]}` — what the Connect screen lists |
| `POST` | `/api/oauth/:provider/start` | `{redirect_to?}` → `{url, state}` |
| `POST` | `/api/oauth/:provider/admin-consent` | `{redirect_to?}` → `{url, state}` — the tenant-wide consent URL (Microsoft only, `400` elsewhere) |
| `GET` | `/api/accounts` | `{accounts:[accountView]}` (tokens never included) |

`accountView` carries the derived permissions of the stored grant:
`capabilities[]` (satisfied by `scopes`), `missing_capabilities[]` (requested by
this release, not granted yet) and `needs_reconnect` (true when the second list is
not empty).
| `POST` | `/api/accounts/:id/verify` | validates the token against the provider → updated `accountView` |
| `DELETE` | `/api/accounts/:id` | `{id, deleted_jobs}` — the jobs using the account are deleted too |
| `GET` | `/api/accounts/:id/drives` | `{drives:[{id,name,kind,owner}]}` |
| `GET` | `/api/accounts/:id/drives/:drive/items?folder_id=` | `{drive_id, folder_id, items:[{id,name,is_dir,size,modified_at,mime_type,hash,unsupported}]}` — folders first, then files, both alphabetical; `folder_id` defaults to the drive root |
| `GET` | `/api/accounts/:id/sites?q=` | `{sites:[{id,name,kind,owner}]}` — SharePoint libraries (empty list for providers without the capability) |

## 6. Jobs

`jobView` = every `sync_jobs` column (`id, name, source_account_id,
destination_account_id, source_drive_id, source_folder_id, source_folder_path,
destination_drive_id, destination_folder_id, destination_folder_path, direction,
conflict_policy, delete_missing, interval_minutes, enabled, last_run_at,
next_run_at, last_status, last_error, created_at, updated_at`) plus
`exclude_patterns: string[]` and `running: boolean`.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/jobs` | `{jobs:[jobView]}` |
| `POST` | `/api/jobs` | body below → **201** `jobView` |
| `GET` | `/api/jobs/:id` | `jobView` |
| `PUT` | `/api/jobs/:id` | replaces the job as a whole (the editor always sends the full form), recomputes `next_run_at` |
| `DELETE` | `/api/jobs/:id` | cancels a run in flight → `{id, cancelled}` |
| `POST` | `/api/jobs/:id/run` | **202** `{job_id, run}` — `409 busy` while another run of the job is in flight |
| `POST` | `/api/jobs/:id/cancel` | `{job_id, cancelled}` — idempotent |
| `GET` | `/api/jobs/:id/schedule` | `{next_run_at, scheduled, preview:[5 timestamps]}` |

Create/update payload:

```json
{
  "name": "Nightly Google → OneDrive",
  "source_account_id": 1,
  "destination_account_id": 2,
  "source_drive_id": "root",
  "source_folder_id": "1AbCdEf",
  "source_folder_path": "My Drive / Photos",
  "destination_drive_id": "b!xyz",
  "destination_folder_id": "01ABCD",
  "destination_folder_path": "OneDrive / Backup",
  "direction": "google_to_microsoft",
  "conflict_policy": "newest_wins",
  "exclude_patterns": ["*.tmp", "cache/**"],
  "delete_missing": false,
  "interval_minutes": 1440,
  "enabled": true
}
```

Validation performed by the service: name required, both accounts must exist and
differ, folders must be selected, source and destination must differ,
`direction` ∈ `google_to_microsoft|microsoft_to_google|bidirectional`,
`conflict_policy` ∈ `newest_wins|source_wins|destination_wins|skip`,
`interval_minutes` `0–10080`, every glob must compile (`path.Match`), blanks and
duplicates are dropped.

## 7. Runs

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/runs?job_id=&status=&limit=` | `{runs:[runView]}` (`limit` default 50, max 200) |
| `GET` | `/api/runs/:id` | `{run, items:[runItemView]}` (max 1000 items) |
| `GET` | `/api/runs/:id/items?limit=` | `{items, limit}` (default 100, max 1000) |

`runView` = the `sync_runs` row plus `running`; `runItemView` =
`{id, action, path, size, evidence?, created_at}`.

## 8. Notifications

| Method | Path | Payload / answer |
|---|---|---|
| `GET` | `/api/notifications` | `{channels:[channel], kinds:[{kind,label,fields:[{key,label,type,required?,secret?,hint?,options?,default?}]}], events:[…]}` |
| `POST` | `/api/notifications` | body below → **201** `channel` |
| `GET` | `/api/notifications/:id` | `channel` |
| `PUT` | `/api/notifications/:id` | body below → `channel` |
| `PUT` | `/api/notifications/:id/enabled` | `{"enabled": true}` → `channel` |
| `POST` | `/api/notifications/:id/test` | delivers a test message → `{id, delivered: true}` or `502 delivery_failed` |
| `DELETE` | `/api/notifications/:id` | `204 No Content` |

```json
{
  "name": "Ops e-mail",
  "type": "smtp",
  "config": { "host": "smtp.example.com", "port": 587, "from": "sync@example.com",
              "to": "ops@example.com", "password": "********" },
  "events": ["sync.success", "sync.run_failed"],
  "enabled": true
}
```

Field schemas, secrets and delivery semantics: [notifications.md](notifications.md).

## 9. Maintenance

| Method | Path | Payload / answer |
|---|---|---|
| `POST` | `/api/maintenance/schedule/run` | `{started}` — one scheduling pass right now |
| `POST` | `/api/maintenance/tokens/refresh` | `{refreshed}` |
| `POST` | `/api/maintenance/oauth/states/prune` | `{removed}` |
| `POST` | `/api/maintenance/runs/prune` | `{keep}` (default 200, 1…10000) → `{keep, pruned:true}` |

## 10. Worked examples

```bash
# health (no session needed)
curl -s https://sync.example.com/api/health

# sign in and keep the cookie
curl -s -c jar.txt -H 'Content-Type: application/json' \
  -d '{"login":"admin@example.com","password":"secret"}' \
  https://sync.example.com/api/auth/login

# dashboard numbers of the last week
curl -s -b jar.txt 'https://sync.example.com/api/stats?days=7'

# create a job and run it
curl -s -b jar.txt -H 'Content-Type: application/json' -d @job.json \
  https://sync.example.com/api/jobs
curl -s -b jar.txt -X POST https://sync.example.com/api/jobs/1/run

# what happened to one path
curl -s -b jar.txt 'https://sync.example.com/api/runs/42/items?limit=200'
```

## 11. Contract with the SPA

The SPA consumes exactly these shapes through `web/src/lib/api.ts`: typed
wrappers per endpoint, `codeOf(error)`/`messageOf(error)` to read `code`/`error`
from a failure. If an endpoint changes, change both sides and this file — the
catalog parity check of [i18n.md](i18n.md) does not cover payload names.
