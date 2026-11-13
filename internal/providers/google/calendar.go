package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

// Compile time guard: the calendar engine discovers this half of the provider
// through an interface assertion, so a renamed method must fail the build.
var _ providers.CalendarProvider = (*Provider)(nil)

// calendarBase is the Calendar v3 API root. It shares the host of the Drive API,
// which is what lets the request guard in client.go cover both APIs.
const calendarBase = "https://www.googleapis.com/calendar/v3"

// calendarIdentifierPattern is the only shape of calendar id this provider puts
// into a request. Calendar ids are not opaque tokens like Drive ids: Google uses
// "primary", an email address (`…@group.calendar.google.com`) and the holiday
// calendars carry a `#` (`en.usa#holiday@group.v.calendar.google.com`). The
// pattern accepts exactly that alphabet and refuses a path separator, a query
// delimiter, a backslash, whitespace and control characters, so a stored id can
// never steer the request (see identifiers.go for the same reasoning).
var calendarIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9._%+#@-]{1,512}$`)

// eventIdentifierPattern is the shape of an event id: the URL-safe alphabet
// Google mints, which is longer than a Drive id but otherwise the same.
var eventIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,900}$`)

// calendarSelect is the projection requested for every listing: the fields the
// engine consumes, nothing else.
const calendarListFields = "items(id,summary,timeZone,primary,accessRole),nextPageToken"

// calendarEventFields is the event projection. The calendar view of an event
// carries the recurrence markers, so a series master is recognisable without a
// second read.
const calendarEventFields = "items(id,status,summary,description,location,start,end,updated,iCalUID,recurringEventId,recurrence,organizer(email),attendees(email)),nextPageToken"

// Calendars implements providers.CalendarProvider.
func (p *Provider) Calendars(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) ([]providers.Calendar, error) {
	client := p.client(tokens)
	list := make([]providers.Calendar, 0, 8)
	pageToken := ""
	for {
		target := withParam(calendarBase+"/users/me/calendarList", "maxResults", "250")
		target = withParam(target, "fields", calendarListFields)
		target = withParam(target, "pageToken", pageToken)
		var page googleCalendarList
		if err := p.doCalendarJSON(ctx, client, http.MethodGet, target, nil, &page); err != nil {
			return nil, fmt.Errorf("google calendar: cannot list the calendars of the account: %w", err)
		}
		for _, item := range page.Items {
			list = append(list, providers.Calendar{
				ID:       item.ID,
				Name:     item.Summary,
				Primary:  item.Primary,
				TimeZone: item.TimeZone,
				ReadOnly: item.AccessRole != "owner" && item.AccessRole != "writer",
			})
		}
		if page.NextPageToken == "" {
			return list, nil
		}
		pageToken = page.NextPageToken
	}
}

// CalendarEvents implements providers.CalendarProvider.
func (p *Provider) CalendarEvents(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, calendarID string, window providers.CalendarWindow) ([]providers.CalendarEvent, error) {
	// The guard is inline, applied to the value that is about to be escaped into
	// the request URL (see identifiers.go).
	if !calendarIdentifierPattern.MatchString(calendarID) {
		return nil, invalidCalendarIdentifierError("calendar", calendarID)
	}
	client := p.client(tokens)
	events := make([]providers.CalendarEvent, 0, 64)
	pageToken := ""
	for {
		target := calendarBase + "/calendars/" + url.PathEscape(calendarID) + "/events"
		target = withParam(target, "timeMin", window.Start.UTC().Format(time.RFC3339))
		target = withParam(target, "timeMax", window.End.UTC().Format(time.RFC3339))
		target = withParam(target, "singleEvents", "false")
		target = withParam(target, "showDeleted", "false")
		target = withParam(target, "maxResults", "2500")
		target = withParam(target, "fields", calendarEventFields)
		target = withParam(target, "pageToken", pageToken)
		var page googleEventList
		if err := p.doCalendarJSON(ctx, client, http.MethodGet, target, nil, &page); err != nil {
			return nil, fmt.Errorf("google calendar: cannot list the events of %s: %w", calendarID, err)
		}
		for _, item := range page.Items {
			event, err := googleEventFrom(item)
			if err != nil {
				// A malformed timestamp is one bad event, not a failed run: the
				// engine reports what it could read and counts the rest.
				continue
			}
			events = append(events, event)
		}
		if page.NextPageToken == "" {
			return events, nil
		}
		pageToken = page.NextPageToken
	}
}

// CreateCalendarEvent implements providers.CalendarProvider.
func (p *Provider) CreateCalendarEvent(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, calendarID string, input providers.CalendarEventInput) (*providers.CalendarEvent, error) {
	if !calendarIdentifierPattern.MatchString(calendarID) {
		return nil, invalidCalendarIdentifierError("calendar", calendarID)
	}
	client := p.client(tokens)
	target := calendarBase + "/calendars/" + url.PathEscape(calendarID) + "/events"
	target = withParam(target, "sendUpdates", "none")
	var created googleEvent
	if err := p.doCalendarJSON(ctx, client, http.MethodPost, target, googleEventPayload(input), &created); err != nil {
		return nil, fmt.Errorf("google calendar: cannot create an event in %s: %w", calendarID, err)
	}
	event, err := googleEventFrom(created)
	if err != nil {
		return nil, fmt.Errorf("google calendar: the event created in %s cannot be read back: %w", calendarID, err)
	}
	return &event, nil
}

// UpdateCalendarEvent implements providers.CalendarProvider. PUT replaces the
// fields the engine manages; fields it does not manage (a conference link, a
// reminder) are preserved because the payload only carries the ones it read.
func (p *Provider) UpdateCalendarEvent(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, calendarID, eventID string, input providers.CalendarEventInput) (*providers.CalendarEvent, error) {
	if !calendarIdentifierPattern.MatchString(calendarID) {
		return nil, invalidCalendarIdentifierError("calendar", calendarID)
	}
	if !eventIdentifierPattern.MatchString(eventID) {
		return nil, invalidCalendarIdentifierError("event", eventID)
	}
	client := p.client(tokens)
	target := calendarBase + "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(eventID)
	target = withParam(target, "sendUpdates", "none")
	var updated googleEvent
	if err := p.doCalendarJSON(ctx, client, http.MethodPut, target, googleEventPayload(input), &updated); err != nil {
		if p.IsNotFound(err) {
			return nil, fmt.Errorf("%w: google calendar event %s", providers.ErrNotFound, eventID)
		}
		return nil, fmt.Errorf("google calendar: cannot update event %s: %w", eventID, err)
	}
	event, err := googleEventFrom(updated)
	if err != nil {
		return nil, fmt.Errorf("google calendar: event %s cannot be read back: %w", eventID, err)
	}
	return &event, nil
}

// DeleteCalendarEvent implements providers.CalendarProvider.
func (p *Provider) DeleteCalendarEvent(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, calendarID, eventID string) error {
	if !calendarIdentifierPattern.MatchString(calendarID) {
		return invalidCalendarIdentifierError("calendar", calendarID)
	}
	if !eventIdentifierPattern.MatchString(eventID) {
		return invalidCalendarIdentifierError("event", eventID)
	}
	client := p.client(tokens)
	target := calendarBase + "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(eventID)
	target = withParam(target, "sendUpdates", "none")
	if err := p.doCalendarJSON(ctx, client, http.MethodDelete, target, nil, nil); err != nil {
		if p.IsNotFound(err) {
			return fmt.Errorf("%w: google calendar event %s", providers.ErrNotFound, eventID)
		}
		return fmt.Errorf("google calendar: cannot delete event %s: %w", eventID, err)
	}
	return nil
}

// googleCalendarList is one page of calendarList.list.
type googleCalendarList struct {
	Items         []googleCalendar `json:"items"`
	NextPageToken string           `json:"nextPageToken"`
}

// googleCalendar is one entry of calendarList.list. AccessRole is what tells a
// readable calendar from a writable one.
type googleCalendar struct {
	ID         string `json:"id"`
	Summary    string `json:"summary"`
	TimeZone   string `json:"timeZone"`
	Primary    bool   `json:"primary"`
	AccessRole string `json:"accessRole"`
}

// googleEventList is one page of events.list.
type googleEventList struct {
	Items         []googleEvent `json:"items"`
	NextPageToken string        `json:"nextPageToken"`
}

// googleEvent is the subset of an event resource the engine consumes. It is used
// both to read a listing and to write a payload: the zero fields stay out of a
// create/update, which is exactly the "only manage what you know" contract.
type googleEvent struct {
	ID               string           `json:"id,omitempty"`
	Status           string           `json:"status,omitempty"`
	Summary          string           `json:"summary,omitempty"`
	Description      string           `json:"description,omitempty"`
	Location         string           `json:"location,omitempty"`
	Start            *googleEventTime `json:"start,omitempty"`
	End              *googleEventTime `json:"end,omitempty"`
	Updated          string           `json:"updated,omitempty"`
	ICalUID          string           `json:"iCalUID,omitempty"`
	Recurrence       []string         `json:"recurrence,omitempty"`
	RecurringEventID string           `json:"recurringEventId,omitempty"`
	Organizer        *googlePerson    `json:"organizer,omitempty"`
	Attendees        []googlePerson   `json:"attendees,omitempty"`
}

// googleEventTime is a date (all-day) or a date-time with its zone.
type googleEventTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"dateTime,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

// googlePerson is an organizer or an attendee.
type googlePerson struct {
	Email string `json:"email,omitempty"`
}

// googleEventFrom normalises a listed event. The UID falls back to the event id
// when Google does not report one, so the engine always has a pairing key.
func googleEventFrom(raw googleEvent) (providers.CalendarEvent, error) {
	start, allDay, zone, err := googleEventTimeFrom(raw.Start)
	if err != nil {
		return providers.CalendarEvent{}, err
	}
	end, _, _, err := googleEventTimeFrom(raw.End)
	if err != nil {
		return providers.CalendarEvent{}, err
	}
	updated := time.Time{}
	if raw.Updated != "" {
		if parsed, err := time.Parse(time.RFC3339, raw.Updated); err == nil {
			updated = parsed
		}
	}
	event := providers.CalendarEvent{
		ID:          raw.ID,
		UID:         raw.ICalUID,
		Summary:     raw.Summary,
		Description: raw.Description,
		Location:    raw.Location,
		Start:       start,
		End:         end,
		AllDay:      allDay,
		TimeZone:    zone,
		UpdatedAt:   updated,
		Recurring:   len(raw.Recurrence) > 0 || raw.RecurringEventID != "",
		Cancelled:   raw.Status == "cancelled",
		Organizer:   googlePersonEmail(raw.Organizer),
		Attendees:   make([]string, 0, len(raw.Attendees)),
	}
	if event.UID == "" {
		event.UID = raw.ID
	}
	for _, attendee := range raw.Attendees {
		if attendee.Email != "" {
			event.Attendees = append(event.Attendees, attendee.Email)
		}
	}
	return event, nil
}

// googleEventTimeFrom decodes a start or end value. An all-day event carries a
// DATE, which is midnight in the event zone; the zone defaults to UTC when the
// provider did not name one the runtime can resolve.
func googleEventTimeFrom(value *googleEventTime) (time.Time, bool, string, error) {
	if value == nil {
		return time.Time{}, false, "", errors.New("google calendar: event without a start or end")
	}
	zone := providers.LoadZone(value.TimeZone)
	if value.Date != "" {
		day, err := time.ParseInLocation("2006-01-02", value.Date, zone)
		if err != nil {
			return time.Time{}, false, "", err
		}
		return day, true, value.TimeZone, nil
	}
	if value.DateTime == "" {
		return time.Time{}, false, "", errors.New("google calendar: event time without a value")
	}
	moment, err := time.Parse(time.RFC3339, value.DateTime)
	if err != nil {
		return time.Time{}, false, "", err
	}
	return moment, false, value.TimeZone, nil
}

// googleEventPayload builds the write body of a create or update. An all-day
// event is sent as a DATE (never as a midnight instant, which would show up on
// the wrong day for a reader in another zone), a timed event as an RFC 3339
// date-time with the zone it was read in.
func googleEventPayload(input providers.CalendarEventInput) googleEvent {
	payload := googleEvent{
		Summary:     input.Summary,
		Description: input.Description,
		Location:    input.Location,
		ICalUID:     input.UID,
	}
	zone := input.TimeZone
	if zone == "" {
		zone = "UTC"
	}
	if input.AllDay {
		payload.Start = &googleEventTime{Date: input.Start.Format("2006-01-02")}
		payload.End = &googleEventTime{Date: input.End.Format("2006-01-02")}
	} else {
		payload.Start = &googleEventTime{DateTime: input.Start.Format(time.RFC3339), TimeZone: zone}
		payload.End = &googleEventTime{DateTime: input.End.Format(time.RFC3339), TimeZone: zone}
	}
	for _, attendee := range input.Attendees {
		if attendee = strings.TrimSpace(attendee); attendee != "" {
			payload.Attendees = append(payload.Attendees, googlePerson{Email: attendee})
		}
	}
	return payload
}

func googlePersonEmail(person *googlePerson) string {
	if person == nil {
		return ""
	}
	return person.Email
}

// invalidCalendarIdentifierError reports a calendar or event id that cannot have
// come from the Google Calendar API. It carries the same sentinel as the Drive
// guard, so the handlers answer 400 for a stored value the provider refuses.
func invalidCalendarIdentifierError(kind, value string) error {
	return fmt.Errorf("%w: google calendar %s %q", providers.ErrInvalidIdentifier, kind, value)
}
