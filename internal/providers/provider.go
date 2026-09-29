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
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// ErrNotFound marks a remote item that no longer exists. The sync engine treats
// it as a deletion instead of an error.
var ErrNotFound = errors.New("remote item not found")

// ErrUnsupported marks an operation a provider cannot express (e.g. downloading
// a native Google document, which has no single file representation).
var ErrUnsupported = errors.New("operation not supported by this provider")

// ErrInvalidIdentifier marks an identifier a caller handed to a provider that
// the provider refuses to put into a request. Remote ids are opaque tokens the
// provider itself hands out (see DriveRoot for the synthetic ones); a value
// carrying a path separator, a query delimiter or whitespace is not one, and is
// refused instead of being interpolated into a request (see
// docs/providers.md — identifiers).
var ErrInvalidIdentifier = errors.New("invalid provider identifier")

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

// Capability names a class of provider operations the operator can recognise in
// the UI ("this account can manage licences"). Capabilities are derived from the
// scope string the provider returned, so a connection made before a scope was
// added reports what it is missing instead of claiming a feature it cannot use.
type Capability string

const (
	// CapabilityFiles covers browsing and transferring files (the sync engine).
	CapabilityFiles Capability = "files"
	// CapabilityUsers covers reading and managing user accounts.
	CapabilityUsers Capability = "users"
	// CapabilityGroups covers groups and Microsoft distribution lists.
	CapabilityGroups Capability = "groups"
	// CapabilityMembers covers group membership.
	CapabilityMembers Capability = "members"
	// CapabilityDomains covers the verified domains of a tenant.
	CapabilityDomains Capability = "domains"
	// CapabilityOrgUnits covers organisational units and their settings.
	CapabilityOrgUnits Capability = "orgunits"
	// CapabilityRoles covers the administrative role catalogue (read-only).
	CapabilityRoles Capability = "roles"
	// CapabilityLicenses covers subscription and licence assignment.
	CapabilityLicenses Capability = "licenses"
)

// ProviderCapabilities lists every capability in the order the UI displays them.
var ProviderCapabilities = []Capability{
	CapabilityFiles,
	CapabilityUsers,
	CapabilityGroups,
	CapabilityMembers,
	CapabilityDomains,
	CapabilityOrgUnits,
	CapabilityRoles,
	CapabilityLicenses,
}

// Permission documents one requested scope: why it is requested, which feature
// needs it, and whether a tenant administrator has to grant it for everybody.
// The slice a provider returns is its single source of truth: Scopes() builds
// the authorization request from it, GET /api/providers/:provider/guide renders
// it and the capability badges are derived from it.
type Permission struct {
	// Capability is the feature class this scope unlocks.
	Capability Capability `json:"capability"`
	// Title is the i18n key suffix of the human readable description
	// (`admin.capability.<title>`); it lets one capability be described by
	// several scopes without repeating the text.
	Title string `json:"title"`
	// Scope is the exact string sent to the provider.
	Scope string `json:"scope"`
	// AdminConsent is true when the scope can only be granted tenant-wide
	// (Google: administrator identity + API controls allowlist; Microsoft:
	// admin consent), which the guide has to explain before the flow starts.
	AdminConsent bool `json:"admin_consent"`
}

// ScopeCatalog is implemented by providers that publish their permission table.
// It is optional so the interface above stays the only mandatory contract.
type ScopeCatalog interface {
	// Permissions returns every scope the provider requests, in request order.
	Permissions() []Permission
	// ConsoleURLs returns the registration-console deep links specific to this
	// provider (API enablement pages), keyed by a stable name.
	ConsoleURLs() map[string]string
}

// Catalog returns the permission table of a provider, or nil when the provider
// does not publish one.
func Catalog(provider Provider) []Permission {
	catalog, ok := provider.(ScopeCatalog)
	if !ok {
		return nil
	}
	return catalog.Permissions()
}

// ConsoleURLs returns the provider specific console links, never nil.
func ConsoleURLs(provider Provider) map[string]string {
	links := map[string]string{}
	if catalog, ok := provider.(ScopeCatalog); ok {
		for key, url := range catalog.ConsoleURLs() {
			links[key] = url
		}
	}
	return links
}

// ScopesOf returns the scope strings of a permission table, in order.
func ScopesOf(permissions []Permission) []string {
	scopes := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		scopes = append(scopes, permission.Scope)
	}
	return scopes
}

// CapabilitiesOf maps a granted scope list onto the capabilities of a permission
// table. Matching is case-insensitive and a capability is only reported when
// every scope it declares was granted, so an incomplete or stale grant never
// claims a feature.
func CapabilitiesOf(permissions []Permission, granted []string) []Capability {
	satisfied := []Capability{}
	for _, capability := range ProviderCapabilities {
		if HasCapability(permissions, granted, capability) {
			satisfied = append(satisfied, capability)
		}
	}
	return satisfied
}

// HasCapability reports whether a grant satisfies every scope of a capability.
func HasCapability(permissions []Permission, granted []string, capability Capability) bool {
	declared := false
	for _, permission := range permissions {
		if permission.Capability != capability {
			continue
		}
		declared = true
		if !containsScope(granted, permission.Scope) {
			return false
		}
	}
	return declared
}

// MissingCapabilities returns the capabilities of a table that a grant does not
// satisfy, in display order.
func MissingCapabilities(permissions []Permission, granted []string) []Capability {
	missing := []Capability{}
	for _, capability := range ProviderCapabilities {
		if HasCapability(permissions, granted, capability) {
			continue
		}
		missing = append(missing, capability)
	}
	return missing
}

// normalizeScope lowercases a scope and drops the Graph resource prefix, so a
// grant reported as "https://graph.microsoft.com/User.Read" and one reported as
// "User.Read" are recognised as the same permission.
func normalizeScope(scope string) string {
	value := strings.ToLower(strings.TrimSpace(scope))
	return strings.TrimPrefix(value, graphResource)
}

// graphResource is the resource prefix of a Microsoft Graph delegated scope. Both
// forms appear in token responses, so the scope matching above is prefix
// tolerant instead of the permission tables having to declare every variant.
const graphResource = "https://graph.microsoft.com/"

// containsScope reports whether a granted scope list holds a scope, ignoring case,
// surrounding whitespace and (for Microsoft Graph) the resource prefix.
func containsScope(granted []string, scope string) bool {
	wanted := normalizeScope(scope)
	for _, item := range granted {
		if normalizeScope(item) == wanted {
			return true
		}
	}
	return false
}

// AuthError is a refusal of the provider's authorization server that the
// operator can act on. Code is the OAuth error code (`access_denied`,
// `invalid_scope`, `consent_required`, …) and Description is the raw text the
// provider sent, which the UI only shows as technical detail.
type AuthError struct {
	Provider    models.ProviderName
	Code        string
	Description string
}

// Error renders a diagnostic message that never contains a token.
func (e *AuthError) Error() string {
	description := strings.TrimSpace(e.Description)
	if description == "" {
		return fmt.Sprintf("%s: the authorization server refused the request (%s)", e.Provider, e.Code)
	}
	return fmt.Sprintf("%s: the authorization server refused the request (%s): %s", e.Provider, e.Code, description)
}

// NewAuthError normalises an authorization-server refusal, dropping an empty or
// unhelpful description so the message stays readable in the UI.
func NewAuthError(provider models.ProviderName, code, description string) *AuthError {
	code = strings.TrimSpace(code)
	if code == "" {
		code = "unknown_error"
	}
	return &AuthError{
		Provider:    provider,
		Code:        code,
		Description: strings.TrimSpace(description),
	}
}

// consentCodes are the OAuth error codes meaning "the permissions were not
// granted": a missing tenant-wide admin consent, a denied prompt or a scope the
// application may not use. They all map to services.ErrConsent instead of the
// generic validation error the caller would otherwise report.
var consentCodes = map[string]bool{
	"access_denied":          true,
	"admin_consent_required": true,
	"consent_required":       true,
	"interaction_required":   true,
	"invalid_scope":          true,
	"unauthorized_client":    true,
}

// IsConsentRequired reports whether err is a provider refusal the operator fixes
// by granting the requested permissions (and, for Microsoft, the tenant-wide
// admin consent) rather than by retrying.
func IsConsentRequired(err error) bool {
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		return false
	}
	return consentCodes[strings.ToLower(strings.TrimSpace(authErr.Code))]
}

// ConsentGranter is implemented by providers whose permissions must be granted
// once for the whole tenant (the Microsoft Entra "admin consent" endpoint).
type ConsentGranter interface {
	// AdminConsentURL builds the tenant-wide consent URL for the credentials.
	AdminConsentURL(creds Credentials, state string) string
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
