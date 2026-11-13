package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Calendar is one calendar of a connected account, as the calendar picker and
// the job editor list it.
type Calendar struct {
	ID   string
	Name string
	// Primary marks the calendar the provider considers the account's own.
	Primary bool
	// TimeZone is the IANA zone the calendar is configured with, when known.
	// Events coming from an .ics feed inherit it as their default.
	TimeZone string
	// ReadOnly marks a calendar the account may read but not write (a holiday
	// calendar, a shared calendar without write access). The engine refuses to
	// pick one as a destination.
	ReadOnly bool
}

// CalendarWindow is the half-open range a calendar listing covers. It is
// exclusive at the end, like every provider range query.
type CalendarWindow struct {
	Start time.Time
	End   time.Time
}

// CalendarEvent is one event as a provider returns it, normalised so the engine
// never reads a provider payload itself.
//
// Start and End are absolute instants. An all-day event is reported at midnight
// of its first day in TimeZone and its End is the exclusive midnight after the
// last day, which is how Google and Microsoft both model it; the engine writes
// it back the same way so the round trip does not shift the day.
type CalendarEvent struct {
	ID          string
	UID         string
	Summary     string
	Description string
	Location    string
	Start       time.Time
	End         time.Time
	AllDay      bool
	TimeZone    string
	// UpdatedAt is the provider's last modification stamp. It feeds the
	// newest_wins conflict rule, so it must be the event stamp, not the sync
	// time.
	UpdatedAt time.Time
	// Recurring marks a series master or an instance of one. The engine skips
	// them instead of copying a single occurrence over the other side.
	Recurring bool
	// Cancelled marks a cancelled event that is still listed.
	Cancelled bool
	Organizer string
	Attendees []string
}

// CalendarEventInput is what the engine writes to a provider. ID, UID and the
// timestamps of the remote event are not part of it: a create has no ID yet and
// an update never moves an event to another identity.
type CalendarEventInput struct {
	// UID is the iCalendar UID the event should carry. Providers that honour it
	// (Google) keep the pairing stable across both sides; Graph stores it but
	// does not accept it on create, which is why the engine also tracks the
	// returned event id.
	UID         string
	Summary     string
	Description string
	Location    string
	Start       time.Time
	End         time.Time
	AllDay      bool
	TimeZone    string
	Attendees   []string
}

// CalendarProvider is the calendar half of a provider. It is a separate
// interface from Provider on purpose: the sync engine only needs the file half,
// the calendar engine only needs this one, and a provider that cannot do
// calendars simply does not implement it (see registry.go).
type CalendarProvider interface {
	// Calendars lists the calendars the account can read.
	Calendars(ctx context.Context, creds Credentials, tokens *Tokens) ([]Calendar, error)

	// CalendarEvents lists the events of one calendar inside a window.
	CalendarEvents(ctx context.Context, creds Credentials, tokens *Tokens, calendarID string, window CalendarWindow) ([]CalendarEvent, error)

	// CreateCalendarEvent creates an event and returns it as stored.
	CreateCalendarEvent(ctx context.Context, creds Credentials, tokens *Tokens, calendarID string, input CalendarEventInput) (*CalendarEvent, error)

	// UpdateCalendarEvent replaces the fields of an existing event.
	UpdateCalendarEvent(ctx context.Context, creds Credentials, tokens *Tokens, calendarID, eventID string, input CalendarEventInput) (*CalendarEvent, error)

	// DeleteCalendarEvent removes an event. A missing event must be reported as
	// ErrNotFound so the engine treats it as already done.
	DeleteCalendarEvent(ctx context.Context, creds Credentials, tokens *Tokens, calendarID, eventID string) error
}

// EventFingerprint is the content identity the engine compares between runs: two
// listings of the same unchanged event produce the same value, an edit on either
// side changes it. It deliberately ignores the provider id and the modification
// stamp (providers bump those on no-op writes) and covers everything the engine
// would copy.
func EventFingerprint(event CalendarEvent) string {
	parts := []string{
		strings.TrimSpace(event.Summary),
		strings.TrimSpace(event.Description),
		strings.TrimSpace(event.Location),
		event.Start.UTC().Format(time.RFC3339),
		event.End.UTC().Format(time.RFC3339),
		fmt.Sprintf("all_day=%t", event.AllDay),
		strings.Join(sortedLower(event.Attendees), ","),
		fmt.Sprintf("recurring=%t", event.Recurring),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// InputFingerprint is EventFingerprint for a value the engine is about to write.
// It is what a freshly written event is expected to list as, so a provider that
// normalises a value (Graph trims a description, Google rewrites an attendee
// list) does not look like a conflict on the next run.
func InputFingerprint(input CalendarEventInput) string {
	return EventFingerprint(CalendarEvent{
		Summary:     input.Summary,
		Description: input.Description,
		Location:    input.Location,
		Start:       input.Start,
		End:         input.End,
		AllDay:      input.AllDay,
		Attendees:   input.Attendees,
	})
}

// EventInputFrom converts a listed event into the input that writes it back
// unchanged.
func EventInputFrom(event CalendarEvent) CalendarEventInput {
	return CalendarEventInput{
		UID:         event.UID,
		Summary:     event.Summary,
		Description: event.Description,
		Location:    event.Location,
		Start:       event.Start,
		End:         event.End,
		AllDay:      event.AllDay,
		TimeZone:    event.TimeZone,
		Attendees:   append([]string(nil), event.Attendees...),
	}
}

// CalendarEventUID derives the iCalendar UID of an event the engine creates from
// a stable seed (the UID of the event on the source side, or a hash of its
// identity). Deriving it instead of letting each provider mint its own is what
// keeps a Google-created copy and a Microsoft-created copy of the same event
// recognisable as the same event when a later run pairs the two calendars in the
// opposite direction.
//
// The result is a compliance-shaped UID: a hex digest and a domain, no spaces,
// no control characters, no colon.
func CalendarEventUID(seed string) string {
	sum := sha256.Sum256([]byte("sync-calendar\x00" + strings.TrimSpace(seed)))
	return hex.EncodeToString(sum[:20]) + "@sync.local"
}

// CalendarsReadable reports whether a registered provider can answer calendar
// calls at all. The handler uses it to explain a provider without support.
func CalendarsReadable(provider any) bool {
	_, ok := provider.(CalendarProvider)
	return ok
}

// sortedLower normalises an attendee list before it is hashed: the order of an
// attendee list is not content, and a provider may return it in any order.
func sortedLower(values []string) []string {
	lowered := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			lowered = append(lowered, value)
		}
	}
	sort.Strings(lowered)
	return lowered
}

// LoadZone resolves an IANA zone name without failing. Calendar payloads carry
// a zone name next to a wall-clock time; when the runtime has no zone database
// (a scratch container) or the name is a Windows one Graph stored, an explicit
// UTC offset still stays correct, so callers fall back to UTC instead of failing
// the listing.
func LoadZone(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return location
}
