package microsoft

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	abstractions "github.com/microsoft/kiota-abstractions-go"
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

// Drives implements providers.Provider. The account's own OneDrive comes first,
// then every document library shared with the signed in user, which is exactly
// the set of roots a job may synchronise.
func (p *Provider) Drives(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) ([]providers.Drive, error) {
	client, err := p.graphClient(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	// The own drive is read on its own endpoint rather than picked out of the
	// collection: on a personal account it is the only entry the picker may
	// open (see ownDrive). It is also listed first, so the root a fresh job
	// falls back to is the one that answers.
	own, err := p.ownDrive(ctx, client)
	if err != nil {
		return nil, err
	}
	ownID := deref(own.GetId())

	page, err := client.Me().Drives().Get(ctx, &users.ItemDrivesRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemDrivesRequestBuilderGetQueryParameters{Select: driveSelect},
	})
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot list the drives of the account: %w", err)
	}
	list := make([]providers.Drive, 0, len(page.GetValue())+1)
	if ownID != "" {
		list = append(list, driveFromGraph(own))
	}
	for _, drive := range page.GetValue() {
		if ownID != "" {
			if deref(drive.GetId()) == ownID {
				continue
			}
			if isPersonalDrive(drive) {
				// A second personal drive of a personal account is never a
				// folder tree: see ownDrive.
				continue
			}
		}
		list = append(list, driveFromGraph(drive))
	}
	return list, nil
}

// ownDrive reads GET /me/drive, the drive the account actually stores files in.
//
// The collection endpoint cannot be used to find it: `GET /me/drives` lists every
// drive the account can see, and a recently created personal account (MSA) also
// exposes internal bookkeeping drives there — an archive named ODCMetadataArchive
// and the Bundle drives — which Microsoft tags with the very same driveType
// "personal". Their roots answer HTTP 400 `invalidRequest` with the message
// "ObjectHandle is Invalid", so a picker that takes the first personal drive of
// the collection lists a root that can never be opened. The singular endpoint
// always names the one drive that answers.
func (p *Provider) ownDrive(ctx context.Context, client *msgraphsdk.GraphServiceClient) (graphmodels.Driveable, error) {
	drive, err := client.Me().Drive().Get(ctx, &users.ItemDriveRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemDriveRequestBuilderGetQueryParameters{Select: driveSelect},
	})
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot read the OneDrive of the account: %w", err)
	}
	return drive, nil
}

// isPersonalDrive tells a personal account's own OneDrive from everything else.
// It reuses the mapping the folder picker groups by, so "personal", "Personal"
// and "PERSONAL" are recognised the same way here and there.
func isPersonalDrive(drive graphmodels.Driveable) bool {
	return driveFromGraph(drive).Kind == "personal"
}

// driveFromGraph converts a Graph drive into the provider neutral root.
//
// The driveType tells the two families apart. The account's own OneDrive is
// "personal" on a personal account and "business" on a work/school one; every
// SharePoint library is "documentLibrary". Folding the work OneDrive into
// document_library is exactly what made the picker show an empty "OneDrive"
// select and list the account's own drive under "SharePoint library" (see
// web/src/components/FolderPicker.vue).
func driveFromGraph(drive graphmodels.Driveable) providers.Drive {
	kind := "document_library"
	switch driveType := strings.ToLower(deref(drive.GetDriveType())); driveType {
	case "personal", "business":
		kind = driveType
	}
	return providers.Drive{
		ID:   deref(drive.GetId()),
		Name: deref(drive.GetName()),
		Kind: kind,
		// Owner carries the drive URL. Every SharePoint site calls its default
		// library "Documents", so the name alone cannot tell two of them apart;
		// the URL is what the picker labels them with (a searched site already
		// overwrites both fields with its own name and webUrl, see siteDrive).
		Owner: deref(drive.GetWebUrl()),
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

// SearchSites implements providers.SiteBrowser. SharePoint libraries are reached
// through the delegated, access-scoped `GET /sites?search=…`; a plain `GET /sites`
// without `$search` is documented as application-only and surfaces at most the
// tenant root site, which is why an empty query searches for the wildcard "*"
// instead of listing the collection. A tenant that refuses the wildcard still
// gets the root site, and ResolveSite keeps a manual URL fallback for the
// libraries neither call returns.
func (p *Provider) SearchSites(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, query string) ([]providers.Drive, error) {
	client, adapter, err := p.graphSession(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	// A personal Microsoft account owns no site collection: its directory has no
	// `Microsoft.FileServices` address, and Graph answers /sites with "This API is
	// not supported for MSA accounts". Asking anyway would turn an empty picker
	// into a 502, so the account is recognised by its own drive (see ownDrive) and
	// answered with no libraries at all. The probe is best effort on purpose: a
	// failure to read it must not block a SharePoint search.
	if own, err := p.ownDrive(ctx, client); err == nil && isPersonalDrive(own) {
		return []providers.Drive{}, nil
	}

	query = strings.TrimSpace(query)
	if query == "" {
		// Every site the signed-in user can reach. Graph rejects the wildcard in
		// some tenants, so the plain collection (the tenant root site) is the
		// fallback rather than an error that would empty the picker.
		if drives, err := p.listSites(ctx, client, adapter, "*"); err == nil {
			return drives, nil
		}
	}
	return p.listSites(ctx, client, adapter, query)
}

// listSites runs one /sites listing and resolves the default document library of
// every hit. No $orderby is carried: a few SharePoint tenants reject the
// combination of $search and $orderby, and the picker does not depend on order.
func (p *Provider) listSites(ctx context.Context, client *msgraphsdk.GraphServiceClient, adapter abstractions.RequestAdapter, query string) ([]providers.Drive, error) {
	params := &sites.SitesRequestBuilderGetQueryParameters{Select: siteSelect}
	if query != "" {
		params.Search = ptr(query)
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

// ResolveSite implements providers.SiteResolver: it turns an operator supplied
// SharePoint location into the default document library of that site. It is the
// manual fallback of the picker, for the libraries a keyword search does not
// surface. The location is normalised into a Graph site id (see
// siteIDFromLocation) and read through GET /sites/{id}.
func (p *Provider) ResolveSite(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, location string) (providers.Drive, error) {
	siteID, err := siteIDFromLocation(location)
	if err != nil {
		return providers.Drive{}, err
	}
	client, err := p.graphClient(tokens)
	if err != nil {
		return providers.Drive{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	site, err := client.Sites().BySiteId(siteID).Get(ctx, &sites.SiteItemRequestBuilderGetRequestConfiguration{
		QueryParameters: &sites.SiteItemRequestBuilderGetQueryParameters{Select: siteSelect},
	})
	if err != nil {
		if p.IsNotFound(err) {
			return providers.Drive{}, fmt.Errorf("%w: microsoft graph site %s", providers.ErrNotFound, siteID)
		}
		return providers.Drive{}, fmt.Errorf("microsoft graph: cannot resolve site %s: %w", siteID, err)
	}
	return p.siteDrive(ctx, client, site)
}

// siteIDFromLocation normalises an operator supplied SharePoint location into the
// Graph site id that GET /sites/{id} understands. Accepted forms:
//
//	https://tenant.sharepoint.com/sites/marketing  → tenant.sharepoint.com:/sites/marketing
//	tenant.sharepoint.com:/sites/marketing         → unchanged (server-relative path id)
//	tenant.sharepoint.com,g1,g2                    → unchanged (Graph site id)
//	tenant.sharepoint.com                          → unchanged (the tenant root site)
//
// A URL that points deeper — a document library or a page — is truncated to its
// site collection root, because that is the resource a library belongs to.
// Anything else (an empty value, a keyword, a value carrying whitespace) is
// refused with providers.ErrInvalidIdentifier, so the caller answers 400 instead
// of interpolating an arbitrary string into a request.
func siteIDFromLocation(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("%w: a SharePoint URL is required", providers.ErrInvalidIdentifier)
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("%w: %q is not a SharePoint URL", providers.ErrInvalidIdentifier, value)
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", fmt.Errorf("%w: %q is not a valid URL", providers.ErrInvalidIdentifier, value)
		}
		host := strings.ToLower(parsed.Hostname())
		if host == "" {
			return "", fmt.Errorf("%w: %q has no host", providers.ErrInvalidIdentifier, value)
		}
		// EscapedPath keeps a percent-encoded site name (%20) intact, which is
		// the form Graph expects inside the composite site id.
		path := siteRootPath(strings.TrimRight(parsed.EscapedPath(), "/"))
		if path == "" {
			return host, nil
		}
		return host + ":" + path, nil
	}
	// Without a scheme the value is either a Graph site id (host,g1,g2), the
	// "host:/path" form SharePoint itself shows, or a bare host.
	if strings.ContainsAny(value, ":,") || strings.Contains(value, ".") {
		return value, nil
	}
	return "", fmt.Errorf("%w: %q is not a SharePoint URL", providers.ErrInvalidIdentifier, value)
}

// siteRootPath truncates a SharePoint URL path to its site collection root, so a
// URL copied from a document library or a page still resolves to its site:
// "/sites/marketing/Shared Documents/Forms/AllItems.aspx" → "/sites/marketing".
// The well known collections are /sites, /teams and /personal (OneDrive); any
// other path is returned untouched.
func siteRootPath(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) >= 2 {
		switch strings.ToLower(segments[0]) {
		case "sites", "teams", "personal":
			return "/" + segments[0] + "/" + segments[1]
		}
	}
	return path
}
