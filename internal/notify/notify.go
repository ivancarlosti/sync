// Package notify delivers Sync events to the channels configured by the
// operator (SMTP e-mail, generic webhook, or any shoutrrr URL).
//
// The package is deliberately independent from the storage layer: it receives an
// already decrypted configuration map plus a rendered Message, which keeps
// secrets out of this package and makes every sender testable with a fake
// channel. See docs/notifications.md for the configuration schemas.
package notify

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Channel kinds understood by the dispatcher.
const (
	// KindSMTP is an e-mail channel described by a structured configuration.
	KindSMTP = "smtp"
	// KindWebhook is an HTTP channel with method, headers and body template.
	KindWebhook = "webhook"
	// KindShoutrrr is a raw shoutrrr URL (slack://, telegram://, ...).
	KindShoutrrr = "shoutrrr"
)

// Kinds lists every channel kind, sorted for stable API/UI output.
func Kinds() []string {
	return []string{KindSMTP, KindShoutrrr, KindWebhook}
}

// ErrUnknownKind marks a channel whose type has no sender in this build.
var ErrUnknownKind = errors.New("notify: unknown channel kind")

// ErrInvalidConfig marks a channel configuration the sender cannot use.
var ErrInvalidConfig = errors.New("notify: invalid channel configuration")

// Message is a rendered notification. Title/Body are already localised by the
// caller (Sync renders notifications in English, see docs/notifications.md);
// Data carries the machine readable payload for webhooks.
type Message struct {
	Event string
	Title string
	Body  string
	Data  map[string]any
	At    time.Time
}

// Text renders the message for transports that only take one string (e-mail
// body, shoutrrr).
func (m Message) Text() string {
	switch {
	case m.Title == "":
		return m.Body
	case m.Body == "":
		return m.Title
	default:
		return m.Title + "\n\n" + m.Body
	}
}

// Subject renders the e-mail subject line.
func (m Message) Subject() string {
	if m.Title == "" {
		return "[Sync] " + m.Event
	}
	return "[Sync] " + m.Title
}

// Payload renders the machine readable JSON document used by the default
// webhook body and by `{...}` placeholders.
func (m Message) Payload() map[string]any {
	at := m.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	payload := map[string]any{
		"event":     m.Event,
		"title":     m.Title,
		"message":   m.Body,
		"timestamp": at.UTC().Format(time.RFC3339),
	}
	if len(m.Data) > 0 {
		payload["data"] = m.Data
	}
	return payload
}

// Sender delivers a message through one channel kind.
type Sender interface {
	// Kind is the channel type this sender handles.
	Kind() string
	// Send delivers msg using an already decrypted configuration map.
	Send(ctx context.Context, config map[string]any, msg Message) error
}

// Options carries the process-wide settings of the dispatcher. They come from
// the environment (see docs/configuration.md), never from a channel: a channel
// cannot widen its own permissions.
type Options struct {
	// AllowPrivateTargets lets a webhook channel point at an address that is not
	// publicly routable: a target on the local network (Gotify, ntfy, Home
	// Assistant), another container addressed by service name, or a Tailscale
	// address. It is off by default because the notification worker runs inside
	// the same host as the rest of Sync, so without the refusal a channel could
	// be used to reach the API itself, the database, another container or the
	// cloud metadata endpoint at 169.254.169.254. Set
	// NOTIFY_ALLOW_PRIVATE_TARGETS=true to allow it.
	AllowPrivateTargets bool
}

// Dispatcher routes a message to the sender registered for its channel kind.
type Dispatcher struct {
	senders map[string]Sender
	// options is the operator's policy, applied by Validate as well as by every
	// send, so a channel that could never deliver is refused before it is saved.
	options Options
}

// NewDispatcher builds the dispatcher with the senders of this build: shoutrrr
// (which covers SMTP and every URL-scheme service) and the native webhook sender.
func NewDispatcher(timeout time.Duration, options Options) *Dispatcher {
	dispatcher := &Dispatcher{senders: map[string]Sender{}, options: options}
	dispatcher.Register(NewShoutrrrSender(timeout))
	dispatcher.Register(NewWebhookSender(timeout, options))
	return dispatcher
}

// handledBy lists every kind a sender answers for. Register uses it so a sender
// covering several kinds (the shoutrrr one covers the structured `smtp` form and
// the raw `shoutrrr` URL) is reachable under each of them: a channel stored with
// the raw kind could otherwise never be validated nor delivered.
func handledBy(sender Sender) []string {
	kinds := []string{sender.Kind()}
	multi, ok := sender.(interface{ Handles(string) bool })
	if !ok {
		return kinds
	}
	for _, kind := range Kinds() {
		if kind == sender.Kind() || !multi.Handles(kind) {
			continue
		}
		kinds = append(kinds, kind)
	}
	return kinds
}

// Register adds or replaces the sender of every channel kind it handles.
func (d *Dispatcher) Register(sender Sender) {
	if sender == nil {
		return
	}
	for _, kind := range handledBy(sender) {
		d.senders[kind] = sender
	}
}

// Kinds lists the registered channel kinds, sorted.
func (d *Dispatcher) Kinds() []string {
	kinds := make([]string, 0, len(d.senders))
	for kind := range d.senders {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// Send delivers a message through one channel. A failing channel never stops the
// others: the caller decides what to do with the error.
func (d *Dispatcher) Send(ctx context.Context, kind string, config map[string]any, msg Message) error {
	sender, ok := d.senders[kind]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	return sender.Send(ctx, config, msg)
}

// Validate checks a configuration with the sender of its kind, without
// delivering anything. It is what Admin > Notifications calls before saving.
func (d *Dispatcher) Validate(kind string, config map[string]any) error {
	switch kind {
	case KindSMTP, KindShoutrrr:
		_, err := ShoutrrrURL(kind, config)
		return err
	case KindWebhook:
		_, err := NewRequest(context.Background(), config, Message{Event: "test", Title: "Sync", Body: "test"}, d.options)
		return err
	default:
		return fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
}

// ---------------------------------------------------------------------------
// Configuration helpers
//
// A channel configuration arrives as a decoded JSON object, so numbers are
// float64 and nested objects are map[string]any. These helpers accept every
// shape an operator may have typed and never panic on a missing key.
// ---------------------------------------------------------------------------

// ConfigString returns a trimmed string value.
func ConfigString(config map[string]any, key string) string {
	value, ok := config[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return fmt.Sprintf("%v", typed)
	case bool:
		return fmt.Sprintf("%t", typed)
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// ConfigInt returns an integer value, falling back on a missing/malformed one.
func ConfigInt(config map[string]any, key string, fallback int) int {
	value, ok := config[key]
	if !ok || value == nil {
		return fallback
	}
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case string:
		var parsed int
		if _, err := fmt.Sscanf(typed, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}

// ConfigBool returns a boolean value, falling back on a missing/malformed one.
func ConfigBool(config map[string]any, key string, fallback bool) bool {
	value, ok := config[key]
	if !ok || value == nil {
		return fallback
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch typed {
		case "true", "yes", "on", "1":
			return true
		case "false", "no", "off", "0":
			return false
		}
	}
	return fallback
}

// ConfigMap returns a nested object.
func ConfigMap(config map[string]any, key string) map[string]any {
	value, ok := config[key]
	if !ok || value == nil {
		return nil
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return nil
}

// ConfigStrings returns a list, accepting a JSON array or a comma separated
// string (what an operator types in a single text field).
func ConfigStrings(config map[string]any, key string) []string {
	value, ok := config[key]
	if !ok || value == nil {
		return nil
	}
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	case []string:
		return typed
	case string:
		fields := make([]string, 0, 4)
		for _, part := range splitCommas(typed) {
			if part != "" {
				fields = append(fields, part)
			}
		}
		return fields
	default:
		return nil
	}
}

// splitCommas splits on commas and trims every part.
func splitCommas(value string) []string {
	parts := []string{}
	current := ""
	for _, r := range value {
		if r == ',' || r == ';' {
			parts = append(parts, trimSpace(current))
			current = ""
			continue
		}
		current += string(r)
	}
	return append(parts, trimSpace(current))
}

// trimSpace trims the ASCII whitespace an operator may paste around a value.
func trimSpace(value string) string {
	start, end := 0, len(value)
	for start < end && isSpace(value[start]) {
		start++
	}
	for end > start && isSpace(value[end-1]) {
		end--
	}
	return value[start:end]
}

// isSpace reports whether b is an ASCII whitespace byte.
func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
