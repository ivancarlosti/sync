# Providers

A *provider* is a cloud storage implementation behind the interface in
`internal/providers/provider.go`. Two are compiled in:

| Provider | Package | Roots it can synchronise |
|---|---|---|
| Google | `internal/providers/google` | "My Drive", shared drives |
| Microsoft | `internal/providers/microsoft` | personal OneDrive, SharePoint document libraries (`Sites.ReadWrite.All`) |

Everything above the package (engine, handlers, SPA) only sees the interface,
which is what makes **intra-provider jobs** (Google → Google, Microsoft →
Microsoft) work with the exact same code path as Google → Microsoft.

## 1. Interface

```go
type Provider interface {
    Name() models.ProviderName
    Scopes() []string
    AuthCodeURL(creds Credentials, state, codeChallenge string) string
    Exchange(ctx, creds, code, codeVerifier) (*Tokens, error)
    Refresh(ctx, creds, refreshToken) (*Tokens, error)
    Account(ctx, creds, tokens) (*Account, error)
    Drives(ctx, creds, tokens) ([]Drive, error)
    Children(ctx, creds, tokens, driveID, folderID string) ([]Item, error)
    Item(ctx, creds, tokens, driveID, itemID string) (*Item, error)
    Download(ctx, creds, tokens, driveID, itemID string) (*Transfer, error)
    Upload(ctx, creds, tokens, req UploadRequest) (*Item, error)
    CreateFolder(ctx, creds, tokens, driveID, parentID, name string) (*Item, error)
    Delete(ctx, creds, tokens, driveID, itemID string) error
    IsNotFound(err error) bool
}

// Optional capability: providers able to discover extra drives.
type SiteBrowser interface {
    SearchSites(ctx, creds, tokens, query string) ([]Drive, error)
}

// Optional capability: providers able to turn an operator supplied location
// (a URL, a server-relative path or a remote id) into a browsable drive. It
// backs the manual fallback of the folder picker.
type SiteResolver interface {
    ResolveSite(ctx, creds, tokens, location string) (Drive, error)
}

// Optional capability: providers that publish their permission table. The table
// is the single source of truth for Scopes(), the setup guide and the capability
// badges of the accounts screen.
type ScopeCatalog interface {
    Permissions() []Permission // {Capability, Title, Scope, AdminConsent}
    ConsoleURLs() map[string]string
}

// Optional capability: providers whose permissions are granted tenant-wide.
type ConsentGranter interface {
    AdminConsentURL(creds Credentials, state string) string
}

// Capability values: files, users, groups, members, domains, orgunits, roles,
// licenses. helpers: Catalog / ScopesOf / CapabilitiesOf / MissingCapabilities /
// HasCapability.
//
// Authorization refusals the operator can act on are returned as *AuthError
// ({Provider, Code, Description}); IsConsentRequired reports the ones that mean
// "the permissions were not granted" (the services layer turns them into
// ErrConsent → HTTP 412 `consent_required`).
```

Key types:

* `Credentials{ClientID, ClientSecret, RedirectURI, TenantID}` — `TenantID` is
  Microsoft only. `Configured()` requires all three of id/secret/redirect.
* `Tokens{AccessToken, RefreshToken, TokenType, Expiry, Scopes}`; `Expired(now)`
  applies a two minute safety margin.
* `Drive{ID, Name, Kind, Owner}` with `Kind` one of `personal`, `business`,
  `shared`, `document_library`, `site` (`personal` and `business` are the two
  flavours of the account's own OneDrive, a personal account vs a work/school
  one; the picker groups them together).
* `Item{ID, Name, Path, ParentID, IsDir, Size, ModifiedAt, MimeType, Hash,
  NativeDoc, Shortcut}` — `Path` is filled in by the engine (relative to the
  synchronised root), `Hash` is the provider checksum (Google `md5Checksum`,
  Graph `quickXorHash`) and may be empty.
* `DriveRoot = "root"` is the well known id of a drive's root for both providers.
* **Remote identifiers are guarded before they reach a URL.** Google matches
  every id it interpolates — item, folder, parent and drive id — against
  `^[A-Za-z0-9_-]{1,512}$`
  (`internal/providers/google/identifiers.go`; empty is accepted in the optional
  *drive* position, where the interface defines it as "the drive the account
  considers its default"). The pattern is the alphabet Drive hands out (opaque
  base64url tokens), plus the synthetic ids Sync adds (`root`, `my-drive`); it
  refuses a path separator or relative segment, a query/fragment/userinfo
  delimiter, the percent-encoded form of one, a backslash, whitespace and
  control characters, and anything longer than a real token. A refused value is
  returned as `providers.ErrInvalidIdentifier`, which the API answers with
  `400`/`validation`: the id comes from a stored job, a crafted explorer link or
  the engine, so the caller has to fix it. The guard is written inline in the
  function that builds the request (a helper would hide the check from a static
  analyser) and `identifiers_test.go` pins both halves — a refused value never
  reaches the network, an accepted one lands in the request untouched.
* **A request is pinned to the Drive API host as well.** The three functions that
  issue a request — `doJSON`, `Download` and `Upload` (its initiation leg and its
  session leg) — match the finished URL against
  `^https://([A-Za-z0-9-]+\.)+googleapis\.com/`
  followed by a path, query and fragment drawn from the URL alphabet
  (`requestURLPattern`, same file). The host is the one every Drive endpoint
  lives under, so no call site can steer a request (or the bytes of a file, on
  the upload session leg) to another destination, and the alphabet keeps
  whitespace, a control character, a backslash and a quote out of the URL. A URL
  that fails the guard is refused as `ErrInvalidIdentifier` too, before
  `http.NewRequestWithContext` sees it. This is the second layer: the
  per-identifier guards answer `400` with the offending value, while this one is
  what keeps a call site that forgets its own guard from leaving Google — the
  only shape on which the guard is recognised, because it is inline and applied
  to the very value handed to the request.
* `NativeMimePrefix = "application/vnd.google-apps."` marks Google native
  documents (Docs/Sheets/Slides) that have no binary body.

Sentinel errors: `providers.ErrNotFound` (the engine records a deletion),
`providers.ErrUnsupported` (a provider cannot express the operation) and
`providers.ErrInvalidIdentifier` (a caller handed the provider an identifier it
refuses to put into a request → `400`/`validation`).

**Throttling is retried before it becomes an error.** Every call a provider
issues with its own HTTP client — Google metadata, downloads and session
initiation, and the two Microsoft byte transfers (`Download`, and each chunk of
an upload session) — goes through `providers.DoWithRetry`
(`internal/providers/retry.go`): a `429`, a `5xx`, and (Google only) the `403`
the Drive API answers a quota or rate excess with (`errors[].reason` ∈
`rateLimitExceeded`, `userRateLimitExceeded`, `quotaExceeded`,
`sharingRateLimitExceeded`, `dailyLimitExceeded`) are retried up to
`DefaultMaxAttempts` (5) times, waiting for the `Retry-After` the server asked
for when it sent one and an exponential backoff with equal jitter otherwise
(`DefaultBaseDelay` 500 ms, capped at `DefaultMaxDelay` 30 s). The waiting is
context aware, so a cancelled run stops at once. Two things are deliberately
**not** retried: a transport error (the helper cannot know whether a request
that may have reached the server is safe to repeat) and a body that cannot be
replayed — the bytes of an upload, which are the caller's stream. Microsoft
metadata does not need this: Kiota's pipeline already retries it. When the
budget is spent on a throttle the caller receives the provider's last answer,
which `errors.Is(err, providers.ErrRateLimited)` recognises → `429`/
`rate_limited` in the API, which the SPA translates with
`accounts.error_rate_limited`.

## 2. Credential resolution (environment vs Admin > Providers)

`ProviderSettings` (`internal/services/credentials.go`) resolves a provider in
this order:

1. **Database override** — `provider.<name>.client_id`, `.client_secret`,
   `.redirect_uri`, `.tenant_id` in the `settings` table, written by
   **Admin > Providers**. The secret is AES-GCM encrypted (`v1:`).
2. **Environment** — `GOOGLE_*` / `MICROSOFT_*`.
3. **Derived** — the redirect URI falls back to `APP_URL` + the callback path.

`GET /api/providers` returns one `ProviderCredentialsInfo` per compiled provider:

```json
{
  "provider": "google",
  "client_id": "1234.apps.googleusercontent.com",
  "secret_set": true,
  "redirect_uri": "https://sync.example.com/api/oauth/google/callback",
  "source": "environment",
  "configured": true
}
```

Microsoft adds `admin_consent: {tenant, client_id, at, granted}` once a tenant
administrator granted the permissions from the setup guide; `granted` is computed
against the client id configured right now, so a consent left over from a
replaced app registration reports `false`.

`source` is `environment`, `database` or `none`; `client_secret` is **never**
returned — only `secret_set`. `PUT /api/providers/:provider` with an empty
`client_secret` keeps the stored one (so the form can be edited without retyping
the secret); `DELETE /api/providers/:provider` drops the override and the
environment value applies again.

## 3. Registering the OAuth applications

The step-by-step instructions (console pages, the exact redirect URI, the exact
permission list, the tenant-wide admin consent and the pitfalls) live in
[app-registration.md](app-registration.md) — and in the product, in
**Admin > Setup guide**, which renders them from the permission table of the
running binary (`GET /api/providers/:provider/guide`, see
`internal/services/guide.go`).

What the registration has to expose:

| Provider | Redirect URI to register | APIs / permissions |
|---|---|---|
| Google | `APP_URL/api/oauth/google/callback` | Drive API + Admin SDK API + Enterprise License Manager API enabled; the scopes of the permission table (sensitive: `admin.directory.*`, `apps.licensing`); the client ID allow-listed in Admin console → Security → API controls |
| Microsoft | `APP_URL/api/oauth/microsoft/callback` | the Graph delegated permissions of the permission table, all of them *admin consent required*; the tenant administrator grants them once (the guide has the consent button) |

Both flows are shown with `prompt=consent`, so a reconnection always returns a
refresh token; Google also uses `access_type=offline` and `include_granted_scopes`.

The permission list is copied differently per console, because that is how the
guide asks for it: the Google consent screen's *Manually add scopes* box takes the
whole comma-separated grant in one paste (`copy: "scopes"`), while the Entra
**Add a permission** picker only accepts the Graph permissions one by one, so the
guide offers one field per permission with the `https://graph.microsoft.com/`
prefix stripped (`copy: "permissions"` — `User.Read`, `Files.ReadWrite.All`, …).


## 4. Behaviour differences that matter

| Topic | Google | Microsoft |
|---|---|---|
| API | `google.golang.org/api/drive/v3` + `oauth2` | `msgraph-sdk-go` v1.103 with a custom kiota token provider fed by our encrypted tokens |
| Drive list | My Drive + shared drives (`drives.list`) | personal OneDrive from `/me/drives` plus SharePoint libraries: `GET /api/accounts/:id/sites` (`$search=*` when no keyword, so every reachable site is listed) and the manual fallback `POST /api/accounts/:id/sites/resolve` for a pasted SharePoint URL |
| Checksum | `md5Checksum` (only for binary files) | `quickXorHash` |
| Native documents (`application/vnd.google-apps.*`) | exposed but marked `unsupported` — V1 never exports them | n/a |
| Shortcuts / links | marked `unsupported`, never followed | links are skipped the same way |
| Deletion | `DELETE /files/:id` (permanent) | `DELETE /drives/:drive/items/:id` (OneDrive/SharePoint still keep the item in the recycle bin) |
| Progress while uploading | resumable upload for large files | upload session for large files (`upload.go`) |

Both providers answer `IsNotFound(err) == true` for a remote item that vanished,
which is how a deletion detected during a run is distinguished from a failure.

## 5. What the UI shows

* **Accounts** — one row per connected account: provider, e-mail, status, granted
  permissions, capability badges (the ones the stored grant satisfies, plus the
  ones the current release asks for on top of it), expiry, last refresh and last
  synchronisation, with *Check the token*, *Reconnect* (only when a permission is
  missing) and *Disconnect* actions.
* **Job editor** — the folder browser calls
  `GET /api/accounts/:id/drives` then
  `GET /api/accounts/:id/drives/:drive/items?folder_id=…`; folders come first,
  then files, both alphabetically. Items with `unsupported: true` are greyed out
  and cannot be selected. The picker is provider-aware (the editor passes the
  `provider` of the account): a Google account gets one *Drive* select holding
  My Drive and its Shared Drives, and the top level of a Shared Drive is listed
  by the drive id — the `root` alias names the My Drive root only. A Microsoft
  365 account instead gets a *OneDrive* select — the account's own drive, tagged
  `personal` on a personal account and `business` on a work/school one — plus,
  under the site field, a separate *SharePoint library* select. That field lists
  the SharePoint libraries of the tenant (`$search=*` on open, `$search=<q>`
  while typing) and resolves a pasted SharePoint URL through
  `POST /api/accounts/:id/sites/resolve` when the search does not surface a
  library. Every library is labelled with the site it lives in, because each site
  names its default library `Documents`; it is only rendered for Microsoft, which
  is the only provider with the capability.
* **Admin > Providers** — client id, client secret, redirect URI, tenant
  (Microsoft), the current `source`, the exact redirect URI to paste into the
  provider console and the tenant-wide consent state (Microsoft).
* **Admin > Setup guide** — the guided registration: ordered steps with console
  deep links, the redirect URI and the permission list to copy (the whole grant in
  one field for Google, one field per permission for Microsoft Entra), the
  capability badges, the caveats, the tenant-wide consent button (Microsoft) and a
  link to [app-registration.md](../docs/app-registration.md).

## 6. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `Connect` button missing on Accounts | no usable client for that provider | configure it in Admin > Providers or via the environment |
| `redirect_uri_mismatch` (Google) / `AADSTS50011` (Microsoft) | the registered URI differs from `APP_URL/api/oauth/<provider>/callback` | register the exact URL; check for a trailing slash and the `https` scheme |
| `424 not_configured` when starting a flow | the client is missing or the secret is empty | `secret_set: false` in `GET /api/providers` tells you which field |
| `412 reconnect` on every call | refresh token revoked/expired | reconnect the account (same remote account → same row, jobs preserved) |
| `412 consent_required` when connecting | the permissions were refused, or the Microsoft admin consent is missing | collect the setup guide (`Admin > Setup guide`), fix the console, retry |
| capability badges marked in red on an account | the account was connected before those permissions were requested | reconnect the account |
| Microsoft: the *files* badge is red right after connecting, and reconnecting changes nothing | the grant is missing a **Graph** permission — `User.Read`, `Files.ReadWrite.All` or `Sites.ReadWrite.All`. The four OpenID Connect scopes are `informational` and never gate the badge (Entra does not report them back) | grant the missing Graph permission (admin consent for the tenant), then reconnect |
| SharePoint libraries missing | `Sites.ReadWrite.All` not granted, no admin consent, or the library belongs to a site the signed-in user cannot reach | grant the scope and reconnect; the picker also accepts a pasted SharePoint URL (the manual fallback of the site field) |
| Google native docs never copied | by design in V1 (`unsupported`) | export them manually, or keep them out of the source folder |
