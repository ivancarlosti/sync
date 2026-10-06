package providers

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The retry budget of a throttled call. The values are deliberately modest: a
// run is bounded by its own timeout, and a provider that keeps refusing must
// fail the run instead of hanging on it.
const (
	// DefaultMaxAttempts counts the first try plus the retries.
	DefaultMaxAttempts = 5
	// DefaultBaseDelay is the first backoff step; it doubles on every retry.
	DefaultBaseDelay = 500 * time.Millisecond
	// DefaultMaxDelay caps one wait, whether it comes from the backoff or from a
	// Retry-After header.
	DefaultMaxDelay = 30 * time.Second
)

// randInt63n is math/rand.Int63n taken through a variable, so a test can make
// the jitter of the backoff deterministic.
var randInt63n = rand.Int63n

// RetryPolicy describes how a throttled call is retried.
type RetryPolicy struct {
	// MaxAttempts is the number of tries, first attempt included. Zero means
	// DefaultMaxAttempts.
	MaxAttempts int
	// BaseDelay is the first backoff step. Zero means DefaultBaseDelay.
	BaseDelay time.Duration
	// MaxDelay caps a single wait. Zero means DefaultMaxDelay.
	MaxDelay time.Duration
	// Retryable decides whether a response is worth retrying. Nil means
	// DefaultRetryable; a provider widens it with the rules the default cannot
	// express (Google answers a quota excess with HTTP 403, not 429).
	Retryable func(*http.Response) bool
}

// DefaultRetryable reports whether a response is a transient refusal rather than
// an answer: the server asked the caller to slow down (429) or failed without
// the request being at fault (5xx). Every other status — including the rest of
// the 4xx family — is a decision the caller has to handle.
func DefaultRetryable(response *http.Response) bool {
	if response == nil {
		return false
	}
	return response.StatusCode == http.StatusTooManyRequests ||
		(response.StatusCode >= 500 && response.StatusCode <= 599)
}

// DoWithRetry sends a request through send until the answer is not worth
// retrying or the attempt budget is exhausted, and returns that last response —
// whose body the caller still has to close and decode. Waiting is context aware,
// so a cancelled run stops retrying at once.
//
// An error from send is returned as is: a transport failure is not a throttle,
// and deciding whether a request that may have reached the server can be
// repeated is the caller's business, not this helper's. Between two attempts the
// discarded response body is drained and closed, so the connection goes back to
// the pool instead of being dropped.
func DoWithRetry(ctx context.Context, policy RetryPolicy, send func(context.Context) (*http.Response, error)) (*http.Response, error) {
	policy = policy.withDefaults()
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, err := send(ctx)
		if err != nil {
			return nil, err
		}
		if attempt >= policy.MaxAttempts || !policy.retryable(response) {
			return response, nil
		}
		delay := policy.wait(attempt, response)
		drain(response)
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// withDefaults fills the unset fields of a policy.
func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = DefaultMaxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = DefaultBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = DefaultMaxDelay
	}
	return p
}

// retryable applies the policy's own rule, or the default one.
func (p RetryPolicy) retryable(response *http.Response) bool {
	if p.Retryable != nil {
		return p.Retryable(response)
	}
	return DefaultRetryable(response)
}

// wait returns how long to sleep before the next attempt: the delay the server
// asked for through Retry-After when it sent one, the exponential backoff
// otherwise.
func (p RetryPolicy) wait(attempt int, response *http.Response) time.Duration {
	if response != nil {
		if delay, ok := RetryAfter(response.Header, time.Now()); ok {
			return p.capped(delay)
		}
	}
	step := p.BaseDelay
	if step > p.MaxDelay {
		step = p.MaxDelay
	}
	for tries := 1; tries < attempt; tries++ {
		step *= 2
		if step <= 0 || step >= p.MaxDelay {
			step = p.MaxDelay
			break
		}
	}
	// Equal jitter: half the step is kept and the other half is random, so
	// concurrent jobs do not march in lockstep while a minimum wait is still
	// guaranteed.
	half := step / 2
	return half + time.Duration(randInt63n(int64(half)+1))
}

// capped bounds a delay to the policy's ceiling.
func (p RetryPolicy) capped(delay time.Duration) time.Duration {
	switch {
	case delay < 0:
		return 0
	case delay > p.MaxDelay:
		return p.MaxDelay
	default:
		return delay
	}
}

// RetryAfter reads the delay a Retry-After header asks for. Both shapes the
// header allows are understood: a number of seconds and an HTTP date. A missing
// or malformed header reports false, and so does a date that is already in the
// past — an explicit "0" is a valid "retry immediately".
func RetryAfter(header http.Header, now time.Time) (time.Duration, bool) {
	if header == nil {
		return 0, false
	}
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	stamp, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	if delay := stamp.Sub(now); delay > 0 {
		return delay, true
	}
	return 0, false
}

// sleep waits for delay, or returns the context error when the call is
// cancelled in the meantime. A non-positive delay never waits.
func sleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// drain empties a response a retry is about to discard, so its connection can be
// reused instead of being dropped, then closes it.
func drain(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	_ = response.Body.Close()
}
