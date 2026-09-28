// Package providers defines the storage abstraction Sync synchronises over.
//
// Two implementations exist today (providers/google for Google Drive and
// providers/microsoft for Microsoft Graph / OneDrive / SharePoint). Everything
// above this package — the sync engine, the handlers, the UI — only knows the
// interfaces declared here, which is what makes intra-provider jobs
// (Google → Google, Microsoft → Microsoft) work with the same code path.
package providers

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// ErrNotFound marks a remote item that no longer exists. The sync engine treats
// it as a deletion instead of an error.
var ErrNotFound = errors.New("remote item not found")

// ErrUnsupported marks an operation a provider cannot express (e.g. downloading
// a native Google document, which has no single file representation).
var ErrUnsupported = errors.New("operation not supported by this provider")

// Credentials is the OAuth client used against one provider. Values come from
// the environment or from Admin > Providers (database override wins).
type Credentials struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	// TenantID is Microsoft only: common | organizations | consumers | <uuid>.
	TenantID string
}

// Configured reports whether a flow can be started with these credentials.
func (c Credentials) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.RedirectURI != ""
}

// Tokens is the OAuth token set as returned by the provider. RefreshToken is
// empty when the provider did not rotate it; callers must then keep the stored
// one.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Expiry       time.Time
	Scopes       []string
}

// Expired reports whether the access token is expired, accounting for a two
// minute clock skew before every provider call.
func (t *Tokens) Expired(now time.Time) bool {
	return !t.Expiry.IsZero() && now.Add(2*time.Minute).After(t.Expiry)
}

// TimeToLive returns the remaining validity, never negative.
func (t *Tokens) TimeToLive(now time.Time) time.Duration {
	if t.Expiry.IsZero() {
		return 0
	}
	if remaining := t.Expiry.Sub(now); remaining > 0 {
		return remaining
	}
	return 0
}

// Account is the remote identity behind a token set.
type Account struct {
	ID        string
	Email     string
	Name      string
	AvatarURL string
}

// Drive is a named root the operator can browse and pick a folder from. For
// Google it is "My Drive" or a shared drive; for Microsoft it is a personal
// OneDrive or a SharePoint document library.
type Drive struct {
	ID   string
	Name string
	// Kind is one of: personal, shared, document_library, site.
	Kind string
	// Owner is an optional human readable owner (Microsoft shared drives).
	Owner string
}

// Item is a file or folder inside a drive. Path is filled in by the sync engine
// (relative to the synchronised root) and is empty for direct API responses.
type Item struct {
	ID         string
	Name       string
	Path       string
	ParentID   string
	IsDir      bool
	Size       int64
	ModifiedAt time.Time
	MimeType   string
	// Hash is the provider checksum (Google md5Checksum, Graph quickXorHash).
	// It is empty when the provider does not expose one for the item.
	Hash string
	// NativeDoc marks Google Docs/Sheets/Slides and other items that must be
	// exported instead of downloaded. Sync V1 skips them (documented).
	NativeDoc bool
	// Shortcut marks Google Drive shortcuts / Graph links, also skipped in V1.
	Shortcut bool
}

// Dir reports whether the item is a folder.
func (i Item) Dir() bool { return i.IsDir }

// Transfer is a streaming download of a single file.
type Transfer struct {
	Body       io.ReadCloser
	Size       int64
	MimeType   string
	ModifiedAt time.Time
	Hash       string
}

// UploadRequest describes one file upload. When ExistingID is set the provider
// overwrites that item, otherwise it creates a new file inside ParentID.
type UploadRequest struct {
	DriveID    string
	ParentID   string
	ExistingID string
	Name       string
	Size       int64
	MimeType   string
	ModifiedAt time.Time
	Body       io.Reader
}

// Provider is implemented once per cloud storage service.
type Provider interface {
	// Name is the value stored in connected_accounts.provider.
	Name() models.ProviderName

	// Scopes are the OAuth scopes requested when connecting an account.
	Scopes() []string

	// AuthCodeURL builds the authorization endpoint URL for the code flow.
	AuthCodeURL(creds Credentials, state, codeChallenge string) string

	// Exchange trades an authorization code (with its PKCE verifier) for tokens.
	Exchange(ctx context.Context, creds Credentials, code, codeVerifier string) (*Tokens, error)

	// Refresh renews an access token from a stored refresh token.
	Refresh(ctx context.Context, creds Credentials, refreshToken string) (*Tokens, error)

	// Account returns the remote identity behind the tokens.
	Account(ctx context.Context, creds Credentials, tokens *Tokens) (*Account, error)

	// Drives lists every root the account can synchronise.
	Drives(ctx context.Context, creds Credentials, tokens *Tokens) ([]Drive, error)

	// Children lists the direct children of folderID (use DriveRoot for the
	// root folder of a drive).
	Children(ctx context.Context, creds Credentials, tokens *Tokens, driveID, folderID string) ([]Item, error)

	// Item resolves a single item by id.
	Item(ctx context.Context, creds Credentials, tokens *Tokens, driveID, itemID string) (*Item, error)

	// Download streams a file.
	Download(ctx context.Context, creds Credentials, tokens *Tokens, driveID, itemID string) (*Transfer, error)

	// Upload creates or overwrites a file and returns the stored item.
	Upload(ctx context.Context, creds Credentials, tokens *Tokens, req UploadRequest) (*Item, error)

	// CreateFolder creates a folder inside parentID.
	CreateFolder(ctx context.Context, creds Credentials, tokens *Tokens, driveID, parentID, name string) (*Item, error)

	// Delete removes an item permanently.
	Delete(ctx context.Context, creds Credentials, tokens *Tokens, driveID, itemID string) error

	// IsNotFound reports whether err is the provider's "gone" error, so the
	// engine can treat a vanished file as a deletion.
	IsNotFound(err error) bool
}

// DriveRoot is the well known id of the root folder of a drive, understood by
// both providers (Google "root", Graph "root").
const DriveRoot = "root"

// NativeMimePrefix marks Google native documents that have no binary body.
const NativeMimePrefix = "application/vnd.google-apps."

// SiteBrowser is implemented by providers able to discover extra drives (for
// example SharePoint document libraries for Microsoft Graph).
type SiteBrowser interface {
	SearchSites(ctx context.Context, creds Credentials, tokens *Tokens, query string) ([]Drive, error)
}
