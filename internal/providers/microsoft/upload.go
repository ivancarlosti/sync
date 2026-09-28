package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	serializationjson "github.com/microsoft/kiota-serialization-json-go"
	graphmodels "github.com/microsoftgraph/msgraph-sdk-go/models"

	"github.com/ivancarlosti/sync/internal/providers"
)

// Upload implements providers.Provider. A file up to simpleUploadLimit travels
// in one PUT; anything larger goes through a resumable upload session, which is
// the only shape in which Graph accepts a multi-gigabyte body.
func (p *Provider) Upload(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, request providers.UploadRequest) (*providers.Item, error) {
	if strings.TrimSpace(request.DriveID) == "" {
		return nil, fmt.Errorf("microsoft graph: a drive id is required to upload a file")
	}
	if strings.TrimSpace(request.Name) == "" {
		return nil, fmt.Errorf("microsoft graph: a file name is required")
	}
	if request.Body == nil {
		return nil, fmt.Errorf("microsoft graph: %q has no content to upload", request.Name)
	}
	// The size is needed twice: to choose the strategy and to build the
	// Content-Range headers of a session.
	if request.Size < 0 {
		return nil, fmt.Errorf("microsoft graph: the size of %q must be known before uploading", request.Name)
	}
	if err := requireToken(tokens); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()

	if request.Size <= simpleUploadLimit {
		return p.simpleUpload(ctx, tokens, request)
	}
	return p.sessionUpload(ctx, tokens, request)
}

// simpleUpload sends the whole body in one PUT and decodes the stored item from
// the response, so the caller learns the new id without a second round trip.
func (p *Provider) simpleUpload(ctx context.Context, tokens *providers.Tokens, request providers.UploadRequest) (*providers.Item, error) {
	target := contentURL(request.DriveID, request.ParentID, request.Name) + "/content"
	if request.ExistingID != "" {
		target = itemURL(request.DriveID, request.ExistingID) + "/content"
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPut, target, request.Body)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot build the upload request for %q: %w", request.Name, err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	httpRequest.Header.Set("Content-Type", uploadMimeType(request))
	httpRequest.ContentLength = request.Size

	response, err := p.streaming.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: uploading %q failed: %w", request.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
			return nil, fmt.Errorf("%w: microsoft graph upload target %s", providers.ErrNotFound, request.Name)
		}
		return nil, decodeGraphError(response)
	}
	stored, err := parseDriveItem(response.Body)
	if err != nil {
		return nil, err
	}
	return p.finishUpload(ctx, tokens, request, stored), nil
}

// sessionUpload opens a resumable session and pushes the body through it in
// uploadChunkSize slices.
func (p *Provider) sessionUpload(ctx context.Context, tokens *providers.Tokens, request providers.UploadRequest) (*providers.Item, error) {
	session, err := p.createUploadSession(ctx, tokens, request)
	if err != nil {
		return nil, err
	}

	buffer := make([]byte, uploadChunkSize)
	var stored graphmodels.DriveItemable
	for offset := int64(0); offset < request.Size; {
		length := int64(len(buffer))
		if remaining := request.Size - offset; remaining < length {
			length = remaining
		}
		if _, err := io.ReadFull(request.Body, buffer[:length]); err != nil {
			return nil, fmt.Errorf("microsoft graph: the upload of %q ended after %d of %d bytes: %w",
				request.Name, offset, request.Size, err)
		}
		chunk, err := p.putChunk(ctx, session, buffer[:length], offset, request.Size)
		if err != nil {
			return nil, err
		}
		if chunk != nil {
			stored = chunk
		}
		offset += length
	}
	if stored == nil {
		return nil, fmt.Errorf("microsoft graph: the upload session of %q returned no item", request.Name)
	}
	return p.finishUpload(ctx, tokens, request, stored), nil
}

// createUploadSession registers the target and returns the pre-authenticated URL
// that receives the bytes.
func (p *Provider) createUploadSession(ctx context.Context, tokens *providers.Tokens, request providers.UploadRequest) (string, error) {
	target := contentURL(request.DriveID, request.ParentID, request.Name) + "/createUploadSession"
	item := map[string]any{"@microsoft.graph.conflictBehavior": "replace"}
	if request.ExistingID != "" {
		target = itemURL(request.DriveID, request.ExistingID) + "/createUploadSession"
	} else {
		item["name"] = request.Name
	}
	payload, err := json.Marshal(map[string]any{"item": item})
	if err != nil {
		return "", fmt.Errorf("microsoft graph: cannot encode the upload session for %q: %w", request.Name, err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("microsoft graph: cannot build the upload session request for %q: %w", request.Name, err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	httpRequest.Header.Set("Content-Type", "application/json")

	response, err := p.streaming.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("microsoft graph: starting the upload of %q failed: %w", request.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", decodeGraphError(response)
	}
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&session); err != nil {
		return "", fmt.Errorf("microsoft graph: cannot decode the upload session for %q: %w", request.Name, err)
	}
	if session.UploadURL == "" {
		return "", fmt.Errorf("microsoft graph: the upload session for %q returned no URL", request.Name)
	}
	return session.UploadURL, nil
}

// putChunk sends one slice of the file. Graph answers 202 while more bytes are
// expected and 200/201 with the stored item once the session is complete. The
// session URL is pre-authenticated, so no bearer token travels with it.
func (p *Provider) putChunk(ctx context.Context, session string, chunk []byte, offset, total int64) (graphmodels.DriveItemable, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPut, session, bytes.NewReader(chunk))
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot build an upload chunk request: %w", err)
	}
	httpRequest.ContentLength = int64(len(chunk))
	httpRequest.Header.Set("Content-Range",
		fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(len(chunk))-1, total))

	response, err := p.streaming.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: sending bytes %d-%d failed: %w",
			offset, offset+int64(len(chunk))-1, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return parseDriveItem(response.Body)
	case http.StatusAccepted:
		// More bytes are expected; the body only carries the expected ranges.
		_, _ = io.Copy(io.Discard, response.Body)
		return nil, nil
	default:
		return nil, decodeGraphError(response)
	}
}

// finishUpload converts the stored item and tries to stamp the source
// modification time on it. The stamp is best effort: a tenant that refuses the
// PATCH must not fail an upload whose bytes are already stored, so the timestamp
// Graph reports is kept in that case.
func (p *Provider) finishUpload(ctx context.Context, tokens *providers.Tokens, request providers.UploadRequest, stored graphmodels.DriveItemable) *providers.Item {
	item := itemFromDriveItem(stored)
	if request.ModifiedAt.IsZero() || item.ID == "" {
		return &item
	}
	if err := p.touch(ctx, tokens, request.DriveID, item.ID, request.ModifiedAt); err != nil {
		return &item
	}
	item.ModifiedAt = request.ModifiedAt.UTC()
	return &item
}

// parseDriveItem decodes a Graph item response through the SDK models, so the
// hand-built transfer requests still benefit from the generated schema.
func parseDriveItem(body io.Reader) (graphmodels.DriveItemable, error) {
	raw, err := io.ReadAll(io.LimitReader(body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot read the item response: %w", err)
	}
	node, err := serializationjson.NewJsonParseNodeFactory().GetRootParseNode("application/json", raw)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot parse the item response: %w", err)
	}
	value, err := node.GetObjectValue(graphmodels.CreateDriveItemFromDiscriminatorValue)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: unexpected item response: %w", err)
	}
	item, ok := value.(graphmodels.DriveItemable)
	if !ok {
		return nil, fmt.Errorf("microsoft graph: unexpected item response type %T", value)
	}
	return item, nil
}

// uploadMimeType falls back to the generic binary type, which Graph accepts for
// every file.
func uploadMimeType(request providers.UploadRequest) string {
	if request.MimeType != "" {
		return request.MimeType
	}
	return "application/octet-stream"
}
