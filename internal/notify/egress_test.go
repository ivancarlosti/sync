package notify

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestIsPublicTarget pins the classification a webhook destination depends on:
// every non-public address is refused, including the IPv4-mapped forms that
// would otherwise let the same address be written in IPv6 notation.
func TestIsPublicTarget(t *testing.T) {
	cases := []struct {
		address string
		want    bool
	}{
		{"1.1.1.1", true},
		{"93.184.216.34", true},
		{"2606:4700:4700::1111", true},
		{"::ffff:1.1.1.1", true},

		{"127.0.0.1", false},
		{"::1", false},
		{"::ffff:127.0.0.1", false},
		{"10.0.0.5", false},
		{"172.16.4.1", false},
		{"192.168.1.10", false},
		{"169.254.169.254", false}, // cloud metadata
		{"::ffff:169.254.169.254", false},
		{"fe80::1", false},
		{"fd00::1", false},
		{"100.64.0.7", false}, // RFC 6598 / Tailscale
		{"100.127.255.254", false},
		{"192.0.0.1", false},
		{"192.0.2.1", false},
		{"198.18.0.1", false},
		{"198.51.100.1", false},
		{"203.0.113.1", false},
		{"240.0.0.1", false},
		{"2001:db8::1", false},
		{"64:ff9b::a00:1", false},
		{"0.0.0.0", false},
		{"::", false},
		{"224.0.0.1", false},
		{"ff02::1", false},
	}
	for _, tc := range cases {
		t.Run(tc.address, func(t *testing.T) {
			if got := isPublicTarget(net.ParseIP(tc.address)); got != tc.want {
				t.Fatalf("isPublicTarget(%s) = %v, want %v", tc.address, got, tc.want)
			}
		})
	}
	if isPublicTarget(nil) {
		t.Fatal("a missing address must not be dialled")
	}
}

// TestWebhookSenderRefusesPrivateTarget checks the connect-time refusal, which is
// what covers a stored channel pointing at an address the shape check cannot
// classify (a name that resolves privately) as well as a literal one: the request
// must not reach the server at all unless the operator allowed it.
func TestWebhookSenderRefusesPrivateTarget(t *testing.T) {
	reached := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	// A literal address (checked against the URL shape) and a name (checked when
	// the connection is opened) are both refused; `localhost` proves the dialer is
	// a barrier of its own and not only the URL check.
	for _, target := range []string{server.URL + "/ok", "http://localhost:" + portOf(t, server.URL) + "/ok"} {
		err := NewWebhookSender(5*time.Second, Options{}).Send(context.Background(), map[string]any{"url": target}, testMessage())
		if !errors.Is(err, ErrPrivateTarget) {
			t.Fatalf("Send(%q) error = %v, want ErrPrivateTarget", target, err)
		}
		if !strings.Contains(err.Error(), "not publicly routable") {
			t.Fatalf("the reason must be readable in the UI: %v", err)
		}
		select {
		case <-reached:
			t.Fatalf("Send(%q) reached a private destination", target)
		default:
		}
	}

	// The opt-in is what makes the same destination deliverable.
	if err := NewWebhookSender(5*time.Second, Options{AllowPrivateTargets: true}).
		Send(context.Background(), map[string]any{"url": server.URL + "/ok"}, testMessage()); err != nil {
		t.Fatalf("Send() with AllowPrivateTargets error = %v", err)
	}
	select {
	case <-reached:
	default:
		t.Fatal("Send() with AllowPrivateTargets did not reach the server")
	}
}

// TestWebhookRedirectPolicy checks both halves of the redirect policy: the number
// of hops is bounded, and a redirect that changes the host loses the signature
// and the headers the channel configured, so an endpoint cannot forward the
// operator's credentials to a third party.
func TestWebhookRedirectPolicy(t *testing.T) {
	var followed struct {
		signature string
		apiKey    string
		event     string
	}
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.signature = r.Header.Get("X-Sync-Signature")
		followed.apiKey = r.Header.Get("X-Api-Key")
		followed.event = r.Header.Get("X-Sync-Event")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer final.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/moved", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	sender := NewWebhookSender(5*time.Second, Options{AllowPrivateTargets: true})
	config := map[string]any{
		"url":     redirect.URL + "/hook",
		"secret":  "s3cret",
		"headers": map[string]any{"X-Api-Key": "v1:sealed"},
	}
	if err := sender.Send(context.Background(), config, testMessage()); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if followed.event != "sync.success" {
		t.Fatalf("a redirect must keep the delivery headers: %+v", followed)
	}
	if followed.signature != "" || followed.apiKey != "" {
		t.Fatalf("a cross-host redirect leaked the operator's secrets: %+v", followed)
	}

	// A loop is bounded instead of holding a sync run open.
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
	}))
	defer loop.Close()
	err := sender.Send(context.Background(), map[string]any{"url": loop.URL + "/hook"}, testMessage())
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("a redirect loop must fail with the reason: %v", err)
	}
}

// portOf extracts the port of a test server URL.
func portOf(t *testing.T, raw string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(raw, "http://"))
	if err != nil {
		t.Fatalf("unexpected test server URL %q: %v", raw, err)
	}
	return port
}
