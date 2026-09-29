package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/notify"
	"github.com/ivancarlosti/sync/internal/services"
)

// channelField describes one input of a channel kind. It exists so the SPA can
// render the form from the server instead of hard-coding the schema: the
// authoritative list of keys is the one documented in docs/notifications.md and
// enforced by the senders.
type channelField struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Secret   bool     `json:"secret,omitempty"`
	Hint     string   `json:"hint,omitempty"`
	Options  []string `json:"options,omitempty"`
	Default  any      `json:"default,omitempty"`
}

// channelKind is a channel type plus its form schema.
type channelKind struct {
	Kind   string         `json:"kind"`
	Label  string         `json:"label"`
	Fields []channelField `json:"fields"`
}

// channelSchemas is the form description of every kind shipped in this build.
var channelSchemas = []channelKind{
	{
		Kind:  notify.KindSMTP,
		Label: "E-mail (SMTP)",
		Fields: []channelField{
			{Key: "host", Label: "SMTP host", Type: "text", Required: true},
			{Key: "port", Label: "Port", Type: "int", Default: 587},
			{Key: "username", Label: "Username", Type: "text"},
			{Key: "password", Label: "Password", Type: "password", Secret: true},
			{Key: "from", Label: "Sender address", Type: "text", Required: true},
			{Key: "from_name", Label: "Sender name", Type: "text"},
			{Key: "to", Label: "Recipients", Type: "text", Required: true,
				Hint: "one or more addresses, comma separated"},
			{Key: "subject", Label: "Subject prefix", Type: "text"},
			{Key: "encryption", Label: "Encryption", Type: "select", Options: []string{"starttls", "ssl", "none"}, Default: "starttls"},
			{Key: "use_html", Label: "Send HTML", Type: "bool", Default: false},
		},
	},
	{
		Kind:  notify.KindWebhook,
		Label: "Webhook",
		Fields: []channelField{
			{Key: "url", Label: "URL", Type: "text", Required: true,
				Hint: "absolute URL with a host, e.g. https://hooks.example.com/sync"},
			{Key: "method", Label: "Method", Type: "select", Options: []string{"POST", "PUT", "PATCH"}, Default: "POST"},
			{Key: "content_type", Label: "Content-Type", Type: "text", Default: "application/json"},
			{Key: "headers", Label: "Extra headers", Type: "json", Hint: "{\"X-Api-Key\":\"…\"}"},
			{Key: "body_template", Label: "Body template", Type: "textarea", Hint: "{event} {title} {message} {json} {data.key}"},
			{Key: "secret", Label: "HMAC secret", Type: "password", Secret: true},
			{Key: "timeout_seconds", Label: "Timeout (seconds)", Type: "int", Default: 20},
		},
	},
	{
		Kind:  notify.KindShoutrrr,
		Label: "Shoutrrr URL",
		Fields: []channelField{
			{Key: "url", Label: "Service URL", Type: "password", Required: true, Secret: true,
				Hint: "slack://…, telegram://…, discord://… (see docs/notifications.md)"},
		},
	},
}

// notificationsInput is the create/update payload of a channel.
type notificationsInput struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Config  map[string]any `json:"config"`
	Events  []string       `json:"events"`
	Enabled *bool          `json:"enabled"`
}

// toChannelInput converts the payload, keeping the "enabled" default of the
// create call while an update always states it explicitly.
func (in notificationsInput) toChannelInput() services.ChannelInput {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	events := in.Events
	if events == nil {
		events = []string{}
	}
	config := in.Config
	if config == nil {
		config = map[string]any{}
	}
	return services.ChannelInput{
		Name:    strings.TrimSpace(in.Name),
		Type:    strings.ToLower(strings.TrimSpace(in.Type)),
		Config:  config,
		Events:  events,
		Enabled: enabled,
	}
}

// handleListNotifications answers GET /api/notifications. It also returns the
// available kinds with their form schema and every event a channel can
// subscribe to, so the settings screen is entirely server driven.
func (s *Server) handleListNotifications(c *gin.Context) {
	channels, err := s.deps.Notifier.Channels(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"channels": channels,
		"kinds":    channelSchemas,
		"events":   s.deps.Notifier.Events(),
	})
}

// handleGetNotification answers GET /api/notifications/:id.
func (s *Server) handleGetNotification(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	channel, err := s.deps.Notifier.Channel(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, channel)
}

// handleCreateNotification answers POST /api/notifications. The service encrypts
// every secret of the configuration before it reaches the database and validates
// the events against the known list.
func (s *Server) handleCreateNotification(c *gin.Context) {
	var input notificationsInput
	if !decode(c, &input) {
		return
	}
	channel, err := s.deps.Notifier.Create(c.Request.Context(), input.toChannelInput())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, channel)
}

// handleUpdateNotification answers PUT /api/notifications/:id. An empty secret
// keeps the stored value (the API never returns a secret, so the form cannot
// echo it back), which is the same contract as the provider credentials.
func (s *Server) handleUpdateNotification(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var input notificationsInput
	if !decode(c, &input) {
		return
	}
	channel, err := s.deps.Notifier.Update(c.Request.Context(), id, input.toChannelInput())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, channel)
}

// handleDeleteNotification answers DELETE /api/notifications/:id.
func (s *Server) handleDeleteNotification(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if err := s.deps.Notifier.Delete(c.Request.Context(), id); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// handleNotificationEnabled answers PUT /api/notifications/:id/enabled: the
// toggle of the channel list, kept apart from the full update so flipping a
// switch never risks rewriting the configuration.
func (s *Server) handleNotificationEnabled(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(c, &input) {
		return
	}
	if input.Enabled == nil {
		abort(c, http.StatusBadRequest, "the request must carry an \"enabled\" boolean")
		return
	}
	if err := s.deps.Notifier.SetEnabled(c.Request.Context(), id, *input.Enabled); err != nil {
		fail(c, err)
		return
	}
	channel, err := s.deps.Notifier.Channel(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, channel)
}

// handleTestNotification answers POST /api/notifications/:id/test: it delivers a
// test event synchronously, so the operator gets the real failure instead of a
// log line. A delivery failure is answered with 502 and the reason from the
// sender (the notifier masks every configuration secret first), which is what
// makes a misconfigured host or an unreachable endpoint diagnosable from the UI.
func (s *Server) handleTestNotification(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if err := s.deps.Notifier.Test(c.Request.Context(), id); err != nil {
		if errors.Is(err, services.ErrNotFound) {
			fail(c, err)
			return
		}
		slog.Warn("testing a notification channel",
			"channel", id, "client", c.ClientIP(), "error", err)
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{
			"code":      codeDeliveryFailed,
			"error":     err.Error(),
			"id":        id,
			"delivered": false,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "delivered": true})
}
