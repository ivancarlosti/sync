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
* `Drive{ID, Name, Kind, Owner}` with `Kind` one of `personal`, `shared`,
  `document_library`, `site`.
* `Item{ID, Name, Path, ParentID, IsDir, Size, ModifiedAt, MimeType, Hash,
  NativeDoc, Shortcut}` — `Path` is filled in by the engine (relative to the
  synchronised root), `Hash` is the provider checksum (Google `md5Checksum`,
  Graph `quickXorHash`) and may be empty.
* `DriveRoot = "root"` is the well known id of a drive's root for both providers.
* `NativeMimePrefix = "application/vnd.google-apps."` marks Google native
  documents (Docs/Sheets/Slides) that have no binary body.

Sentinel errors: `providers.ErrNotFound` (the engine records a deletion) and
`providers.ErrUnsupported` (a provider cannot express the operation).

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


## 4. Behaviour differences that matter

| Topic | Google | Microsoft |
|---|---|---|
| API | `google.golang.org/api/drive/v3` + `oauth2` | `msgraph-sdk-go` v1.103 with a custom kiota token provider fed by our encrypted tokens |
| Drive list | My Drive + shared drives (`drives.list`) | personal OneDrive + SharePoint libraries reached through `GET /api/accounts/:id/sites?q=` |
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
  and cannot be selected.
* **Admin > Providers** — client id, client secret, redirect URI, tenant
  (Microsoft), the current `source`, the exact redirect URI to paste into the
  provider console and the tenant-wide consent state (Microsoft).
* **Admin > Setup guide** — the guided registration: ordered steps with console
  deep links, the redirect URI and the permission list to copy, the capability
  badges, the caveats, the tenant-wide consent button (Microsoft) and a link to
  [app-registration.md](../docs/app-registration.md).

## 6. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `Connect` button missing on Accounts | no usable client for that provider | configure it in Admin > Providers or via the environment |
| `redirect_uri_mismatch` (Google) / `AADSTS50011` (Microsoft) | the registered URI differs from `APP_URL/api/oauth/<provider>/callback` | register the exact URL; check for a trailing slash and the `https` scheme |
| `424 not_configured` when starting a flow | the client is missing or the secret is empty | `secret_set: false` in `GET /api/providers` tells you which field |
| `412 reconnect` on every call | refresh token revoked/expired | reconnect the account (same remote account → same row, jobs preserved) |
| `412 consent_required` when connecting | the permissions were refused, or the Microsoft admin consent is missing | collect the setup guide (`Admin > Setup guide`), fix the console, retry |
| capability badges marked in red on an account | the account was connected before those permissions were requested | reconnect the account |
| SharePoint libraries missing | `Sites.ReadWrite.All` not granted or no admin consent | grant it and reconnect |
| Google native docs never copied | by design in V1 (`unsupported`) | export them manually, or keep them out of the source folder |
