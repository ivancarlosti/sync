package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/services"
)

// Error codes returned beside the human readable message. The SPA translates
// the code with i18n and shows the message as technical detail, so the UI never
// has to match on an English string (see docs/i18n.md).
const (
	codeValidation   = "validation"
	codeNotFound     = "not_found"
	codeUnauthorized = "unauthorized"
	codeForbidden    = "forbidden"
	codeBusy         = "busy"
	codeReconnect    = "reconnect"
	// codeConsentRequired means the provider permissions were never granted (or
	// the tenant-wide admin consent is missing). The UI answers with the setup
	// guide link instead of a retry button.
	codeConsentRequired = "consent_required"
	codeNotConfigured   = "not_configured"
	codeInternal        = "internal"
	// codeDeliveryFailed is specific to the notification test endpoint: the
	// channel is valid but the remote end refused it, so the operator needs the
	// reason (codeInternal would hide it behind "check the server logs").
	codeDeliveryFailed = "delivery_failed"
)

// errorBody is the JSON shape of every error answer.
type errorBody struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// statusFor maps a service sentinel error to the HTTP status the API answers
// with. Keeping the mapping in one place means no handler ever string-matches an
// error message.
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, services.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, services.ErrValidation):
		return http.StatusBadRequest
	case errors.Is(err, services.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, services.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, services.ErrBusy):
		return http.StatusConflict
	case errors.Is(err, services.ErrReconnect):
		// The request is understood but cannot be satisfied until the operator
		// connects the account again.
		return http.StatusPreconditionFailed
	case errors.Is(err, services.ErrConsent):
		// Same family as ErrReconnect: the request is understood, and the fix
		// is an action in the provider console, not a retry of the request.
		return http.StatusPreconditionFailed
	case errors.Is(err, services.ErrNotConfigured):
		return http.StatusFailedDependency
	case errors.Is(err, http.ErrHandlerTimeout):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// codeFor is the machine readable counterpart of statusFor.
func codeFor(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, services.ErrNotFound):
		return codeNotFound
	case errors.Is(err, services.ErrValidation):
		return codeValidation
	case errors.Is(err, services.ErrUnauthorized):
		return codeUnauthorized
	case errors.Is(err, services.ErrForbidden):
		return codeForbidden
	case errors.Is(err, services.ErrBusy):
		return codeBusy
	case errors.Is(err, services.ErrReconnect):
		return codeReconnect
	case errors.Is(err, services.ErrConsent):
		return codeConsentRequired
	case errors.Is(err, services.ErrNotConfigured):
		return codeNotConfigured
	default:
		return codeInternal
	}
}

// fail answers with the status that matches err and aborts the chain. Server
// side errors are logged in full and reported to the client without their
// details: a database or provider message may mention host names or paths.
func fail(c *gin.Context, err error) {
	status := statusFor(err)
	if status == http.StatusInternalServerError {
		logRequestError(c, err)
		c.AbortWithStatusJSON(status, errorBody{Code: codeInternal, Error: "unexpected error, check the server logs"})
		return
	}
	c.AbortWithStatusJSON(status, errorBody{Code: codeFor(err), Error: messageFor(err, status)})
}

// abort answers with an explicit status and message, for the cases that never
// come from the service layer (a missing path parameter, a malformed body).
func abort(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, errorBody{Code: codeForStatus(status), Error: strings.TrimSpace(message)})
}

// messageFor hides the details of a failure the caller cannot act on.
func messageFor(err error, status int) string {
	if status >= http.StatusInternalServerError {
		return "unexpected error, check the server logs"
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return http.StatusText(status)
	}
	return message
}

// codeForStatus is the fallback code of an explicit status.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return codeValidation
	case http.StatusUnauthorized:
		return codeUnauthorized
	case http.StatusForbidden:
		return codeForbidden
	case http.StatusNotFound:
		return codeNotFound
	case http.StatusConflict:
		return codeBusy
	default:
		if status >= http.StatusInternalServerError {
			return codeInternal
		}
		return codeValidation
	}
}

// parseID reads an unsigned integer path parameter, answering 400 when the value
// cannot be an identifier.
func parseID(c *gin.Context, name string) (uint, bool) {
	raw := strings.TrimSpace(c.Param(name))
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 {
		abort(c, http.StatusBadRequest, "invalid identifier in the request path: "+raw)
		return 0, false
	}
	return uint(value), true
}

// limit reads the `limit` query parameter, clamped to [1, max] with fallback for
// a missing value. The API bounds every list endpoint so one request can never
// load an unbounded run history.
func limit(c *gin.Context, fallback, max int) int {
	raw := strings.TrimSpace(c.Query("limit"))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}

// queryID reads an optional numeric query parameter, answering 400 when it is
// present but is not an identifier. Absent means "no filter" (0).
func queryID(c *gin.Context, name string) (uint, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		abort(c, http.StatusBadRequest, "invalid query parameter "+name+": "+raw)
		return 0, false
	}
	return uint(value), true
}

// queryInt reads an optional integer query parameter, clamped to [min, max]. A
// missing or malformed value falls back to the default instead of failing: a
// window size is never worth a 400.
func queryInt(c *gin.Context, name string, fallback, min, max int) int {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// decode binds a JSON body, answering 400 when it is malformed or holds an
// unexpected type. It returns false when the caller must stop.
func decode(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		abort(c, http.StatusBadRequest, "the request body is not valid JSON: "+err.Error())
		return false
	}
	return true
}
