package microsoft

import (
	"context"
	"fmt"
	"strings"
	"time"

	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	msgraphgocore "github.com/microsoftgraph/msgraph-sdk-go-core"
	"github.com/microsoftgraph/msgraph-sdk-go/drives"
	graphmodels "github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/sites"
	"github.com/microsoftgraph/msgraph-sdk-go/users"

	"github.com/ivancarlosti/sync/internal/providers"
)

// itemSelect is the projection requested by every listing: only the properties
// the sync engine consumes, which keeps large listings cheap.
var itemSelect = []string{
	"id", "name", "size", "file", "folder", "fileSystemInfo",
	"parentReference", "lastModifiedDateTime", "webUrl", "remoteItem",
}

// driveSelect is the projection used when listing drives.
var driveSelect = []string{"id", "name", "driveType", "webUrl"}

// siteSelect is the projection used when searching sites.
var siteSelect = []string{"id", "displayName", "name", "webUrl"}

// directoryMimeType is the synthetic mime type reported for folders. Graph has
// no mime type of its own for a folder (the "folder" facet marks it), so the
// IANA directory type is used, which is also what WebDAV reports.
const directoryMimeType = "inode/directory"

// normaliseItemID maps the well known root identifiers onto Graph's "root".
func normaliseItemID(itemID string) string {
	if itemID == "" || itemID == providers.DriveRoot {
		return providers.DriveRoot
	}
	return itemID
}

// itemFromDriveItem converts a Graph driveItem into the provider neutral item.
func itemFromDriveItem(driveItem graphmodels.DriveItemable) providers.Item {
	item := providers.Item{
		ID:       deref(driveItem.GetId()),
		Name:     deref(driveItem.GetName()),
		IsDir:    driveItem.GetFolder() != nil,
		Shortcut: driveItem.GetRemoteItem() != nil,
	}
	if item.IsDir {
		item.MimeType = directoryMimeType
	}
	if parent := driveItem.GetParentReference(); parent != nil {
		item.ParentID = deref(parent.GetId())
	}
	if size := driveItem.GetSize(); size != nil && *size > 0 {
		item.Size = *size
	}
	if modified := modifiedAt(driveItem); !modified.IsZero() {
		item.ModifiedAt = modified
	}
	if file := driveItem.GetFile(); file != nil {
		item.Hash = hashOf(file.GetHashes())
		if mime := deref(file.GetMimeType()); mime != "" {
			item.MimeType = mime
		}
	}
	return item
}

// hashOf prefers quickXorHash (the OneDrive and SharePoint hash) and falls back
// to sha1Hash, which some personal OneDrive items still carry.
func hashOf(hashes graphmodels.Hashesable) string {
	if hashes == nil {
		return ""
	}
	if quick := deref(hashes.GetQuickXorHash()); quick != "" {
		return quick
	}
	return deref(hashes.GetSha1Hash())
}

// modifiedAt prefers the file system timestamp — the moment the bytes actually
// changed, which is what the engine propagates between the two services — over
// SharePoint's "last modified" property, and returns the zero time when Graph
// reports neither.
func modifiedAt(driveItem graphmodels.DriveItemable) time.Time {
	if info := driveItem.GetFileSystemInfo(); info != nil {
		if stamp := info.GetLastModifiedDateTime(); stamp != nil {
			return stamp.UTC()
		}
	}
	if stamp := driveItem.GetLastModifiedDateTime(); stamp != nil {
		return stamp.UTC()
	}
	return time.Time{}
}

// Drives implements providers.Provider. /me/drives returns the personal OneDrive
// plus every document library shared with the signed in user, which is exactly
// the set of roots a job may synchronise.
func (p *Provider) Drives(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) ([]providers.Drive, error) {
	client, err := p.graphClient(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	page, err := client.Me().Drives().Get(ctx, &users.ItemDrivesRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemDrivesRequestBuilderGetQueryParameters{Select: driveSelect},
	})
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot list the drives of the account: %w", err)
	}
	list := make([]providers.Drive, 0, len(page.GetValue()))
	for _, drive := range page.GetValue() {
		list = append(list, driveFromGraph(drive))
	}
	return list, nil
}

// driveFromGraph converts a Graph drive into the provider neutral root.
func driveFromGraph(drive graphmodels.Driveable) providers.Drive {
	kind := "document_library"
	if strings.EqualFold(deref(drive.GetDriveType()), "personal") {
		kind = "personal"
	}
	return providers.Drive{
		ID:   deref(drive.GetId()),
		Name: deref(drive.GetName()),
		Kind: kind,
	}
}

// Children implements providers.Provider. The SDK page iterator follows the
// OData nextLink, so a folder holding more than one page is returned whole.
func (p *Provider) Children(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, driveID, folderID string) ([]providers.Item, error) {
	if strings.TrimSpace(driveID) == "" {
		return nil, fmt.Errorf("microsoft graph: a drive id is required to list children")
	}
	client, adapter, err := p.graphSession(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	folder := normaliseItemID(folderID)
	page, err := client.Drives().ByDriveId(driveID).Items().ByDriveItemId(folder).
		Children().Get(ctx, &drives.ItemItemsItemChildrenRequestBuilderGetRequestConfiguration{
		QueryParameters: &drives.ItemItemsItemChildrenRequestBuilderGetQueryParameters{
			// No $orderby: a few SharePoint libraries reject it, and the
			// engine does not depend on the order of a listing.
			Select: itemSelect,
			Top:    ptr(int32(200)),
		},
	})
	if err != nil {
		if p.IsNotFound(err) {
			return nil, fmt.Errorf("%w: microsoft graph folder %s", providers.ErrNotFound, folder)
		}
		return nil, fmt.Errorf("microsoft graph: cannot list the children of %s: %w", folder, err)
	}

	iterator, err := msgraphgocore.NewPageIterator[graphmodels.DriveItemable](page, adapter,
		graphmodels.CreateDriveItemFromDiscriminatorValue)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot page the children of %s: %w", folder, err)
	}
	items := make([]providers.Item, 0, len(page.GetValue()))
	if err := iterator.Iterate(ctx, func(child graphmodels.DriveItemable) bool {
		items = append(items, itemFromDriveItem(child))
		return true
	}); err != nil {
		return nil, fmt.Errorf("microsoft graph: listing the children of %s failed: %w", folder, err)
	}
	return items, nil
}

// Item implements providers.Provider.
func (p *Provider) Item(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, driveID, itemID string) (*providers.Item, error) {
	if strings.TrimSpace(driveID) == "" {
		return nil, fmt.Errorf("microsoft graph: a drive id is required to resolve an item")
	}
	client, err := p.graphClient(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	id := normaliseItemID(itemID)
	driveItem, err := client.Drives().ByDriveId(driveID).Items().ByDriveItemId(id).
		Get(ctx, &drives.ItemItemsDriveItemItemRequestBuilderGetRequestConfiguration{
			QueryParameters: &drives.ItemItemsDriveItemItemRequestBuilderGetQueryParameters{Select: itemSelect},
		})
	if err != nil {
		if p.IsNotFound(err) {
			return nil, fmt.Errorf("%w: microsoft graph item %s", providers.ErrNotFound, id)
		}
		return nil, fmt.Errorf("microsoft graph: cannot read item %s: %w", id, err)
	}
	item := itemFromDriveItem(driveItem)
	return &item, nil
}

// CreateFolder implements providers.Provider.
func (p *Provider) CreateFolder(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, driveID, parentID, name string) (*providers.Item, error) {
	if strings.TrimSpace(driveID) == "" {
		return nil, fmt.Errorf("microsoft graph: a drive id is required to create a folder")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("microsoft graph: a folder name is required")
	}
	client, err := p.graphClient(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	body := graphmodels.NewDriveItem()
	body.SetName(ptr(name))
	body.SetFolder(graphmodels.NewFolder())
	// "fail" keeps the sync engine authoritative: it checks for an existing
	// folder before creating one, so a conflict here is a real race rather than
	// something to paper over silently.
	body.SetAdditionalData(map[string]any{"@microsoft.graph.conflictBehavior": "fail"})

	parent := normaliseItemID(parentID)
	created, err := client.Drives().ByDriveId(driveID).Items().ByDriveItemId(parent).
		Children().Post(ctx, body, nil)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot create the folder %q inside %s: %w", name, parent, err)
	}
	item := itemFromDriveItem(created)
	return &item, nil
}

// Delete implements providers.Provider. A vanished item is not an error: the
// caller asked for it to be gone, and it is.
func (p *Provider) Delete(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, driveID, itemID string) error {
	if strings.TrimSpace(driveID) == "" {
		return fmt.Errorf("microsoft graph: a drive id is required to delete an item")
	}
	client, err := p.graphClient(tokens)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	id := normaliseItemID(itemID)
	if err := client.Drives().ByDriveId(driveID).Items().ByDriveItemId(id).Delete(ctx, nil); err != nil && !p.IsNotFound(err) {
		return fmt.Errorf("microsoft graph: cannot delete item %s: %w", id, err)
	}
	return nil
}

// touch stamps the file system lastModifiedDateTime of a freshly uploaded item.
// Uploading the bytes makes Graph record "now", so without this step a
// bidirectional job would see a newer destination file on the next pass and
// copy the same content back and forth forever.
func (p *Provider) touch(ctx context.Context, tokens *providers.Tokens, driveID, itemID string, modified time.Time) error {
	if modified.IsZero() || itemID == "" {
		return nil
	}
	client, err := p.graphClient(tokens)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	info := graphmodels.NewFileSystemInfo()
	info.SetLastModifiedDateTime(ptr(modified.UTC()))
	body := graphmodels.NewDriveItem()
	body.SetFileSystemInfo(info)

	if _, err := client.Drives().ByDriveId(driveID).Items().ByDriveItemId(itemID).Patch(ctx, body, nil); err != nil {
		return fmt.Errorf("microsoft graph: cannot set the timestamp of item %s: %w", itemID, err)
	}
	return nil
}

// SearchSites implements providers.SiteBrowser. Graph rejects an empty $search,
// so an empty query lists the root site of the tenant instead, which keeps the
// drive picker useful even before the operator types anything.
func (p *Provider) SearchSites(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, query string) ([]providers.Drive, error) {
	client, adapter, err := p.graphSession(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	params := &sites.SitesRequestBuilderGetQueryParameters{Select: siteSelect}
	if q := strings.TrimSpace(query); q != "" {
		// $search and $orderby cannot be combined on /sites.
		params.Search = ptr(q)
	}
	page, err := client.Sites().Get(ctx, &sites.SitesRequestBuilderGetRequestConfiguration{QueryParameters: params})
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot search sites: %w", err)
	}
	iterator, err := msgraphgocore.NewPageIterator[graphmodels.Siteable](page, adapter,
		graphmodels.CreateSiteFromDiscriminatorValue)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot page the site search results: %w", err)
	}

	list := make([]providers.Drive, 0, len(page.GetValue()))
	var failure error
	if err := iterator.Iterate(ctx, func(site graphmodels.Siteable) bool {
		drive, err := p.siteDrive(ctx, client, site)
		if err != nil {
			if p.IsNotFound(err) {
				// A site without a default library is simply not syncable.
				return true
			}
			failure = err
			return false
		}
		list = append(list, drive)
		return true
	}); err != nil {
		return nil, fmt.Errorf("microsoft graph: searching sites failed: %w", err)
	}
	if failure != nil {
		return nil, failure
	}
	return list, nil
}

// siteDrive resolves the default document library of a site, labelled with the
// site name so the operator can tell libraries apart.
func (p *Provider) siteDrive(ctx context.Context, client *msgraphsdk.GraphServiceClient, site graphmodels.Siteable) (providers.Drive, error) {
	siteID := deref(site.GetId())
	if siteID == "" {
		return providers.Drive{}, fmt.Errorf("%w: microsoft graph site without an id", providers.ErrNotFound)
	}
	drive, err := client.Sites().BySiteId(siteID).Drive().Get(ctx, &sites.ItemDriveRequestBuilderGetRequestConfiguration{
		QueryParameters: &sites.ItemDriveRequestBuilderGetQueryParameters{Select: driveSelect},
	})
	if err != nil {
		if p.IsNotFound(err) {
			return providers.Drive{}, fmt.Errorf("%w: microsoft graph site %s has no default library", providers.ErrNotFound, siteID)
		}
		return providers.Drive{}, fmt.Errorf("microsoft graph: cannot read the default library of site %s: %w", siteID, err)
	}
	result := driveFromGraph(drive)
	result.Kind = "site"
	result.Name = siteLabel(site)
	result.Owner = deref(site.GetWebUrl())
	return result, nil
}

// siteLabel picks the friendliest name a site exposes.
func siteLabel(site graphmodels.Siteable) string {
	if name := deref(site.GetDisplayName()); name != "" {
		return name
	}
	if name := deref(site.GetName()); name != "" {
		return name
	}
	return deref(site.GetWebUrl())
}
