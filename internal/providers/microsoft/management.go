package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ivancarlosti/sync/internal/providers"
)

// The Admin operations of the Microsoft provider: copying a mailbox, copying the
// account's OneDrive into a SharePoint document library (optionally provisioning
// the site first), transferring calendars and reporting folder sizes.
//
// Graph answers these through plain REST verbs; the SDK builders cover the sync
// engine's metadata calls, but a management operation is a short, explicit
// sequence of calls whose raw payloads are easier to read (and to test) when they
// are issued directly. doGraph below carries the token, the timeout and the error
// mapping in one place, exactly as doJSON does for Google.
const (
	// mailboxCopyDefaultMax bounds a mailbox copy when the operator does not say
	// otherwise.
	mailboxCopyDefaultMax = 200
	// mailboxCopyMax is the hard ceiling of a single copy task.
	mailboxCopyMax = 5000
	// folderWalkMaxItems bounds a folder-size walk so a giant drive cannot turn
	// one task into millions of requests.
	folderWalkMaxItems = 20000
	// folderWalkMaxDepth bounds the recursion of the folder-size walk.
	folderWalkMaxDepth = 12
)

// Compile time guards: the operations registry hands these out as interfaces, so a
// drifted signature must fail the build rather than a task at runtime.
var (
	_ providers.MailboxCopier       = (*Provider)(nil)
	_ providers.CalendarTransferrer = (*Provider)(nil)
	_ providers.FolderSizeReporter  = (*Provider)(nil)
	_ providers.SiteContentCopier   = (*Provider)(nil)
)

// isGraphStatus reports whether err is a Graph answer with the given status.
// The Graph error shape (graphError) is declared in provider.go beside the raw
// transfer helpers it already serves.
func isGraphStatus(err error, status int) bool {
	var failure *graphError
	if !errors.As(err, &failure) {
		return false
	}
	return failure.Status == status
}

// doGraph issues one Graph call, decoding the answer into out (which may be nil).
// It is deliberately small: a bounded timeout, a Bearer token and the OData error
// mapping. A 202/204 with an empty body is a success.
func (p *Provider) doGraph(ctx context.Context, tokens *providers.Tokens, method, target string, body, out any) error {
	if !strings.HasPrefix(target, baseURL+"/") {
		return fmt.Errorf("microsoft graph: refusing a request outside the Graph host: %s", target)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("microsoft graph: cannot encode request body: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("microsoft graph: cannot build %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("microsoft graph: %s %s failed: %w", method, target, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return graphFailure(resp.StatusCode, raw)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("microsoft graph: cannot decode %s response: %w", method, err)
	}
	return nil
}

// graphFailure turns a non-2xx Graph answer into a *graphError, reading the OData
// error document when there is one.
func graphFailure(status int, raw []byte) error {
	failure := &graphError{Status: status, Message: strings.TrimSpace(string(raw))}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Code != "" {
		failure.Code = envelope.Error.Code
		failure.Message = envelope.Error.Message
	}
	return failure
}

// graphRaw reads one Graph page into memory, which the collection walkers decode
// twice (once for the page link, once for the items).
func (p *Provider) graphRaw(ctx context.Context, tokens *providers.Tokens, target string) (json.RawMessage, error) {
	if !strings.HasPrefix(target, baseURL+"/") {
		return nil, fmt.Errorf("microsoft graph: refusing a request outside the Graph host: %s", target)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot build a request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: GET %s failed: %w", target, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot read the response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, graphFailure(resp.StatusCode, raw)
	}
	return json.RawMessage(raw), nil
}

// graphList walks every page of a Graph collection, handing each page to visit
// and following @odata.nextLink until the collection is exhausted.
func (p *Provider) graphList(ctx context.Context, tokens *providers.Tokens, target string, visit func(page json.RawMessage) error) error {
	for target != "" {
		raw, err := p.graphRaw(ctx, tokens, target)
		if err != nil {
			return err
		}
		var page struct {
			NextLink string `json:"@odata.nextLink"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("microsoft graph: cannot read a page: %w", err)
		}
		if err := visit(raw); err != nil {
			return err
		}
		target = page.NextLink
	}
	return nil
}

// reportStep forwards a step to the caller, tolerating a nil progress func.
func reportStep(progress providers.ProgressFunc, name, status, detail string, items int, bytes int64) {
	if progress == nil {
		return
	}
	progress(providers.ManagementStep{Name: name, Status: status, Detail: detail, Items: items, Bytes: bytes})
}

// ownDriveID resolves the id of the signed-in account's own OneDrive.
func (p *Provider) ownDriveID(ctx context.Context, tokens *providers.Tokens) (string, error) {
	var drive struct {
		ID string `json:"id"`
	}
	if err := p.doGraph(ctx, tokens, http.MethodGet, baseURL+"/me/drive?$select=id", nil, &drive); err != nil {
		return "", err
	}
	if drive.ID == "" {
		return "", fmt.Errorf("microsoft graph: the account has no OneDrive")
	}
	return drive.ID, nil
}

// CopyMailbox implements providers.MailboxCopier: read the messages of the source
// mailbox and create each one in the target mailbox. Graph's own copy action only
// copies inside one mailbox, so a cross-mailbox copy is a read followed by a
// create; the subject, body and recipients are carried over, which is what the
// operator needs (attachments are reported as skipped, see docs/management.md).
func (p *Provider) CopyMailbox(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, input providers.CopyMailboxInput, progress providers.ProgressFunc) (providers.ManagementResult, error) {
	source := strings.TrimSpace(input.Source)
	if source == "" {
		source = "me"
	}
	target := strings.TrimSpace(input.Target)
	if target == "" {
		return providers.ManagementResult{}, fmt.Errorf("%w: a target mailbox is required", providers.ErrInvalidIdentifier)
	}
	max := input.Max
	if max <= 0 {
		max = mailboxCopyDefaultMax
	}
	if max > mailboxCopyMax {
		max = mailboxCopyMax
	}
	result := providers.ManagementResult{Data: map[string]string{"source": source, "target": target}}

	listURL := baseURL + "/users/" + graphSegment(source) + "/messages?$top=50&$select=id,subject,body,from,toRecipients,ccRecipients"
	copied := 0
	err := p.graphList(ctx, tokens, listURL, func(page json.RawMessage) error {
		var decoded struct {
			Value []graphMessage `json:"value"`
		}
		if err := json.Unmarshal(page, &decoded); err != nil {
			return fmt.Errorf("microsoft graph: cannot read the message page: %w", err)
		}
		for _, message := range decoded.Value {
			if copied >= max {
				return nil
			}
			body := map[string]any{"subject": message.Subject, "body": message.Body,
				"toRecipients": message.ToRecipients, "ccRecipients": message.CcRecipients}
			create := baseURL + "/users/" + graphSegment(target) + "/messages"
			if err := p.doGraph(ctx, tokens, http.MethodPost, create, body, nil); err != nil {
				reportStep(progress, message.Subject, providers.StepFailed, err.Error(), 0, 0)
				result.Data["failed"] = itoa(counterOr(result.Data["failed"]) + 1)
				continue
			}
			reportStep(progress, message.Subject, providers.StepSuccess, "copied", 1, 0)
			copied++
			result.Items++
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Message = fmt.Sprintf("copied %d message(s)", result.Items)
	return result, nil
}

// graphMessage is the subset of a Graph message a copy carries over.
type graphMessage struct {
	Subject      string          `json:"subject"`
	Body         json.RawMessage `json:"body"`
	ToRecipients json.RawMessage `json:"toRecipients"`
	CcRecipients json.RawMessage `json:"ccRecipients"`
}

// graphSegment escapes a value that goes into a Graph URL path segment.
func graphSegment(value string) string {
	return strings.TrimSpace(value)
}

// counterOr parses a small counter kept in the result details.
func counterOr(raw string) int {
	value := 0
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0
		}
		value = value*10 + int(char-'0')
	}
	return value
}

// itoa renders a small counter.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// TransferCalendars implements providers.CalendarTransferrer: read the events of
// the source calendar and create each one on the target, then delete the source
// copy. Graph cannot reassign an organiser, so recreating the event in the target
// mailbox is what makes it theirs; the delete turns the copy into a transfer.
func (p *Provider) TransferCalendars(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, input providers.CalendarTransferInput, progress providers.ProgressFunc) (providers.ManagementResult, error) {
	source := strings.TrimSpace(input.Source)
	if source == "" {
		source = "me"
	}
	target := strings.TrimSpace(input.Target)
	if target == "" {
		return providers.ManagementResult{}, fmt.Errorf("%w: a target mailbox is required", providers.ErrInvalidIdentifier)
	}
	result := providers.ManagementResult{Data: map[string]string{"target": target}}

	listURL := baseURL + "/users/" + graphSegment(source) + "/events?$top=100&$select=id,subject,body,start,end,location,attendees,isAllDay"
	err := p.graphList(ctx, tokens, listURL, func(page json.RawMessage) error {
		var decoded struct {
			Value []graphEvent `json:"value"`
		}
		if err := json.Unmarshal(page, &decoded); err != nil {
			return fmt.Errorf("microsoft graph: cannot read the event page: %w", err)
		}
		for _, event := range decoded.Value {
			create := baseURL + "/users/" + graphSegment(target) + "/events"
			if err := p.doGraph(ctx, tokens, http.MethodPost, create, event.createBody(), nil); err != nil {
				reportStep(progress, event.Subject, providers.StepFailed, err.Error(), 0, 0)
				result.Data["failed"] = itoa(counterOr(result.Data["failed"]) + 1)
				continue
			}
			deleteURL := listURL[:strings.Index(listURL, "/events")] + "/events/" + graphSegment(event.ID)
			if err := p.doGraph(ctx, tokens, http.MethodDelete, deleteURL, nil, nil); err != nil && !isGraphStatus(err, http.StatusNotFound) {
				reportStep(progress, event.Subject, providers.StepSkipped, "copied, but the source could not be removed: "+err.Error(), 1, 0)
			} else {
				reportStep(progress, event.Subject, providers.StepSuccess, "transferred", 1, 0)
			}
			result.Items++
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Message = fmt.Sprintf("transferred %d event(s)", result.Items)
	return result, nil
}

// graphEvent is the subset of a Graph event a transfer carries over.
type graphEvent struct {
	ID        string          `json:"id"`
	Subject   string          `json:"subject"`
	Body      json.RawMessage `json:"body"`
	Start     json.RawMessage `json:"start"`
	End       json.RawMessage `json:"end"`
	Location  json.RawMessage `json:"location"`
	Attendees json.RawMessage `json:"attendees"`
	IsAllDay  *bool           `json:"isAllDay"`
}

// createBody builds the POST /events payload of a transfer.
func (e graphEvent) createBody() map[string]any {
	body := map[string]any{"subject": e.Subject}
	if len(e.Body) > 0 {
		body["body"] = e.Body
	}
	if len(e.Start) > 0 {
		body["start"] = e.Start
	}
	if len(e.End) > 0 {
		body["end"] = e.End
	}
	if len(e.Location) > 0 {
		body["location"] = e.Location
	}
	if len(e.Attendees) > 0 {
		body["attendees"] = e.Attendees
	}
	if e.IsAllDay != nil {
		body["isAllDay"] = *e.IsAllDay
	}
	return body
}

// ReportFolderSizes implements providers.FolderSizeReporter: it walks the drive
// depth-first and reports the recursive size and file count of every folder. The
// walk is bounded (folderWalkMaxItems, folderWalkMaxDepth) so a giant library
// cannot turn one task into an unbounded crawl.
func (p *Provider) ReportFolderSizes(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, input providers.FolderSizeInput, progress providers.ProgressFunc) (providers.ManagementResult, error) {
	driveID := strings.TrimSpace(input.DriveID)
	if driveID == "" {
		own, err := p.ownDriveID(ctx, tokens)
		if err != nil {
			return providers.ManagementResult{}, err
		}
		driveID = own
	}
	result := providers.ManagementResult{Data: map[string]string{"drive_id": driveID}}
	walk := &folderWalk{limit: folderWalkMaxItems}
	bytes, files, err := p.walkFolder(ctx, tokens, driveID, providers.DriveRoot, "", 0, walk, progress)
	if err != nil {
		return result, err
	}
	result.Bytes = bytes
	result.Items = files
	result.Data["folders"] = itoa(walk.folders)
	if walk.capped {
		result.Data["truncated"] = "true"
	}
	result.Message = fmt.Sprintf("%d folder(s), %d file(s)", walk.folders, files)
	return result, nil
}

// folderWalk carries the shared counters of one folder-size walk.
type folderWalk struct {
	limit   int
	seen    int
	folders int
	capped  bool
}

// walkFolder sums one folder recursively, reporting a step per folder.
func (p *Provider) walkFolder(ctx context.Context, tokens *providers.Tokens, driveID, folderID, path string, depth int, walk *folderWalk, progress providers.ProgressFunc) (int64, int, error) {
	if depth > folderWalkMaxDepth {
		walk.capped = true
		return 0, 0, nil
	}
	driveItemPath := folderID
	if driveItemPath == "" {
		driveItemPath = providers.DriveRoot
	}
	url := baseURL + "/drives/" + graphSegment(driveID) + "/items/" + graphSegment(driveItemPath) + "/children?$top=200&$select=id,name,folder,file,size"
	var totalBytes int64
	var totalFiles int
	err := p.graphList(ctx, tokens, url, func(page json.RawMessage) error {
		var decoded struct {
			Value []graphChild `json:"value"`
		}
		if err := json.Unmarshal(page, &decoded); err != nil {
			return fmt.Errorf("microsoft graph: cannot read the folder page: %w", err)
		}
		for _, child := range decoded.Value {
			if walk.seen >= walk.limit {
				walk.capped = true
				return nil
			}
			walk.seen++
			childPath := path + "/" + child.Name
			if child.Folder != nil {
				walk.folders++
				bytes, files, err := p.walkFolder(ctx, tokens, driveID, child.ID, childPath, depth+1, walk, progress)
				if err != nil {
					reportStep(progress, childPath, providers.StepFailed, err.Error(), 0, 0)
					continue
				}
				reportStep(progress, childPath, providers.StepSuccess, "", files, bytes)
				totalBytes += bytes
				totalFiles += files
				continue
			}
			totalBytes += child.Size
			totalFiles++
		}
		return nil
	})
	return totalBytes, totalFiles, err
}

// graphChild is the subset of a Graph driveItem the folder-size walk reads.
type graphChild struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Folder json.RawMessage `json:"folder"`
	File   json.RawMessage `json:"file"`
	Size   int64           `json:"size"`
}

// CopyDriveToSite implements providers.SiteContentCopier: it copies the account's
// OneDrive into a SharePoint document library, provisioning the site first when
// the operator named one. Graph copies a folder with its subtree, so only the top
// level of the source is enumerated; the copy runs asynchronously and this
// operation reports each item as queued.
func (p *Provider) CopyDriveToSite(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, input providers.SiteCopyInput, progress providers.ProgressFunc) (providers.ManagementResult, error) {
	result := providers.ManagementResult{Data: map[string]string{}}

	targetDriveID := strings.TrimSpace(input.TargetDriveID)
	if targetDriveID == "" && strings.TrimSpace(input.SiteName) != "" {
		siteID, driveID, err := p.createSite(ctx, tokens, input.SiteName, input.SiteDisplayName)
		if err != nil {
			return result, err
		}
		targetDriveID = driveID
		result.Data["site_id"] = siteID
		reportStep(progress, "create site", providers.StepSuccess, siteID, 1, 0)
		result.Items++
	}
	if targetDriveID == "" {
		return result, fmt.Errorf("%w: a target drive or a site name is required", providers.ErrInvalidIdentifier)
	}
	result.Data["target_drive_id"] = targetDriveID

	targetFolderID := strings.TrimSpace(input.TargetFolderID)
	if targetFolderID == "" {
		var root struct {
			ID string `json:"id"`
		}
		if err := p.doGraph(ctx, tokens, http.MethodGet, baseURL+"/drives/"+graphSegment(targetDriveID)+"/root?$select=id", nil, &root); err != nil {
			return result, err
		}
		targetFolderID = root.ID
	}

	sourceDriveID, err := p.ownDriveID(ctx, tokens)
	if err != nil {
		return result, err
	}
	result.Data["source_drive_id"] = sourceDriveID

	listURL := baseURL + "/drives/" + graphSegment(sourceDriveID) + "/root/children?$top=200&$select=id,name,folder,size"
	err = p.graphList(ctx, tokens, listURL, func(page json.RawMessage) error {
		var decoded struct {
			Value []graphChild `json:"value"`
		}
		if err := json.Unmarshal(page, &decoded); err != nil {
			return fmt.Errorf("microsoft graph: cannot read the source page: %w", err)
		}
		for _, child := range decoded.Value {
			copyURL := baseURL + "/drives/" + graphSegment(sourceDriveID) + "/items/" + graphSegment(child.ID) + "/copy"
			body := map[string]any{"parentReference": map[string]string{"driveId": targetDriveID, "id": targetFolderID}}
			if err := p.doGraph(ctx, tokens, http.MethodPost, copyURL, body, nil); err != nil {
				reportStep(progress, child.Name, providers.StepFailed, err.Error(), 0, child.Size)
				result.Data["failed"] = itoa(counterOr(result.Data["failed"]) + 1)
				continue
			}
			reportStep(progress, child.Name, providers.StepSuccess, "copy queued", 1, child.Size)
			result.Items++
			result.Bytes += child.Size
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Message = fmt.Sprintf("queued %d item(s) for copy", result.Items)
	return result, nil
}

// createSite provisions a SharePoint site and resolves the document library Graph
// creates with it.
func (p *Provider) createSite(ctx context.Context, tokens *providers.Tokens, name, displayName string) (string, string, error) {
	name = strings.TrimSpace(name)
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = name
	}
	body := map[string]string{"name": name, "displayName": displayName}
	var site struct {
		ID string `json:"id"`
	}
	if err := p.doGraph(ctx, tokens, http.MethodPost, baseURL+"/sites", body, &site); err != nil {
		return "", "", fmt.Errorf("microsoft graph: creating the site %s failed: %w", name, err)
	}
	if site.ID == "" {
		return "", "", fmt.Errorf("microsoft graph: the new site has no id")
	}
	var drive struct {
		ID string `json:"id"`
	}
	if err := p.doGraph(ctx, tokens, http.MethodGet, baseURL+"/sites/"+graphSegment(site.ID)+"/drive?$select=id", nil, &drive); err != nil {
		return site.ID, "", fmt.Errorf("microsoft graph: resolving the site drive failed: %w", err)
	}
	if drive.ID == "" {
		return site.ID, "", fmt.Errorf("microsoft graph: the new site has no document library")
	}
	return site.ID, drive.ID, nil
}
