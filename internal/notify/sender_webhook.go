package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// webhookURLPattern is the only shape a webhook destination may have: an
// absolute http(s) URL whose host is a bracketed IPv6 literal or a host name,
// with an optional port and an optional path, query and fragment. A non-http
// scheme (`ftp:`, `javascript:`), a relative path, credentials in the authority
// part (`http://trusted@127.0.0.1/`), a missing host, a malformed name and any
// space or control character — the last one also keeps a newline from splitting
// the request line — are refused.
var webhookURLPattern = regexp.MustCompile(`^https?://(?:\[[0-9A-Fa-f:.]+\]|[a-zA-Z0-9_](?:[a-zA-Z0-9_-]{0,61}[a-zA-Z0-9_])?(?:\.[a-zA-Z0-9_](?:[a-zA-Z0-9_-]{0,61}[a-zA-Z0-9_])?)*\.?)(?::[0-9]{1,5})?(?:[/?#][^[:space:][:cntrl:]]*)?$`)

// maxWebhookRedirects bounds how many hops a delivery may make: a webhook that
// redirects in a loop must not hold a sync run open.
const maxWebhookRedirects = 5

// WebhookSender delivers notifications to an arbitrary HTTP endpoint. It exists
// because a generic "call this URL" requirement needs a free choice of method,
// headers and body, which a shoutrrr URL cannot express.
//
// Configuration keys (all secrets are already decrypted when they arrive):
//
//	url            destination URL (required, http/https)
//	method         HTTP method, POST by default
//	content_type   Content-Type header, application/json by default
//	headers        map of extra headers (e.g. {"X-Api-Key":"..."} )
//	body_template  optional body; supports {event} {title} {message} {json}
//	               and {data.<key>} placeholders
//	secret         optional HMAC-SHA256 key used to sign the body
//	timeout_seconds optional per-request timeout
type WebhookSender struct {
	client *http.Client
	// options is the operator's policy, kept so rendering a request (which the
	// API does before saving a channel) applies the same destination rules as
	// delivering one.
	options Options
}

// NewWebhookSender builds the sender around a shared HTTP client. The transport
// refuses a destination that is not publicly routable unless the operator opted
// in with Options.AllowPrivateTargets, which is what keeps a stored channel from
// reaching the host's own network, and redirects follow this package's own
// policy (guardRedirect).
func NewWebhookSender(timeout time.Duration, options Options) *WebhookSender {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &WebhookSender{
		client: &http.Client{
			Timeout:   timeout,
			Transport: newWebhookTransport(options.AllowPrivateTargets),
		},
		options: options,
	}
}

// Kind implements Sender.
func (s *WebhookSender) Kind() string { return KindWebhook }

// Send builds and executes the webhook request.
func (s *WebhookSender) Send(ctx context.Context, config map[string]any, msg Message) error {
	// A channel may tighten the timeout of the shared client, so one slow
	// endpoint cannot hold a sync run open for longer than the operator said.
	if seconds := ConfigInt(config, "timeout_seconds", 0); seconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		defer cancel()
	}
	request, err := NewRequest(ctx, config, msg, s.options)
	if err != nil {
		return err
	}
	// The client is copied so this delivery carries the redirect policy of this
	// channel; the copy shares the transport, so no connection is wasted.
	client := *s.client
	client.CheckRedirect = guardRedirect(config)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("notify: calling the webhook: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	// The body is drained so the connection can be reused; it is never logged
	// because a webhook may answer with sensitive data.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("notify: the webhook answered %s", response.Status)
	}
	return nil
}

// guardRedirect builds the redirect policy of one delivery. It bounds the hops,
// and a redirect to another host loses the operator's secrets: the signature and
// every header the channel configured are dropped before the request is sent
// again. The reasoning is the same as stripTokenOnHostChange in
// internal/providers/microsoft, and it is what keeps a legitimate endpoint's
// redirect from leaking an API key to a third party.
func guardRedirect(config map[string]any) func(*http.Request, []*http.Request) error {
	headers := ConfigMap(config, "headers")
	configured := make([]string, 0, len(headers))
	for name := range headers {
		configured = append(configured, name)
	}
	return func(request *http.Request, via []*http.Request) error {
		if len(via) >= maxWebhookRedirects {
			return fmt.Errorf("notify: the webhook redirected more than %d times", maxWebhookRedirects)
		}
		if len(via) == 0 || request.URL.Host == via[0].URL.Host {
			return nil
		}
		request.Header.Del("X-Sync-Signature")
		for _, name := range configured {
			request.Header.Del(name)
		}
		return nil
	}
}

// NewRequest renders the webhook request described by a configuration. It is
// exported so the API can validate a channel (and so the tests can assert the
// rendered body and signature) without performing a call. Rendering takes the
// operator's policy because whether a private destination is acceptable is a
// process-wide decision, not a property of the channel.
func NewRequest(ctx context.Context, config map[string]any, msg Message, options Options) (*http.Request, error) {
	target := ConfigString(config, "url")
	if target == "" {
		return nil, fmt.Errorf("%w: the webhook URL is required", ErrInvalidConfig)
	}
	// This check is deliberately inline, applied to the very value handed to
	// http.NewRequestWithContext below. A channel may be created by anyone who can
	// administer the instance and its URL is stored in the database, so this is
	// the point where an arbitrary destination is narrowed to an absolute http(s)
	// URL — a security scanner recognises a regexp used as a guard on the request
	// URL, but only while the guard and the use are in the same function.
	if !webhookURLPattern.MatchString(target) {
		return nil, fmt.Errorf("%w: the webhook URL must be an absolute http:// or https:// URL with a host, without credentials, spaces or control characters (got %q)", ErrInvalidConfig, target)
	}
	if err := validateWebhookURLShape(target, options.AllowPrivateTargets); err != nil {
		return nil, err
	}
	method := strings.ToUpper(ConfigString(config, "method"))
	if method == "" {
		method = http.MethodPost
	}

	contentType := ConfigString(config, "content_type")
	if contentType == "" {
		contentType = "application/json"
	}

	body, err := RenderBody(config, msg)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("User-Agent", "Sync-Webhook/1")
	request.Header.Set("X-Sync-Event", msg.Event)
	request.Header.Set("X-Sync-Delivery", deliveryID(msg))
	for key, value := range ConfigMap(config, "headers") {
		if text, ok := value.(string); ok && key != "" {
			request.Header.Set(key, text)
		}
	}
	if secret := ConfigString(config, "secret"); secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		request.Header.Set("X-Sync-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return request, nil
}

// RenderBody renders the request body: either the configured template or the
// default JSON document carrying the whole payload.
func RenderBody(config map[string]any, msg Message) ([]byte, error) {
	payload := msg.Payload()
	template := ConfigString(config, "body_template")
	if template == "" {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("notify: encoding the webhook payload: %w", err)
		}
		return body, nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("notify: encoding the webhook payload: %w", err)
	}
	replacer := &placeholderRenderer{payload: payload, encoded: string(encoded), event: msg.Event, title: msg.Title, message: msg.Body}
	return []byte(replacer.render(template)), nil
}

// placeholderRenderer expands the documented `{...}` placeholders. A template
// language would be more powerful and far easier to abuse, so the webhook body
// only understands literal placeholders.
type placeholderRenderer struct {
	payload map[string]any
	encoded string
	event   string
	title   string
	message string
}

// render expands every placeholder of the template.
func (r *placeholderRenderer) render(template string) string {
	out := template
	out = strings.ReplaceAll(out, "{event}", r.event)
	out = strings.ReplaceAll(out, "{title}", r.title)
	out = strings.ReplaceAll(out, "{message}", r.message)
	out = strings.ReplaceAll(out, "{json}", r.encoded)
	out = strings.ReplaceAll(out, "{timestamp}", fmt.Sprintf("%v", r.payload["timestamp"]))
	for key, value := range ConfigMap(r.payload, "data") {
		out = strings.ReplaceAll(out, "{data."+key+"}", fmt.Sprintf("%v", value))
	}
	return out
}

// deliveryID derives a stable-ish identifier for one delivery, useful to
// correlate a webhook call with the Sync logs.
func deliveryID(msg Message) string {
	at := msg.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return fmt.Sprintf("%s-%d", msg.Event, at.UTC().UnixNano())
}
