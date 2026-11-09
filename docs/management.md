# Admin operations

Admin operations are the provider-backed administrative tasks an operator runs
against a connected account: moving a Drive into a Shared Drive, archiving a
departing user's address into a group, copying a mailbox, transferring calendars,
reporting folder sizes. Each one runs in the background as an **auditable task**
(a `management_tasks` row plus an append-only journal of steps), exactly like a
directory report or an audit run.

The screen lives at **Admin operations** in the top navigation, with two tabs:

- **Operations** — the catalogue of what a selected account can run right now.
  Each entry opens a full **operation page** (not a small dialog) with the
  account selector, the operation's fields, live users/groups/drives and the
  explanation of what each value means.
- **Activity** — the history of every task with its journal, cancel and CSV
  export.

The operation page reads its selects from the account itself:
`/api/management/options` lists the users and groups of the directory and the
drives the account can see, so an operator picks an existing mailbox, group or
drive instead of typing an address a typo can turn into a new object.

## Operations

| Kind | Provider | Capability | Destructive | Parameters |
| --- | --- | --- | --- | --- |
| `google_drive_to_new_shared_drive` | Google | `files` | yes | `name`*, `source_drive_id`, `confirm`* |
| `google_drive_to_shared_drive` | Google | `files` | yes | `target_drive_id`*, `source_drive_id`, `target_folder_id`, `target_folder_name`, `confirm`* |
| `google_archive_user_group` | Google | `groups` | yes | `user_email`*, `group_email`*, `group_name`, `member_email`, `archive_email`, `confirm`* |
| `microsoft_copy_mailbox` | Microsoft | `mail` | no | `source`, `target`*, `max` |
| `microsoft_onedrive_to_site` | Microsoft | `sitesadmin` | yes | `site_name`, `site_display_name`, `target_drive_id`, `target_folder_id`, `confirm`* |
| `microsoft_transfer_calendars` | Microsoft | `calendars` | yes | `source`, `target`*, `confirm`* |
| `microsoft_folder_sizes` | Microsoft | `files` | no | `drive_id` |

`*` marks a required value. A destructive operation is refused unless `confirm`
is `true` (the request is validated server-side; the UI checkbox is convenience,
not the guard).

`source` defaults to the connected account's own mailbox/drive. `target` is the
mailbox, shared drive or account that receives the content.

## What each operation does

- **Drive → new / existing Shared Drive** — enumerates the top level of the
  source drive (My Drive or a Shared Drive) and re-parents every item into the
  target shared drive. A folder moves with its whole subtree, so one pass moves
  everything; the source items leave the source drive. On an existing target the
  form can select a folder inside it (`target_folder_id`) or create a new
  subfolder (`target_folder_name`) and move the items there.
- **Archive a user into a group** — `user_email` is the departing user;
  `archive_email` is the optional new address that frees the old one;
  `group_email` is the destination group (pick an existing one or type a new
  address); `group_name` is the display name used only when the group is
  created; `member_email` is the successor added to the group. A group or
  membership that already exists is a `skipped` step, so a re-run converges.
- **Copy a mailbox (Microsoft)** — reads each message of the source mailbox and
  recreates it in the target with the delegated Graph grant. Bounded by `max`
  (default 200, hard ceiling 5000). The Google variant is not offered; see below.
- **Transfer calendars (Microsoft)** — recreates each event in the target mailbox
  and deletes the source copy, with the delegated Graph grant.
- **Report folder sizes** — walks a Microsoft drive depth-first and reports the
  recursive size and file count of every folder. Bounded in depth and item count.

## Scopes and prerequisites

The Microsoft operations need these delegated Graph permissions; the setup guide
renders them and the Accounts screen derives the reconnect hints from the stored
grant. The surviving Google operations use the scopes the connection already
holds (Drive, directory users and groups) and need no extra grant.

| Provider | Capability | Scope |
| --- | --- | --- |
| Microsoft | `mail` | `Mail.ReadWrite` |
| Microsoft | `calendars` | `Calendars.ReadWrite` |
| Microsoft | `sitesadmin` | `Sites.Manage.All` |

An account connected before these scopes existed reports them as missing until it
is reconnected; the catalogue then disables the operation that needs them and
offers the reconnect hint instead of a start button.

### Google operations that are not offered

Three Google operations are deliberately absent from the catalogue because Google
refuses the cross-mailbox calls to a normal OAuth token:

- **Copy a mailbox** — reading or writing another user's mailbox needs
  domain-wide delegation.
- **Transfer calendars** — handing another user's calendars to a successor needs
  domain-wide delegation.
- **Manage Gmail delegates** — `users.settings.delegates` is restricted to a
  service account with domain-wide delegation, even for the administrator's own
  mailbox.

Domain-wide delegation is a service-account arrangement authorised separately in
each Workspace domain. Rather than make every connected domain configure it, the
screen offers only the operations the OAuth connection can complete. Use GAM or a
dedicated delegated service account for these three tasks. The surviving Google
operations (Drive moves and the user → group archive) use the normal delegated
grant and need no delegation setup.

### Microsoft consent

`Mail.ReadWrite`, `Calendars.ReadWrite` and `Sites.Manage.All` are delegated Graph
permissions that need the tenant-wide **admin consent** (see
`docs/app-registration.md`), and they are only applicable to a work or school
account. The acting identity still needs the matching Entra role; the grant is
what the application may do, not what the signed-in user is allowed to do.

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/management/operations?account_id=` | The catalogue of an account, decorated with `supported` / `missing_capability`. |
| `GET` | `/api/management/options?account_id=&kind=` | The users, groups and drives the form of one operation offers. |
| `GET` | `/api/management/tasks?account_id=&limit=` | The task history (all accounts when `account_id` is absent). |
| `POST` | `/api/management/tasks` | Start a task: `{account_id, kind, params}`. `202` with the row; `409` while another task of the same account runs. |
| `GET` | `/api/management/tasks/:id` | One task with its journal. |
| `POST` | `/api/management/tasks/:id/cancel` | Abort a running task. |
| `GET` | `/api/management/tasks/:id/export` | The journal as a CSV (UTF-8 BOM, one row per step). |

## Task lifecycle

A task is written in the `running` state before the operation starts, the journal
is appended while it works, and the summary is written once at the end. Statuses
are the shared run vocabulary: `running`, `success`, `partial` (some steps
failed), `failed`, `cancelled`, `timeout`. A task that a restart interrupts is
closed as `failed` by the scheduler at boot, so an account is never blocked by a
ghost reservation. One task per account runs at a time; the history keeps the
newest 50 tasks per account.

Each `management_task_steps` row records what happened (`success`, `failed` or
`skipped`), a readable detail and the items/bytes it moved — which is what makes
the history an audit trail rather than a log line.

