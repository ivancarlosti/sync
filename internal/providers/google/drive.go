package google

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

// driveFile is the Drive API representation of a file or folder, reduced to the
// fields Sync requests through itemFields.
type driveFile struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	MimeType     string   `json:"mimeType"`
	ModifiedTime string   `json:"modifiedTime"`
	MD5          string   `json:"md5Checksum"`
	Parents      []string `json:"parents"`
	Trashed      bool     `json:"trashed"`
	DriveID      string   `json:"driveId"`
	Size         string   `json:"size"`
	Shortcut     *struct {
		TargetID       string `json:"targetId"`
		TargetMimeType string `json:"targetMimeType"`
	} `json:"shortcutDetails"`
}

// item converts the API payload into the provider-neutral item.
func (f driveFile) item() providers.Item {
	item := providers.Item{
		ID:       f.ID,
		Name:     f.Name,
		MimeType: f.MimeType,
		Hash:     f.MD5,
		IsDir:    f.MimeType == folderMimeType,
		NativeDoc: strings.HasPrefix(f.MimeType, providers.NativeMimePrefix) &&
			f.MimeType != folderMimeType,
		Shortcut: f.Shortcut != nil,
	}
	if len(f.Parents) > 0 {
		item.ParentID = f.Parents[0]
	}
	if f.Size != "" {
		if size, err := strconv.ParseInt(f.Size, 10, 64); err == nil {
			item.Size = size
		}
	}
	if f.ModifiedTime != "" {
		if parsed, err := time.Parse(time.RFC3339, f.ModifiedTime); err == nil {
			item.ModifiedAt = parsed.UTC()
		}
	}
	return item
}

// folderMimeType is the Drive mime type of a folder.
const folderMimeType = "application/vnd.google-apps.folder"

// withDriveScope adds the corpora parameters selecting the drive to browse. The
// synthetic MyDrive id (and an empty one) means "the user's own drive", which is
// the Drive default and needs no driveId.
func withDriveScope(raw, driveID string) string {
	if driveID == "" || driveID == providers.DriveRoot || driveID == MyDrive {
		return withParam(raw, "corpora", "user")
	}
	return withParam(withParam(raw, "corpora", "drive"), "driveId", driveID)
}

// rootParent maps the root folder of a drive onto the folder id the Drive API
// expects, either in a `parents` filter or in the `parents` list of new
// metadata. The "root" alias names the My Drive root only — Drive documents
// parents.isRoot as true for that folder alone — so the top level of a shared
// drive has to be addressed by the drive id, which is the id of the shared
// drive's root folder. Asking for "'root' in parents" of a shared drive
// therefore answers the children of My Drive, which is the bug this helper
// closes.
func rootParent(driveID, folderID string) string {
	if folderID != providers.DriveRoot {
		return folderID
	}
	if driveID == "" || driveID == providers.DriveRoot || driveID == MyDrive {
		return folderID
	}
	return driveID
}

// drivesFields is the projection requested when listing shared drives.
const drivesFields = "nextPageToken,drives(id,name)"

// Drives implements providers.Provider: "My Drive" plus every shared drive the
// authorised user can see.
func (p *Provider) Drives(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) ([]providers.Drive, error) {
	client := p.client(tokens)
	base := withParam(listURL("/drives", drivesFields), "pageSize", "100")

	drives := []providers.Drive{{ID: MyDrive, Name: "My Drive", Kind: "personal"}}
	pageToken := ""
	for {
		target := base
		if pageToken != "" {
			target = withParam(base, "pageToken", pageToken)
		}
		var page struct {
			NextPageToken string `json:"nextPageToken"`
			Drives        []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"drives"`
		}
		if err := p.doJSON(ctx, client, http.MethodGet, target, nil, &page); err != nil {
			return nil, err
		}
		for _, drive := range page.Drives {
			drives = append(drives, providers.Drive{ID: drive.ID, Name: drive.Name, Kind: "shared"})
		}
		pageToken = page.NextPageToken
		if pageToken == "" {
			return drives, nil
		}
	}
}

// Children implements providers.Provider, walking the pagination of the Drive
// list endpoint (1000 entries per page).
func (p *Provider) Children(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, driveID, folderID string) ([]providers.Item, error) {
	if folderID == "" {
		folderID = providers.DriveRoot
	}
	// Both ids are interpolated into the request built below — the folder into
	// the `q` filter, the drive into the corpus selection — so both are narrowed
	// here, in the function that uses them (see identifierPattern). A stored job
	// or a crafted explorer link cannot turn them into another path or host.
	if driveID != "" && !optionalIdentifierPattern.MatchString(driveID) {
		return nil, invalidIdentifierError("drive id", driveID)
	}
	// The alias is resolved before the guard below so the id interpolated into
	// the `q` filter is the one that is validated.
	folderID = rootParent(driveID, folderID)
	if !identifierPattern.MatchString(folderID) {
		return nil, invalidIdentifierError("folder id", folderID)
	}
	client := p.client(tokens)
	base := withDriveScope(listURL("/files", "nextPageToken,files("+itemFields+")"), driveID)
	base = withParam(base, "q", fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(folderID, "'", "\\'")))
	base = withParam(base, "pageSize", "1000")
	base = withParam(base, "orderBy", "folder,name")

	items := make([]providers.Item, 0, 64)
	pageToken := ""
	for {
		target := base
		if pageToken != "" {
			target = withParam(base, "pageToken", pageToken)
		}
		var page struct {
			NextPageToken string      `json:"nextPageToken"`
			Files         []driveFile `json:"files"`
		}
		if err := p.doJSON(ctx, client, http.MethodGet, target, nil, &page); err != nil {
			return nil, err
		}
		for _, file := range page.Files {
			if file.Trashed {
				continue
			}
			items = append(items, file.item())
		}
		pageToken = page.NextPageToken
		if pageToken == "" {
			return items, nil
		}
	}
}

// Item implements providers.Provider, resolving a single id. A vanished id is
// reported as providers.ErrNotFound so the engine records a deletion.
func (p *Provider) Item(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, driveID, itemID string) (*providers.Item, error) {
	// The item id becomes a path segment of the request URL below, so it is
	// narrowed here, in the function that interpolates it (see identifierPattern).
	if !identifierPattern.MatchString(itemID) {
		return nil, invalidIdentifierError("item id", itemID)
	}
	if driveID != "" && !optionalIdentifierPattern.MatchString(driveID) {
		return nil, invalidIdentifierError("drive id", driveID)
	}
	target := withParam(listURL("/files/"+itemID, itemFields), "supportsAllDrives", "true")
	if driveID != "" && driveID != MyDrive {
		target = withParam(target, "driveId", driveID)
	}
	var file driveFile
	if err := p.doJSON(ctx, p.client(tokens), http.MethodGet, target, nil, &file); err != nil {
		if p.IsNotFound(err) {
			return nil, fmt.Errorf("%w: google drive item %s", providers.ErrNotFound, itemID)
		}
		return nil, err
	}
	item := file.item()
	return &item, nil
}

// Download implements providers.Provider, streaming the binary content of a
// file. Native Google documents have no downloadable body and are reported as
// providers.ErrUnsupported (Sync V1 skips them during the scan).
func (p *Provider) Download(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, driveID, itemID string) (*providers.Transfer, error) {
	// The item id becomes a path segment of the request URL below, so it is
	// narrowed here, in the function that interpolates it (see identifierPattern).
	if !identifierPattern.MatchString(itemID) {
		return nil, invalidIdentifierError("item id", itemID)
	}
	if driveID != "" && !optionalIdentifierPattern.MatchString(driveID) {
		return nil, invalidIdentifierError("drive id", driveID)
	}
	target := withParam(apiBase+"/files/"+itemID, "alt", "media")
	target = withParam(target, "supportsAllDrives", "true")
	if driveID != "" && driveID != MyDrive {
		target = withParam(target, "driveId", driveID)
	}
	// This function issues its own request instead of going through doJSON, so
	// the URL is narrowed here too, in the function that sends it (see
	// requestURLPattern).
	if !requestURLPattern.MatchString(target) {
		return nil, unsafeRequestURLError("download URL", target)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("google drive: cannot build download request: %w", err)
	}
	req.Header.Set("Accept", "*/*")

	resp, err := p.client(tokens).Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("google drive: downloading %s failed: %w", itemID, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := decodeError(resp)
		resp.Body.Close()
		cancel()
		if p.IsNotFound(apiErr) {
			return nil, fmt.Errorf("%w: google drive item %s", providers.ErrNotFound, itemID)
		}
		var typed *apiError
		if asAPIError(apiErr, &typed) && typed.Status == http.StatusForbidden &&
			strings.Contains(typed.Reason, "notDownloadable") {
			return nil, fmt.Errorf("%w: google drive item %s cannot be downloaded", providers.ErrUnsupported, itemID)
		}
		return nil, apiErr
	}

	transfer := &providers.Transfer{
		Body:     &cancelReadCloser{inner: resp.Body, cancel: cancel},
		MimeType: resp.Header.Get("Content-Type"),
	}
	if resp.ContentLength > 0 {
		transfer.Size = resp.ContentLength
	}
	return transfer, nil
}

// Delete implements providers.Provider. Deleting an item that is already gone is
// not an error: the engine only needs the postcondition.
func (p *Provider) Delete(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, driveID, itemID string) error {
	// The item id becomes a path segment of the request URL below, so it is
	// narrowed here, in the function that interpolates it (see identifierPattern).
	if !identifierPattern.MatchString(itemID) {
		return invalidIdentifierError("item id", itemID)
	}
	if driveID != "" && !optionalIdentifierPattern.MatchString(driveID) {
		return invalidIdentifierError("drive id", driveID)
	}
	target := withParam(apiBase+"/files/"+itemID, "supportsAllDrives", "true")
	if driveID != "" && driveID != MyDrive {
		target = withParam(target, "driveId", driveID)
	}
	if err := p.doJSON(ctx, p.client(tokens), http.MethodDelete, target, nil, nil); err != nil {
		if p.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// cancelReadCloser releases the request context when the caller closes the
// stream, which stops the transfer as soon as the engine is done copying.
type cancelReadCloser struct {
	inner  io.ReadCloser
	cancel context.CancelFunc
}

// Read implements io.Reader.
func (c *cancelReadCloser) Read(p []byte) (int, error) { return c.inner.Read(p) }

// Close implements io.Closer.
func (c *cancelReadCloser) Close() error {
	err := c.inner.Close()
	c.cancel()
	return err
}
