package ical

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// MaxFeedBytes bounds one download. A calendar feed of a busy year is a few
// hundred kilobytes; eight megabytes is far above that and still keeps a stored
// URL from turning a run into a memory problem.
const MaxFeedBytes = 8 << 20

// fetchTimeout bounds one request, credential challenge included.
const fetchTimeout = 60 * time.Second

var (
	// ErrPrivateTarget marks a URL that is not publicly routable. A subscription
	// is a stored setting, so the refusal is what keeps it from being pointed at
	// the host's own network: the loopback interface, another container, a
	// Tailscale address or the cloud metadata endpoint. An operator who really
	// needs an internal feed opts in with CALENDAR_ALLOW_PRIVATE_TARGETS=true.
	ErrPrivateTarget = errors.New("ical: the subscription URL is not publicly routable")
	// ErrInvalidURL marks a URL the fetcher refuses to dial.
	ErrInvalidURL = errors.New("ical: invalid subscription URL")
	// ErrTooLarge marks a feed above MaxFeedBytes.
	ErrTooLarge = errors.New("ical: the feed is larger than the allowed limit")
)

// reservedBlocks are the special-purpose ranges net.IP does not cover (its
// IsGlobalUnicast answers true for every one of them).
var reservedBlocks = []*net.IPNet{
	mustCIDR("100.64.0.0/10"),   // RFC 6598 carrier-grade NAT (Tailscale)
	mustCIDR("192.0.0.0/24"),    // IETF protocol assignments
	mustCIDR("192.0.2.0/24"),    // TEST-NET-1
	mustCIDR("198.18.0.0/15"),   // benchmarking
	mustCIDR("198.51.100.0/24"), // TEST-NET-2
	mustCIDR("203.0.113.0/24"),  // TEST-NET-3
	mustCIDR("240.0.0.0/4"),     // reserved for future use
	mustCIDR("64:ff9b::/96"),    // NAT64
	mustCIDR("2001:db8::/32"),   // documentation
}

func mustCIDR(cidr string) *net.IPNet {
	_, block, err := net.ParseCIDR(cidr)
	if err != nil {
		panic("ical: invalid reserved range " + cidr + ": " + err.Error())
	}
	return block
}

// isPublicTarget reports whether ip may be dialled. IPv4-mapped IPv6 forms need
// no special case: net.IP answers for them, so 127.0.0.1 cannot dodge the check
// by being written as ::ffff:127.0.0.1.
func isPublicTarget(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, block := range reservedBlocks {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

// Subscription is one fetch of a stored feed: the URL, the optional credentials
// and the validators the previous fetch returned.
type Subscription struct {
	URL          string
	Username     string
	Password     string
	ETag         string
	LastModified string
	// AllowPrivate permits a target that is not publicly routable. It is the
	// operator's policy, read from CALENDAR_ALLOW_PRIVATE_TARGETS.
	AllowPrivate bool
}

// Result is the outcome of a fetch. NotModified is the answer of a conditional
// request the server satisfied with 304, in which case Body is empty.
type Result struct {
	Body         []byte
	ETag         string
	LastModified string
	NotModified  bool
}

// feedURLPattern is the only shape a stored subscription may have: an absolute
// http(s) URL whose host is a bracketed IPv6 literal or a host name, with an
// optional port and an optional path, query and fragment. A non-http scheme
// (`file:`, `javascript:`), a relative path, credentials in the authority part
// (`https://user:secret@host/feed.ics`), a missing host, a malformed name and any
// space or control character — the last one also keeps a newline from splitting
// the request line — are refused. It mirrors webhookURLPattern in
// internal/notify, which faces the same problem.
var feedURLPattern = regexp.MustCompile(`^https?://(?:\[[0-9A-Fa-f:.]+\]|[a-zA-Z0-9_](?:[a-zA-Z0-9_-]{0,61}[a-zA-Z0-9_])?(?:\.[a-zA-Z0-9_](?:[a-zA-Z0-9_-]{0,61}[a-zA-Z0-9_])?)*\.?)(?::[0-9]{1,5})?(?:[/?#][^[:space:][:cntrl:]]*)?$`)

// ValidateURL applies the checks a stored subscription has to pass before it is
// saved: an absolute http(s) URL with a host, no embedded credentials (the
// dedicated fields exist for those) and a literal address that is publicly
// routable unless the operator allowed private targets. A host name is checked
// again at connect time, where the address actually dialled is known.
func ValidateURL(target string, allowPrivate bool) (*url.URL, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: the URL is required", ErrInvalidURL)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: only http and https are supported", ErrInvalidURL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%w: the URL must include a host", ErrInvalidURL)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: the URL must not carry credentials", ErrInvalidURL)
	}
	// The same allow-list the fetcher applies right before it builds the request:
	// applied here as well, a subscription whose shape can never be dialled is
	// refused while the operator saves the job instead of on its first run.
	if !feedURLPattern.MatchString(trimmed) {
		return nil, fmt.Errorf("%w: the URL must be an absolute http:// or https:// URL with a host, without credentials, spaces or control characters", ErrInvalidURL)
	}
	if host := parsed.Hostname(); host != "" && !allowPrivate {
		if ip := net.ParseIP(host); ip != nil && !isPublicTarget(ip) {
			return nil, fmt.Errorf("%w: %s", ErrPrivateTarget, host)
		}
	}
	return parsed, nil
}

// clients keeps one HTTP client per egress policy: the transport is what enforces
// the private-target rule at connect time, so the policy has to be baked into it.
var clients = sync.OnceValues(func() (*http.Client, *http.Client) {
	return newClient(false), newClient(true)
})

// Fetch downloads a feed, honouring the validators of the previous fetch. A
// server answer of 304 is reported as NotModified instead of an error, which is
// what makes an unchanged subscription cost one conditional request.
func Fetch(ctx context.Context, sub Subscription) (Result, error) {
	target := strings.TrimSpace(sub.URL)
	if _, err := ValidateURL(target, sub.AllowPrivate); err != nil {
		return Result{}, err
	}
	// The pattern is applied to the very value handed to
	// http.NewRequestWithContext below: a subscription URL is a stored setting,
	// so this is the point where an arbitrary destination is narrowed to an
	// absolute http(s) URL. It also has to stay in this function — a security
	// scanner recognises an allow-list pattern used as a guard on the request
	// URL, but only while the guard and the use are in the same function.
	if !feedURLPattern.MatchString(target) {
		return Result{}, fmt.Errorf("%w: the subscription URL must be an absolute http:// or https:// URL with a host, without credentials, spaces or control characters", ErrInvalidURL)
	}
	strict, relaxed := clients()
	client := strict
	if sub.AllowPrivate {
		client = relaxed
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	request.Header.Set("Accept", "text/calendar, text/plain;q=0.5, */*;q=0.1")
	request.Header.Set("User-Agent", "sync-calendar/1.0")
	if etag := strings.TrimSpace(sub.ETag); etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	if modified := strings.TrimSpace(sub.LastModified); modified != "" {
		request.Header.Set("If-Modified-Since", modified)
	}
	if strings.TrimSpace(sub.Username) != "" {
		request.SetBasicAuth(sub.Username, sub.Password)
	}

	response, err := client.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("ical: fetching the feed failed: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotModified {
		return Result{ETag: sub.ETag, LastModified: sub.LastModified, NotModified: true}, nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return Result{}, fmt.Errorf("ical: the feed answered %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxFeedBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("ical: reading the feed failed: %w", err)
	}
	if len(body) > MaxFeedBytes {
		return Result{}, fmt.Errorf("%w: %d bytes", ErrTooLarge, MaxFeedBytes)
	}
	return Result{
		Body:         body,
		ETag:         strings.TrimSpace(response.Header.Get("ETag")),
		LastModified: strings.TrimSpace(response.Header.Get("Last-Modified")),
	}, nil
}

// newClient builds the client of one egress policy. The dialer resolves the host
// once, skips every answer that is not publicly routable (unless allowed) and
// dials the address it checked, so a name cannot pass the check and then resolve
// to a private address when the connection opens (DNS rebinding).
func newClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, resolved := range addresses {
				if !allowPrivate && !isPublicTarget(resolved.IP) {
					continue
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			}
			return nil, fmt.Errorf("%w: %s resolves to a reserved address", ErrPrivateTarget, host)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   fetchTimeout,
		// A redirect is followed only after it is re-checked the same way: a
		// public feed that answers with a Location pointing at 169.254.169.254
		// must not reach it.
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("ical: too many redirects")
			}
			if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
				return fmt.Errorf("%w: a redirect to %s is not allowed", ErrInvalidURL, request.URL.Scheme)
			}
			if host := request.URL.Hostname(); !allowPrivate && host != "" {
				if ip := net.ParseIP(host); ip != nil && !isPublicTarget(ip) {
					return fmt.Errorf("%w: %s", ErrPrivateTarget, host)
				}
			}
			return nil
		},
	}
}
