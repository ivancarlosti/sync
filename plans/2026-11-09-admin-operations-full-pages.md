# Admin operations: full pages, live selects and a clearer archive

## Context

The Admin operations screen opened every task in a small dialog with free-text
inputs. The Drive move could not name a source drive or a target folder, the
archive form did not say what `member_email` and `archive_email` mean, the
delegate form had no list of the current delegates, and the account select placed
the chevron at the far right of the card because the width class was applied to
the native select instead of the wrapper that positions the icon.

The n8n collector this screen replaces worked from the directory itself: users,
groups and drives were chosen from what the tenant already had.

## Goal

* Every operation has a **full page** with room for the fields and help text.
* Drive operations select the source root (My Drive or a Shared Drive), the
  target Shared Drive, a target folder, and can create a target subfolder.
* Archive selects the departing user and the destination group from the
  directory, allows a new group address, and explains member/archive addresses.
* Copy/transfer forms choose mailboxes from the directory.
* Manage delegates shows the current delegates, adds/removes them, and refreshes
  the list after every change.
* The Select chevron stays inside the control.

## Steps

1. `providers.DirectoryReader` (Google Admin SDK + Microsoft Graph) and
   `MailboxDelegator.ListDelegates`; handlers `GET /api/management/options` and
   `GET /api/management/delegates`.
2. `SharedDriveManager.MoveIntoSharedDrive` gains a target folder id; the service
   creates `target_folder_name` when one is given.
3. The frontend route `management/operations/:kind` renders `OperationView.vue`.
   `OperationsView` becomes the catalogue + history page and navigates there.
4. `OperationView` is data-driven from the operation params: user/group selects,
   drive selects, FolderPicker for target folders, delegate list with add/remove,
   danger confirmation, and help text per field.
5. `Select.vue` applies the width class to the wrapper, so the chevron is inside
   the box.
6. Docs and i18n in every catalog.

## Risks

* **Options cost calls.** The form reads only the collections the kind needs
  (users/groups for archive, drives for Drive operations), so a Drive form does
  not pay for the directory listing.
* **Delegates need the Gmail API.** The list uses the same Gmail settings scope
  as the existing delegate operation; a Google Cloud project without the Gmail
  API shows that error in the form.
* **Group lists can be empty.** The group field is a datalist, so the operator
  can type a new address when the tenant has no group or the group listing is
  empty.
* **New target folder.** The service creates it before moving; a creation
  failure stops the task before any item is re-parented.
