package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testMessage builds the message used across the rendering tests.
func testMessage() Message {
	return Message{
		Event: "sync.success",
		Title: "Sync completed: nightly",
		Body:  "The synchronization finished successfully.",
		At:    time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
		Data:  map[string]any{"added": float64(3), "job": "nightly"},
	}
}

func TestMessageTextSubjectAndPayload(t *testing.T) {
	msg := testMessage()
	if got, want := msg.Text(), "Sync completed: nightly\n\nThe synchronization finished successfully."; got != want {
		t.Fatalf("Text() = %q, want %q", got, want)
	}
	if got, want := msg.Subject(), "[Sync] Sync completed: nightly"; got != want {
		t.Fatalf("Subject() = %q, want %q", got, want)
	}
	payload := msg.Payload()
	if payload["event"] != "sync.success" || payload["title"] != msg.Title || payload["message"] != msg.Body {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if payload["timestamp"] != "2026-09-27T12:00:00Z" {
		t.Fatalf("timestamp = %v", payload["timestamp"])
	}
	data, ok := payload["data"].(map[string]any)
	if !ok || data["job"] != "nightly" {
		t.Fatalf("payload data = %+v", payload["data"])
	}

	empty := Message{Event: "test"}
	if empty.Text() != "" {
		t.Fatalf("empty Text() = %q", empty.Text())
	}
	if empty.Subject() != "[Sync] test" {
		t.Fatalf("empty Subject() = %q", empty.Subject())
	}
	if empty.Payload()["timestamp"] == "" {
		t.Fatal("a zero time must still produce a timestamp")
	}
}

func TestConfigHelpers(t *testing.T) {
	config := map[string]any{
		"text":    "value",
		"number":  float64(587),
		"integer": 42,
		"quoted":  "25",
		"flag":    true,
		"off":     "off",
		"list":    []any{"a", "b", ""},
		"csv":     " a@b.c ; d@e.f , ",
		"nested":  map[string]any{"k": "v"},
		"absent":  nil,
		"unknown": struct{}{},
	}
	if got := ConfigString(config, "text"); got != "value" {
		t.Errorf("ConfigString(text) = %q", got)
	}
	if got := ConfigString(config, "number"); got != "587" {
		t.Errorf("ConfigString(number) = %q", got)
	}
	if got := ConfigString(config, "flag"); got != "true" {
		t.Errorf("ConfigString(flag) = %q", got)
	}
	if got := ConfigInt(config, "number", 1); got != 587 {
		t.Errorf("ConfigInt(number) = %d", got)
	}
	if got := ConfigInt(config, "integer", 1); got != 42 {
		t.Errorf("ConfigInt(integer) = %d", got)
	}
	if got := ConfigInt(config, "quoted", 1); got != 25 {
		t.Errorf("ConfigInt(quoted) = %d", got)
	}
	if got := ConfigInt(config, "missing", 7); got != 7 {
		t.Errorf("ConfigInt(missing) = %d", got)
	}
	if !ConfigBool(config, "flag", false) {
		t.Error("ConfigBool(flag) = false")
	}
	if ConfigBool(config, "off", true) {
		t.Error("ConfigBool(off) = true")
	}
	if !ConfigBool(config, "missing", true) {
		t.Error("ConfigBool(missing) = false")
	}
	if list := ConfigStrings(config, "list"); len(list) != 2 || list[0] != "a" || list[1] != "b" {
		t.Errorf("ConfigStrings(list) = %v", list)
	}
	if list := ConfigStrings(config, "csv"); len(list) != 2 || list[0] != "a@b.c" || list[1] != "d@e.f" {
		t.Errorf("ConfigStrings(csv) = %v", list)
	}
	if ConfigStrings(config, "missing") != nil {
		t.Error("ConfigStrings(missing) must be nil")
	}
	if nested := ConfigMap(config, "nested"); nested["k"] != "v" {
		t.Errorf("ConfigMap(nested) = %v", nested)
	}
	if ConfigMap(config, "text") != nil {
		t.Error("ConfigMap of a scalar must be nil")
	}
	if got := ConfigString(config, "unknown"); got == "" {
		t.Error("ConfigString must render any scalar shape")
	}
	if _, err := json.Marshal(config); err != nil {
		t.Fatalf("the fixture must stay encodable: %v", err)
	}
}

func TestSMPTURL(t *testing.T) {
	config := map[string]any{
		"host":       "smtp.example.com",
		"port":       float64(465),
		"username":   "sync",
		"password":   "p@ss word",
		"from":       "sync@example.com",
		"from_name":  "Sync Alerts",
		"to":         "ops@example.com, oncall@example.com",
		"encryption": "implicit",
		"use_html":   true,
		"subject":    "[Sync] nightly",
	}
	raw, err := SMPTURL(config)
	if err != nil {
		t.Fatalf("SMPTURL() error = %v", err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the built URL must parse: %v", err)
	}
	if parsed.Scheme != "smtp" || parsed.Host != "smtp.example.com:465" {
		t.Fatalf("unexpected target: %s", parsed)
	}
	if parsed.User.Username() != "sync" {
		t.Fatalf("username = %q", parsed.User.Username())
	}
	password, _ := parsed.User.Password()
	if password != "p@ss word" {
		t.Fatalf("password = %q", password)
	}
	query := parsed.Query()
	if query.Get("from") != "sync@example.com" || query.Get("fromname") != "Sync Alerts" {
		t.Fatalf("from fields = %v", query)
	}
	if query.Get("to") != "ops@example.com,oncall@example.com" {
		t.Fatalf("to = %q", query.Get("to"))
	}
	if query.Get("encryption") != "ImplicitTLS" {
		t.Fatalf("encryption = %q", query.Get("encryption"))
	}
	if query.Get("usehtml") != "yes" || query.Get("subject") != "[Sync] nightly" {
		t.Fatalf("optional fields = %v", query)
	}

	// The default port is 587 and the default encryption is Auto.
	raw, err = SMPTURL(map[string]any{"host": "h", "from": "a@b.c", "to": "c@d.e"})
	if err != nil {
		t.Fatalf("SMPTURL() with defaults error = %v", err)
	}
	parsed, _ = url.Parse(raw)
	if parsed.Host != "h:587" || parsed.Query().Get("encryption") != "Auto" {
		t.Fatalf("defaults not applied: %s", parsed)
	}
	if _, password := parsed.User.Password(); password {
		t.Fatal("no credentials must be emitted without a username")
	}
}

func TestSMPTURLValidation(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]any
	}{
		{"missing host", map[string]any{"from": "a@b.c", "to": "c@d.e"}},
		{"missing from", map[string]any{"host": "h", "to": "c@d.e"}},
		{"missing to", map[string]any{"host": "h", "from": "a@b.c"}},
		{"invalid port", map[string]any{"host": "h", "from": "a@b.c", "to": "c@d.e", "port": float64(0)}},
		{"huge port", map[string]any{"host": "h", "from": "a@b.c", "to": "c@d.e", "port": float64(70000)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SMPTURL(tc.config)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}

	// A rejection must never echo a secret back to the caller/logs.
	secret := "sup3r-s3cret"
	_, err := SMPTURL(map[string]any{"host": "h", "password": secret, "port": float64(99999)})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("the error must be free of secrets: %v", err)
	}
}

func TestEncryptionValue(t *testing.T) {
	cases := map[string]string{
		"":           "Auto",
		"auto":       "Auto",
		"none":       "none",
		"off":        "none",
		"starttls":   "ExplicitTLS",
		" EXPLICIT ": "ExplicitTLS",
		"implicit":   "ImplicitTLS",
		"ssl":        "ImplicitTLS",
		"nonsense":   "Auto",
	}
	for input, want := range cases {
		if got := encryptionValue(input); got != want {
			t.Errorf("encryptionValue(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestShoutrrrURL(t *testing.T) {
	raw, err := ShoutrrrURL(KindShoutrrr, map[string]any{"url": "slack://token-a/token-b/token-c@channel"})
	if err != nil {
		t.Fatalf("ShoutrrrURL() error = %v", err)
	}
	if !strings.HasPrefix(raw, "slack://") {
		t.Fatalf("raw URL = %q", raw)
	}
	if _, err := ShoutrrrURL(KindShoutrrr, map[string]any{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("missing URL error = %v", err)
	}
	if _, err := ShoutrrrURL(KindShoutrrr, map[string]any{"url": "relative/path"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("relative URL error = %v", err)
	}
	if _, err := ShoutrrrURL("carrier-pigeon", map[string]any{"url": "pigeon://x"}); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("unknown kind error = %v", err)
	}
	smtp, err := ShoutrrrURL(KindSMTP, map[string]any{"host": "h", "from": "a@b.c", "to": "c@d.e"})
	if err != nil || !strings.HasPrefix(smtp, "smtp://h:587") {
		t.Fatalf("smtp URL = %q (%v)", smtp, err)
	}
}

func TestWebhookRequestDefaults(t *testing.T) {
	msg := testMessage()
	request, err := NewRequest(context.Background(), map[string]any{"url": "https://hooks.example.com/sync"}, msg)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected request: %s %s", request.Method, request.Header.Get("Content-Type"))
	}
	if request.Header.Get("X-Sync-Event") != "sync.success" || request.Header.Get("X-Sync-Delivery") == "" {
		t.Fatalf("missing Sync headers: %v", request.Header)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("the default body must be JSON: %v", err)
	}
	if payload["event"] != "sync.success" || payload["title"] != msg.Title {
		t.Fatalf("unexpected payload: %s", body)
	}
}

func TestWebhookRequestTemplateHeadersAndSignature(t *testing.T) {
	msg := testMessage()
	msg.Data["link"] = "https://sync.example.com/runs/7"
	config := map[string]any{
		"url":           "https://hooks.example.com/sync",
		"method":        "put",
		"content_type":  "text/plain",
		"secret":        "topsecret",
		"body_template": "{event}|{title}|{message}|{data.job}|{data.link}|{timestamp}",
		"headers": map[string]any{
			"X-Api-Key":     "v1:sealed",
			"X-Not-A-Value": 12,
		},
	}
	request, err := NewRequest(context.Background(), config, msg)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if request.Method != "PUT" || request.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("unexpected request: %s %s", request.Method, request.Header.Get("Content-Type"))
	}
	if request.Header.Get("X-Api-Key") != "v1:sealed" {
		t.Fatalf("custom header lost: %v", request.Header)
	}
	if request.Header.Get("X-Not-A-Value") != "" {
		t.Fatal("a non string header value must be ignored")
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	want := "sync.success|" + msg.Title + "|" + msg.Body + "|nightly|https://sync.example.com/runs/7|2026-09-27T12:00:00Z"
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
	mac := hmac.New(sha256.New, []byte("topsecret"))
	mac.Write(body)
	if got, want := request.Header.Get("X-Sync-Signature"), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

func TestWebhookRequestValidation(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]any
	}{
		{"missing url", map[string]any{}},
		{"empty url", map[string]any{"url": "  "}},
		{"scheme not supported", map[string]any{"url": "ftp://example.com/hook"}},
		{"relative url", map[string]any{"url": "/hook"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRequest(context.Background(), tc.config, testMessage()); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
	if _, err := NewRequest(context.Background(), map[string]any{
		"url": "https://hooks.example.com/sync", "body_template": "{json}",
	}, testMessage()); err != nil {
		t.Fatalf("a {json} template must render: %v", err)
	}
}

func TestWebhookSenderSend(t *testing.T) {
	var captured struct {
		method  string
		event   string
		sign    string
		body    string
		reached bool
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.method = r.Method
		captured.event = r.Header.Get("X-Sync-Event")
		captured.sign = r.Header.Get("X-Sync-Signature")
		captured.body = string(body)
		captured.reached = true
		if r.URL.Path == "/broken" {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sender := NewWebhookSender(5 * time.Second)
	config := map[string]any{"url": server.URL + "/ok", "secret": "s3cret"}
	if err := sender.Send(context.Background(), config, testMessage()); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if !captured.reached || captured.method != http.MethodPost || captured.event != "sync.success" {
		t.Fatalf("unexpected delivery: %+v", captured)
	}
	if !strings.Contains(captured.body, "sync.success") || captured.sign == "" {
		t.Fatalf("unexpected body/signature: %+v", captured)
	}

	config["url"] = server.URL + "/broken"
	err := sender.Send(context.Background(), config, testMessage())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("a non 2xx answer must fail: %v", err)
	}
}

func TestDispatcher(t *testing.T) {
	dispatcher := NewDispatcher(time.Second)
	// `shoutrrr` is the raw URL kind of the sender that also provides the
	// structured `smtp` form, so both must be routed to it.
	if got := dispatcher.Kinds(); len(got) != 3 || got[0] != KindShoutrrr || got[1] != KindSMTP || got[2] != KindWebhook {
		t.Fatalf("Kinds() = %v", got)
	}
	if err := dispatcher.Send(context.Background(), "carrier-pigeon", nil, testMessage()); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Send() error = %v", err)
	}
	// The raw kind must reach the shoutrrr sender instead of being reported as
	// unknown: the error proves the configuration was inspected.
	if err := dispatcher.Send(context.Background(), KindShoutrrr, map[string]any{"url": "not-a-url"}, testMessage()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Send(shoutrrr) error = %v", err)
	}
	if err := dispatcher.Validate(KindSMTP, map[string]any{"host": "h", "from": "a@b.c", "to": "c@d.e"}); err != nil {
		t.Fatalf("Validate(smtp) error = %v", err)
	}
	if err := dispatcher.Validate(KindWebhook, map[string]any{"url": "https://example.com/hook"}); err != nil {
		t.Fatalf("Validate(webhook) error = %v", err)
	}
	if err := dispatcher.Validate("carrier-pigeon", nil); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Validate(unknown) error = %v", err)
	}
	if kinds := Kinds(); len(kinds) != 3 || kinds[0] != KindSMTP || kinds[1] != KindShoutrrr || kinds[2] != KindWebhook {
		t.Fatalf("Kinds() = %v", kinds)
	}
}
