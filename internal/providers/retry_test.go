package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// cannedResponse builds a response with a body a test can read back.
func cannedResponse(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// fixedJitter makes the backoff deterministic for the duration of a test.
func fixedJitter(t *testing.T) {
	t.Helper()
	original := randInt63n
	randInt63n = func(int64) int64 { return 0 }
	t.Cleanup(func() { randInt63n = original })
}

func TestDefaultRetryableClassifiesThrottles(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusBadRequest, false},
		{http.StatusOK, false},
	}
	for _, testCase := range cases {
		if got := DefaultRetryable(cannedResponse(testCase.status, nil, "")); got != testCase.want {
			t.Errorf("DefaultRetryable(%d) = %v, want %v", testCase.status, got, testCase.want)
		}
	}
	if DefaultRetryable(nil) {
		t.Error("DefaultRetryable(nil) = true, want false")
	}
}

func TestRetryAfterReadsBothShapes(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	header := func(value string) http.Header {
		h := http.Header{}
		if value != "" {
			h.Set("Retry-After", value)
		}
		return h
	}
	cases := []struct {
		name      string
		value     string
		want      time.Duration
		wantFound bool
	}{
		{name: "seconds", value: "12", want: 12 * time.Second, wantFound: true},
		{name: "zero", value: "0", want: 0, wantFound: true},
		{name: "padded", value: " 5 ", want: 5 * time.Second, wantFound: true},
		{name: "http date", value: now.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second, wantFound: true},
		{name: "past date", value: now.Add(-time.Minute).Format(http.TimeFormat), wantFound: false},
		{name: "negative", value: "-3", wantFound: false},
		{name: "garbage", value: "soon", wantFound: false},
		{name: "missing", value: "", wantFound: false},
	}
	for _, testCase := range cases {
		got, ok := RetryAfter(header(testCase.value), now)
		if ok != testCase.wantFound || (ok && got != testCase.want) {
			t.Errorf("RetryAfter(%q) = %v, %v, want %v, %v",
				testCase.value, got, ok, testCase.want, testCase.wantFound)
		}
	}
	if _, ok := RetryAfter(nil, now); ok {
		t.Error("RetryAfter(nil) reported a delay")
	}
}

func TestRetryPolicyWaitPrefersRetryAfter(t *testing.T) {
	policy := RetryPolicy{BaseDelay: time.Second, MaxDelay: 10 * time.Second}.withDefaults()

	header := http.Header{}
	header.Set("Retry-After", "4")
	if got := policy.wait(1, cannedResponse(http.StatusTooManyRequests, header, "")); got != 4*time.Second {
		t.Errorf("wait = %v, want the Retry-After delay (4s)", got)
	}
	header.Set("Retry-After", "600")
	if got := policy.wait(1, cannedResponse(http.StatusTooManyRequests, header, "")); got != 10*time.Second {
		t.Errorf("wait = %v, want a Retry-After capped at MaxDelay", got)
	}
	// Without the header the wait is the jittered exponential backoff: never
	// below half the step, never above the step.
	if got := policy.wait(1, nil); got < 500*time.Millisecond || got > time.Second {
		t.Errorf("wait(1) = %v, want it within [base/2, base]", got)
	}
	if got := policy.wait(3, nil); got < 2*time.Second || got > 4*time.Second {
		t.Errorf("wait(3) = %v, want it within [base*4/2, base*4]", got)
	}
	// The step is capped, and so is the wait that derives from it.
	if got := policy.wait(20, nil); got < 5*time.Second || got > 10*time.Second {
		t.Errorf("wait(20) = %v, want it within [MaxDelay/2, MaxDelay]", got)
	}
}

func TestDoWithRetryRetriesAThrottleAndReturnsTheAnswer(t *testing.T) {
	fixedJitter(t)
	statuses := []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusOK}
	sent := 0

	response, err := DoWithRetry(context.Background(),
		RetryPolicy{BaseDelay: time.Microsecond, MaxDelay: time.Microsecond},
		func(context.Context) (*http.Response, error) {
			status := statuses[sent]
			sent++
			return cannedResponse(status, nil, "answer"), nil
		})
	if err != nil {
		t.Fatalf("DoWithRetry = %v", err)
	}
	if sent != 3 {
		t.Errorf("the request was sent %d times, want 3", sent)
	}
	if response.StatusCode != http.StatusOK {
		t.Errorf("response.StatusCode = %d, want the last answer (200)", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "answer" {
		t.Errorf("body = %q, %v, want the last answer", body, err)
	}
}

func TestDoWithRetryGivesUpAndReturnsTheLastAnswer(t *testing.T) {
	fixedJitter(t)
	sent := 0

	response, err := DoWithRetry(context.Background(),
		RetryPolicy{MaxAttempts: 3, BaseDelay: time.Microsecond, MaxDelay: time.Microsecond},
		func(context.Context) (*http.Response, error) {
			sent++
			return cannedResponse(http.StatusTooManyRequests, nil, "throttled"), nil
		})
	if err != nil {
		t.Fatalf("DoWithRetry = %v", err)
	}
	if sent != 3 {
		t.Errorf("the request was sent %d times, want the attempt budget (3)", sent)
	}
	if response.StatusCode != http.StatusTooManyRequests {
		t.Errorf("response.StatusCode = %d, want the last answer (429)", response.StatusCode)
	}
}

func TestDoWithRetryReturnsADecisionImmediately(t *testing.T) {
	sent := 0
	response, err := DoWithRetry(context.Background(), RetryPolicy{},
		func(context.Context) (*http.Response, error) {
			sent++
			return cannedResponse(http.StatusForbidden, nil, "no"), nil
		})
	if err != nil {
		t.Fatalf("DoWithRetry = %v", err)
	}
	if sent != 1 {
		t.Errorf("the request was sent %d times, want 1 (403 is not retried)", sent)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("response.StatusCode = %d, want 403", response.StatusCode)
	}
}

func TestDoWithRetryReturnsATransportErrorUntouched(t *testing.T) {
	transportErr := errors.New("connection reset")
	sent := 0
	_, err := DoWithRetry(context.Background(), RetryPolicy{},
		func(context.Context) (*http.Response, error) {
			sent++
			return nil, transportErr
		})
	if !errors.Is(err, transportErr) {
		t.Errorf("DoWithRetry = %v, want the transport error", err)
	}
	if sent != 1 {
		t.Errorf("the request was sent %d times, want 1", sent)
	}
}

func TestDoWithRetryStopsOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sent := 0
	_, err := DoWithRetry(ctx, RetryPolicy{BaseDelay: time.Hour, MaxDelay: time.Hour},
		func(context.Context) (*http.Response, error) {
			sent++
			cancel()
			return cannedResponse(http.StatusTooManyRequests, nil, ""), nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("DoWithRetry = %v, want context.Canceled", err)
	}
	if sent != 1 {
		t.Errorf("the request was sent %d times, want 1", sent)
	}
}

// trackedBody records whether a response body was drained and closed.
type trackedBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

// TestDoWithRetryDropsTheDiscardedAnswer pins that a retried answer is released
// (drained then closed) so its connection goes back to the pool, while the
// answer the caller receives is still open.
func TestDoWithRetryDropsTheDiscardedAnswer(t *testing.T) {
	fixedJitter(t)
	first := &trackedBody{reader: strings.NewReader("throttled")}
	last := &trackedBody{reader: strings.NewReader("answer")}
	bodies := []*trackedBody{first, last}
	sent := 0

	response, err := DoWithRetry(context.Background(),
		RetryPolicy{MaxAttempts: 2, BaseDelay: time.Microsecond, MaxDelay: time.Microsecond},
		func(context.Context) (*http.Response, error) {
			body := bodies[sent]
			sent++
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: body}, nil
		})
	if err != nil {
		t.Fatalf("DoWithRetry = %v", err)
	}
	_ = response
	if !first.closed || first.read != len("throttled") {
		t.Errorf("the discarded answer was not drained and closed (read %d, closed %v)", first.read, first.closed)
	}
	if last.closed {
		t.Error("the answer handed to the caller was closed by the helper")
	}
}
