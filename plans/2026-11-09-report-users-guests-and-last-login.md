# Reports: separate guests from users and add last login

## Context

The Microsoft 365 Users report listed every account of the directory, guests
included, while the Guests report already existed as the filtered half. The n8n
collector this screen replaces split the two sheets on `#EXT#` and never repeated
a guest in Users, so a report of the workforce was mixed with external
identities.

The n8n workflow also had a (disabled) `signInActivity` node: last login was the
missing column. Microsoft Graph exposes it precisely as
`signInActivity.lastSuccessfulSignInDateTime`, but the property requires
Microsoft Entra ID P1/P2. Microsoft 365 Business Standard includes Entra ID Free,
so the property is refused on the tenant this is for.

The Microsoft 365 unified audit log records the same event as `UserLoggedIn` in
the `Audit.AzureActiveDirectory` content type. Audit (Standard) is part of
Business Standard, and the Office 365 Management Activity API already serves the
app-only `ActivityFeed.Read` permission the Audit Logs screen uses.

## Goal

* `users` lists member accounts only; `guests` lists guest accounts only. The
  two sections stay complementary halves of the one users page, so neither pays
  an extra call.
* `users` and `guests` carry a `last_login` column:
  * `signInActivity.lastSuccessfulSignInDateTime` when the tenant has Entra ID
    P1/P2 (then the last interactive and non-interactive values as fallbacks);
  * otherwise the newest `UserLoggedIn` record in the unified audit log window
    (seven days, the limit of the activity API);
  * an empty cell when neither answered — never a guessed zero.
* The last-login fallback is best effort: no ActivityFeed consent or no audit
  record must not fail the users section.

## Steps

1. `isGuestUser` recognises both `userType == Guest` and the `#EXT#` principal
   name; `usersTable` skips the other half for each section.
2. `usersTarget` takes two optional projections, `assignedLicenses` and
   `signInActivity`. `loadUsers` tries both, then drops the refused one, then
   falls back to the plain listing on 400/403 — the same contract the licence
   property already had.
3. `lastlogin.go`: `signInActivityOf`, `lastLoginOf`, `loadLastLogins` and
   `unifiedLastLogins`. The unified read subscribes to
   `Audit.AzureActiveDirectory` (idempotently), walks the seven 24-hour windows
   the API keeps, and maps the newest successful `UserLoggedIn` per address.
4. `CollectReport` now keeps the `Credentials` it already received (it used to
   discard them) because the activity feed is app-only.
5. Tests: users/guests split, `#EXT#` detection, the signInActivity preference
   order, the audit-record parser, and a mocked management-API pass that pins the
   app-only flow and newest-record-wins.
6. `docs/reports.md`: the complementary sections, the two last-login sources and
   the seven-day limitation.

## Risks

* **Seven-day window.** A user who has not signed in during the activity API
  window keeps a blank cell. That is the honest answer; the precise property
  needs Entra ID P1/P2.
* **One extra optional projection on the users listing.** A tenant without P1
  pays one refused users call, then the same listing without the property.
* **The audit fallback needs the activity subscription.** Without
  ActivityFeed.Read, the column stays empty and the users section still succeeds.
  The first subscription can take up to 12 hours to produce content, as the
  existing Audit Logs feed does.
* **Guests disappear from Users.** That is the requested contract; a caller that
  summed both tables must read the two sections instead.
