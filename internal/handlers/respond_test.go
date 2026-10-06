package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/services"
)

// TestStatusForMapsASpentThrottle pins the answer a throttled provider gets once
// its automatic retries are over: a 429 the SPA translates with
// `accounts.error_rate_limited`, not a 500 that hides the cause behind "check the
// server logs".
func TestStatusForMapsASpentThrottle(t *testing.T) {
	wrapped := fmt.Errorf("listing the folder: %w", providers.ErrRateLimited)
	if got := statusFor(wrapped); got != http.StatusTooManyRequests {
		t.Errorf("statusFor(ErrRateLimited) = %d, want 429", got)
	}
	if got := codeFor(wrapped); got != codeRateLimited {
		t.Errorf("codeFor(ErrRateLimited) = %q, want %q", got, codeRateLimited)
	}
}

// TestStatusForKeepsTheNeighbouringMappings guards the entries around the new
// one, so a mapping cannot silently swallow another.
func TestStatusForKeepsTheNeighbouringMappings(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "consent",
			err:        fmt.Errorf("connect the account: %w", services.ErrConsent),
			wantStatus: http.StatusPreconditionFailed,
			wantCode:   codeConsentRequired,
		},
		{
			name:       "reconnect",
			err:        fmt.Errorf("refresh the token: %w", services.ErrReconnect),
			wantStatus: http.StatusPreconditionFailed,
			wantCode:   codeReconnect,
		},
		{
			name:       "not configured",
			err:        fmt.Errorf("no client: %w", services.ErrNotConfigured),
			wantStatus: http.StatusFailedDependency,
			wantCode:   codeNotConfigured,
		},
		{
			name:       "unknown",
			err:        errors.New("boom"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   codeInternal,
		},
	}
	for _, testCase := range cases {
		if got := statusFor(testCase.err); got != testCase.wantStatus {
			t.Errorf("statusFor(%s) = %d, want %d", testCase.name, got, testCase.wantStatus)
		}
		if got := codeFor(testCase.err); got != testCase.wantCode {
			t.Errorf("codeFor(%s) = %q, want %q", testCase.name, got, testCase.wantCode)
		}
	}
}
