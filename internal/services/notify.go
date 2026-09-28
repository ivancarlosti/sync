package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ivancarlosti/sync/internal/crypto"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/notify"
)

// maskedSecret is what the API returns in place of a stored secret. Sending it
// back on update keeps the stored value, exactly like the provider credentials.
const maskedSecret = "********"

// EventPublisher publishes a notification event. It is the narrow seam between
// the services that detect an event (OAuth callback, sync engine, scheduler) and
// the delivery logic, so those services stay testable without a mail server.
type EventPublisher interface {
	// Publish delivers an event. It must never block the caller.
	Publish(ctx context.Context, event string, data map[string]any)
}

// ChannelInput is the payload accepted from the API when a channel is created or
// updated. Secrets arrive in plaintext, are sealed here and never leave it.
type ChannelInput struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Config  map[string]any `json:"config"`
	Events  []string       `json:"events"`
	Enabled bool           `json:"enabled"`
}

// ChannelView is the masked channel returned by the API: every secret is
// replaced by maskedSecret. Config is a copy, so callers cannot mutate stored
// values by accident.
type ChannelView struct {
	ID         uint           `json:"id"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Config     map[string]any `json:"config"`
	Events     []string       `json:"events"`
	Enabled    bool           `json:"enabled"`
	LastStatus string         `json:"last_status,omitempty"`
	LastError  string         `json:"last_error,omitempty"`
	LastUsedAt *time.Time     `json:"last_used_at,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// Notifier owns the notification channels: its CRUD operations keep the secrets
// encrypted, and its Publish method delivers events to every subscribed channel.
type Notifier struct {
	store      *Store
	dispatcher *notify.Dispatcher
	box        *SecretBox
	link       string
	now        func() time.Time
	wg         sync.WaitGroup
}

// NewNotifier builds the notifier. The dispatcher is built by NewDispatcher and
// can be swapped in tests with a fake sender.
func NewNotifier(store *Store, dispatcher *notify.Dispatcher, box *SecretBox) *Notifier {
	return &Notifier{
		store:      store,
		dispatcher: dispatcher,
		box:        box,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// SetLink configures the `{link}` value used in messages (the public base URL of
// this deployment). An empty value keeps the messages link-free.
func (n *Notifier) SetLink(baseURL string) {
	n.link = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
}

// Kinds lists the channel kinds this build can deliver through.
func (n *Notifier) Kinds() []string { return n.dispatcher.Kinds() }

// Events lists the subscribable events.
func (n *Notifier) Events() []string { return append([]string{}, models.AllEvents...) }

// Publish implements EventPublisher. Delivery runs in a detached goroutine so a
// slow mail server can never delay a sync run or an OAuth callback; use Wait on
// shutdown to let the in-flight deliveries finish.
func (n *Notifier) Publish(ctx context.Context, event string, data map[string]any) {
	channels, err := n.store.ListChannels(ctx)
	if err != nil {
		slog.Warn("listing notification channels", "event", event, "error", err)
		return
	}
	at := n.now()
	msg := n.message(event, data, at)
	for _, channel := range channels {
		if !channel.Enabled || !subscribesTo(channel, event) {
			continue
		}
		channel := channel
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			// The caller context may already be cancelled (request finished),
			// so delivery gets its own bounded context.
			deliverCtx, cancel := context.WithTimeout(context.Background(), n.deliveryTimeout())
			defer cancel()
			if err := n.deliver(deliverCtx, &channel, msg); err != nil {
				slog.Warn("delivering a notification",
					"event", event, "channel", channel.Name, "kind", channel.Type, "error", err)
			}
		}()
	}
}

// Wait blocks until every in-flight delivery finished. It is called on shutdown.
func (n *Notifier) Wait() { n.wg.Wait() }

// deliveryTimeout bounds one delivery attempt.
func (n *Notifier) deliveryTimeout() time.Duration { return 30 * time.Second }

// SendToChannel delivers a message to one channel synchronously, which is what
// the "Send a test notification" button and the scheduled digest use.
func (n *Notifier) SendToChannel(ctx context.Context, id uint, msg notify.Message) error {
	channel, err := n.store.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	return n.deliver(ctx, channel, msg)
}

// Test sends a test notification through the stored channel.
func (n *Notifier) Test(ctx context.Context, id uint) error {
	// Test always delivers: it must not be filtered by the event subscription.
	msg := n.message(models.EventTest, nil, n.now())
	return n.SendToChannel(ctx, id, msg)
}

// deliver opens the channel configuration and records the attempt outcome.
func (n *Notifier) deliver(ctx context.Context, channel *models.NotificationChannel, msg notify.Message) error {
	config, err := n.openConfig(channel)
	if err != nil {
		_ = n.store.TouchChannel(ctx, channel.ID, "error", err.Error(), n.now())
		return err
	}
	err = n.dispatcher.Send(ctx, channel.Type, config, msg)
	status, message := "ok", ""
	if err != nil {
		// The error is recorded and returned to the operator (the "send a test
		// notification" button shows it), so the secrets of the configuration
		// are masked before it leaves this function.
		err = sanitizeConfigError(channel.Type, config, err)
		status, message = "error", err.Error()
	}
	if touchErr := n.store.TouchChannel(ctx, channel.ID, status, message, n.now()); touchErr != nil {
		slog.Warn("recording a notification attempt", "channel", channel.ID, "error", touchErr)
	}
	return err
}

// ---------------------------------------------------------------------------
// Channel configuration secrets
// ---------------------------------------------------------------------------

// secretKeyNames lists the configuration keys (case insensitive) whose value is
// treated as a secret: sealed before storage and masked in every API response.
var secretKeyNames = map[string]bool{
	"password":      true,
	"pass":          true,
	"secret":        true,
	"token":         true,
	"api_key":       true,
	"apikey":        true,
	"access_key":    true,
	"authorization": true,
	"auth":          true,
}

// isSecretKey reports whether a configuration key holds a secret.
func isSecretKey(key string) bool {
	return secretKeyNames[strings.ToLower(strings.TrimSpace(key))]
}

// kindSecretKeys lists the keys that are a secret for a whole channel kind even
// though their name is not explicit, such as the shoutrrr URL which embeds the
// credentials of the destination service.
func kindSecretKeys(kind string) map[string]bool {
	if kind == notify.KindShoutrrr {
		return map[string]bool{"url": true}
	}
	return nil
}

// sealConfig encrypts every secret of a channel configuration. Values equal to
// the mask are taken from the previous configuration, which lets the UI submit a
// channel without retyping its secrets.
func (n *Notifier) sealConfig(kind string, config, previous map[string]any) (string, error) {
	sealed := copyConfig(config)
	implicit := kindSecretKeys(kind)
	for key, value := range sealed {
		if !implicit[key] && !isSecretKey(key) {
			continue
		}
		ciphertext, err := n.sealValue(key, value, previous[key])
		if err != nil {
			return "", err
		}
		if ciphertext == "" {
			delete(sealed, key)
			continue
		}
		sealed[key] = ciphertext
	}
	if headers, ok := sealed["headers"].(map[string]any); ok {
		for key, value := range headers {
			if !isSecretKey(key) {
				continue
			}
			text, ok := value.(string)
			if !ok {
				continue
			}
			ciphertext, err := n.sealValue(key, text, headerValue(previous, key))
			if err != nil {
				return "", err
			}
			if ciphertext == "" {
				delete(headers, key)
				continue
			}
			headers[key] = ciphertext
		}
	}
	encoded, err := json.Marshal(sealed)
	if err != nil {
		return "", fmt.Errorf("services: encoding the channel configuration: %w", err)
	}
	return string(encoded), nil
}

// sealValue encrypts one secret value. Submitting the mask keeps the stored
// secret (the UI never shows a value, so the field round-trips as the mask);
// submitting an empty string clears it.
func (n *Notifier) sealValue(key string, value any, previous any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", nil
	}
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return "", nil
	case text == maskedSecret:
		stored, ok := previous.(string)
		if !ok || stored == "" || stored == maskedSecret {
			return "", nil
		}
		if crypto.IsEncrypted(stored) {
			return stored, nil
		}
		sealed, err := n.box.Encrypt(stored)
		if err != nil {
			return "", fmt.Errorf("services: encrypting %s: %w", key, err)
		}
		return sealed, nil
	case crypto.IsEncrypted(text):
		return text, nil
	default:
		sealed, err := n.box.Encrypt(text)
		if err != nil {
			return "", fmt.Errorf("services: encrypting %s: %w", key, err)
		}
		return sealed, nil
	}
}

// headerValue reads one header value from a previous configuration.
func headerValue(previous map[string]any, key string) any {
	headers, ok := previous["headers"].(map[string]any)
	if !ok {
		return nil
	}
	return headers[key]
}

// openConfig decodes a stored channel configuration and decrypts its secrets.
func (n *Notifier) openConfig(channel *models.NotificationChannel) (map[string]any, error) {
	config := map[string]any{}
	if strings.TrimSpace(channel.Config) != "" {
		if err := json.Unmarshal([]byte(channel.Config), &config); err != nil {
			return nil, fmt.Errorf("%w: the %s channel configuration is not valid JSON", ErrValidation, channel.Name)
		}
	}
	implicit := kindSecretKeys(channel.Type)
	for key, value := range config {
		if !implicit[key] && !isSecretKey(key) {
			continue
		}
		text, ok := value.(string)
		if !ok || !crypto.IsEncrypted(text) {
			continue
		}
		plaintext, err := n.box.Decrypt(text)
		if err != nil {
			return nil, fmt.Errorf("%w: the %s secret of channel %q cannot be decrypted, enter it again",
				ErrValidation, key, channel.Name)
		}
		config[key] = plaintext
	}
	if headers, ok := config["headers"].(map[string]any); ok {
		for key, value := range headers {
			text, ok := value.(string)
			if !ok || !crypto.IsEncrypted(text) {
				continue
			}
			plaintext, err := n.box.Decrypt(text)
			if err != nil {
				return nil, fmt.Errorf("%w: the %s header of channel %q cannot be decrypted, enter it again",
					ErrValidation, key, channel.Name)
			}
			headers[key] = plaintext
		}
	}
	return config, nil
}

// maskConfig returns a copy of a configuration where every secret is replaced by
// the mask, which is what the API is allowed to return.
func (n *Notifier) maskConfig(kind string, config map[string]any) map[string]any {
	masked := copyConfig(config)
	implicit := kindSecretKeys(kind)
	for key, value := range masked {
		if !implicit[key] && !isSecretKey(key) {
			continue
		}
		if text, ok := value.(string); ok && text != "" {
			masked[key] = maskedSecret
		}
	}
	if headers, ok := masked["headers"].(map[string]any); ok {
		for key, value := range headers {
			if !isSecretKey(key) {
				continue
			}
			if text, ok := value.(string); ok && text != "" {
				headers[key] = maskedSecret
			}
		}
	}
	return masked
}

// sanitizeConfigError masks every plaintext secret of a channel configuration
// inside an error message.
//
// It is applied to the errors a delivery can produce, because senders echo the
// destination back at the caller (`Post "https://…/token": dial tcp …`, or the
// shoutrrr URL, which embeds the credentials of the service). Those errors end
// up in `notification_channels.last_error`, which the admin API returns, and the
// whole point of sealing a secret is that it is not readable at rest.
func sanitizeConfigError(kind string, config map[string]any, err error) error {
	if err == nil || len(config) == 0 {
		return err
	}
	message := err.Error()
	changed := false
	implicit := kindSecretKeys(kind)
	for key, value := range config {
		if !implicit[key] && !isSecretKey(key) {
			continue
		}
		text, ok := value.(string)
		if !ok || text == "" || text == maskedSecret || !strings.Contains(message, text) {
			continue
		}
		message = strings.ReplaceAll(message, text, maskedSecret)
		changed = true
	}
	if !changed {
		return err
	}
	return errors.New(message)
}

// copyConfig deep copies a configuration so callers can mutate their copy.
func copyConfig(config map[string]any) map[string]any {
	out := make(map[string]any, len(config))
	for key, value := range config {
		out[key] = value
	}
	return out
}

// subscribesTo reports whether a channel subscribed to an event. An empty event
// list means "every event", which is the default of the admin form.
func subscribesTo(channel models.NotificationChannel, event string) bool {
	events := channelEvents(channel)
	if len(events) == 0 {
		return true
	}
	for _, name := range events {
		if name == event {
			return true
		}
	}
	return false
}

// channelEvents decodes the stored event subscription list. It accepts the JSON
// array written by the API and the comma separated form an operator may have
// typed directly in the database, ignores unknown names, and returns nil (which
// means "every event") when nothing usable is left: a corrupt row must never
// silence the alerts.
func channelEvents(channel models.NotificationChannel) []string {
	raw := strings.TrimSpace(channel.Events)
	if raw == "" {
		return nil
	}
	var candidates []string
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &candidates); err != nil {
			return nil
		}
	} else {
		candidates = strings.Split(raw, ",")
	}
	events := []string{}
	for _, part := range candidates {
		value := strings.TrimSpace(part)
		if !containsString(models.AllEvents, value) || containsString(events, value) {
			continue
		}
		events = append(events, value)
	}
	if len(events) == 0 {
		return nil
	}
	return events
}

// ---------------------------------------------------------------------------
// Channel CRUD
// ---------------------------------------------------------------------------

// Channels lists every configured channel in its masked form.
func (n *Notifier) Channels(ctx context.Context) ([]ChannelView, error) {
	rows, err := n.store.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]ChannelView, 0, len(rows))
	for _, row := range rows {
		views = append(views, n.view(&row))
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views, nil
}

// Channel returns one masked channel.
func (n *Notifier) Channel(ctx context.Context, id uint) (ChannelView, error) {
	row, err := n.store.GetChannel(ctx, id)
	if err != nil {
		return ChannelView{}, err
	}
	return n.view(row), nil
}

// Create stores a new channel. The configuration is validated with the sender of
// its kind before anything is written, so an unreachable or malformed channel is
// rejected at the API boundary.
func (n *Notifier) Create(ctx context.Context, input ChannelInput) (ChannelView, error) {
	plain, err := n.validateInput(input, nil)
	if err != nil {
		return ChannelView{}, err
	}
	sealed, err := n.sealConfig(input.Type, plain, nil)
	if err != nil {
		return ChannelView{}, err
	}
	row := &models.NotificationChannel{
		Name:    strings.TrimSpace(input.Name),
		Type:    input.Type,
		Config:  sealed,
		Events:  encodeEvents(input.Events),
		Enabled: input.Enabled,
	}
	if err := n.store.SaveChannel(ctx, row); err != nil {
		return ChannelView{}, err
	}
	return n.view(row), nil
}

// Update replaces the editable fields of a channel. Secrets submitted as the
// mask (or omitted) keep their stored value.
func (n *Notifier) Update(ctx context.Context, id uint, input ChannelInput) (ChannelView, error) {
	row, err := n.store.GetChannel(ctx, id)
	if err != nil {
		return ChannelView{}, err
	}
	previous, err := n.openConfig(row)
	if err != nil {
		return ChannelView{}, err
	}
	plain, err := n.validateInput(input, previous)
	if err != nil {
		return ChannelView{}, err
	}
	sealed, err := n.sealConfig(input.Type, plain, previous)
	if err != nil {
		return ChannelView{}, err
	}
	row.Name = strings.TrimSpace(input.Name)
	row.Type = input.Type
	row.Config = sealed
	row.Events = encodeEvents(input.Events)
	row.Enabled = input.Enabled
	// The delivery history of the previous configuration is not relevant
	// anymore, so the last attempt is reset with the new one.
	row.LastStatus = ""
	row.LastError = ""
	if err := n.store.SaveChannel(ctx, row); err != nil {
		return ChannelView{}, err
	}
	return n.view(row), nil
}

// SetEnabled turns a channel on or off without touching its configuration.
func (n *Notifier) SetEnabled(ctx context.Context, id uint, enabled bool) error {
	row, err := n.store.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	row.Enabled = enabled
	return n.store.SaveChannel(ctx, row)
}

// Delete removes a channel.
func (n *Notifier) Delete(ctx context.Context, id uint) error {
	return n.store.DeleteChannel(ctx, id)
}

// view masks a stored channel for the API.
func (n *Notifier) view(channel *models.NotificationChannel) ChannelView {
	config := map[string]any{}
	if err := json.Unmarshal([]byte(channel.Config), &config); err != nil {
		config = map[string]any{}
	}
	return ChannelView{
		ID:         channel.ID,
		Name:       channel.Name,
		Type:       channel.Type,
		Config:     n.maskConfig(channel.Type, config),
		Events:     channelEvents(*channel),
		Enabled:    channel.Enabled,
		LastStatus: channel.LastStatus,
		LastError:  channel.LastError,
		LastUsedAt: channel.LastUsedAt,
		CreatedAt:  channel.CreatedAt,
		UpdatedAt:  channel.UpdatedAt,
	}
}

// validateInput checks a channel submission and returns the plaintext
// configuration to seal. The mask is resolved against the previous plaintext so
// an update can keep a secret the operator did not retype.
func (n *Notifier) validateInput(input ChannelInput, previous map[string]any) (map[string]any, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: the channel name is required", ErrValidation)
	}
	kind := strings.TrimSpace(input.Type)
	if !containsString(n.dispatcher.Kinds(), kind) {
		return nil, fmt.Errorf("%w: unknown channel type %q (supported: %s)",
			ErrValidation, input.Type, strings.Join(n.dispatcher.Kinds(), ", "))
	}
	for _, event := range input.Events {
		if !containsString(models.AllEvents, strings.TrimSpace(event)) {
			return nil, fmt.Errorf("%w: unknown notification event %q", ErrValidation, event)
		}
	}
	plain := copyConfig(input.Config)
	for key, value := range plain {
		text, ok := value.(string)
		if !ok || text != maskedSecret {
			continue
		}
		stored, ok := previous[key].(string)
		if !ok || stored == "" {
			return nil, fmt.Errorf("%w: the stored %s cannot be recovered, enter it again", ErrValidation, key)
		}
		plain[key] = stored
	}
	if err := n.dispatcher.Validate(kind, plain); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return plain, nil
}

// encodeEvents stores the subscription list as the JSON array of the model.
func encodeEvents(events []string) string {
	cleaned := make([]string, 0, len(events))
	for _, event := range events {
		if value := strings.TrimSpace(event); value != "" && !containsString(cleaned, value) {
			cleaned = append(cleaned, value)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// ---------------------------------------------------------------------------
// Message rendering
// ---------------------------------------------------------------------------

// message renders a notification and adds the deployment link.
func (n *Notifier) message(event string, data map[string]any, at time.Time) notify.Message {
	payload := make(map[string]any, len(data)+1)
	for key, value := range data {
		payload[key] = value
	}
	if n.link != "" {
		if _, ok := payload["link"]; !ok {
			payload["link"] = n.link
		}
	}
	return renderMessage(event, payload, at)
}

// renderMessage turns an event and its payload into the e-mail/webhook message.
// The copy is English and free of HTML on purpose: every delivery target must be
// able to display it, and translations live in the UI, not in the alerts.
func renderMessage(event string, data map[string]any, at time.Time) notify.Message {
	msg := notify.Message{Event: event, At: at, Data: data}
	job := dataString(data, "job")
	switch event {
	case models.EventSyncSuccess:
		msg.Title = "Sync completed"
		if job != "" {
			msg.Title += ": " + job
		}
		msg.Body = "The synchronization finished successfully."
		if summary := countsSummary(data); summary != "" {
			msg.Body += "\n" + summary
		}
		if duration := dataString(data, "duration"); duration != "" {
			msg.Body += "\nDuration: " + duration
		}
	case models.EventSyncRunFailed:
		msg.Title = "Sync failed"
		if job != "" {
			msg.Title += ": " + job
		}
		msg.Body = "The synchronization could not be completed."
		if reason := dataString(data, "error"); reason != "" {
			msg.Body += "\nReason: " + reason
		}
		if summary := countsSummary(data); summary != "" {
			msg.Body += "\n" + summary
		}
	case models.EventSyncError:
		msg.Title = "Sync problem"
		msg.Body = "Synchronization reported a problem."
		if reason := dataString(data, "error"); reason != "" {
			msg.Body += "\nReason: " + reason
		}
	case models.EventAccountConnected:
		msg.Title = "Account connected"
		msg.Body = fmt.Sprintf("The %s account %s was connected.", dataString(data, "provider"), dataString(data, "email"))
	case models.EventAccountError:
		msg.Title = "Account needs attention"
		msg.Body = fmt.Sprintf("The %s account %s needs to be reconnected.", dataString(data, "provider"), dataString(data, "email"))
		if reason := dataString(data, "error"); reason != "" {
			msg.Body += "\nReason: " + reason
		}
	case models.EventTest:
		msg.Title = "Sync test notification"
		msg.Body = "This is a test notification: the channel is configured correctly."
	default:
		msg.Title = "Sync notification"
		msg.Body = "Sync reported the event " + event + "."
	}
	msg.Body = strings.TrimSpace(msg.Body)
	if link := dataString(data, "link"); link != "" {
		msg.Body += "\n\n" + link
	}
	return msg
}

// countsSummary renders the file counters of a sync run, skipping zeros.
func countsSummary(data map[string]any) string {
	labels := []struct {
		key   string
		label string
	}{
		{"added", "added"},
		{"updated", "updated"},
		{"deleted", "deleted"},
		{"skipped", "unchanged"},
		{"failed", "failed"},
	}
	parts := []string{}
	for _, entry := range labels {
		if value := dataString(data, entry.key); value != "" && value != "0" {
			parts = append(parts, fmt.Sprintf("%s: %s", entry.label, value))
		}
	}
	return strings.Join(parts, ", ")
}

// dataString renders one payload value as text, tolerating numbers and booleans.
func dataString(data map[string]any, key string) string {
	value, ok := data[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return fmt.Sprintf("%g", typed)
	case bool:
		return fmt.Sprintf("%t", typed)
	default:
		return fmt.Sprintf("%v", typed)
	}
}
