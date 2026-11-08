# Admin operations tab

## Context

The Sync SPA is an operations console for Google Workspace and Microsoft 365, but
its tabs only covered *file* work (jobs, runs, reports, audit logs). The reference
product this instance replaces (`saasadmin`) also offered the administrative
operations an operator runs when someone joins, leaves or changes role: move a
Drive into a Shared Drive, archive a departing user's address into a group, copy a
mailbox, transfer calendars, report folder sizes.

Those are all reachable through the provider APIs Sync already speaks (Drive v3,
Admin SDK Directory, Gmail, Calendar v3, Microsoft Graph). What was missing was a
first-class, auditable task model: an operation had to be a row with a request, a
journal of every step and a final summary, not a one-shot endpoint whose result is
lost the moment the response is sent.

This plan adds a top-level **Admin operations** tab that runs those operations as
real, provider-backed, auditable background tasks.

## Goal

- A new top-level tab ("Admin operations") with two screens: the operation
  catalogue of an account, and the auditable history of every task.
- Six Google operations and four Microsoft operations, each a real provider call
  behind an optional provider interface (so a provider that cannot express one is
  never offered it).
- Every task is a `management_tasks` row with an append-only
  `management_task_steps` journal, counters, a status and a CSV export — the same
  async pattern as `DirectoryReport`.
- New scopes are requested up front: Google `gmail.modify`, `calendar`,
  `gmail.settings.sharing`; Microsoft `Mail.ReadWrite`, `Calendars.ReadWrite`,
  `Sites.Manage.All`. Four new capabilities (`mail`, `calendars`, `delegation`,
  `sitesadmin`) drive the reconnect hints and the guide.
- i18n parity across the seven shipped catalogs, and a green `check-i18n`.

## Steps

### Phase 0 — Vocabulary and spine

1. `internal/models/management.go`: `ManagementTask`, `ManagementTaskStep`, the
   kind constants, the step outcomes. Migrated by `database.Migrate`.
2. `internal/providers/management.go`: the shared step/progress/result types and
   the optional interfaces `SharedDriveManager`, `DirectoryArchiver`,
   `MailboxDelegator`, `MailboxCopier`, `CalendarTransferrer`,
   `FolderSizeReporter`, `SiteContentCopier`.
3. `internal/services/management.go` + `management_store.go`: the `ManagementService`
   (reserve → resolve → background run → journal → finish → prune), the operation
   catalogue, the capability negotiation and the store methods.
4. `Scheduler` closes interrupted tasks at boot; `main.go` wires the service and
   `Wait()`s it on shutdown.

### Phase 1 — Provider implementations

- **Google** (`internal/providers/google/management.go`): shared drives (list,
  create, move top-level items with their subtree), user→group archive (rename
  mailbox, create group, add member), Gmail mailbox copy (raw message replay),
  calendar ownership transfer (owner ACL), Gmail delegate add/remove/list.
- **Microsoft** (`internal/providers/microsoft/management.go`): mailbox copy
  (read + create in the target), OneDrive→SharePoint (optional site provisioning,
  cross-drive copy), calendar transfer (recreate in the target, delete the
  source), recursive folder-size report.

### Phase 2 — HTTP surface

`GET /api/management/operations?account_id=`, `GET/POST /api/management/tasks`,
`GET /api/management/tasks/:id`, `POST /api/management/tasks/:id/cancel`,
`GET /api/management/tasks/:id/export`. Destructive operations require an explicit
`confirm` parameter (validated server-side, not only in the UI).

### Phase 3 — Frontend

`ManagementLayout` with Operations and Activity tabs; the catalogue renders one
card per operation (danger badge, capability, reconnect hint when the grant is
missing), a start dialog built from the operation's parameter schema, and a
history table with a detail dialog, cancel and CSV export.

### Phase 4 — i18n and documentation

Four capability labels, the `nav.management` entry and the `management.*`
namespace in all seven catalogs; `docs/management.md`; this plan.

### Phase 5 — Tests

Catalogue invariants, start validation, status mapping, the empty history and the
required query parameter are covered by Go tests; the SPA is typechecked and
built.

## Risks

- **Reconnect churn.** New scopes mean an existing connection reports `mail`,
  `calendars`, `delegation` and `sitesadmin` as missing until it is reconnected.
  The Accounts screen already surfaces this as an actionable "reconnect" hint; the
  catalogue additionally disables an operation whose grant does not cover it.
- **Google mailbox copy needs domain-wide delegation.** A delegated token can only
  act as its own user; copying *another* mailbox requires service-account DWD. The
  code issues the correct Gmail calls and surfaces Google's 401/403 as the task
  error, and `docs/management.md` documents the DWD prerequisite.
- **Destructive operations.** Moving a Drive, renaming a mailbox, transferring
  ownership and deleting a source event are irreversible from Sync's side. Every
  one is marked `danger`, requires `confirm`, and is fully journalled.
- **Throttling.** A large mailbox copy or folder walk is slow. Each task has a
  30-minute budget, a bounded item count and ends as `timeout` instead of holding
  the account slot forever.
- **Microsoft cross-drive copy is asynchronous.** Graph queues the copy and this
  task reports the items as queued; it does not poll Graph's monitor URL to
  completion (documented).
