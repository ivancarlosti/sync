package microsoft

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	msgraphgocore "github.com/microsoftgraph/msgraph-sdk-go-core"
	graphmodels "github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/users"

	"github.com/ivancarlosti/sync/internal/providers"
)

// Compile time guard: the calendar engine discovers this half of the provider
// through an interface assertion, so a renamed method must fail the build.
var _ providers.CalendarProvider = (*Provider)(nil)

// calendarIDPattern is the only shape of calendar or event id this provider puts
// into a Graph request. Graph ids are base64 (plus the URL-safe variant the
// calendar endpoints return), so the alphabet carries `=`, `+`, `/`, `-` and `_`;
// everything that could split a request line or a header (whitespace, control
// characters, a backslash) is refused. The id itself is never interpolated into a
// URL here: the generated client escapes the path parameter, so the guard narrows
// the value rather than building a URL.
var calendarIDPattern = regexp.MustCompile(`^[A-Za-z0-9._=+/-]{1,900}$`)

// eventSelect is the projection requested by a calendar view: the fields the
// engine consumes, which also keeps a month of events a cheap listing.
var calendarEventSelect = []string{
	"id", "iCalUId", "subject", "body", "bodyPreview", "location", "start", "end",
	"isAllDay", "isCancelled", "type", "seriesMasterId", "recurrence",
	"organizer", "attendees",
}

// Calendars implements providers.CalendarProvider.
func (p *Provider) Calendars(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) ([]providers.Calendar, error) {
	client, adapter, err := p.graphSession(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	page, err := client.Me().Calendars().Get(ctx, &users.ItemCalendarsRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemCalendarsRequestBuilderGetQueryParameters{Top: ptr(int32(100))},
	})
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot list the calendars of the account: %w", err)
	}
	// A collection-response factory, so a second page decodes as a collection
	// instead of a single calendar (see Children in graph.go).
	iterator, err := msgraphgocore.NewPageIterator[graphmodels.Calendarable](page, adapter,
		graphmodels.CreateCalendarCollectionResponseFromDiscriminatorValue)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot page the calendars: %w", err)
	}
	list := make([]providers.Calendar, 0, len(page.GetValue()))
	if err := iterator.Iterate(ctx, func(calendar graphmodels.Calendarable) bool {
		list = append(list, calendarFromGraph(calendar))
		return true
	}); err != nil {
		return nil, fmt.Errorf("microsoft graph: listing the calendars failed: %w", err)
	}
	return list, nil
}

// CalendarEvents implements providers.CalendarProvider. The calendar view is
// used instead of the event collection: it expands a recurring series into the
// instances that actually fall inside the window, which is what a reader of the
// destination calendar expects to see.
func (p *Provider) CalendarEvents(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, calendarID string, window providers.CalendarWindow) ([]providers.CalendarEvent, error) {
	if !calendarIDPattern.MatchString(calendarID) {
		return nil, invalidCalendarIDError("calendar", calendarID)
	}
	client, adapter, err := p.graphSession(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	page, err := client.Me().Calendars().ByCalendarId(calendarID).CalendarView().Get(ctx,
		&users.ItemCalendarsItemCalendarViewRequestBuilderGetRequestConfiguration{
			QueryParameters: &users.ItemCalendarsItemCalendarViewRequestBuilderGetQueryParameters{
				StartDateTime: ptr(window.Start.UTC().Format(time.RFC3339)),
				EndDateTime:   ptr(window.End.UTC().Format(time.RFC3339)),
				Top:           ptr(int32(250)),
				Select:        calendarEventSelect,
			},
		})
	if err != nil {
		if p.IsNotFound(err) {
			return nil, fmt.Errorf("%w: microsoft calendar %s", providers.ErrNotFound, calendarID)
		}
		return nil, fmt.Errorf("microsoft graph: cannot read the events of %s: %w", calendarID, err)
	}
	iterator, err := msgraphgocore.NewPageIterator[graphmodels.Eventable](page, adapter,
		graphmodels.CreateEventCollectionResponseFromDiscriminatorValue)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot page the events of %s: %w", calendarID, err)
	}
	events := make([]providers.CalendarEvent, 0, len(page.GetValue()))
	if err := iterator.Iterate(ctx, func(event graphmodels.Eventable) bool {
		normalised, err := eventFromGraph(event)
		if err != nil {
			// One unreadable event does not fail the listing: the engine counts
			// what it could read and reports the rest as skipped.
			return true
		}
		events = append(events, normalised)
		return true
	}); err != nil {
		return nil, fmt.Errorf("microsoft graph: listing the events of %s failed: %w", calendarID, err)
	}
	return events, nil
}

// CreateCalendarEvent implements providers.CalendarProvider.
func (p *Provider) CreateCalendarEvent(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, calendarID string, input providers.CalendarEventInput) (*providers.CalendarEvent, error) {
	if !calendarIDPattern.MatchString(calendarID) {
		return nil, invalidCalendarIDError("calendar", calendarID)
	}
	client, err := p.graphClient(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	created, err := client.Me().Calendars().ByCalendarId(calendarID).Events().Post(ctx, eventPayload(input), nil)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: cannot create an event in %s: %w", calendarID, err)
	}
	event, err := eventFromGraph(created)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: the event created in %s cannot be read back: %w", calendarID, err)
	}
	return &event, nil
}

// UpdateCalendarEvent implements providers.CalendarProvider.
func (p *Provider) UpdateCalendarEvent(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, calendarID, eventID string, input providers.CalendarEventInput) (*providers.CalendarEvent, error) {
	if !calendarIDPattern.MatchString(calendarID) {
		return nil, invalidCalendarIDError("calendar", calendarID)
	}
	if !calendarIDPattern.MatchString(eventID) {
		return nil, invalidCalendarIDError("event", eventID)
	}
	client, err := p.graphClient(tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	updated, err := client.Me().Calendars().ByCalendarId(calendarID).Events().ByEventId(eventID).Patch(ctx, eventPayload(input), nil)
	if err != nil {
		if p.IsNotFound(err) {
			return nil, fmt.Errorf("%w: microsoft event %s", providers.ErrNotFound, eventID)
		}
		return nil, fmt.Errorf("microsoft graph: cannot update event %s: %w", eventID, err)
	}
	event, err := eventFromGraph(updated)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: event %s cannot be read back: %w", eventID, err)
	}
	return &event, nil
}

// DeleteCalendarEvent implements providers.CalendarProvider.
func (p *Provider) DeleteCalendarEvent(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, calendarID, eventID string) error {
	if !calendarIDPattern.MatchString(calendarID) {
		return invalidCalendarIDError("calendar", calendarID)
	}
	if !calendarIDPattern.MatchString(eventID) {
		return invalidCalendarIDError("event", eventID)
	}
	client, err := p.graphClient(tokens)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	if err := client.Me().Calendars().ByCalendarId(calendarID).Events().ByEventId(eventID).Delete(ctx, nil); err != nil {
		if p.IsNotFound(err) {
			return fmt.Errorf("%w: microsoft event %s", providers.ErrNotFound, eventID)
		}
		return fmt.Errorf("microsoft graph: cannot delete event %s: %w", eventID, err)
	}
	return nil
}

// calendarFromGraph normalises a Graph calendar. `canEdit` is what tells the
// job editor which calendars may be a destination; a calendar the property does
// not mention is assumed writable, which is Graph's own default for a mailbox
// calendar.
func calendarFromGraph(calendar graphmodels.Calendarable) providers.Calendar {
	readOnly := false
	if canEdit := calendar.GetCanEdit(); canEdit != nil && !*canEdit {
		readOnly = true
	}
	return providers.Calendar{
		ID:       deref(calendar.GetId()),
		Name:     deref(calendar.GetName()),
		Primary:  boolValue(calendar.GetIsDefaultCalendar()),
		ReadOnly: readOnly,
	}
}

// eventFromGraph normalises a Graph event. Graph does not expose a modification
// stamp on the event model of this SDK version, so UpdatedAt stays zero unless a
// value can be read: the engine then falls back to the content fingerprint and
// never guesses which side is newer (see services.decide).
func eventFromGraph(event graphmodels.Eventable) (providers.CalendarEvent, error) {
	start, err := graphEventTime(event.GetStart())
	if err != nil {
		return providers.CalendarEvent{}, err
	}
	end, err := graphEventTime(event.GetEnd())
	if err != nil {
		return providers.CalendarEvent{}, err
	}
	allDay := boolValue(event.GetIsAllDay())
	zone := ""
	if startValue := event.GetStart(); startValue != nil {
		zone = deref(startValue.GetTimeZone())
	}
	normalised := providers.CalendarEvent{
		ID:          deref(event.GetId()),
		UID:         deref(event.GetICalUId()),
		Summary:     deref(event.GetSubject()),
		Description: graphBody(event.GetBody()),
		Location:    graphLocation(event.GetLocation()),
		Start:       start,
		End:         end,
		AllDay:      allDay,
		TimeZone:    zone,
		Recurring:   graphRecurring(event),
		Cancelled:   boolValue(event.GetIsCancelled()),
		Organizer:   graphRecipient(event.GetOrganizer()),
		Attendees:   make([]string, 0, len(event.GetAttendees())),
	}
	if normalised.UID == "" {
		normalised.UID = normalised.ID
	}
	for _, attendee := range event.GetAttendees() {
		if email := graphRecipient(attendee); email != "" {
			normalised.Attendees = append(normalised.Attendees, email)
		}
	}
	return normalised, nil
}

// graphRecurring reports whether the event belongs to a series, whether it is
// the master or one of its occurrences: both are skipped by the engine because a
// single occurrence cannot be copied without inventing a rule.
func graphRecurring(event graphmodels.Eventable) bool {
	if event.GetRecurrence() != nil {
		return true
	}
	if deref(event.GetSeriesMasterId()) != "" {
		return true
	}
	if kind := event.GetTypeEscaped(); kind != nil && *kind != graphmodels.SINGLEINSTANCE_EVENTTYPE {
		return true
	}
	return false
}

// eventPayload builds the body of a create or update. Only the fields the engine
// manages are set: a conference link or a reminder the user configured on the
// destination calendar therefore keeps its value on a patch.
func eventPayload(input providers.CalendarEventInput) graphmodels.Eventable {
	payload := graphmodels.NewEvent()
	payload.SetSubject(ptr(input.Summary))
	payload.SetIsAllDay(ptr(input.AllDay))

	body := graphmodels.NewItemBody()
	body.SetContentType(ptr(graphmodels.TEXT_BODYTYPE))
	body.SetContent(ptr(input.Description))
	payload.SetBody(body)

	if strings.TrimSpace(input.Location) != "" {
		location := graphmodels.NewLocation()
		location.SetDisplayName(ptr(input.Location))
		payload.SetLocation(location)
	}

	startValue, startZone := graphTime(input.Start, input.AllDay, input.TimeZone)
	endValue, _ := graphTime(input.End, input.AllDay, input.TimeZone)
	start := graphmodels.NewDateTimeTimeZone()
	start.SetDateTime(ptr(startValue))
	start.SetTimeZone(ptr(startZone))
	payload.SetStart(start)
	end := graphmodels.NewDateTimeTimeZone()
	end.SetDateTime(ptr(endValue))
	end.SetTimeZone(ptr(startZone))
	payload.SetEnd(end)

	for _, email := range input.Attendees {
		if email = strings.TrimSpace(email); email == "" {
			continue
		}
		address := graphmodels.NewEmailAddress()
		address.SetAddress(ptr(email))
		attendee := graphmodels.NewAttendee()
		attendee.SetEmailAddress(address)
		payload.SetAttendees(append(payload.GetAttendees(), attendee))
	}
	return payload
}

// graphTime renders an instant the way Graph expects it: a wall-clock value with
// no offset plus the zone that value belongs to. An all-day event is always sent
// as UTC midnight, because Graph interprets the date part in the named zone and a
// zone shift would move the day. A zone the runtime cannot resolve falls back to
// UTC, which keeps the instant correct even though it names a different zone.
func graphTime(moment time.Time, allDay bool, zone string) (string, string) {
	if allDay {
		return moment.UTC().Format("2006-01-02T15:04:05"), "UTC"
	}
	if strings.TrimSpace(zone) == "" {
		return moment.UTC().Format("2006-01-02T15:04:05"), "UTC"
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return moment.UTC().Format("2006-01-02T15:04:05"), "UTC"
	}
	return moment.In(providers.LoadZone(zone)).Format("2006-01-02T15:04:05"), zone
}

// graphEventTime decodes a Graph start or end value. Graph returns the wall-clock
// time without an offset and names the zone next to it, so the zone has to be
// applied when parsing; a value that carries an offset (some integrations send
// one) is parsed as the instant it already is.
func graphEventTime(value graphmodels.DateTimeTimeZoneable) (time.Time, error) {
	if value == nil {
		return time.Time{}, errors.New("microsoft graph: event without a start or end")
	}
	raw := strings.TrimSpace(deref(value.GetDateTime()))
	if raw == "" {
		return time.Time{}, errors.New("microsoft graph: event time without a value")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed, nil
	}
	location := providers.LoadZone(deref(value.GetTimeZone()))
	for _, layout := range []string{"2006-01-02T15:04:05.0000000", "2006-01-02T15:04:05.000", "2006-01-02T15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, raw, location); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("microsoft graph: cannot read the event time %q", raw)
}

func graphBody(body graphmodels.ItemBodyable) string {
	if body == nil {
		return ""
	}
	return deref(body.GetContent())
}

func graphLocation(location graphmodels.Locationable) string {
	if location == nil {
		return ""
	}
	return deref(location.GetDisplayName())
}

func graphRecipient(recipient graphmodels.Recipientable) string {
	if recipient == nil || recipient.GetEmailAddress() == nil {
		return ""
	}
	return deref(recipient.GetEmailAddress().GetAddress())
}

// invalidCalendarIDError reports a calendar or event id this provider refuses to
// put into a request. It carries the sentinel the handlers map to 400.
func invalidCalendarIDError(kind, value string) error {
	return fmt.Errorf("%w: microsoft calendar %s %q", providers.ErrInvalidIdentifier, kind, value)
}

func boolValue(value *bool) bool {
	return value != nil && *value
}
