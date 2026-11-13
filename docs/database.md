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
| `billing_schema` | `none` | line-item vocabulary of the billing report (`none`/`oci`/`aws`/`gcp`/`custom`) |
| `billing_schema_custom` | `DefaultBillingSchemaDocument()` | the JSON document behind the `custom` schema |
| `billing_rate_per_gib` | `0` | price of one GiB for the cost column (0 hides it) |
| `billing_currency` | `USD` | currency of the cost column (three letter code) |

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
| `kind` | `VARCHAR(16)` | `kind`, omitted when empty | `personal` \| `work` — the family of the account as the provider reported it (`providers.AccountKind`). Empty for a provider that cannot tell, and an empty value filters no permission out, so rows written before the column existed behave exactly as they did. Never cleared by a classification that failed |
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
| `source_drive_name`, `destination_drive_name` | `VARCHAR(191)` | display name of the root, as the provider listed it |
| `source_drive_kind`, `destination_drive_kind` | `VARCHAR(32)` | `personal` \| `business` \| `shared` \| `document_library` |
| `source_drive_owner`, `destination_drive_owner` | `VARCHAR(191)` | the drive URL (what tells two SharePoint libraries of one tenant apart) |
| `source_folder_path`, `destination_folder_path` | `VARCHAR(1024)` | human readable, rendered by the UI without a provider call |
| `direction` | `VARCHAR(32)` | `google_to_microsoft` \| `microsoft_to_google` \| `bidirectional` |
| `conflict_policy` | `VARCHAR(32)` | `newest_wins` \| `source_wins` \| `destination_wins` \| `skip` |
| `exclude_patterns` | `TEXT` | JSON array of globs, **not** serialised raw (the API returns the decoded array) |
| `delete_missing` | `BOOLEAN` | propagate deletions instead of re-copying; also removes the emptied folders |
| `sync_empty_folders` | `BOOLEAN` | mirror folders that hold no file; default `true`, which is what the rows stored before the column existed inherit |
| `interval_minutes` | `INT` | `0` = manual only, capped at `10080` (7 days) |
| `enabled` | `BOOLEAN` | default `true` |
| `last_run_at`, `next_run_at` | `DATETIME` (nullable) | next run recomputed on every save |
| `last_status`, `last_error` | `VARCHAR(32)` / `TEXT` | summary of the last run |

`sync_runs` — one row per execution (the audit trail and the source of the
dashboard numbers). Counters are `files_scanned`, `files_created`,
`files_updated`, `files_deleted`, `files_skipped`, `folders_created`,
`folders_deleted`, `conflicts`, `errors`, `bytes_transferred`; `status` is one of
`running`, `success`, `partial`, `failed`, `cancelled`, `timeout` (the run hit its
time budget, typically while a provider throttled it), `trigger` is `manual` or
`scheduled`, and `duration_ms` is computed from `started_at`/`finished_at`.

`sync_items` — the per-file detail of a run: `run_id` + `job_id` (both indexed),
`action` (`created`, `updated`, `deleted`, `skipped`, `conflict`, `failed`,
`folder_created`, `folder_deleted`, `unsupported`, `renamed`, `in_sync`), `path`,
`size` and the `evidence` string that explains the decision.

`sync_files` — **the engine state**: one row per path already in sync
(`uniqueIndex idx_syncfile_job_path` on `job_id` + `path`) with the provider item
ids of both sides, size, provider hash, both modification times, `last_synced_at`,
`last_direction` and `status`. It is what makes deletions, renames and conflicts
detectable without hashing the whole tree on every run.

`sync_folders` — **the folder half of the engine state**: one row per folder both
sides listed at the end of a run (`uniqueIndex idx_syncfolder_job_path` on
`job_id` + `path`). "This side lists the folder, the other does not" means either
that it has not travelled yet or that it was deleted there, and a listing cannot
tell the two apart; these rows are the memory that separates them (see
[architecture.md](architecture.md) §3). They are written as a delta — new shared
paths only, `ON CONFLICT DO NOTHING` — and dropped for the paths neither side lists
any more.

`audit_runs` — one row per content audit (see [api.md](api.md) §9): the audited
root (`account_id`, `account_email`, `provider`, `drive_id`, `drive_name`
(`VARCHAR(191)`), `drive_kind` (`VARCHAR(32)`), `drive_owner` (`VARCHAR(191)`),
`root_folder_id`, `root_path`), `max_depth`, `status`/`trigger`/timing like
`sync_runs`, and the summary counters `files`, `folders`, `total_size`,
`max_depth_reached`, `truncated`.

`audit_entries` — the report of an audit: `audit_id` (indexed) + `kind`
(`folder` \| `file`), `path`, `name`, `depth`, `size`, `total_size`, `files`,
`folders` (the subtree totals of a folder — exact even for a folder whose
children the depth limit keeps out of the report), `expanded` (false only when the
walk could not descend into the folder), `listed` (false when the folder's
children are not part of the report), `modified_at` and `mime_type`. The rows are
batch-inserted after the walk, so their `id` order is the DFS preorder of the tree.

## 4. Calendar jobs, runs and links

Four tables mirror the file family. They are deliberately separate from
`sync_jobs` instead of sharing it: a calendar job has two calendar ids instead of
four folder ids, no `exclude_patterns`, no byte counters, and an `.ics` source
with no account at all.

`calendar_jobs` — a durable source → destination calendar pairing (see
[calendars.md](calendars.md)).

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGINT UNSIGNED` | |
| `name` | `VARCHAR(191)` | |
| `source_kind` | `VARCHAR(32)` | `google` \| `microsoft` \| `ics`, default `google` |
| `source_account_id`, `destination_account_id` | `BIGINT UNSIGNED` | indexed; `source_account_id` is `0` for an `.ics` job |
| `source_calendar_id`, `destination_calendar_id` | `VARCHAR(512)` | provider calendar ids (`primary` is accepted by Google); empty means the account's primary calendar |
| `source_calendar_name`, `destination_calendar_name` | `VARCHAR(191)` | display names as the provider listed them, so a row renders without a provider call |
| `source_url` | `VARCHAR(1024)` | the `.ics` subscription |
| `source_username` | `VARCHAR(191)` | HTTP Basic user, if any |
| `source_secret` | `TEXT` | **never serialised** (`json:"-"`); AES-GCM `v1:` payload |
| `source_etag`, `source_last_modified` | `VARCHAR(255)` / `VARCHAR(64)` | validators of the last successful fetch (the `source_etag` column name is pinned: the naming strategy would split the initialism into `source_e_tag`) |
| `source_fetched_at` | `DATETIME` (nullable) | when the current validators were obtained |
| `direction` | `VARCHAR(32)` | `bidirectional` \| `one_way` (an `.ics` job is always `one_way`) |
| `conflict_policy` | `VARCHAR(32)` | `newest_wins` \| `source_wins` \| `destination_wins` \| `skip` |
| `delete_missing` | `BOOLEAN` | propagate a deletion instead of re-creating the event |
| `past_days`, `future_days` | `INT` | listing window around *now*, default `30` / `365` |
| `interval_minutes`, `enabled` | `INT` / `BOOLEAN` | `0` = manual only, capped at `10080`; a new job is enabled |
| `last_run_at`, `next_run_at` | `DATETIME` (nullable) | `next_run_at` is recomputed on every save and moved one interval past the start of every finished run; `NULL` for a manual-only or disabled job |
| `last_status`, `last_error` | `VARCHAR(32)` / `TEXT` | summary of the last run |

`calendar_event_links` — **the engine state**: one row per event both sides know
about (`uniqueIndex idx_calendarlink_job_uid` on `job_id` + `uid`). The pairing
cannot rely on the provider event id (Google and Microsoft mint their own) nor on
the UID alone (Graph ignores an `iCalUId` on create), so the row carries
`uid` (`VARCHAR(768)`), `source_event_id` and `destination_event_id`
(`VARCHAR(768)`), both content fingerprints (`source_fingerprint`,
`destination_fingerprint`, each `VARCHAR(64)` — the destination column name is
pinned for the same reason as `source_etag`), both last known starts
(`source_start`, `destination_start`, UTC), `summary`, `last_action`,
`last_synced_at`. The fingerprints make "which side moved since the last run"
answerable without storing a copy of the event; the starts are what separate an
event that travelled out of the listing window from one that was deleted.

`calendar_runs` — one row per execution, the shape of `sync_runs` with event
counters instead of file counters: `job_id` (indexed), `job_name`, `status`
(indexed), `trigger`, `started_at`/`finished_at`/`duration_ms`, `events_scanned`,
`events_created`, `events_updated`, `events_deleted`, `events_skipped`,
`conflicts`, `errors` and a `message` for the failure the counters cannot express.

`calendar_run_items` — the per-event evidence of a run: `run_id` and `job_id`
(both indexed), `action` (indexed), `uid`, `summary` (the title at the time of
the decision), `start_at` and the `evidence` string that explains the decision.
At most `1000` rows are written per run.

## 5. Table map

```text
settings                              (independent key/value store)
oauth_states ──► ephemeral (deleted after 10 minutes or on redemption)

connected_accounts ──1:N──► sync_jobs (source_account_id, destination_account_id)
connected_accounts ──1:N──► audit_runs (account_id)
connected_accounts ──1:N──► calendar_jobs (source_account_id, destination_account_id)
connected_accounts ──1:N──► traffic_entries (account_id)

sync_jobs ──1:N──► sync_runs ──1:N──► sync_items
sync_jobs ──1:N──► sync_files   (engine state, one row per path)
sync_jobs ──1:N──► sync_folders (engine state, one row per shared folder)

calendar_jobs ──1:N──► calendar_runs ──1:N──► calendar_run_items
calendar_jobs ──1:N──► calendar_event_links (engine state, one row per UID)

audit_runs ──1:N──► audit_entries

notification_channels ──N:M──► events (JSON array inside the row, no join table)
api_tokens                            (bearer credentials of the public API)
traffic_entries ──► sync_runs.run_id  (unique per run + account + direction; a calendar
                                      run moves no bytes and writes none)
```

There are deliberately **no foreign key constraints**
(`DisableForeignKeyConstraintWhenMigrating`): deleting an account cascades in the
service layer, where the jobs that use it and that account's audits are removed
together with it (that is what the accounts screen warns about before
confirming).

## 6. Upgrade path

1. Stop the old container (or let the orchestrator replace it).
2. Start the new image. `AutoMigrate` adds new tables/columns and indexes;
   existing data is preserved, and `Seed` never overwrites an edited setting.
3. If a release changes the meaning of a stored value, the release notes say so:
   the schema is additive and historic rows are never rewritten silently.

Downgrades are not supported: a new column added by an upgrade simply stays
unused by an older binary, but a value written by a new release may be unknown to
it (an unknown `status` renders as-is in the UI).

## 7. Retention and housekeeping

| Data | Bound | Trigger |
|---|---|---|
| `sync_runs` + `sync_items` | `runKeepCount = 50` newest runs per job | `Store.PruneRuns` — called on demand through `POST /api/maintenance/runs/prune` (default `keep: 200`, allowed 1…10000) |
| `oauth_states` | 10 minutes | pruned at boot and through `POST /api/maintenance/oauth/states/prune` |
| `sync_files` | one row per path, updated in place | nothing to prune; deleting a job deletes its state |
| `sync_folders` | one row per folder both sides have, written as a delta | nothing to prune: rows of folders neither side lists any more are dropped at the end of the run; deleting a job deletes its state |
| `audit_runs` + `audit_entries` | `auditKeepCount = 50` newest audits per account | `Store.PruneAudits` — called on demand through `POST /api/maintenance/audits/prune` (default `keep: 50`, allowed 1…10000) |
| `calendar_runs` + `calendar_run_items` | newest 50 runs per job (the same `runKeepCount` bound as a file job) | pruned at the end of every calendar run; the `/api/maintenance/*` pruners do not touch them. Deleting a job keeps its history: the rows stay for the record, while the endpoints stop answering because they resolve the job first |
| `calendar_event_links` | one row per iCalendar UID, updated in place | nothing to prune; a row whose event is gone from both sides is dropped by the run (`forget_link`), and deleting a job deletes its state |

## 8. Backup and restore

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

## 9. Queries an operator actually needs

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

-- the last twenty calendar runs of one job (event counters, no bytes)
SELECT id, job_name, status, trigger, started_at, events_scanned,
       events_created, events_updated, events_deleted, events_skipped,
       conflicts, errors
FROM calendar_runs WHERE job_id = 7 ORDER BY id DESC LIMIT 20;

-- why a specific event behaved the way it did
SELECT r.id, r.started_at, i.action, i.uid, i.summary, i.evidence
FROM calendar_run_items i JOIN calendar_runs r ON r.id = i.run_id
WHERE r.job_id = 7 AND i.summary LIKE '%standup%' ORDER BY i.id DESC LIMIT 50;

-- accounts whose token died
SELECT id, provider, email, status, last_error FROM connected_accounts WHERE status <> 'connected';

-- prove that no plaintext token reached the database (must return 0 rows)
SELECT COUNT(*) FROM connected_accounts
WHERE (access_token <> '' AND access_token NOT LIKE 'v1:%')
   OR (refresh_token <> '' AND refresh_token NOT LIKE 'v1:%');

-- every editable setting
SELECT `key`, value FROM settings WHERE `key` NOT LIKE 'provider.%' ORDER BY `key`;

-- the traffic of the last 90 days, the shape the billing screen groups by
SELECT day,
       SUM(CASE WHEN direction = 'inbound'  THEN bytes ELSE 0 END) AS inbound_bytes,
       SUM(CASE WHEN direction = 'outbound' THEN bytes ELSE 0 END) AS outbound_bytes,
       SUM(operations) AS operations
FROM traffic_entries
WHERE day >= DATE_FORMAT(UTC_DATE() - INTERVAL 90 DAY, '%Y-%m-%d')
GROUP BY day ORDER BY day DESC;

-- what one run moved, and through which account
SELECT account_email, direction, bytes, files, operations
FROM traffic_entries WHERE run_id = 42 ORDER BY direction, account_id;

-- the API tokens (the hash is never a secret to read: regenerate instead)
SELECT id, name, prefix, scopes, expires_at, revoked_at, last_used_at, last_used_ip, requests
FROM api_tokens ORDER BY id DESC;
```

## 10. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `database: ping failed` on boot | host/port/credentials or the DB is not up | verify `DB_*`; in Docker remember `host.docker.internal` needs the `extra_hosts` mapping |
| `Error 1114 table … is full` | disk full on the database host | prune runs (`POST /api/maintenance/runs/prune`) or extend the volume |
| `Error 1071 Specified key was too long` | MariaDB/MySQL with the old `utf8` charset | create the database with `CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci` |
| runs stay `running` forever | the process was killed mid-run | restart once: the bootstrap closes interrupted runs (`Scheduler.Bootstrap`) |
| `access_token` looks like plaintext | a row was edited by hand | delete the row and reconnect the account; `crypto.Decrypt` refuses non-`v1:` values |
