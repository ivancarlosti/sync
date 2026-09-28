package notify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/containrrr/shoutrrr"
	"github.com/containrrr/shoutrrr/pkg/types"
)

// ShoutrrrSender delivers notifications through the shoutrrr engine. It handles
// two kinds:
//
//	smtp      a structured e-mail configuration (host, port, credentials, ...)
//	          that is translated into a shoutrrr `smtp://` URL;
//	shoutrrr  a raw shoutrrr URL (slack://, telegram://, ...) used as is.
//
// Using one engine for both keeps SMTP support (including STARTTLS, implicit TLS
// and OAuth2) identical to the URL-scheme services an operator may already use.
type ShoutrrrSender struct {
	timeout time.Duration
}

// NewShoutrrrSender builds the sender.
func NewShoutrrrSender(timeout time.Duration) *ShoutrrrSender {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &ShoutrrrSender{timeout: timeout}
}

// Kind implements Sender. It reports the structured SMTP kind; the raw
// `shoutrrr` kind is handled by Handle below.
func (s *ShoutrrrSender) Kind() string { return KindSMTP }

// Handles reports whether the sender can deliver for a channel kind.
func (s *ShoutrrrSender) Handles(kind string) bool {
	return kind == KindSMTP || kind == KindShoutrrr
}

// Send builds the shoutrrr URL of the channel, delivers the message and returns
// the first error reported by the engine.
func (s *ShoutrrrSender) Send(ctx context.Context, config map[string]any, msg Message) error {
	kind := KindSMTP
	if raw := ConfigString(config, "kind"); raw != "" {
		kind = raw
	}
	rawURL, err := ShoutrrrURL(kind, config)
	if err != nil {
		return err
	}
	if trimSpace(msg.Text()) == "" {
		return ErrEmptyMessage
	}

	// shoutrrr has no context support, so the call runs in a goroutine bounded
	// both by the router timeout and by the guard timer below: a hanging SMTP
	// server must never block a scheduled sync run.
	done := make(chan error, 1)
	go func() {
		router, err := shoutrrr.CreateSender(rawURL)
		if err != nil {
			done <- fmt.Errorf("%w: %v", ErrInvalidConfig, err)
			return
		}
		router.Timeout = s.timeout
		params := types.Params{}
		params.SetTitle(msg.Subject())
		// The SMTP service reads its own `subject` property, the URL-scheme
		// services use the common `title` param.
		params["subject"] = msg.Subject()
		for _, result := range router.Send(msg.Text(), &params) {
			if result != nil {
				done <- result
				return
			}
		}
		done <- nil
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("notify: sending the notification: %w", ctx.Err())
	case <-time.After(s.timeout + time.Second):
		return fmt.Errorf("notify: sending the notification timed out after %s", s.timeout)
	case err := <-done:
		if err != nil {
			return fmt.Errorf("notify: sending the notification: %w", err)
		}
		return nil
	}
}

// ShoutrrrURL builds the shoutrrr URL described by a channel configuration. It is
// exported so the API can validate a channel before it is stored.
func ShoutrrrURL(kind string, config map[string]any) (string, error) {
	switch kind {
	case KindShoutrrr:
		raw := ConfigString(config, "url")
		if raw == "" {
			return "", fmt.Errorf("%w: a shoutrrr URL is required", ErrInvalidConfig)
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidConfig, err)
		}
		if parsed.Scheme == "" {
			return "", fmt.Errorf("%w: %q is not an absolute URL", ErrInvalidConfig, raw)
		}
		return raw, nil
	case KindSMTP:
		return SMPTURL(config)
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
}

// SMPTURL translates the structured SMTP configuration into the shoutrrr
// `smtp://user:password@host:port/?from=..&to=..&encryption=..` URL. Keeping the
// translation in one place is what makes the admin form and the sender agree.
func SMPTURL(config map[string]any) (string, error) {
	host := ConfigString(config, "host")
	if host == "" {
		return "", fmt.Errorf("%w: the SMTP host is required", ErrInvalidConfig)
	}
	from := ConfigString(config, "from")
	if from == "" {
		return "", fmt.Errorf("%w: the SMTP sender address is required", ErrInvalidConfig)
	}
	to := ConfigStrings(config, "to")
	if len(to) == 0 {
		return "", fmt.Errorf("%w: at least one SMTP recipient is required", ErrInvalidConfig)
	}
	port := ConfigInt(config, "port", 587)
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("%w: the SMTP port must be between 1 and 65535", ErrInvalidConfig)
	}

	query := url.Values{}
	query.Set("from", from)
	query.Set("to", strings.Join(to, ","))
	if name := ConfigString(config, "from_name"); name != "" {
		query.Set("fromname", name)
	}
	if subject := ConfigString(config, "subject"); subject != "" {
		query.Set("subject", subject)
	}
	query.Set("encryption", encryptionValue(ConfigString(config, "encryption")))
	if ConfigBool(config, "use_html", false) {
		query.Set("usehtml", "yes")
	}

	target := url.URL{
		Scheme:   "smtp",
		Host:     host + ":" + strconv.Itoa(port),
		RawQuery: query.Encode(),
	}
	if username := ConfigString(config, "username"); username != "" {
		if password := ConfigString(config, "password"); password != "" {
			target.User = url.UserPassword(username, password)
		} else {
			target.User = url.User(username)
		}
	}
	return target.String(), nil
}

// encryptionValue maps the vocabulary used by the admin form onto the enum
// expected by shoutrrr (`none`, `explicittls`, `implicittls`, `auto`).
func encryptionValue(raw string) string {
	switch strings.ToLower(trimSpace(raw)) {
	case "none", "off", "plain", "no":
		return "none"
	case "starttls", "explicit", "explicittls", "tls":
		return "ExplicitTLS"
	case "implicit", "implicittls", "ssl", "tls-implicit":
		return "ImplicitTLS"
	default:
		return "Auto"
	}
}

// ErrEmptyMessage marks a message with neither title nor body.
var ErrEmptyMessage = errors.New("notify: nothing to send")
