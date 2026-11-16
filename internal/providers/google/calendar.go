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
// second read, and `originalStartTime` is what identifies an exception.
const calendarEventFields = "items(id,status,summary,description,location,start,end,updated,transparency,iCalUID,recurringEventId,originalStartTime,recurrence,eventType,organizer(email),attendees(email)),nextPageToken"

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
		target = withParam(target, "showDeleted", "true")
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
	if strings.TrimSpace(input.SeriesID) != "" {
		// An occurrence of a series already exists (its master generated it):
		// there is nothing to create, only something to change.
		return p.writeInstance(ctx, client, calendarID, input)
	}
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
	if strings.TrimSpace(input.SeriesID) != "" {
		return p.writeInstance(ctx, client, calendarID, input)
	}
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

// writeInstance writes one occurrence of a series. Google identifies an
// occurrence of a master through `events.instances?originalStart=`, so the engine
// can target an occurrence it never listed: it only has to know the master and
// the start the master gave the occurrence.
func (p *Provider) writeInstance(ctx context.Context, client *http.Client, calendarID string, input providers.CalendarEventInput) (*providers.CalendarEvent, error) {
	instanceID, err := p.resolveInstance(ctx, client, calendarID, input)
	if err != nil {
		return nil, err
	}
	target := calendarBase + "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(instanceID)
	target = withParam(target, "sendUpdates", "none")
	var saved googleEvent
	if err := p.doCalendarJSON(ctx, client, http.MethodPut, target, googleEventPayload(input), &saved); err != nil {
		if p.IsNotFound(err) {
			return nil, fmt.Errorf("%w: google calendar occurrence %s of %s", providers.ErrNotFound, instanceID, input.SeriesID)
		}
		return nil, fmt.Errorf("google calendar: cannot write occurrence %s of %s: %w", instanceID, input.SeriesID, err)
	}
	event, err := googleEventFrom(saved)
	if err != nil {
		return nil, fmt.Errorf("google calendar: the occurrence %s cannot be read back: %w", instanceID, err)
	}
	return &event, nil
}

// resolveInstance answers the id of the occurrence of input.SeriesID that the rule
// started at input.OriginalStart. An all-day occurrence is matched by its DATE,
// which is what Google publishes for one; a timed occurrence by its instant. A
// series whose instance list does not hold that occurrence yet answers
// ErrNotFound, so the engine retries on the next run instead of guessing (and
// writing the wrong occurrence of a series is worse than a retry).
func (p *Provider) resolveInstance(ctx context.Context, client *http.Client, calendarID string, input providers.CalendarEventInput) (string, error) {
	masterID := input.SeriesID
	originalStart := input.OriginalStart
	if !eventIdentifierPattern.MatchString(masterID) {
		return "", invalidCalendarIdentifierError("event", masterID)
	}
	if originalStart.IsZero() {
		return "", fmt.Errorf("%w: google calendar occurrence without an original start", providers.ErrNotFound)
	}
	target := calendarBase + "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(masterID) + "/instances"
	target = withParam(target, "originalStart", originalStart.UTC().Format(time.RFC3339))
	target = withParam(target, "maxResults", "5")
	target = withParam(target, "showDeleted", "true")
	var page googleEventList
	if err := p.doCalendarJSON(ctx, client, http.MethodGet, target, nil, &page); err != nil {
		if p.IsNotFound(err) {
			return "", fmt.Errorf("%w: google calendar series %s", providers.ErrNotFound, masterID)
		}
		return "", fmt.Errorf("google calendar: cannot list the occurrences of %s: %w", masterID, err)
	}
	wanted := originalStart.UTC()
	wantedDate := originalStart.In(providers.LoadZone(input.TimeZone)).Format("2006-01-02")
	for _, item := range page.Items {
		if item.ID == "" || item.OriginalStart == nil {
			continue
		}
		if input.AllDay && item.OriginalStart.Date != "" {
			if item.OriginalStart.Date == wantedDate {
				return item.ID, nil
			}
			continue
		}
		moment, _, _, err := googleEventTimeFrom(item.OriginalStart)
		if err == nil && moment.Equal(wanted) {
			return item.ID, nil
		}
	}
	return "", fmt.Errorf("%w: google calendar occurrence of %s at %s", providers.ErrNotFound, masterID, wanted.Format(time.RFC3339))
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
	Transparency     string           `json:"transparency,omitempty"`
	ICalUID          string           `json:"iCalUID,omitempty"`
	Recurrence       []string         `json:"recurrence,omitempty"`
	RecurringEventID string           `json:"recurringEventId,omitempty"`
	OriginalStart    *googleEventTime `json:"originalStartTime,omitempty"`
	EventType        string           `json:"eventType,omitempty"`
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
	original := time.Time{}
	if raw.OriginalStart != nil {
		if moment, _, _, err := googleEventTimeFrom(raw.OriginalStart); err == nil {
			original = moment
		}
	}
	kind := providers.EventKindSingle
	switch {
	case len(raw.Recurrence) > 0:
		kind = providers.EventKindMaster
	case raw.RecurringEventID != "":
		// A listing that does not expand the series returns the master and the
		// occurrences somebody moved, edited or cancelled: each of those is an
		// exception, whether or not it still carries a start.
		kind = providers.EventKindException
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
		// Google omits `transparency` for a normal (busy) event, so an absent
		// value is the busy default rather than "unknown".
		FreeBusy:      freeBusyFromGoogle(raw),
		UpdatedAt:     updated,
		Recurring:     kind != providers.EventKindSingle,
		Kind:          kind,
		SeriesID:      raw.RecurringEventID,
		OriginalStart: original,
		Recurrence:    append([]string(nil), raw.Recurrence...),
		Cancelled:     raw.Status == "cancelled",
		Organizer:     googlePersonEmail(raw.Organizer),
		Attendees:     make([]string, 0, len(raw.Attendees)),
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
//
// The zone it returns is always a canonical IANA name, never the raw label Google
// stored: the value is written back to whichever provider the job copies into,
// and Google refuses a payload whose start names a label it cannot read.
func googleEventTimeFrom(value *googleEventTime) (time.Time, bool, string, error) {
	if value == nil {
		return time.Time{}, false, "", errors.New("google calendar: event without a start or end")
	}
	zoneName := providers.ZoneName(value.TimeZone)
	zone := providers.LoadZone(zoneName)
	if value.Date != "" {
		day, err := time.ParseInLocation("2006-01-02", value.Date, zone)
		if err != nil {
			return time.Time{}, false, "", err
		}
		return day, true, zoneName, nil
	}
	if value.DateTime == "" {
		return time.Time{}, false, "", errors.New("google calendar: event time without a value")
	}
	moment, err := time.Parse(time.RFC3339, value.DateTime)
	if err != nil {
		return time.Time{}, false, "", err
	}
	return moment, false, zoneName, nil
}

// googleEventPayload builds the write body of a create or update. An all-day
// event is sent as a DATE (never as a midnight instant, which would show up on
// the wrong day for a reader in another zone), a timed event as an RFC 3339
// date-time rendered in the zone it is copied with.
//
// The zone is canonicalised here, at the edge of the request: this is the guard
// that keeps a Windows label or a feed TZID out of `start.timeZone`, which Google
// answers with "Invalid time zone definition for start time".
func googleEventPayload(input providers.CalendarEventInput) googleEvent {
	payload := googleEvent{
		Summary:     input.Summary,
		Description: input.Description,
		Location:    input.Location,
		ICalUID:     input.UID,
	}
	applyFreeBusy(&payload, input.FreeBusy)
	if len(input.Recurrence) > 0 {
		payload.Recurrence = googleRecurrence(input.Recurrence)
	}
	if input.Cancelled {
		payload.Status = "cancelled"
	}
	if input.AllDay {
		payload.Start = &googleEventTime{Date: input.Start.Format("2006-01-02")}
		payload.End = &googleEventTime{Date: input.End.Format("2006-01-02")}
	} else {
		start, zone := providers.InZone(input.Start, input.TimeZone)
		end, _ := providers.InZone(input.End, input.TimeZone)
		payload.Start = &googleEventTime{DateTime: start.Format(time.RFC3339), TimeZone: zone}
		payload.End = &googleEventTime{DateTime: end.Format(time.RFC3339), TimeZone: zone}
	}
	for _, attendee := range input.Attendees {
		if attendee = strings.TrimSpace(attendee); attendee != "" {
			payload.Attendees = append(payload.Attendees, googlePerson{Email: attendee})
		}
	}
	return payload
}

// freeBusyFromGoogle normalises the free/busy state of a listed event. Google
// spreads it over three properties: `status` says cancelled or tentative,
// `eventType` says out-of-office (and focus time, which blocks like a busy
// event), and `transparency` says free. The API leaves `transparency` out for a
// busy event, which is why an absent value is the busy default.
func freeBusyFromGoogle(raw googleEvent) providers.FreeBusy {
	if raw.Status == "cancelled" {
		return providers.FreeBusyCancelled
	}
	switch raw.EventType {
	case "outOfOffice":
		return providers.FreeBusyOutOfOffice
	}
	if raw.Status == "tentative" {
		return providers.FreeBusyTentative
	}
	if providers.FreeBusyName(raw.Transparency) == providers.FreeBusyFree {
		return providers.FreeBusyFree
	}
	return providers.FreeBusyBusy
}

// applyFreeBusy writes a free/busy state onto a payload. An empty state leaves
// the payload untouched, so an unrelated update keeps the marker the operator set
// on the destination calendar.
func applyFreeBusy(payload *googleEvent, state providers.FreeBusy) {
	switch providers.FreeBusyName(string(state)) {
	case providers.FreeBusyFree:
		payload.Transparency = "transparent"
		payload.Status = "confirmed"
	case providers.FreeBusyBusy:
		payload.Transparency = "opaque"
		payload.Status = "confirmed"
	case providers.FreeBusyTentative:
		payload.Transparency = "opaque"
		payload.Status = "tentative"
	case providers.FreeBusyOutOfOffice:
		payload.Transparency = "opaque"
		payload.Status = "confirmed"
		payload.EventType = "outOfOffice"
	case providers.FreeBusyWorkingElsewhere:
		// Google has no third busy state: working elsewhere blocks the calendar
		// like a plain busy event.
		payload.Transparency = "opaque"
		payload.Status = "confirmed"
	case providers.FreeBusyCancelled:
		payload.Status = "cancelled"
	}
}

// googleRecurrence normalises the rule lines a write carries. The API expects the
// RFC 5545 form with the property name in front, which is what a Microsoft or
// .ics source does not always produce.
func googleRecurrence(lines []string) []string {
	normalized := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.Contains(line, ":") {
			line = "RRULE:" + line
		}
		normalized = append(normalized, line)
	}
	return normalized
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
