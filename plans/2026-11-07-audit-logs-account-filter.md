# Filter the Audit Logs screen by account

## Context

`/audit-logs` filters by `provider` (`action`, `target`, `search`, `from`, `to`)
but the screen never lets the operator name the account it is about: the table
shows `org_id`, a tenant id or a Workspace domain, and the operator has to
recognise it. Opening the screen therefore reads as "which tenant is this?", not
"what did the Contoso administrator account do?".

The feed itself is organisation-wide on purpose (`docs/audit-logs.md` §2): the
collector groups the connected accounts by `(provider, organisation)` and reads
each group once with any healthy administrator
(`internal/services/auditlog.go:112-185`). Two administrator accounts of one
tenant therefore share a single feed, and "filter by account" can only mean
"filter by the organisation that account belongs to".

## Goal

Let the operator pick a connected account on `/audit-logs` and see exactly that
account's organisation feed, without inventing an API filter the stored events
cannot honour.

## Steps

1. `web/src/lib/api.ts`: type `org_id` on `ConnectedAccount` — the API has
   returned it since the column existed (`internal/handlers/accounts.go`), the
   TypeScript interface just never said so.
2. `internal/handlers/accounts.go`: report the organisation the collector really
   uses. `accountViewOf` filled `org_id` from the stored column only, while
   `AuditLogService.organisations` falls back to the tenant claim of the stored
   access token for a row connected before the column existed. Without the same
   fallback an upgraded instance lists a working Microsoft feed whose account
   cannot be selected. Resolve it in `accountOrg`, next to `accountKind`.
3. `internal/handlers/account_permissions_test.go`: pin the fallback — an account
   with no `org_id` column but a `tid` claim answers that tenant, a personal
   account keeps it omitted.
4. `web/src/views/AuditLogsView.vue`: an account `Select` under Provider, loaded
   from `/api/accounts` (scoped to the token's organisation by the API). Picking
   an account writes `provider` + `org_id` into the applied filters, so the list,
   the pager, the cursor banner and the CSV export keep using the documented
   parameters. Accounts without an organisation (personal accounts, no `hd`
   claim) are offered disabled: there is no feed to read. The Organisation column
   resolves `org_id` back to the account addresses, so the table says who.
5. Locales (seven): `auditLogs.account`, `accountAll`, `accountHint` (with the
   `{org}` placeholder) and `accountNoFeed`.
6. Docs: `docs/audit-logs.md` §4 (the account selector and the organisation-wide
   caveat) and `docs/api.md` §5 (`accountView.org_id` and its fallback).

## Risks

* **A filter that reads per account but lists per organisation.** Mitigated by
  saying so in the field hint and in the docs, instead of letting the operator
  believe the events were attributed to the account.
* **Two accounts, one organisation.** Selecting either lists the same events;
  the hint names the organisation so this looks intentional rather than broken.
* **Accounts with no organisation.** Personal accounts (an MSA, a `gmail.com`
  account) have no administrator feed; the option is disabled with the reason.
* **Legacy rows.** Accounts connected before the `org_id` column existed are
  covered by the token fallback of step 2, exactly as the collector already does.
* **No API contract change.** The list endpoint, its filters and the export are
  untouched, so nothing in the documented vocabulary moves.

## Notes on validation

Verified against a throwaway MariaDB 11 with the built server and four seeded
accounts (two Microsoft administrator accounts of one tenant, a personal
Microsoft account, a Google Workspace account) plus six events:

* the account selector lists every account and greys out the personal one
  ("no organisation feed");
* picking an administrator account switches the provider and shows the hint that
  names the tenant;
* applying lists only that organisation (4 of 6 events, "4 events · page 1 of
  1"), and `GET /api/audit-logs/export` with the same `provider` + `org_id`
  answers the same 4 rows with `X-Total-Count: 4`;
* `GET /api/accounts` reports `org_id` for the three accounts that have one.

**`docs/screenshots/10-audit-logs.png` is now stale**: the filter form gained a
field. Regenerating it needs the demo dataset described in the README (the
screenshots of the gallery share one dataset), so it is captured with
`SCREENSHOT_STEPS=10-audit-logs npm run screenshots` against such an instance
rather than against a blank local one.
