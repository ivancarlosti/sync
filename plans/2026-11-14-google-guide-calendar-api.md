# Google setup guide: Calendar API and the Google Auth Platform links

## Context

The Google tab of Admin > Setup guide (`GET /api/providers/google/guide`,
`internal/services/guide.go`) walks the operator through creating the OAuth
client, but it only asks to enable the **Drive API**, even though the provider
requests the Calendar scope too (`internal/providers/google/permissions.go`).
A files-and-calendars instance therefore has no in-app instruction to enable the
Google Calendar API.

The console links of the same walkthrough still point at the legacy
`console.cloud.google.com/apis/credentials*` pages. Google moved OAuth
configuration into the **Google Auth Platform**, so the "Add the requested
permissions" step opens the credentials list instead of the page that actually
holds the scopes (`/auth/scopes`). The consent, client and redirect steps are
stale for the same reason.

## Goal

* The Google walkthrough asks to enable the Calendar API, next to the Drive API,
  without forcing a files-only installation to do it first (optional step).
* Every Google console link opens the current Google Auth Platform page:
  `/auth/overview` (consent/audience), `/auth/clients` (client + redirect URI)
  and `/auth/scopes` (the scope list).
* All console links live in `google.Provider.ConsoleURLs()` so a moved console
  page is a one-line change, pinned by tests.
* The seven i18n catalogs and `docs/app-registration.md` describe the same
  steps as the code.

## Steps

1. `internal/providers/google/permissions.go`: add `calendar_api`,
   `auth_overview`, `auth_clients` and `auth_scopes` to `ConsoleURLs()`.
2. `internal/services/guide.go`: insert the optional `calendar_api` step after
   `apis`; point `consent`, `client`, `redirect` and `permissions` at the links
   returned by the provider (`permissions` → `/auth/scopes`).
3. i18n (all seven catalogs): new `admin.guide.steps.google.calendar_api`
   strings; refresh `intro.google`, the Google step bodies and the
   `api_enablement` warning to name both APIs and the new console tabs.
4. Tests: extend `internal/providers/google/permissions_test.go` with the new
   link keys; add `TestGoogleGuideLinksTheCurrentConsolePages` to
   `internal/services/guide_test.go` (calendar step present, scopes step on
   `/auth/scopes`, no `/apis/credentials` left).
5. `docs/app-registration.md`: add the `calendars` row to the permission table,
   enable both APIs in §2.1 and list the new `console_urls` keys in §4.
6. Validate: `gofmt`, `go test ./internal/...`,
   `node scripts/check-i18n.mjs` and `vue-tsc` from `web/`.

## Risks

* The i18n parity checks (`internal/services/i18n_test.go`,
  `web/scripts/check-i18n.mjs`) fail until every catalog carries the new key —
  intentional, it keeps the step translatable everywhere.
* Google console routes have already moved twice: keeping the URLs in
  `ConsoleURLs()` and pinning them in tests limits the next change to one file.
* The `calendar_api` step is `Optional` on purpose: the calendar scope is
  requested from the first connection, but only calendar jobs dial the API, so
  a files-only instance can postpone the enablement.
* Unrelated to this change: one open Dependabot alert (medium,
  `go.opentelemetry.io/otel*` 1.46.0, patched in 1.47.0) is tracked separately
  and is not part of this plan.
