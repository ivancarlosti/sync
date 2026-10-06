package microsoft

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

// maxTransferRedirects bounds the hops a pre-authenticated download URL may
// take before the transfer is abandoned.
const maxTransferRedirects = 5

// stripTokenOnHostChange is the redirect policy of the byte transfers. The
// /content endpoint answers with a short lived, pre-authenticated redirect to
// the tenant storage host; that hop must be followed, but the bearer token must
// not travel with it.
func stripTokenOnHostChange(request *http.Request, via []*http.Request) error {
	if len(via) >= maxTransferRedirects {
		return fmt.Errorf("microsoft graph: more than %d redirects while transferring bytes", maxTransferRedirects)
	}
	if len(via) > 0 && !strings.EqualFold(request.URL.Host, via[0].URL.Host) {
		request.Header.Del("Authorization")
	}
	return nil
}

// Download implements providers.Provider by streaming /content straight to the
// caller. No deadline is applied here: the sync engine bounds a whole run and a
// large file may legitimately take minutes.
func (p *Provider) Download(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, driveID, itemID string) (*providers.Transfer, error) {
	if strings.TrimSpace(driveID) == "" {
		return nil, fmt.Errorf("microsoft graph: a drive id is required to download an item")
	}
	if err := requireToken(tokens); err != nil {
		return nil, err
	}

	id := normaliseItemID(itemID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		itemURL(driveID, id)+"/content", nil)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot build the download request for %s: %w", id, err)
	}
	request.Header.Set("Authorization", "Bearer "+tokens.AccessToken)

	// Graph answers a throttle with 429 and a Retry-After, which the default
	// retry policy understands (see providers.DoWithRetry). The metadata calls
	// already retry through Kiota's middleware; this is the byte transfer, which
	// the streaming client issues itself.
	response, err := providers.DoWithRetry(ctx, providers.RetryPolicy{},
		func(ctx context.Context) (*http.Response, error) {
			return p.streaming.Do(request.Clone(ctx))
		})
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: downloading item %s failed: %w", id, err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		defer response.Body.Close()
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
			return nil, fmt.Errorf("%w: microsoft graph item %s", providers.ErrNotFound, id)
		}
		return nil, decodeGraphError(response)
	}

	size := response.ContentLength
	if size < 0 {
		size = 0
	}
	return &providers.Transfer{
		Body:       response.Body,
		Size:       size,
		MimeType:   mediaType(response.Header.Get("Content-Type")),
		ModifiedAt: lastModified(response.Header),
	}, nil
}

// itemURL builds the Graph URL of a single item.
func itemURL(driveID, itemID string) string {
	return fmt.Sprintf("%s/drives/%s/items/%s", baseURL, pathSegment(driveID), pathSegment(itemID))
}

// contentURL builds the URL of a file addressed by its path, which is how a new
// file is created inside a folder ("/items/{parent}:/{name}:").
func contentURL(driveID, parentID, name string) string {
	return fmt.Sprintf("%s/drives/%s/items/%s:/%s:", baseURL, pathSegment(driveID),
		pathSegment(normaliseItemID(parentID)), pathSegment(name))
}

// pathEscaper escapes exactly the characters that would change the meaning of a
// Graph path. A full url.PathEscape is deliberately avoided: provider ids are
// opaque and commonly contain characters such as "!", which Graph expects
// verbatim ("items/b!xyz"), while "%", "/", "?", "#", ":" and whitespace must
// never reach the path unescaped.
var pathEscaper = strings.NewReplacer(
	"%", "%25",
	"/", "%2F",
	"\\", "%5C",
	"?", "%3F",
	"#", "%23",
	":", "%3A",
	" ", "%20",
	"\t", "%09",
	"\n", "%0A",
	"\r", "%0D",
)

// pathSegment makes one value safe to place inside a Graph path.
func pathSegment(value string) string { return pathEscaper.Replace(value) }

// requireToken rejects a call that would otherwise fail with an opaque 401.
func requireToken(tokens *providers.Tokens) error {
	if tokens == nil || tokens.AccessToken == "" {
		return fmt.Errorf("microsoft graph: the account has no access token (reconnect it)")
	}
	return nil
}

// mediaType strips the parameters of a Content-Type header.
func mediaType(contentType string) string {
	if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
		return parsed
	}
	return ""
}

// lastModified reads the Last-Modified header of a download response.
func lastModified(header http.Header) time.Time {
	stamp, err := http.ParseTime(header.Get("Last-Modified"))
	if err != nil {
		return time.Time{}
	}
	return stamp.UTC()
}
