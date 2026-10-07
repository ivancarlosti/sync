# Database

Sync stores everything in an **external** MySQL 8 / MariaDB 10.5+ database. The
schema is owned by GORM `AutoMigrate`, which runs on every boot; there is no SQL
file to apply by hand and no migration tool to run.

* Engine: InnoDB, `utf8mb4` (emoji and every catalogue script), `parseTime=True`,
  `loc=UTC` — every timestamp is written and compared in UTC.
* IDs: auto-increment `BIGINT UNSIGNED` (GORM `uint`).
* JSON-ish blobs (`notification_channels.config`, `sync_jobs.exclude_patterns`,
  `notification_channels.events`) live in `TEXT` columns and are marshalled by
  `internal/services`: the schema stays portable and the rows stay readable.
* Secrets are AES-256-GCM ciphertexts (`v1:…`). A plaintext secret in a secret
  column is treated as invalid by `crypto.Decrypt`, so a mistake is loud.

## 1. Settings

`settings` — the key/value store behind **Admin > Settings** and the provider
credential overrides.

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGINT UNSIGNED` | primary key |
| `key` | `VARCHAR(191)` | **unique**; `key` is quoted in every query (reserved word) |
| `value` | `TEXT` | free form; `INT`/`BOOL` helpers parse it |
| `created_at`, `updated_at` | `DATETIME` | UTC |

Seeded keys (`internal/services/settings.go`, `Defaults`):

| Key | Seeded from | Meaning |
|---|---|---|
| `default_locale` | `DEFAULT_LOCALE` | language for a visitor without a stored choice |
| `default_theme` | `DEFAULT_THEME` | theme for a visitor without a stored choice |
| `sync_default_interval_minutes` | `60` | schedule proposed for a new job |
| `sync_run_timeout_minutes` | `30` | hard limit of one run |

Provider overrides use the prefix `provider.` (e.g. `provider.google.client_id`,
`provider.google.client_secret` — the latter encrypted). `GET /api/settings/raw`
never returns that prefix.

## 2. Connected accounts and OAuth state

`connected_accounts` — one row per authorisation. A reconnect of the same remote
account rotates the row (`uniqueIndex idx_account_identity` on
`provider` + `provider_account_id`) instead of inserting a second one.

| Column | Type | JSON | Notes |
|---|---|---|---|
| `id` | `BIGINT UNSIGNED` | `id` | |
| `provider` | `VARCHAR(32)` | `provider` | `google` \| `microsoft` |
| `provider_account_id` | `VARCHAR(191)` | `provider_account_id` | part of the unique index |
| `email`, `display_name` | `VARCHAR(191)` | same names | `email` is indexed |
| `avatar_url` | `VARCHAR(512)` | `avatar_url` | |
| `access_token`, `refresh_token` | `TEXT` | **never serialised** (`json:"-"`) | AES-GCM `v1:` ciphertext |
| `token_type` | `VARCHAR(32)` | **never serialised** | |
| `scopes` | `TEXT` | `scopes` | space separated, exposed as an array |
| `expires_at` | `DATETIME` | `expires_at` | refreshed 2 min early on every call |
| `status` | `VARCHAR(32)` | `status` | `connected` \| `error` |
| `last_error` | `TEXT` | `last_error` | set after a failed refresh |
| `refreshed_at`, `last_synced_at` | `DATETIME` (nullable) | same | shown in the accounts table |

`oauth_states` — the server side of an in-flight authorization code flow: `state`
(unique, HMAC-signed), `flow` (`oauth` \| `keycloak`), `provider`, `code_verifier`
(PKCE, never serialised), `redirect_to` and `expires_at`. Rows live 10 minutes
(`stateTTL`); they are pruned at boot and by
`POST /api/maintenance/oauth/states/prune`.

## 3. Sync jobs, runs, items and files

`sync_jobs` — a durable source → destination definition.

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGINT UNSIGNED` | |
| `name` | `VARCHAR(191)` | unique by convention, not by index |
| `source_account_id`, `destination_account_id` | `BIGINT UNSIGNED` | indexed; no FK constraint (portability) |
| `source_drive_id`, `source_folder_id`, `destination_drive_id`, `destination_folder_id` | `VARCHAR(191)` | provider item ids |
| `source_folder_path`, `destination_folder_path` | `VARCHAR(1024)` | human readable, rendered by the UI without a provider call |
| `direction` | `VARCHAR(32)` | `google_to_microsoft` \| `microsoft_to_google` \| `bidirectional` |
| `conflict_policy` | `VARCHAR(32)` | `newest_wins` \| `source_wins` \| `destination_wins` \| `skip` |
| `exclude_patterns` | `TEXT` | JSON array of globs, **not** serialised raw (the API returns the decoded array) |
| `delete_missing` | `BOOLEAN` | propagate deletions instead of re-copying |
| `interval_minutes` | `INT` | `0` = manual only, capped at `10080` (7 days) |
| `enabled` | `BOOLEAN` | default `true` |
| `last_run_at`, `next_run_at` | `DATETIME` (nullable) | next run recomputed on every save |
| `last_status`, `last_error` | `VARCHAR(32)` / `TEXT` | summary of the last run |

`sync_runs` — one row per execution (the audit trail and the source of the
dashboard numbers). Counters are `files_scanned`, `files_created`,
`files_updated`, `files_deleted`, `files_skipped`, `folders_created`,
`conflicts`, `errors`, `bytes_transferred`; `status` is one of `running`,
`success`, `partial`, `failed`, `cancelled`, `timeout` (the run hit its time
budget, typically while a provider throttled it), `trigger` is `manual` or
`scheduled`, and `duration_ms` is computed from `started_at`/`finished_at`.

`sync_items` — the per-file detail of a run: `run_id` + `job_id` (both indexed),
`action` (`created`, `updated`, `deleted`, `skipped`, `conflict`, `failed`,
`folder_created`, `unsupported`, `renamed`, `in_sync`), `path`, `size` and the
`evidence` string that explains the decision.

`sync_files` — **the engine state**: one row per path already in sync
(`uniqueIndex idx_syncfile_job_path` on `job_id` + `path`) with the provider item
ids of both sides, size, provider hash, both modification times, `last_synced_at`,
`last_direction` and `status`. It is what makes deletions, renames and conflicts
detectable without hashing the whole tree on every run.

`notification_channels` — see [notifications.md](notifications.md) for the
`config` documents; the table itself keeps `name`, `type` (`smtp` \| `webhook` \|
`shoutrrr`), `config` (JSON, secrets encrypted), `events` (JSON array),
`enabled`, `last_status`, `last_error`, `last_used_at`.

## 4. Relationships

```text
connected_accounts ──1:N──► sync_jobs (source_account_id / destination_account_id)
sync_jobs ──1:N──► sync_runs ──1:N──► sync_items
sync_jobs ──1:N──► sync_files          (state, one row per path)
notification_channels ──N:M──► events (JSON array inside the row, no join table)
settings                              (independent key/value store)
oauth_states ──► ephemeral (deleted after 10 minutes or on redemption)
```

There are deliberately **no foreign key constraints**
(`DisableForeignKeyConstraintWhenMigrating`): deleting an account cascades in the
service layer, where the jobs that use it are removed together with it (that is
what the accounts screen warns about before confirming).

## 5. Upgrade path

1. Stop the old container (or let the orchestrator replace it).
2. Start the new image. `AutoMigrate` adds new tables/columns and indexes;
   existing data is preserved, and `Seed` never overwrites an edited setting.
3. If a release changes the meaning of a stored value, the release notes say so:
   the schema is additive, historic rows are never rewritten silently.

Downgrades are not supported: a new column added by an upgrade simply stays
unused by an older binary, but a value written by a new release may be unknown to
it (an unknown `status` renders as-is in the UI).

## 6. Retention and housekeeping

| Data | Bound | Trigger |
|---|---|---|
| `sync_runs` + `sync_items` | `runKeepCount = 50` newest runs per job | `Store.PruneRuns` — called on demand through `POST /api/maintenance/runs/prune` (default `keep: 200`, allowed 1…10000) |
| `oauth_states` | 10 minutes | pruned at boot and through `POST /api/maintenance/oauth/states/prune` |
| `sync_files` | one row per path, updated in place | nothing to prune; deleting a job deletes its state |

## 7. Backup and restore

A logical dump is enough; there is no on-disk state outside this database.

```bash
mysqldump --single-transaction --routines --triggers \
  -h 127.0.0.1 -u sync -p sync > sync-$(date +%F).sql
# restore
mysql -h 127.0.0.1 -u sync -p sync < sync-2026-09-28.sql
```

Keep `ENCRYPTION_KEY` safe and **together** with the dump: without it the tokens
and notification secrets in it cannot be decrypted (everything else stays usable,
but the accounts must be reconnected and the channel secrets re-entered).

## 8. Queries an operator actually needs

```sql
-- what is scheduled next
SELECT id, name, interval_minutes, enabled, next_run_at FROM sync_jobs ORDER BY next_run_at;

-- the last twenty runs with their counters
SELECT id, job_name, status, trigger, started_at, duration_ms,
       files_created, files_updated, files_deleted, conflicts, errors
FROM sync_runs ORDER BY id DESC LIMIT 20;

-- why a specific path behaved the way it did
SELECT r.id, r.started_at, i.action, i.path, i.evidence
FROM sync_items i JOIN sync_runs r ON r.id = i.run_id
WHERE i.path LIKE 'docs/%' ORDER BY i.id DESC LIMIT 50;

-- accounts whose token died
SELECT id, provider, email, status, last_error FROM connected_accounts WHERE status <> 'connected';

-- prove that no plaintext token reached the database (must return 0 rows)
SELECT COUNT(*) FROM connected_accounts
WHERE (access_token <> '' AND access_token NOT LIKE 'v1:%')
   OR (refresh_token <> '' AND refresh_token NOT LIKE 'v1:%');

-- every editable setting
SELECT `key`, value FROM settings WHERE `key` NOT LIKE 'provider.%' ORDER BY `key`;
```

## 9. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `database: ping failed` on boot | host/port/credentials or the DB is not up | verify `DB_*`; in Docker remember `host.docker.internal` needs the `extra_hosts` mapping |
| `Error 1114 table … is full` | disk full on the database host | prune runs (`POST /api/maintenance/runs/prune`) or extend the volume |
| `Error 1071 Specified key was too long` | MariaDB/MySQL with the old `utf8` charset | create the database with `CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci` |
| runs stay `running` forever | the process was killed mid-run | restart once: the bootstrap closes interrupted runs (`Scheduler.Bootstrap`) |
| `access_token` looks like plaintext | a row was edited by hand | delete the row and reconnect the account; `crypto.Decrypt` refuses non-`v1:` values |
