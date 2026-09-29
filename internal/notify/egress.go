package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrPrivateTarget marks a webhook destination that is not publicly routable. A
// channel is a stored setting, so this refusal is what keeps a channel from
// being pointed at the host's own network: the loopback interface, another
// container on a private network, the carrier-grade NAT range Tailscale hands
// out, or the cloud metadata endpoint at 169.254.169.254. Operators who need a
// LAN target opt in with NOTIFY_ALLOW_PRIVATE_TARGETS=true.
var ErrPrivateTarget = errors.New("notify: webhook destination is not publicly routable")

// reservedTargetBlocks are the special-purpose ranges the net.IP helpers do not
// cover: IsGlobalUnicast() is true for every one of them, so without this list a
// destination inside one of these blocks would be dialled.
var reservedTargetBlocks = []*net.IPNet{
	mustParseCIDR("100.64.0.0/10"),   // RFC 6598 carrier-grade NAT (also used by Tailscale)
	mustParseCIDR("192.0.0.0/24"),    // IETF protocol assignments
	mustParseCIDR("192.0.2.0/24"),    // TEST-NET-1
	mustParseCIDR("198.18.0.0/15"),   // benchmarking
	mustParseCIDR("198.51.100.0/24"), // TEST-NET-2
	mustParseCIDR("203.0.113.0/24"),  // TEST-NET-3
	mustParseCIDR("240.0.0.0/4"),     // reserved for future use
	mustParseCIDR("64:ff9b::/96"),    // NAT64
	mustParseCIDR("2001:db8::/32"),   // documentation
}

// mustParseCIDR parses a compiled-in constant, so a typo is a programming error
// rather than something an operator can fix at run time.
func mustParseCIDR(cidr string) *net.IPNet {
	_, block, err := net.ParseCIDR(cidr)
	if err != nil {
		panic("notify: invalid reserved range " + cidr + ": " + err.Error())
	}
	return block
}

// isPublicTarget reports whether ip may be dialled as a webhook destination.
// IPv4-mapped forms need no special case, net.IP answers for them: IsLoopback()
// is true for ::ffff:127.0.0.1 and IsLinkLocalUnicast() is true for
// ::ffff:169.254.169.254, so an address cannot dodge the check by writing it in
// IPv6 notation.
func isPublicTarget(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() ||
		ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, block := range reservedTargetBlocks {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

// validateWebhookURLShape applies the checks a regular expression cannot express:
// the port has to be usable and a literal address has to be publicly routable
// unless the operator allowed private targets.
//
// Host names are deliberately not resolved here. This runs while the operator
// saves a channel, and the address that matters is the one dialled a few seconds
// (or days) later, so the name is checked at connect time instead, in
// newWebhookTransport, where the answer cannot change between check and use.
func validateWebhookURLShape(target string, allowPrivate bool) error {
	parsed, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("%w: the webhook URL cannot be parsed: %v", ErrInvalidConfig, err)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%w: the webhook URL must include a host", ErrInvalidConfig)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: the webhook URL must not carry credentials", ErrInvalidConfig)
	}
	if raw := parsed.Port(); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("%w: the webhook URL port must be between 1 and 65535 (got %q)", ErrInvalidConfig, raw)
		}
	}
	if host := parsed.Hostname(); host != "" && !allowPrivate {
		if ip := net.ParseIP(host); ip != nil && !isPublicTarget(ip) {
			return fmt.Errorf("%w: %s", ErrPrivateTarget, host)
		}
	}
	return nil
}

// newWebhookTransport builds the transport every webhook goes through. The
// dialer is the last line of defence and the one that matters for a stored
// channel: it resolves the destination once, skips every answer that is not
// publicly routable (unless the operator allowed private targets) and then dials
// the address it checked. The name is never resolved a second time, so a host
// cannot pass the check and then resolve to a private address when the
// connection is opened (DNS rebinding).
func newWebhookTransport(allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		// An operator behind a mandatory egress proxy keeps working; the proxy is
		// then the one connecting, so its own policy applies to the target.
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(addrs) == 0 {
				return nil, fmt.Errorf("%w: %s resolved to no address", ErrPrivateTarget, host)
			}
			for _, resolved := range addrs {
				if !allowPrivate && !isPublicTarget(resolved.IP) {
					continue
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			}
			return nil, fmt.Errorf("%w: %s resolves to %s", ErrPrivateTarget, host, joinAddresses(addrs))
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// joinAddresses renders the resolved addresses for an error message.
func joinAddresses(addrs []net.IPAddr) string {
	parts := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		parts = append(parts, addr.IP.String())
	}
	return strings.Join(parts, ", ")
}
