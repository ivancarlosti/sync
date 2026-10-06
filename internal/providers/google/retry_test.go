package google

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

// cannedAnswer is one response a cannedTransport hands out.
type cannedAnswer struct {
	status int
	body   string
}

// cannedTransport answers the requests in order, repeating the last answer once
// the script is exhausted, and counts how many were sent — which is what proves
// that a throttle was retried.
type cannedTransport struct {
	answers []cannedAnswer
	sent    int
}

// RoundTrip implements http.RoundTripper.
func (t *cannedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	answer := t.answers[0]
	if len(t.answers) > 1 {
		t.answers = t.answers[1:]
	}
	t.sent++
	return &http.Response{
		StatusCode: answer.status,
		Status:     http.StatusText(answer.status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(answer.body)),
		Request:    nil,
	}, nil
}

// fastRetries shrinks the retry backoff for the duration of a test.
func fastRetries(t *testing.T) {
	t.Helper()
	original := retryPolicy
	retryPolicy.BaseDelay = time.Microsecond
	retryPolicy.MaxDelay = time.Microsecond
	t.Cleanup(func() { retryPolicy = original })
}

// throttleEnvelope is the 429 body the Drive API answers with, reason included.
const throttleEnvelope = `{"error":{"code":429,"message":"Rate Limit Exceeded",` +
	`"errors":[{"reason":"rateLimitExceeded","message":"Rate Limit Exceeded"}]}}`

func TestRetryableGoogleAcceptsTheQuotaForbidden(t *testing.T) {
	throttle := func(reason string) *http.Response {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"errors":[{"reason":"` + reason + `"}]}}`)),
		}
	}
	for _, reason := range []string{"rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded"} {
		if !retryableGoogle(throttle(reason)) {
			t.Errorf("retryableGoogle(403 %s) = false, want true", reason)
		}
	}

	// A genuine permission error is a decision, not a throttle, and its body
	// must still be readable by the caller that decodes it.
	denied := throttle("insufficientFilePermissions")
	if retryableGoogle(denied) {
		t.Error("retryableGoogle(403 insufficientFilePermissions) = true, want false")
	}
	raw, err := io.ReadAll(denied.Body)
	if err != nil || !strings.Contains(string(raw), "insufficientFilePermissions") {
		t.Errorf("the body was consumed by the predicate: %q, %v", raw, err)
	}

	if !retryableGoogle(&http.Response{StatusCode: http.StatusTooManyRequests}) {
		t.Error("retryableGoogle(429) = false, want true")
	}
	if retryableGoogle(nil) {
		t.Error("retryableGoogle(nil) = true, want false")
	}
}

// TestRateLimitedErrorClassification pins how a throttle is recognised once the
// retries are over: errors.Is must answer true for both shapes, without breaking
// the API-error unwrapping the other classifications rely on.
func TestRateLimitedErrorClassification(t *testing.T) {
	provider := testProvider(&recordingTransport{})
	cases := []struct {
		name string
		err  *apiError
		want bool
	}{
		{name: "429", err: &apiError{Status: http.StatusTooManyRequests}, want: true},
		{name: "403 quota", err: &apiError{Status: http.StatusForbidden, Reason: "quotaExceeded"}, want: true},
		{name: "403 permission", err: &apiError{Status: http.StatusForbidden, Reason: "insufficientFilePermissions"}},
		{name: "404", err: &apiError{Status: http.StatusNotFound}},
	}
	for _, testCase := range cases {
		if got := errors.Is(testCase.err, providers.ErrRateLimited); got != testCase.want {
			t.Errorf("errors.Is(%s, ErrRateLimited) = %v, want %v", testCase.name, got, testCase.want)
		}
	}
	// The classification must not have replaced the apiError in the chain.
	if !provider.IsNotFound(&apiError{Status: http.StatusNotFound}) {
		t.Error("IsNotFound no longer recognises a 404 after the throttle classification")
	}
}

// TestUnauthorizedErrorClassification pins the 401 detection the engine relies on
// to renew a token mid-run: only a rejected-access-token answer counts, and a
// genuine permission problem (403, quota included) stays out so it is never
// mistaken for an expired token.
func TestUnauthorizedErrorClassification(t *testing.T) {
	provider := testProvider(&recordingTransport{})
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "401", err: &apiError{Status: http.StatusUnauthorized, Reason: "authError"}, want: true},
		{name: "401 wrapped", err: fmt.Errorf("uploading a file: %w", &apiError{Status: http.StatusUnauthorized}), want: true},
		{name: "403 permission", err: &apiError{Status: http.StatusForbidden, Reason: "insufficientFilePermissions"}},
		{name: "403 quota", err: &apiError{Status: http.StatusForbidden, Reason: "quotaExceeded"}},
		{name: "404", err: &apiError{Status: http.StatusNotFound}},
		{name: "other", err: errors.New("boom")},
		{name: "nil", err: nil},
	}
	for _, testCase := range cases {
		if got := provider.IsUnauthorized(testCase.err); got != testCase.want {
			t.Errorf("IsUnauthorized(%s) = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

// TestDoJSONRetriesAThrottle pins the retry of a throttled metadata call: the
// second attempt answers 200 and the decoded body is the answer of that attempt.
func TestDoJSONRetriesAThrottle(t *testing.T) {
	fastRetries(t)
	transport := &cannedTransport{answers: []cannedAnswer{
		{status: http.StatusTooManyRequests, body: throttleEnvelope},
		{status: http.StatusOK, body: `{"id":"item-1","name":"note.txt"}`},
	}}
	client := &http.Client{Transport: transport}

	var out driveFile
	target := withParam(apiBase+"/files/1AbCdEfGh_-2", "supportsAllDrives", "true")
	if err := testProvider(transport).doJSON(context.Background(), client, http.MethodGet, target, nil, &out); err != nil {
		t.Fatalf("doJSON = %v, want the retried call to succeed", err)
	}
	if transport.sent != 2 {
		t.Errorf("the request was sent %d times, want 2", transport.sent)
	}
	if out.ID != "item-1" {
		t.Errorf("decoded item = %+v, want the answer of the last attempt", out)
	}
}

// TestDoJSONReportsAnExhaustedThrottle pins the other end: when the whole budget
// is spent on 429s the caller receives the last error, classified as a throttle.
func TestDoJSONReportsAnExhaustedThrottle(t *testing.T) {
	fastRetries(t)
	retryPolicy.MaxAttempts = 3
	transport := &cannedTransport{answers: []cannedAnswer{
		{status: http.StatusTooManyRequests, body: throttleEnvelope},
	}}
	client := &http.Client{Transport: transport}

	err := testProvider(transport).doJSON(context.Background(), client, http.MethodGet,
		apiBase+"/files", nil, nil)
	if !errors.Is(err, providers.ErrRateLimited) {
		t.Errorf("doJSON = %v, want providers.ErrRateLimited", err)
	}
	if transport.sent != 3 {
		t.Errorf("the request was sent %d times, want the attempt budget (3)", transport.sent)
	}
}
