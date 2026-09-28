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
)

// IsNotFound reports whether err is (or wraps) ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
