// Package ical parses and fetches iCalendar (.ics) subscriptions.
//
// Only the parts of RFC 5545 the calendar engine needs are implemented: one
// VEVENT per event, DTSTART/DTEND (or DURATION), the text properties, the
// recurrence markers (which the engine skips) and the modification stamp. A
// VTIMEZONE block is not interpreted; a TZID is resolved against the runtime zone
// database, which knows every zone a feed realistically names, and an unknown one
// falls back to UTC instead of failing the feed.
package ical

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxEvents bounds one feed. A subscription that lists more is truncated, not
	// refused: the engine reports how many events it read.
	MaxEvents = 20000
	// maxLineLength bounds a single unfolded line, so a hostile feed cannot make
	// the parser allocate without limit.
	maxLineLength = 64 << 10
)

// ErrInvalidFeed marks a payload that is not a calendar at all.
var ErrInvalidFeed = errors.New("ical: the payload is not an iCalendar feed")

// Options tunes the parser.
//
// DefaultTimeZone is what a floating time is interpreted in (a DTSTART without
// a trailing Z and without a TZID): a feed published with local times carries no
// other hint.
//
// WindowStart/WindowEnd, when set, drop the events that start outside the range.
// The zero value disables the filter, and an event whose start cannot be read is
// kept so the caller can report it.
type Options struct {
	DefaultTimeZone string
	WindowStart     time.Time
	WindowEnd       time.Time
}

// Event is one VEVENT as a feed describes it.
type Event struct {
	UID         string
	Summary     string
	Description string
	Location    string
	Start       time.Time
	End         time.Time
	AllDay      bool
	TimeZone    string
	// Recurring marks a master or an instance of a series (RRULE or
	// RECURRENCE-ID). The engine skips both.
	Recurring bool
	Cancelled bool
	Organizer string
	Attendees []string
	UpdatedAt time.Time
}

// property is one parsed content line: its value plus the parameters it carried.
type property struct {
	value  string
	params map[string]string
}

// Parse reads a feed and returns its events in feed order.
func Parse(data []byte, opts Options) ([]Event, error) {
	lines := unfold(data)
	events := make([]Event, 0, 16)
	var current map[string][]property
	inEvent := false
	seenCalendar := false
	for _, line := range lines {
		name, prop, ok := parseLine(line)
		if !ok {
			continue
		}
		switch name {
		case "BEGIN":
			switch strings.ToUpper(prop.value) {
			case "VCALENDAR":
				seenCalendar = true
			case "VEVENT":
				inEvent = true
				current = map[string][]property{}
			}
			continue
		case "END":
			if strings.ToUpper(prop.value) != "VEVENT" || !inEvent {
				continue
			}
			inEvent = false
			if len(events) >= MaxEvents {
				current = nil
				continue
			}
			if event, ok := buildEvent(current, opts); ok {
				events = append(events, event)
			}
			current = nil
			continue
		}
		if inEvent {
			current[name] = append(current[name], prop)
		}
	}
	if !seenCalendar && len(events) == 0 {
		return nil, fmt.Errorf("%w: no VCALENDAR component", ErrInvalidFeed)
	}
	return events, nil
}

// buildEvent turns the properties of one VEVENT into an Event. An event without
// a UID or without a readable start is dropped: neither can be paired with
// anything on the other side of a job.
func buildEvent(props map[string][]property, opts Options) (Event, bool) {
	if len(props) == 0 {
		return Event{}, false
	}
	uid := strings.TrimSpace(first(props, "UID").value)
	if uid == "" {
		return Event{}, false
	}
	startProp := first(props, "DTSTART")
	if len(props["DTSTART"]) == 0 {
		return Event{}, false
	}
	start, allDay, zone, err := parseMoment(startProp, opts.DefaultTimeZone)
	if err != nil {
		return Event{}, false
	}

	end := time.Time{}
	if endProp := first(props, "DTEND"); len(props["DTEND"]) > 0 {
		if parsed, endAllDay, _, err := parseMoment(endProp, opts.DefaultTimeZone); err == nil {
			end = parsed
			allDay = allDay || endAllDay
		}
	}
	if end.IsZero() {
		if duration := first(props, "DURATION"); len(props["DURATION"]) > 0 {
			if parsed, err := parseDuration(duration.value); err == nil {
				end = start.Add(parsed)
			}
		}
	}
	if end.IsZero() || !end.After(start) {
		// A feed may omit DTEND (a zero-length reminder) or send an end before
		// the start. One day is the RFC default for an all-day event and one
		// hour keeps a timed event valid for the destination provider.
		if allDay {
			end = start.AddDate(0, 0, 1)
		} else {
			end = start.Add(time.Hour)
		}
	}

	event := Event{
		UID:         uid,
		Summary:     text(first(props, "SUMMARY").value),
		Description: text(first(props, "DESCRIPTION").value),
		Location:    text(first(props, "LOCATION").value),
		Start:       start,
		End:         end,
		AllDay:      allDay,
		TimeZone:    zone,
		Recurring:   hasAny(props, "RRULE", "RECURRENCE-ID"),
		Cancelled:   strings.EqualFold(strings.TrimSpace(first(props, "STATUS").value), "CANCELLED"),
		Organizer:   address(first(props, "ORGANIZER").value),
		UpdatedAt:   stamp(props, opts.DefaultTimeZone),
	}
	for _, attendee := range props["ATTENDEE"] {
		if email := address(attendee.value); email != "" {
			event.Attendees = append(event.Attendees, email)
		}
	}
	if !opts.WindowStart.IsZero() && start.Before(opts.WindowStart) {
		return Event{}, false
	}
	if !opts.WindowEnd.IsZero() && !start.Before(opts.WindowEnd) {
		return Event{}, false
	}
	return event, true
}

// unfold splits a feed into logical lines: a CRLF (or LF) followed by a space or
// a tab is a fold, not a line break. A folded line longer than maxLineLength is
// cut instead of being allowed to grow without bound.
func unfold(data []byte) []string {
	raw := strings.ReplaceAll(string(data), "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	physical := strings.Split(raw, "\n")
	lines := make([]string, 0, len(physical))
	var builder strings.Builder
	flush := func() {
		if builder.Len() > 0 {
			lines = append(lines, builder.String())
			builder.Reset()
		}
	}
	for _, line := range physical {
		if line == "" {
			flush()
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if builder.Len()+len(line) > maxLineLength {
				continue
			}
			builder.WriteString(line[1:])
			continue
		}
		flush()
		builder.WriteString(line)
	}
	flush()
	return lines
}

// parseLine splits one content line into `NAME;PARAM=VALUE:value`. Quoted
// parameter values may contain ':' and ';', which is the only case a plain
// split would get wrong.
func parseLine(line string) (string, property, bool) {
	colon := indexUnquoted(line, ':')
	if colon < 0 {
		return "", property{}, false
	}
	parts := splitUnquoted(line[:colon], ';')
	if len(parts) == 0 {
		return "", property{}, false
	}
	name := strings.ToUpper(strings.TrimSpace(parts[0]))
	if name == "" {
		return "", property{}, false
	}
	prop := property{value: line[colon+1:], params: map[string]string{}}
	for _, parameter := range parts[1:] {
		key, raw, found := cutUnquoted(parameter, '=')
		if !found {
			continue
		}
		prop.params[strings.ToUpper(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(raw), "\"")
	}
	return name, prop, true
}

// indexUnquoted returns the index of sep outside a double-quoted section.
func indexUnquoted(value string, sep byte) int {
	quoted := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"':
			quoted = !quoted
		case sep:
			if !quoted {
				return i
			}
		}
	}
	return -1
}

// cutUnquoted splits value at the first unquoted sep.
func cutUnquoted(value string, sep byte) (string, string, bool) {
	index := indexUnquoted(value, sep)
	if index < 0 {
		return value, "", false
	}
	return value[:index], value[index+1:], true
}

// splitUnquoted splits value on every unquoted sep.
func splitUnquoted(value string, sep byte) []string {
	parts := []string{}
	for {
		head, tail, found := cutUnquoted(value, sep)
		parts = append(parts, head)
		if !found {
			return parts
		}
		value = tail
	}
}

// first returns the first occurrence of a property, or an empty one.
func first(props map[string][]property, name string) property {
	values := props[name]
	if len(values) == 0 {
		return property{}
	}
	return values[0]
}

// hasAny reports whether any of the named properties is present.
func hasAny(props map[string][]property, names ...string) bool {
	for _, name := range names {
		if len(props[name]) > 0 {
			return true
		}
	}
	return false
}

// text unescapes an iCalendar TEXT value: the RFC escapes the separator, the
// comma, the semicolon, the backslash and the newline.
func text(value string) string {
	if !strings.Contains(value, "\\") {
		return strings.TrimSpace(value)
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 >= len(value) {
			builder.WriteByte(value[i])
			continue
		}
		i++
		switch value[i] {
		case 'n', 'N':
			builder.WriteByte('\n')
		case 't', 'T':
			builder.WriteByte('\t')
		default:
			builder.WriteByte(value[i])
		}
	}
	return strings.TrimSpace(builder.String())
}

// address strips the `mailto:` scheme of an organizer or attendee value. A
// value with no scheme (and no mailto) is returned as it is: some feeds carry a
// plain address.
func address(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if lower := strings.ToLower(trimmed); strings.HasPrefix(lower, "mailto:") {
		return strings.TrimSpace(trimmed[len("mailto:"):])
	}
	return trimmed
}

// stamp reads the modification time of an event: LAST-MODIFIED is the actual
// stamp, DTSTAMP the fallback a feed that never updates LAST-MODIFIED leaves.
func stamp(props map[string][]property, fallbackZone string) time.Time {
	for _, name := range []string{"LAST-MODIFIED", "DTSTAMP", "CREATED"} {
		if parsed := first(props, name); len(props[name]) > 0 {
			if moment, _, _, err := parseMoment(parsed, fallbackZone); err == nil {
				return moment
			}
		}
	}
	return time.Time{}
}

// parseMoment decodes a DTSTART/DTEND value. It answers the instant, whether the
// value is a whole day and the zone name the event should carry.
//
// Three forms are accepted: a DATE (`20260102`, VALUE=DATE), a UTC DATE-TIME
// (`20260102T100000Z`) and a local DATE-TIME with a TZID (or floating, in which
// case the feed default applies). The zone is reported so a destination provider
// can write the event back with the same wall clock the source published.
func parseMoment(prop property, fallbackZone string) (time.Time, bool, string, error) {
	raw := strings.TrimSpace(prop.value)
	if raw == "" {
		return time.Time{}, false, "", errors.New("ical: empty date value")
	}
	if strings.EqualFold(prop.params["VALUE"], "DATE") || len(raw) == 8 {
		if parsed, err := time.ParseInLocation("20060102", raw, resolveZone(fallbackZone)); err == nil {
			return parsed, true, "", nil
		}
	}
	utc := strings.HasSuffix(raw, "Z")
	trimmed := strings.TrimSuffix(raw, "Z")
	zone := strings.TrimSpace(prop.params["TZID"])
	if zone == "" {
		zone = strings.TrimSpace(fallbackZone)
	}
	location := resolveZone(zone)
	if utc {
		location = time.UTC
	}
	if location == nil {
		location = resolveZone(fallbackZone)
	}
	for _, layout := range []string{"20060102T150405", "20060102T1504", "20060102T15"} {
		if parsed, err := time.ParseInLocation(layout, trimmed, location); err == nil {
			if utc {
				return parsed.UTC(), false, "UTC", nil
			}
			return parsed, false, zone, nil
		}
	}
	return time.Time{}, false, "", errors.New("ical: unreadable date value")
}

// resolveZone loads a zone name, answering UTC for an empty or unknown one: a
// feed must not fail because the runtime keyed a TZID differently.
func resolveZone(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	if location, err := time.LoadLocation(name); err == nil {
		return location
	}
	return time.UTC
}

// parseDuration decodes the RFC 5545 duration form the engine cares about: an
// optional sign, days and weeks followed by a time part (`P1DT2H30M`, `PT45M`,
// `P2W`). A month or a year has no fixed length, so a value that uses one is
// refused rather than guessed.
func parseDuration(value string) (time.Duration, error) {
	raw := strings.ToUpper(strings.TrimSpace(value))
	if raw == "" {
		return 0, errors.New("ical: empty duration")
	}
	negative := false
	switch raw[0] {
	case '+':
		raw = raw[1:]
	case '-':
		negative = true
		raw = raw[1:]
	}
	if !strings.HasPrefix(raw, "P") {
		return 0, errors.New("ical: a duration must start with P")
	}
	raw = raw[1:]
	var total time.Duration
	inTime := false
	number := ""
	units := 0
	for i := 0; i < len(raw); i++ {
		char := raw[i]
		if char == 'T' {
			inTime = true
			number = ""
			continue
		}
		if char >= '0' && char <= '9' {
			number += string(char)
			continue
		}
		amount, err := strconv.Atoi(number)
		if err != nil {
			return 0, errors.New("ical: a duration component has no number")
		}
		number = ""
		units++
		switch char {
		case 'W':
			total += time.Duration(amount) * 7 * 24 * time.Hour
		case 'D':
			total += time.Duration(amount) * 24 * time.Hour
		case 'H':
			if !inTime {
				return 0, errors.New("ical: hours belong to the time part of a duration")
			}
			total += time.Duration(amount) * time.Hour
		case 'M':
			if !inTime {
				return 0, errors.New("ical: a month has no fixed length")
			}
			total += time.Duration(amount) * time.Minute
		case 'S':
			if !inTime {
				return 0, errors.New("ical: seconds belong to the time part of a duration")
			}
			total += time.Duration(amount) * time.Second
		default:
			return 0, fmt.Errorf("ical: unknown duration unit %q", string(char))
		}
	}
	if units == 0 {
		return 0, errors.New("ical: a duration has no component")
	}
	if negative {
		return -total, nil
	}
	return total, nil
}
