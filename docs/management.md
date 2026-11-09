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
drive instead of typing an address a typo can turn into a new object. Mailbox
delegates get a live list with add/remove and a refresh after every change.

## Operations

| Kind | Provider | Capability | Destructive | Parameters |
| --- | --- | --- | --- | --- |
| `google_drive_to_new_shared_drive` | Google | `files` | yes | `name`*, `source_drive_id`, `confirm`* |
| `google_drive_to_shared_drive` | Google | `files` | yes | `target_drive_id`*, `source_drive_id`, `target_folder_id`, `target_folder_name`, `confirm`* |
| `google_archive_user_group` | Google | `groups` | yes | `user_email`*, `group_email`*, `group_name`, `member_email`, `archive_email`, `confirm`* |
| `google_copy_mailbox` | Google | `mail` | no | `source`, `target`*, `max` |
| `google_transfer_calendars` | Google | `calendars` | yes | `source`, `target`*, `confirm`* |
| `google_manage_delegate` | Google | `delegation` | conditional | `mailbox`, `delegate`, `action`* (`add`/`remove`/`list`), `confirm`* |
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
- **Copy a mailbox** — reads each message of the source mailbox and recreates it
  in the target (Google replays the raw RFC 822 message; Microsoft re-creates the
  subject/body/recipients). Bounded by `max` (default 200, hard ceiling 5000).
- **Transfer calendars** — Google inserts an owner ACL entry for the target on
  every calendar of the source (events stay on the calendar); Microsoft recreates
  each event in the target mailbox and deletes the source copy.
- **Manage delegates** — lists, adds or removes a Gmail delegate of a mailbox.
  The page keeps the current delegates on screen and refreshes them after every
  change. Gmail restricts `users.settings.delegates` to a **service account with
  domain-wide delegation**; a normal OAuth token answers `403 Access restricted
  to service accounts that have been delegated domain-wide authority`, even for
  the administrator's own mailbox. Configure the delegation grant described
  below, mount the key and set `GOOGLE_SERVICE_ACCOUNT_FILE`; the form then
  impersonates the selected mailbox for the Gmail call.
- **Report folder sizes** — walks a Microsoft drive depth-first and reports the
  recursive size and file count of every folder. Bounded in depth and item count.

## Scopes and prerequisites

Two new capability groups were added to the permission table of each provider;
the setup guide renders them and the Accounts screen derives the reconnect hints
from the stored grant.

| Provider | Capability | Scope |
| --- | --- | --- |
| Google | `mail` | `https://www.googleapis.com/auth/gmail.modify` |
| Google | `calendars` | `https://www.googleapis.com/auth/calendar` |
| Google | `delegation` | `https://www.googleapis.com/auth/gmail.settings.sharing` |
| Microsoft | `mail` | `Mail.ReadWrite` |
| Microsoft | `calendars` | `Calendars.ReadWrite` |
| Microsoft | `sitesadmin` | `Sites.Manage.All` |

An account connected before these scopes existed reports them as missing until it
is reconnected; the catalogue then disables the operation that needs them and
offers the reconnect hint instead of a start button.

### Google mailbox copy and domain-wide delegation

A Google OAuth **delegated** token can only act as its own user. Reading another
user's mailbox (`users/{other}/messages`) requires **domain-wide delegation**: a
service account, authorised in the Workspace admin console for the OAuth client's
client id, impersonating the target user. Without it Google answers `401/403` and
the task fails with that message. The copy code issues the correct Gmail calls and
works today for the connected account's own mailbox; cross-user copy is a
deployment decision documented here rather than a hidden behaviour.

### Google Gmail delegates (domain-wide delegation)

Gmail delegate management is the one operation that cannot use the OAuth
connection: Google requires a **service account with domain-wide delegation**.
The setup is one-time per Google Workspace domain:

1. In the Google Cloud project that owns the OAuth client (the project shown in
   the Gmail API error), create a **service account** and a JSON key.
2. Enable **domain-wide delegation** on that service account and copy its
   numeric **client ID**.
3. In the Google Workspace Admin console go to **Security → Access and data
   control → API controls → Manage Domain Wide Delegation** and add the client
   ID with the scope:
   ```text
   https://www.googleapis.com/auth/gmail.settings.sharing
   ```
4. Put the JSON key on the server, mount it into the container and set:
   ```text
   GOOGLE_SERVICE_ACCOUNT_FILE=/app/google-service-account.json
   ```
   For the compose file that is a read-only volume:
   ```yaml
   volumes:
     - ./google-service-account.json:/app/google-service-account.json:ro
   ```
5. Restart Sync. The delegate form now impersonates the selected mailbox for
   the Gmail settings call; `gmail.settings.sharing` must also remain in the
   connected account's OAuth grant for the capability to be offered.

The key is a credential with powerful delegation. Keep it out of the repository
and the image, mount it read-only, and restrict the DWD grant to the one scope
above. The app never logs or returns the key.

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
| `GET` | `/api/management/delegates?account_id=&mailbox=` | The current delegates of one mailbox. |
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

