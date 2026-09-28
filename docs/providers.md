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

`source` is `environment`, `database` or `none`; `client_secret` is **never**
returned — only `secret_set`. `PUT /api/providers/:provider` with an empty
`client_secret` keeps the stored one (so the form can be edited without retyping
the secret); `DELETE /api/providers/:provider` drops the override and the
environment value applies again.

## 3. Registering the OAuth applications

### Google Drive

1. Google Cloud console → APIs & Services → enable **Google Drive API**.
2. OAuth consent screen: internal or external; add the test users or publish.
3. Credentials → OAuth client ID → *Web application*.
4. Authorized redirect URI: `APP_URL/api/oauth/google/callback`.
5. Set `GOOGLE_CLIENT_ID` + `GOOGLE_CLIENT_SECRET` (or paste them in Admin > Providers).

The consent screen is shown with `prompt=consent` + `include_granted_scopes`, so a
reconnection always returns a refresh token.

### Microsoft 365 / OneDrive / SharePoint

1. Entra ID → App registrations → **New registration**.
2. Supported account types: *Accounts in this organizational directory* (single
   tenant), or multitenant if you set `MICROSOFT_TENANT_ID=organizations`/`common`.
3. Redirect URI → *Web* → `APP_URL/api/oauth/microsoft/callback`.
4. API permissions → Microsoft Graph → **Delegated** → `User.Read`,
   `Files.ReadWrite.All`, `Sites.ReadWrite.All`; grant admin consent.
5. Certificates & secrets → new client secret → `MICROSOFT_CLIENT_SECRET`.
6. `MICROSOFT_CLIENT_ID`, `MICROSOFT_TENANT_ID` (`common`, `organizations`,
   `consumers` or a tenant UUID).

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
  permissions, expiry, last refresh and last synchronisation, with *Check the
  token* and *Disconnect* actions.
* **Job editor** — the folder browser calls
  `GET /api/accounts/:id/drives` then
  `GET /api/accounts/:id/drives/:drive/items?folder_id=…`; folders come first,
  then files, both alphabetically. Items with `unsupported: true` are greyed out
  and cannot be selected.
* **Admin > Providers** — the wizard: client id, client secret, redirect URI,
  tenant (Microsoft), the current `source`, and the exact redirect URI to paste
  into the provider console.

## 6. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `Connect` button missing on Accounts | no usable client for that provider | configure it in Admin > Providers or via the environment |
| `redirect_uri_mismatch` (Google) / `AADSTS50011` (Microsoft) | the registered URI differs from `APP_URL/api/oauth/<provider>/callback` | register the exact URL; check for a trailing slash and the `https` scheme |
| `424 not_configured` when starting a flow | the client is missing or the secret is empty | `secret_set: false` in `GET /api/providers` tells you which field |
| `412 reconnect` on every call | refresh token revoked/expired | reconnect the account (same remote account → same row, jobs preserved) |
| SharePoint libraries missing | `Sites.ReadWrite.All` not granted or no admin consent | grant it and reconnect |
| Google native docs never copied | by design in V1 (`unsupported`) | export them manually, or keep them out of the source folder |
