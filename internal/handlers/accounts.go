package handlers

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/services"
)

// accountView is the API representation of a connected account. Token columns
// are never part of it: the model already hides them (`json:"-"`), and this view
// documents the contract (see docs/api.md).
type accountView struct {
	ID                uint     `json:"id"`
	Provider          string   `json:"provider"`
	ProviderAccountID string   `json:"provider_account_id"`
	Email             string   `json:"email"`
	DisplayName       string   `json:"display_name"`
	AvatarURL         string   `json:"avatar_url"`
	Scopes            []string `json:"scopes"`
	// Capabilities lists the features the stored grant satisfies, and
	// MissingCapabilities the ones the provider now requests but this grant does
	// not cover, so the UI can explain what a reconnection adds.
	Capabilities        []providers.Capability `json:"capabilities"`
	MissingCapabilities []providers.Capability `json:"missing_capabilities"`
	// NeedsReconnect is true when the account was connected with fewer
	// permissions than the provider asks for today.
	NeedsReconnect bool       `json:"needs_reconnect"`
	Status         string     `json:"status"`
	LastError      string     `json:"last_error,omitempty"`
	ExpiresAt      time.Time  `json:"expires_at"`
	RefreshedAt    *time.Time `json:"refreshed_at,omitempty"`
	LastSyncedAt   *time.Time `json:"last_synced_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// driveView is one root the account can synchronise.
type driveView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Kind  string `json:"kind,omitempty"`
	Owner string `json:"owner,omitempty"`
}

// itemView is one entry of a folder listing. It is intentionally smaller than
// providers.Item: the browser only needs what it displays.
type itemView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	IsDir      bool      `json:"is_dir"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	MimeType   string    `json:"mime_type,omitempty"`
	Hash       string    `json:"hash,omitempty"`
	// Unsupported marks the items the sync engine skips in V1 (native Google
	// documents and shortcuts), so the browser can grey them out.
	Unsupported bool `json:"unsupported"`
}

// newAccountView converts a stored account into its API representation.
// permissions is the permission table of its provider (providers.Catalog); when
// it is missing the capability lists stay empty instead of guessing.
func newAccountView(account models.ConnectedAccount, permissions []providers.Permission) accountView {
	granted := providers.SplitScopes(account.Scopes)
	view := accountView{
		ID:                  account.ID,
		Provider:            account.Provider,
		ProviderAccountID:   account.ProviderAccountID,
		Email:               account.Email,
		DisplayName:         account.DisplayName,
		AvatarURL:           account.AvatarURL,
		Scopes:              granted,
		Capabilities:        []providers.Capability{},
		MissingCapabilities: []providers.Capability{},
		Status:              account.Status,
		LastError:           account.LastError,
		ExpiresAt:           account.ExpiresAt,
		RefreshedAt:         account.RefreshedAt,
		LastSyncedAt:        account.LastSyncedAt,
		CreatedAt:           account.CreatedAt,
		UpdatedAt:           account.UpdatedAt,
	}
	if permissions == nil {
		return view
	}
	view.Capabilities = providers.CapabilitiesOf(permissions, granted)
	view.MissingCapabilities = providers.MissingCapabilities(permissions, granted)
	view.NeedsReconnect = len(view.MissingCapabilities) > 0
	return view
}

// permissionsFor returns the permission table of a provider, so the API can
// report what a stored grant covers. A provider this build does not ship (a row
// left behind by another build) yields nil.
func (s *Server) permissionsFor(provider string) []providers.Permission {
	implementation, err := s.deps.Registry.Get(models.ProviderName(provider))
	if err != nil {
		return nil
	}
	return providers.Catalog(implementation)
}

// newItemView converts a provider item, marking the types V1 does not transfer.
func newItemView(item providers.Item) itemView {
	return itemView{
		ID:          item.ID,
		Name:        item.Name,
		IsDir:       item.IsDir,
		Size:        item.Size,
		ModifiedAt:  item.ModifiedAt,
		MimeType:    item.MimeType,
		Hash:        item.Hash,
		Unsupported: item.NativeDoc || item.Shortcut,
	}
}

// handleListAccounts answers GET /api/accounts.
func (s *Server) handleListAccounts(c *gin.Context) {
	accounts, err := s.deps.Store.ListAccounts(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		views = append(views, newAccountView(account, s.permissionsFor(account.Provider)))
	}
	c.JSON(http.StatusOK, gin.H{"accounts": views})
}

// handleDeleteAccount answers DELETE /api/accounts/:id. Deleting an account also
// removes the jobs that used it, so the response reports how many were dropped:
// the SPA warns the operator before the fact and confirms it afterwards.
func (s *Server) handleDeleteAccount(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	jobs, err := s.deps.Store.DeleteAccount(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "deleted_jobs": jobs})
}

// handleVerifyAccount answers POST /api/accounts/:id/verify: it resolves a valid
// access token (refreshing it when needed) and reads the remote identity, which
// proves the stored credentials still work. It is the "test this account" button
// of the accounts screen.
//
// The identity the provider returns is written back: a renamed or rebranded
// profile would otherwise keep showing the name captured when the account was
// connected, on the very card the operator just tested.
func (s *Server) handleVerifyAccount(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var account *models.ConnectedAccount
	err := s.deps.Tokens.Use(c.Request.Context(), id, func(session services.ProviderSession) error {
		remote, err := session.Provider.Account(c.Request.Context(), session.Credentials, session.Tokens)
		if err != nil {
			return err
		}
		account = session.Account
		refreshIdentity(account, remote)
		return s.deps.Store.SaveAccount(c.Request.Context(), account)
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, newAccountView(*account, s.permissionsFor(account.Provider)))
}

// refreshIdentity copies the remote profile onto the stored account. The account
// id and the tokens are not touched: this is about what the card displays.
func refreshIdentity(account *models.ConnectedAccount, remote *providers.Account) {
	if remote.Email != "" {
		account.Email = remote.Email
	}
	if remote.Name != "" {
		account.DisplayName = remote.Name
	}
	if remote.AvatarURL != "" {
		account.AvatarURL = remote.AvatarURL
	}
}

// handleAccountDrives answers GET /api/accounts/:id/drives.
func (s *Server) handleAccountDrives(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	drives := []driveView{}
	err := s.deps.Tokens.Use(c.Request.Context(), id, func(session services.ProviderSession) error {
		remote, err := session.Provider.Drives(c.Request.Context(), session.Credentials, session.Tokens)
		if err != nil {
			return err
		}
		drives = make([]driveView, 0, len(remote))
		for _, drive := range remote {
			drives = append(drives, driveView{ID: drive.ID, Name: drive.Name, Kind: drive.Kind, Owner: drive.Owner})
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"drives": drives})
}

// handleAccountItems answers GET /api/accounts/:id/drives/:drive/items: the
// folder browser of the job editor. `folder_id` defaults to the root of the
// drive; the answer lists folders first, then files, both alphabetically.
func (s *Server) handleAccountItems(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	driveID := strings.TrimSpace(c.Param("drive"))
	if driveID == "" {
		driveID = providers.DriveRoot
	}
	folderID := strings.TrimSpace(c.Query("folder_id"))
	if folderID == "" {
		folderID = providers.DriveRoot
	}

	items := []itemView{}
	err := s.deps.Tokens.Use(c.Request.Context(), id, func(session services.ProviderSession) error {
		remote, err := session.Provider.Children(c.Request.Context(), session.Credentials, session.Tokens, driveID, folderID)
		if err != nil {
			return err
		}
		items = make([]itemView, 0, len(remote))
		for _, item := range remote {
			items = append(items, newItemView(item))
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	c.JSON(http.StatusOK, gin.H{"drive_id": driveID, "folder_id": folderID, "items": items})
}

// handleAccountSites answers GET /api/accounts/:id/sites?q=…: it searches the
// extra drives of a provider (SharePoint document libraries for Microsoft) so an
// operator can synchronise a library outside the personal OneDrive. Providers
// without the SiteBrowser capability answer an empty list instead of an error.
func (s *Server) handleAccountSites(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	query := strings.TrimSpace(c.Query("q"))
	drives := []driveView{}
	err := s.deps.Tokens.Use(c.Request.Context(), id, func(session services.ProviderSession) error {
		browser, ok := s.deps.Registry.SiteBrowserFor(session.Provider.Name())
		if !ok {
			return nil
		}
		remote, err := browser.SearchSites(c.Request.Context(), session.Credentials, session.Tokens, query)
		if err != nil {
			return err
		}
		drives = make([]driveView, 0, len(remote))
		for _, drive := range remote {
			drives = append(drives, driveView{ID: drive.ID, Name: drive.Name, Kind: drive.Kind, Owner: drive.Owner})
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"sites": drives})
}
