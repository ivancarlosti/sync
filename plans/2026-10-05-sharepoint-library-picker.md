# SharePoint libraries: the folder picker only offered OneDrive

* **Date:** 2026-10-05
* **Repo:** `ivancarlosti/sync`
* **Status:** done (see the progress log at the bottom)
* **Related:** [`2026-09-28-app-registration-guide.md`](./2026-09-28-app-registration-guide.md), [`../docs/providers.md`](../docs/providers.md)

## 1. Objective

Creating a Microsoft 365 job cannot reach SharePoint. The drive selector of the
folder picker lists the personal OneDrive (and whatever `/me/drives` happens to
return) and nothing else, with no way to search for a site and no way to point
the picker at one by hand. Two things are wanted:

1. **Every accessible site shows up**, without the operator guessing a keyword.
2. **A manual escape hatch**: paste a SharePoint URL (or a keyword) and reach the
   library it belongs to.

## 2. Root cause

| # | Symptom | Cause |
|---|---|---|
| 2.1 | The picker never offers a SharePoint library | `FolderPicker.vue` calls `accounts.sites(id)` with **no** `q`. The handler forwards the empty query to `SearchSites`, whose (then) code passed no `$search` at all, so the request was `GET /sites` — Microsoft documents that as **application-only** (it returns at most the tenant root site for a delegated token, and commonly `403`). Only `/me/drives` ever filled the selector, which is OneDrive plus the libraries the signed-in user already had shared with them. |
| 2.2 | No way back if the dropdown misses the library | The UI had no search box and no manual field; the only control was the `Select`. |
| 2.3 | A failure looked like an absent feature | `loadSites()` swallowed every error into `sites.value = []`. |

The delegated endpoints that do work are `GET /sites?search={q}` (access-scoped
to the signed-in user) and `GET /sites/{host}:{path}` → `/drive`, which is what
the picker needs.

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| 3.1 | An empty query searches for the wildcard `*` (`$search=*`), falling back to the plain collection | that is the delegated listing that returns the sites of the tenant; the fallback keeps the tenant root site in the picker when a tenant rejects the wildcard, instead of emptying it |
| 3.2 | One field, two calls: a keyword searches, a pasted SharePoint URL resolves | the operator asked for both, and the field can tell them apart (`http(s)://`, `*.sharepoint.com`, a Graph site id `host,g1,g2`, or `host:/path`) |
| 3.3 | A new optional capability `providers.SiteResolver` rather than a method on `SiteBrowser` | Google implements `SiteBrowser` and has no site URL to resolve; a separate interface keeps `providers/google` untouched and the registry lookup explicit (`SiteResolverFor`) |
| 3.4 | The location is normalised to a Graph site id (`host:/path`) before the call, and a URL pointing deeper is truncated to its site collection root | a URL copied from a library or a page (`/sites/marketing/Shared Documents/Forms/AllItems.aspx`) must resolve to `/sites/marketing`; Graph's path addressing takes the site, not an item |
| 3.5 | A value that is not a location is refused with `providers.ErrInvalidIdentifier` (400); a site the account cannot reach with `providers.ErrNotFound` (404) | the operator can act on both, and neither should look like a server failure |
| 3.6 | `providers.ErrNotFound` is mapped to 404 in `respond.go` | it was falling through to 500; a remote item (or site) that no longer exists is a 404 |
| 3.7 | The site list and the drives are deduped **at render time** in `driveOptions` | `/me/drives` already returns the libraries the user has access to, so a search would otherwise list the same root twice; keeping the raw list preserves the "N found" count |
| 3.8 | A search replaces the list, a resolved URL is prepended to it | a search is a new query; a resolution must not make the operator lose the results they were looking at |

## 4. Changes

### 4.1 Provider capability — `internal/providers/provider.go`, `registry.go`

* new `SiteResolver` interface (`ResolveSite(ctx, creds, tokens, location)`),
* `Registry.SiteResolverFor(name)` next to `SiteBrowserFor`.

### 4.2 Microsoft Graph — `internal/providers/microsoft/graph.go`

* `SearchSites` now searches `*` when the query is empty and retries without
  `$search` when the wildcard is refused; the paging/`siteDrive` logic moved into
  `listSites` (shared by both attempts),
* `ResolveSite` + `siteIDFromLocation` (URL / `host:/path` / `host,g1,g2` / bare
  host) + `siteRootPath` (`/sites`, `/teams`, `/personal` truncation),
* compile-time guard for `providers.SiteResolver`.

### 4.3 API — `internal/handlers/accounts.go`, `routes.go`, `respond.go`

* `POST /api/accounts/:id/sites/resolve` (`handleResolveSite`), body
  `{"url": "…"}`, answering the same `{sites:[…]}` shape as `GET …/sites` so the
  picker merges both the same way (`424` without the capability, `400` for a bad
  value, `404` for an unreachable site),
* `providers.ErrNotFound` → `404 not_found` in `statusFor` / `codeFor`.

### 4.4 UI — `web/src/lib/api.ts`, `web/src/components/FolderPicker.vue`

* `accounts.sites(id, q?)` and `accounts.resolveSite(id, url)`,
* a "Find a SharePoint library" field above the drive selector: debounced
  (300 ms) search, Enter/button to run at once, keyword-or-URL detection, errors
  surfaced through an `Alert` (and through `loadSites` instead of being
  swallowed), deduped options.

### 4.5 Catalogs — six new keys in all seven locales

| Key | en-US |
|---|---|
| `browser.siteSearch` | Find a SharePoint library |
| `browser.sitePlaceholder` | Keyword or SharePoint URL |
| `browser.siteFind` | Search |
| `browser.siteHint` | Type a keyword to search the sites of the organisation, or paste the URL of a library that does not show up. |
| `browser.siteResults` | `{count}` SharePoint libraries found |
| `browser.siteError` | The sites could not be searched. |

499 → **505** keys per catalog (the count in `docs/i18n.md` is updated).

### 4.6 Tests

* `internal/providers/microsoft/provider_test.go`: `TestSearchSitesSearchesForTheWildcard`,
  `TestSearchSitesFallsBackToTheCollection`, `TestSiteIDFromLocation`,
  `TestResolveSiteReadsTheDefaultLibrary` (through the real Graph builders behind
  a canned transport, extended with a per-path status),
* `internal/handlers/handlers_test.go`: `stubProvider.ResolveSite` plus the
  resolve assertions (200 with the library, 404 for an unreachable site) inside
  the account-screens test.

### 4.7 Docs

* `docs/api.md` (both endpoints), `docs/providers.md` (the capability list, the
  behaviour table, the job-editor bullet, the troubleshooting row),
* `docs/i18n.md` (the key count).

## 5. Verification

```bash
cd web && node scripts/check-i18n.mjs && npx vue-tsc --noEmit && npm run build
go build ./... && go vet ./internal/... && go test ./...
```

## 6. Progress log

* 2026-10-05 — reconnaissance: root-caused 2.1 (empty `q` → application-only
  `GET /sites`), 2.2 and 2.3. Plan written before the first edit.
* 2026-10-05 — implemented 4.1–4.7.

