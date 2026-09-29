package google

import (
	"fmt"
	"regexp"

	"github.com/ivancarlosti/sync/internal/providers"
)

// identifierPattern is the only shape of identifier this provider accepts in a
// request: the URL-safe alphabet Drive hands out (file, folder and shared drive
// ids are opaque base64url tokens) plus the synthetic ids Sync adds of its own —
// providers.DriveRoot ("root") and MyDrive ("my-drive").
//
// It excludes everything that could steer a request: a path separator or a
// relative segment (`/`, `..`), a query, fragment or userinfo delimiter (`?`,
// `#`, `@`, `:`), the percent-encoded form of any of those, a backslash, and
// whitespace or control characters (so an encoded newline cannot split a request
// line either). The length bound is generous next to the ~44 characters Drive
// returns today, while a stray value can never bloat a URL.
//
// A guard using this pattern must stay inline, applied to the very value that
// is interpolated into the request URL, because that is the shape a security
// scanner recognises as "the request can no longer be steered by the caller"
// (see plans/2026-09-29-code-scanning-alerts.md); invalidIdentifierError below
// only words the refusal.
var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)

// optionalIdentifierPattern is identifierPattern for the drive id position,
// which providers.Provider also accepts as an empty value meaning "the drive
// the account considers its default".
var optionalIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{0,512}$`)

// requestURLPattern is the only shape of URL this provider sends a request to:
// an absolute https URL on Google's API host, followed by a path, query and
// fragment drawn from the URL-safe alphabet.
//
// The host is pinned to a subdomain of `googleapis.com` — Google's own domain,
// the one the Drive API, the upload root and the resumable session URI of an
// upload all live under — so a request can never leave the provider it belongs
// to, whichever call site built it. The alphabet keeps a space, a control
// character (a newline splitting the request line), a backslash and a quote out
// of the URL as well.
//
// This is the second layer of the guard: the per-identifier patterns above
// refuse a bad id with a 400 before a URL is built at all, and this one is
// applied at every request site (doJSON, the download and the two legs of an
// upload), so a future call site that forgets its own guard still cannot steer a
// request away from Google. As with identifierPattern, a guard using this
// pattern must stay inline, applied to the very value handed to
// http.NewRequest*, because that is the shape a security scanner recognises.
var requestURLPattern = regexp.MustCompile(`^https://([A-Za-z0-9-]+\.)+googleapis\.com/[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%-]*$`)

// invalidIdentifierError reports a value that cannot have come from the Drive
// API. It wraps providers.ErrInvalidIdentifier, which the handlers map to a 400
// (internal/handlers/respond.go): the id comes from a stored job, a crafted
// explorer link or the engine, and the caller has to fix it instead of retrying.
func invalidIdentifierError(kind, value string) error {
	return fmt.Errorf("%w: google drive %s %q", providers.ErrInvalidIdentifier, kind, value)
}

// unsafeRequestURLError reports a request URL that is not one this provider may
// send: an identifier that reached the URL without being narrowed. It carries
// the same sentinel as an invalid identifier, so the caller is told which of
// its values is the problem instead of reading a 500.
func unsafeRequestURLError(kind, target string) error {
	return fmt.Errorf("%w: google drive %s %q is not a URL of the Drive API", providers.ErrInvalidIdentifier, kind, target)
}
