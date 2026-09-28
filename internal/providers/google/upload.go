package google

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

// uploadTimeout bounds a whole file upload (initiation + transfer). The engine
// additionally bounds every run, so this is only a safety net.
const uploadTimeout = 30 * time.Minute

// Upload implements providers.Provider using a resumable session, which has no
// size limit (unlike the 5 MB multipart endpoint).
func (p *Provider) Upload(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, request providers.UploadRequest) (*providers.Item, error) {
	mimeType := request.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	parentID := request.ParentID
	if parentID == "" {
		parentID = providers.DriveRoot
	}
	item, err := p.upload(ctx, tokens, request.DriveID, parentID, request.ExistingID,
		request.Name, mimeType, request.ModifiedAt, request.Size, request.Body)
	if err != nil {
		if p.IsNotFound(err) {
			return nil, fmt.Errorf("%w: google drive upload target %s", providers.ErrNotFound, request.ExistingID)
		}
		return nil, err
	}
	return item, nil
}

// upload performs the two-step resumable transfer: the initiation request
// carries the metadata and returns a session URI, which then receives the bytes.
func (p *Provider) upload(ctx context.Context, tokens *providers.Tokens, driveID, parentID, itemID, name, mimeType string, modified time.Time, size int64, body io.Reader) (*providers.Item, error) {
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	client := p.client(tokens)

	metadata := map[string]any{"name": name}
	if !modified.IsZero() {
		metadata["modifiedTime"] = modified.UTC().Format(time.RFC3339)
	}
	// A file keeps its parent when it is updated: parents are only sent when a
	// new file is created.
	if itemID == "" && parentID != "" {
		metadata["parents"] = []string{parentID}
	}

	target := uploadBase + "/files"
	method := http.MethodPost
	if itemID != "" {
		target += "/" + itemID
		method = http.MethodPatch
	}
	target = withParam(withParam(withParam(withParam(target, "uploadType", "resumable"),
		"supportsAllDrives", "true"), "fields", itemFields), "driveId", driveIDOrNone(driveID, MyDrive))

	payload, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("google drive: cannot encode upload metadata: %w", err)
	}
	initRequest, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("google drive: cannot build upload request: %w", err)
	}
	initRequest.Header.Set("Content-Type", "application/json; charset=UTF-8")
	initRequest.Header.Set("X-Upload-Content-Type", mimeType)
	if size > 0 {
		initRequest.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
	}

	response, err := client.Do(initRequest)
	if err != nil {
		return nil, fmt.Errorf("google drive: starting the upload of %q failed: %w", name, err)
	}
	session := response.Header.Get("Location")
	if response.StatusCode < 200 || response.StatusCode > 299 {
		apiErr := decodeError(response)
		response.Body.Close()
		return nil, apiErr
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if session == "" {
		return nil, fmt.Errorf("google drive: the upload session for %q was not returned", name)
	}

	putRequest, err := http.NewRequestWithContext(ctx, http.MethodPut, session, body)
	if err != nil {
		return nil, fmt.Errorf("google drive: cannot build the upload session request: %w", err)
	}
	putRequest.Header.Set("Content-Type", mimeType)
	if size > 0 {
		putRequest.ContentLength = size
	}
	uploadResponse, err := client.Do(putRequest)
	if err != nil {
		return nil, fmt.Errorf("google drive: uploading %q failed: %w", name, err)
	}
	defer uploadResponse.Body.Close()
	if uploadResponse.StatusCode < 200 || uploadResponse.StatusCode > 299 {
		return nil, decodeError(uploadResponse)
	}
	var stored driveFile
	if err := json.NewDecoder(uploadResponse.Body).Decode(&stored); err != nil {
		return nil, fmt.Errorf("google drive: cannot decode the upload response for %q: %w", name, err)
	}
	item := stored.item()
	return &item, nil
}

// CreateFolder implements providers.Provider.
func (p *Provider) CreateFolder(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, driveID, parentID, name string) (*providers.Item, error) {
	if parentID == "" {
		parentID = providers.DriveRoot
	}
	target := listURL("/files", itemFields)
	if driveID != "" && driveID != MyDrive {
		target = withParam(target, "driveId", driveID)
	}
	payload := map[string]any{
		"name":     name,
		"mimeType": folderMimeType,
		"parents":  []string{parentID},
	}
	var stored driveFile
	if err := p.doJSON(ctx, p.client(tokens), http.MethodPost, target, payload, &stored); err != nil {
		return nil, err
	}
	item := stored.item()
	return &item, nil
}

// driveIDOrNone returns the drive id to send, or an empty string when the
// synthetic "My Drive"/"root" identifiers are used.
func driveIDOrNone(driveID, myDrive string) string {
	if driveID == "" || driveID == providers.DriveRoot || driveID == myDrive {
		return ""
	}
	return driveID
}
