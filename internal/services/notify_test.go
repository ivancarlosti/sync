package services

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/crypto"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/notify"
)

// testNotifier builds a notifier with no store: the configuration, rendering and
// validation helpers do not touch the database, so they are testable in a unit
// test run that has no MySQL instance.
func testNotifier(t *testing.T) *Notifier {
	t.Helper()
	key, err := crypto.RandomBytes(32)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	return NewNotifier(nil, notify.NewDispatcher(time.Second, notify.Options{}), NewSecretBox(key))
}

func TestSealAndOpenConfig(t *testing.T) {
	n := testNotifier(t)
	plain := map[string]any{
		"host":     "smtp.example.com",
		"port":     float64(587),
		"username": "sync",
		"password": "hunter2",
		"from":     "sync@example.com",
		"to":       "ops@example.com",
	}
	sealed, err := n.sealConfig(notify.KindSMTP, plain, nil)
	if err != nil {
		t.Fatalf("sealConfig() error = %v", err)
	}
	if strings.Contains(sealed, "hunter2") {
		t.Fatal("the password must not be stored in clear")
	}
	stored := map[string]any{}
	if err := json.Unmarshal([]byte(sealed), &stored); err != nil {
		t.Fatalf("the sealed configuration must stay JSON: %v", err)
	}
	password, _ := stored["password"].(string)
	if !crypto.IsEncrypted(password) {
		t.Fatalf("the password is not sealed: %q", password)
	}
	if stored["host"] != "smtp.example.com" {
		t.Fatalf("non secret values must stay readable: %v", stored)
	}

	channel := &models.NotificationChannel{Name: "ops", Type: notify.KindSMTP, Config: sealed}
	open, err := n.openConfig(channel)
	if err != nil {
		t.Fatalf("openConfig() error = %v", err)
	}
	if open["password"] != "hunter2" {
		t.Fatalf("password = %v", open["password"])
	}

	masked := n.maskConfig(notify.KindSMTP, stored)
	if masked["password"] != maskedSecret {
		t.Fatalf("masked password = %v", masked["password"])
	}
	if masked["username"] != "sync" {
		t.Fatalf("a non secret value must not be masked: %v", masked["username"])
	}
	if Mask(true) != maskedSecret {
		t.Fatalf("Mask(true) = %q, want %q", Mask(true), maskedSecret)
	}
}

func TestSealConfigKeepsStoredSecret(t *testing.T) {
	n := testNotifier(t)
	previous, err := n.sealConfig(notify.KindSMTP, map[string]any{"password": "hunter2"}, nil)
	if err != nil {
		t.Fatalf("sealConfig() error = %v", err)
	}
	previousPlain, err := n.openConfig(&models.NotificationChannel{Name: "ops", Type: notify.KindSMTP, Config: previous})
	if err != nil {
		t.Fatalf("openConfig() error = %v", err)
	}

	// The UI submits the mask when the operator does not retype the secret: the
	// stored secret must survive the round trip.
	sealed, err := n.sealConfig(notify.KindSMTP, map[string]any{"host": "h", "password": maskedSecret}, previousPlain)
	if err != nil {
		t.Fatalf("sealConfig(mask) error = %v", err)
	}
	stored := map[string]any{}
	if err := json.Unmarshal([]byte(sealed), &stored); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if password, _ := stored["password"].(string); !crypto.IsEncrypted(password) {
		t.Fatalf("the mask must keep a sealed secret: %v", stored["password"])
	}
	open, err := n.openConfig(&models.NotificationChannel{Name: "ops", Type: notify.KindSMTP, Config: sealed})
	if err != nil {
		t.Fatalf("openConfig() error = %v", err)
	}
	if open["password"] != "hunter2" {
		t.Fatalf("the stored secret was not reused: %v", open["password"])
	}

	sealed, err = n.sealConfig(notify.KindSMTP, map[string]any{"host": "h", "password": ""}, previousPlain)
	if err != nil {
		t.Fatalf("sealConfig(empty) error = %v", err)
	}
	stored = map[string]any{}
	if err := json.Unmarshal([]byte(sealed), &stored); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if _, ok := stored["password"]; ok {
		t.Fatalf("an empty submission must drop the secret: %v", stored["password"])
	}

	// A brand new plaintext secret replaces the stored one.
	sealed, err = n.sealConfig(notify.KindSMTP, map[string]any{"password": "new-secret"}, previousPlain)
	if err != nil {
		t.Fatalf("sealConfig(new) error = %v", err)
	}
	open, err = n.openConfig(&models.NotificationChannel{Name: "ops", Type: notify.KindSMTP, Config: sealed})
	if err != nil {
		t.Fatalf("openConfig() error = %v", err)
	}
	if open["password"] != "new-secret" {
		t.Fatalf("password = %v", open["password"])
	}
}

func TestSealConfigWebhookSecrets(t *testing.T) {
	n := testNotifier(t)
	plain := map[string]any{
		"url":    "https://hooks.example.com/sync",
		"secret": "signing-key",
		"headers": map[string]any{
			"Authorization": "Bearer abc123",
			"X-Trace":       "keep-me",
		},
	}
	sealed, err := n.sealConfig(notify.KindWebhook, plain, nil)
	if err != nil {
		t.Fatalf("sealConfig() error = %v", err)
	}
	if strings.Contains(sealed, "signing-key") || strings.Contains(sealed, "Bearer abc123") {
		t.Fatalf("secrets leaked into the stored configuration: %s", sealed)
	}
	channel := &models.NotificationChannel{Name: "hook", Type: notify.KindWebhook, Config: sealed}
	open, err := n.openConfig(channel)
	if err != nil {
		t.Fatalf("openConfig() error = %v", err)
	}
	if open["secret"] != "signing-key" {
		t.Fatalf("secret = %v", open["secret"])
	}
	headers, ok := open["headers"].(map[string]any)
	if !ok || headers["Authorization"] != "Bearer abc123" {
		t.Fatalf("headers = %v", open["headers"])
	}
	if headers["X-Trace"] != "keep-me" {
		t.Fatalf("a non secret header must stay readable: %v", headers["X-Trace"])
	}
	masked := n.maskConfig(notify.KindWebhook, plain)
	if masked["secret"] != maskedSecret {
		t.Fatalf("masked secret = %v", masked["secret"])
	}
	if headers, ok := masked["headers"].(map[string]any); !ok || headers["Authorization"] != maskedSecret {
		t.Fatalf("masked headers = %v", masked["headers"])
	}

	// The shoutrrr URL embeds credentials, so it is a secret of its own kind.
	sealed, err = n.sealConfig(notify.KindShoutrrr, map[string]any{"url": "slack://token-a/token-b/token-c"}, nil)
	if err != nil {
		t.Fatalf("sealConfig(shoutrrr) error = %v", err)
	}
	if strings.Contains(sealed, "slack://") {
		t.Fatalf("the shoutrrr URL must be sealed: %s", sealed)
	}
	open, err = n.openConfig(&models.NotificationChannel{Name: "slack", Type: notify.KindShoutrrr, Config: sealed})
	if err != nil {
		t.Fatalf("openConfig(shoutrrr) error = %v", err)
	}
	if open["url"] != "slack://token-a/token-b/token-c" {
		t.Fatalf("url = %v", open["url"])
	}
	if masked := n.maskConfig(notify.KindShoutrrr, map[string]any{"url": "slack://secret"}); masked["url"] != maskedSecret {
		t.Fatalf("masked url = %v", masked["url"])
	}
}

func TestOpenConfigRejectsCorruptedSecret(t *testing.T) {
	n := testNotifier(t)
	channel := &models.NotificationChannel{
		Name:   "ops",
		Type:   notify.KindSMTP,
		Config: `{"password":"v1:this-is-not-a-valid-payload"}`,
	}
	if _, err := n.openConfig(channel); !errors.Is(err, ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
	broken := &models.NotificationChannel{Name: "x", Type: notify.KindSMTP, Config: "{not json"}
	if _, err := n.openConfig(broken); !errors.Is(err, ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
	// A configuration without secrets opens as is.
	open, err := n.openConfig(&models.NotificationChannel{Name: "x", Type: notify.KindWebhook, Config: `{"url":"https://a.b"}`})
	if err != nil || open["url"] != "https://a.b" {
		t.Fatalf("openConfig() = %v (%v)", open, err)
	}
}

func TestValidateInput(t *testing.T) {
	n := testNotifier(t)
	base := ChannelInput{
		Name:    "ops",
		Type:    notify.KindSMTP,
		Config:  map[string]any{"host": "h", "from": "a@b.c", "to": "c@d.e"},
		Events:  []string{models.EventSyncSuccess},
		Enabled: true,
	}
	plain, err := n.validateInput(base, nil)
	if err != nil {
		t.Fatalf("validateInput() error = %v", err)
	}
	if plain["host"] != "h" {
		t.Fatalf("plain configuration = %v", plain)
	}

	cases := []struct {
		name  string
		input ChannelInput
		bases map[string]any
	}{
		{"missing name", ChannelInput{Type: notify.KindSMTP}, nil},
		{"unknown type", ChannelInput{Name: "x", Type: "carrier-pigeon"}, nil},
		{"unknown event", ChannelInput{Name: "x", Type: notify.KindWebhook, Events: []string{"sync.exploded"}}, nil},
		{"incomplete smtp", ChannelInput{Name: "x", Type: notify.KindSMTP, Config: map[string]any{"host": "h"}}, nil},
		{"mask without a stored secret", ChannelInput{Name: "x", Type: notify.KindWebhook, Config: map[string]any{"url": maskedSecret}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := n.validateInput(tc.input, tc.bases); !errors.Is(err, ErrValidation) {
				t.Fatalf("error = %v, want ErrValidation", err)
			}
		})
	}

	// The mask resolves against the previous plaintext configuration.
	input := ChannelInput{
		Name:   "hook",
		Type:   notify.KindWebhook,
		Config: map[string]any{"url": maskedSecret, "method": "POST"},
	}
	plain, err = n.validateInput(input, map[string]any{"url": "https://hooks.example.com/sync"})
	if err != nil {
		t.Fatalf("validateInput(mask) error = %v", err)
	}
	if plain["url"] != "https://hooks.example.com/sync" {
		t.Fatalf("url = %v", plain["url"])
	}
}

func TestRenderMessage(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	success := renderMessage(models.EventSyncSuccess, map[string]any{
		"job":      "nightly",
		"added":    float64(2),
		"updated":  1,
		"deleted":  float64(0),
		"skipped":  float64(3),
		"duration": "1m12s",
	}, at)
	if !strings.Contains(success.Title, "nightly") {
		t.Fatalf("title = %q", success.Title)
	}
	for _, want := range []string{"added: 2", "updated: 1", "unchanged: 3", "Duration: 1m12s"} {
		if !strings.Contains(success.Body, want) {
			t.Fatalf("body %q is missing %q", success.Body, want)
		}
	}
	if strings.Contains(success.Body, "deleted") {
		t.Fatalf("a zero counter must be hidden: %q", success.Body)
	}
	if !success.At.Equal(at) || success.Event != models.EventSyncSuccess {
		t.Fatalf("unexpected message: %+v", success)
	}

	failed := renderMessage(models.EventSyncRunFailed, map[string]any{"job": "nightly", "error": "drive is down"}, at)
	if failed.Title != "Sync failed: nightly" || !strings.Contains(failed.Body, "drive is down") {
		t.Fatalf("unexpected failure message: %+v", failed)
	}

	problem := renderMessage(models.EventSyncError, map[string]any{"error": "quota exceeded"}, at)
	if problem.Title != "Sync problem" || !strings.Contains(problem.Body, "quota exceeded") {
		t.Fatalf("unexpected problem message: %+v", problem)
	}

	connected := renderMessage(models.EventAccountConnected, map[string]any{"provider": "google", "email": "a@b.c"}, at)
	if !strings.Contains(connected.Body, "google") || !strings.Contains(connected.Body, "a@b.c") {
		t.Fatalf("unexpected connected message: %+v", connected)
	}

	needsAttention := renderMessage(models.EventAccountError, map[string]any{"provider": "microsoft", "email": "a@b.c", "error": "invalid_grant"}, at)
	if !strings.Contains(needsAttention.Body, "invalid_grant") {
		t.Fatalf("unexpected account error message: %+v", needsAttention)
	}

	tested := renderMessage(models.EventTest, nil, at)
	if tested.Title != "Sync test notification" {
		t.Fatalf("unexpected test message: %+v", tested)
	}

	other := renderMessage("sync.something", nil, at)
	if other.Title != "Sync notification" || !strings.Contains(other.Body, "sync.something") {
		t.Fatalf("unexpected fallback message: %+v", other)
	}
}

func TestNotifierMessageAddsLink(t *testing.T) {
	n := testNotifier(t)
	msg := n.message(models.EventTest, nil, time.Now())
	if strings.Contains(msg.Body, "https://") {
		t.Fatalf("no link must be added without one: %q", msg.Body)
	}
	n.SetLink("https://sync.example.com/")
	msg = n.message(models.EventTest, nil, time.Now())
	if !strings.Contains(msg.Body, "https://sync.example.com") {
		t.Fatalf("the deployment link is missing: %q", msg.Body)
	}
	if msg.Data["link"] != "https://sync.example.com" {
		t.Fatalf("the payload must carry the link: %v", msg.Data)
	}
	// A payload that brings its own link wins.
	msg = n.message(models.EventTest, map[string]any{"link": "https://runbook.example.com"}, time.Now())
	if !strings.Contains(msg.Body, "https://runbook.example.com") || strings.Contains(msg.Body, "sync.example.com") {
		t.Fatalf("the payload link must win: %q", msg.Body)
	}
	// The caller payload is never mutated.
	data := map[string]any{"job": "nightly"}
	_ = n.message(models.EventTest, data, time.Now())
	if _, ok := data["link"]; ok {
		t.Fatal("the caller payload was mutated")
	}
}

func TestSubscribesTo(t *testing.T) {
	cases := []struct {
		events string
		event  string
		want   bool
	}{
		{"", models.EventSyncSuccess, true},
		{`["sync.success","test"]`, models.EventSyncSuccess, true},
		{`["test"]`, models.EventSyncSuccess, false},
		{"sync.success, sync.error", models.EventSyncError, true},
		{"sync.success, sync.error", models.EventTest, false},
		{"{not a list", models.EventSyncSuccess, true},
	}
	for _, tc := range cases {
		channel := models.NotificationChannel{Events: tc.events}
		if got := subscribesTo(channel, tc.event); got != tc.want {
			t.Errorf("subscribesTo(%q, %q) = %t, want %t", tc.events, tc.event, got, tc.want)
		}
	}
}

func TestEncodeEvents(t *testing.T) {
	if got := encodeEvents(nil); got != "" {
		t.Fatalf("encodeEvents(nil) = %q", got)
	}
	if got := encodeEvents([]string{" ", ""}); got != "" {
		t.Fatalf("encodeEvents(blank) = %q", got)
	}
	got := encodeEvents([]string{"test", "test", " sync.success "})
	want := `["test","sync.success"]`
	if got != want {
		t.Fatalf("encodeEvents() = %q, want %q", got, want)
	}
	if events := channelEvents(models.NotificationChannel{Events: got}); len(events) != 2 || events[1] != "sync.success" {
		t.Fatalf("channelEvents() = %v", events)
	}
}

func TestNotifierViewMasksSecrets(t *testing.T) {
	n := testNotifier(t)
	sealed, err := n.sealConfig(notify.KindSMTP, map[string]any{
		"host":     "h",
		"password": "s3cret",
		"from":     "a@b.c",
		"to":       "c@d.e",
	}, nil)
	if err != nil {
		t.Fatalf("sealConfig() error = %v", err)
	}
	used := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	view := n.view(&models.NotificationChannel{
		ID:         3,
		Name:       "ops",
		Type:       notify.KindSMTP,
		Config:     sealed,
		Events:     `["sync.success"]`,
		Enabled:    true,
		LastStatus: "ok",
		LastUsedAt: &used,
	})
	if view.ID != 3 || view.Name != "ops" || !view.Enabled || view.LastStatus != "ok" {
		t.Fatalf("unexpected view: %+v", view)
	}
	if view.Config["password"] != maskedSecret {
		t.Fatalf("the view leaked the password: %v", view.Config["password"])
	}
	if view.Config["host"] != "h" {
		t.Fatalf("the view lost a field: %v", view.Config)
	}
	if len(view.Events) != 1 || view.Events[0] != models.EventSyncSuccess {
		t.Fatalf("events = %v", view.Events)
	}
	if view.LastUsedAt == nil || !view.LastUsedAt.Equal(used) {
		t.Fatalf("last used at = %v", view.LastUsedAt)
	}

	// A configuration that is not valid JSON must not panic the API.
	broken := n.view(&models.NotificationChannel{Name: "broken", Type: notify.KindWebhook, Config: "{oops"})
	if len(broken.Config) != 0 {
		t.Fatalf("config = %v", broken.Config)
	}
}

// TestSanitizeConfigErrorMasksSecrets checks that a failure reported by a sender
// cannot carry a configuration secret back into the database or the API: the
// shoutrrr URL embeds the credentials of the destination and HTTP errors repeat
// the target they called.
func TestSanitizeConfigErrorMasksSecrets(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		config   map[string]any
		message  string
		wanted   string
		unwanted string
	}{
		{
			name:     "the shoutrrr url is a secret for the whole kind",
			kind:     notify.KindShoutrrr,
			config:   map[string]any{"url": "slack://xoxb-12345-secret@chan/a"},
			message:  `notify: invalid channel configuration: "slack://xoxb-12345-secret@chan/a" is not an absolute URL`,
			wanted:   maskedSecret,
			unwanted: "xoxb-12345-secret",
		},
		{
			name:     "a key named like a secret",
			kind:     notify.KindSMTP,
			config:   map[string]any{"host": "smtp.example.com", "password": "hunter2"},
			message:  "notify: authenticating hunter2 failed",
			wanted:   maskedSecret,
			unwanted: "hunter2",
		},
		{
			name:    "an error without any secret is left alone",
			kind:    notify.KindWebhook,
			config:  map[string]any{"url": "http://127.0.0.1:1/hook"},
			message: `notify: calling the webhook: Post "http://127.0.0.1:1/hook": connection refused`,
			wanted:  "connection refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sanitizeConfigError(tc.kind, tc.config, errors.New(tc.message))
			if err == nil {
				t.Fatal("sanitizeConfigError() = nil, want the original failure")
			}
			if !strings.Contains(err.Error(), tc.wanted) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wanted)
			}
			if tc.unwanted != "" && strings.Contains(err.Error(), tc.unwanted) {
				t.Errorf("error = %q, it must not contain %q", err, tc.unwanted)
			}
		})
	}

	if err := sanitizeConfigError(notify.KindSMTP, map[string]any{"password": "x"}, nil); err != nil {
		t.Errorf("sanitizeConfigError(nil) = %v, want nil", err)
	}
}

func TestNotifierKindsAndEvents(t *testing.T) {
	n := testNotifier(t)
	kinds := n.Kinds()
	if len(kinds) != 3 || kinds[0] != notify.KindShoutrrr || kinds[1] != notify.KindSMTP || kinds[2] != notify.KindWebhook {
		t.Fatalf("Kinds() = %v", kinds)
	}
	events := n.Events()
	if len(events) != len(models.AllEvents) {
		t.Fatalf("Events() = %v", events)
	}
	events[0] = "mutated"
	if models.AllEvents[0] == "mutated" {
		t.Fatal("Events() must return a copy")
	}
}
