package services

import "errors"

// Sentinel errors returned by the service layer. Handlers translate them into
// HTTP status codes (see handlers.statusFor) so a provider failure, a missing
// resource and a validation problem never have to be string-matched.
var (
	// ErrNotFound marks a resource that does not exist (or no longer does).
	ErrNotFound = errors.New("services: resource not found")
	// ErrValidation marks input the caller must fix.
	ErrValidation = errors.New("services: invalid input")
	// ErrNotConfigured marks a provider without usable credentials.
	ErrNotConfigured = errors.New("services: provider is not configured")
	// ErrUnauthorized marks a missing or invalid session.
	ErrUnauthorized = errors.New("services: authentication required")
	// ErrForbidden marks an authenticated identity that is not allowed in.
	ErrForbidden = errors.New("services: access denied")
	// ErrBusy marks a resource already in use (a job that is already running).
	ErrBusy = errors.New("services: resource is busy")
	// ErrReconnect marks a stored token the provider rejected: the operator has
	// to connect the account again.
	ErrReconnect = errors.New("services: account must be reconnected")
	// ErrConsent marks a provider refusal the operator fixes by granting the
	// requested permissions: a denied prompt, a scope the OAuth client may not
	// use, or — for Microsoft — a tenant-wide admin consent that was never
	// given. It maps to 412 with `code: "consent_required"`, which is what
	// tells the UI to offer the setup guide instead of "try again".
	ErrConsent = errors.New("services: provider permissions are missing")
)

// IsNotFound reports whether err is (or wraps) ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsConsentRequired reports whether err is (or wraps) ErrConsent.
func IsConsentRequired(err error) bool { return errors.Is(err, ErrConsent) }
