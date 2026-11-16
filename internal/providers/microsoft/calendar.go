package microsoft

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	msgraphgocore "github.com/microsoftgraph/msgraph-sdk-go-core"
	graphmodels "github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/users"

	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/recurrence"
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
	"isAllDay", "isCancelled", "type", "seriesMasterId", "recurrence", "showAs",
	"originalStart", "originalStartTimeZone", "organizer", "attendees",
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

	// A calendar view expands a series and never returns the master itself, so
	// the rule of every series an exception names is read on its own. A master
	// that disappeared between the two calls is skipped: its exceptions are then
	// skipped too, with the evidence the run reports.
	masters := make(map[string]struct{}, 8)
	for _, event := range events {
		if event.Kind == providers.EventKindException && event.SeriesID != "" {
			masters[event.SeriesID] = struct{}{}
		}
	}
	for masterID := range masters {
		master, err := p.seriesMaster(ctx, client, calendarID, masterID)
		if err != nil {
			continue
		}
		events = append(events, *master)
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

	if strings.TrimSpace(input.SeriesID) != "" {
		// An occurrence of a series already exists (its master generated it):
		// there is nothing to create, only something to change.
		return p.writeInstance(ctx, client, calendarID, input)
	}
	payload, err := eventPayload(input)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: %w", err)
	}
	created, err := client.Me().Calendars().ByCalendarId(calendarID).Events().Post(ctx, payload, nil)
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

	if strings.TrimSpace(input.SeriesID) != "" {
		return p.writeInstance(ctx, client, calendarID, input)
	}
	payload, err := eventPayload(input)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: %w", err)
	}
	updated, err := client.Me().Calendars().ByCalendarId(calendarID).Events().ByEventId(eventID).Patch(ctx, payload, nil)
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
		ID:            deref(event.GetId()),
		UID:           deref(event.GetICalUId()),
		Summary:       deref(event.GetSubject()),
		Description:   graphBody(event.GetBody()),
		Location:      graphLocation(event.GetLocation()),
		Start:         start,
		End:           end,
		AllDay:        allDay,
		TimeZone:      providers.ZoneName(zone),
		FreeBusy:      freeBusyFromGraph(event.GetShowAs()),
		Recurring:     graphRecurring(event),
		Kind:          graphKind(event),
		SeriesID:      deref(event.GetSeriesMasterId()),
		OriginalStart: graphOriginalStart(event),
		Cancelled:     boolValue(event.GetIsCancelled()),
		Organizer:     graphRecipient(event.GetOrganizer()),
		Attendees:     make([]string, 0, len(event.GetAttendees())),
	}
	if rule, err := recurrence.FromGraph(event.GetRecurrence()); err == nil {
		normalised.Recurrence = rule
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

// graphKind classifies an event by its place in a series. Graph publishes the
// classification itself, so a master, an expanded occurrence and an edited
// occurrence of the same series are told apart without guessing.
func graphKind(event graphmodels.Eventable) providers.EventKind {
	if kind := event.GetTypeEscaped(); kind != nil {
		switch *kind {
		case graphmodels.SERIESMASTER_EVENTTYPE:
			return providers.EventKindMaster
		case graphmodels.EXCEPTION_EVENTTYPE:
			return providers.EventKindException
		case graphmodels.OCCURRENCE_EVENTTYPE:
			return providers.EventKindOccurrence
		}
	}
	if event.GetRecurrence() != nil {
		return providers.EventKindMaster
	}
	if deref(event.GetSeriesMasterId()) != "" {
		return providers.EventKindException
	}
	return providers.EventKindSingle
}

// graphRecurring reports whether the event belongs to a series, whether it is the
// master or one of its occurrences.
func graphRecurring(event graphmodels.Eventable) bool {
	return graphKind(event) != providers.EventKindSingle
}

// graphOriginalStart is the start the master gave an occurrence. Graph publishes
// it in UTC; it is the identity of an exception.
func graphOriginalStart(event graphmodels.Eventable) time.Time {
	if value := event.GetOriginalStart(); value != nil {
		return value.UTC()
	}
	return time.Time{}
}

// eventPayload builds the body of a create or update. Only the fields the engine
// manages are set: a conference link or a reminder the user configured on the
// destination calendar therefore keeps its value on a patch.
//
// It answers an error when the rule of a series cannot be represented by Graph.
// The caller reports that as a failed write instead of creating a single event
// where a series was meant.
func eventPayload(input providers.CalendarEventInput) (graphmodels.Eventable, error) {
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

	// The free/busy flag is only written when the job asked for a value: a plain
	// update would otherwise turn a destination event the operator marked as
	// out-of-office or tentative back into a plain busy one.
	if status, ok := graphShowAs(input.FreeBusy); ok {
		payload.SetShowAs(&status)
	}
	if len(input.Recurrence) > 0 {
		value, err := recurrence.ToGraph(input.Recurrence, input.Start, startZone)
		if err != nil {
			return nil, err
		}
		payload.SetRecurrence(value)
	}

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
	return payload, nil
}

// writeInstance writes one occurrence of a series. Graph identifies an occurrence
// through the instance list of its master, because `originalStart` is the only
// stable handle an occurrence has: the engine knows the master and the start the
// rule gave the occurrence, and resolves the rest here.
//
// A cancellation has no writable representation on Graph (`isCancelled` is
// read-only), so it deletes the occurrence, which is the same statement inside the
// series.
func (p *Provider) writeInstance(ctx context.Context, client *msgraphsdk.GraphServiceClient, calendarID string, input providers.CalendarEventInput) (*providers.CalendarEvent, error) {
	instanceID, err := p.resolveInstance(ctx, client, calendarID, input)
	if err != nil {
		return nil, err
	}
	builder := client.Me().Calendars().ByCalendarId(calendarID).Events().ByEventId(instanceID)
	if input.Cancelled {
		if err := builder.Delete(ctx, nil); err != nil {
			if p.IsNotFound(err) {
				return nil, fmt.Errorf("%w: microsoft occurrence %s of %s", providers.ErrNotFound, instanceID, input.SeriesID)
			}
			return nil, fmt.Errorf("microsoft graph: cannot cancel occurrence %s of %s: %w", instanceID, input.SeriesID, err)
		}
		return &providers.CalendarEvent{ID: instanceID, UID: input.UID, Cancelled: true, Kind: providers.EventKindException}, nil
	}
	payload, err := eventPayload(input)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: %w", err)
	}
	saved, err := builder.Patch(ctx, payload, nil)
	if err != nil {
		if p.IsNotFound(err) {
			return nil, fmt.Errorf("%w: microsoft occurrence %s of %s", providers.ErrNotFound, instanceID, input.SeriesID)
		}
		return nil, fmt.Errorf("microsoft graph: cannot write occurrence %s of %s: %w", instanceID, input.SeriesID, err)
	}
	event, err := eventFromGraph(saved)
	if err != nil {
		return nil, fmt.Errorf("microsoft graph: the occurrence %s cannot be read back: %w", instanceID, err)
	}
	return &event, nil
}

// resolveInstance answers the id of the occurrence of input.SeriesID whose original
// start is the one the rule gave it. An all-day occurrence is also matched at the
// UTC midnight of its wall date, because Graph publishes `originalStart` in UTC
// and the instant a source stores for a midnight depends on its own zone.
func (p *Provider) resolveInstance(ctx context.Context, client *msgraphsdk.GraphServiceClient, calendarID string, input providers.CalendarEventInput) (string, error) {
	masterID := input.SeriesID
	originalStart := input.OriginalStart
	if !calendarIDPattern.MatchString(masterID) {
		return "", invalidCalendarIDError("event", masterID)
	}
	if originalStart.IsZero() {
		return "", fmt.Errorf("%w: microsoft occurrence without an original start", providers.ErrNotFound)
	}
	start := originalStart.UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	end := originalStart.UTC().Add(48 * time.Hour).Format(time.RFC3339)
	page, err := client.Me().Calendars().ByCalendarId(calendarID).Events().ByEventId(masterID).Instances().Get(ctx,
		&users.ItemCalendarsItemEventsItemInstancesRequestBuilderGetRequestConfiguration{
			QueryParameters: &users.ItemCalendarsItemEventsItemInstancesRequestBuilderGetQueryParameters{
				StartDateTime: &start,
				EndDateTime:   &end,
				Top:           ptr(int32(50)),
				Select:        calendarEventSelect,
			},
		})
	if err != nil {
		if p.IsNotFound(err) {
			return "", fmt.Errorf("%w: microsoft series %s", providers.ErrNotFound, masterID)
		}
		return "", fmt.Errorf("microsoft graph: cannot list the occurrences of %s: %w", masterID, err)
	}
	wanted := originalStart.UTC()
	wantedAllDay := time.Time{}
	if input.AllDay {
		if date, err := time.Parse("2006-01-02", originalStart.In(providers.LoadZone(input.TimeZone)).Format("2006-01-02")); err == nil {
			wantedAllDay = date.UTC()
		}
	}
	for _, instance := range page.GetValue() {
		moment := graphOriginalStart(instance)
		if moment.IsZero() {
			continue
		}
		if !moment.Equal(wanted) && (wantedAllDay.IsZero() || !moment.Equal(wantedAllDay)) {
			continue
		}
		if id := deref(instance.GetId()); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("%w: microsoft occurrence of %s at %s", providers.ErrNotFound, masterID, wanted.Format(time.RFC3339))
}

// seriesMaster reads one master by id. A calendar view never returns a master
// (it expands the series), so the master of a copied exception has to be fetched
// on its own.
func (p *Provider) seriesMaster(ctx context.Context, client *msgraphsdk.GraphServiceClient, calendarID, masterID string) (*providers.CalendarEvent, error) {
	if !calendarIDPattern.MatchString(masterID) {
		return nil, invalidCalendarIDError("event", masterID)
	}
	master, err := client.Me().Calendars().ByCalendarId(calendarID).Events().ByEventId(masterID).Get(ctx,
		&users.ItemCalendarsItemEventsEventItemRequestBuilderGetRequestConfiguration{
			QueryParameters: &users.ItemCalendarsItemEventsEventItemRequestBuilderGetQueryParameters{
				Select: calendarEventSelect,
			},
		})
	if err != nil {
		return nil, err
	}
	event, err := eventFromGraph(master)
	if err != nil {
		return nil, err
	}
	return &event, nil
}

// graphTime renders an instant the way Graph expects it: a wall-clock value with
// no offset plus the zone that value belongs to. An all-day event is always sent
// as UTC midnight, because Graph interprets the date part in the named zone and a
// zone shift would move the day. The zone is canonicalised here, at the edge of
// the request: a Windows label stored by an earlier read, or a TZID of a feed, is
// not a value Graph reliably reads back, and the graphEventTime round trip only
// works when the label it wrote is one the runtime can load again.
func graphTime(moment time.Time, allDay bool, zone string) (string, string) {
	if allDay {
		return moment.UTC().Format("2006-01-02T15:04:05"), "UTC"
	}
	wall, name := providers.InZone(moment, zone)
	return wall.Format("2006-01-02T15:04:05"), name
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

// freeBusyFromGraph reads Graph's `showAs` one to one. An unset value is busy,
// which is Graph's own default, and `unknown` is treated the same way so an event
// never silently stops blocking the calendar.
func freeBusyFromGraph(showAs *graphmodels.FreeBusyStatus) providers.FreeBusy {
	if showAs == nil {
		return providers.FreeBusyBusy
	}
	switch *showAs {
	case graphmodels.FREE_FREEBUSYSTATUS:
		return providers.FreeBusyFree
	case graphmodels.TENTATIVE_FREEBUSYSTATUS:
		return providers.FreeBusyTentative
	case graphmodels.OOF_FREEBUSYSTATUS:
		return providers.FreeBusyOutOfOffice
	case graphmodels.WORKINGELSEWHERE_FREEBUSYSTATUS:
		return providers.FreeBusyWorkingElsewhere
	default:
		return providers.FreeBusyBusy
	}
}

// graphShowAs is the write half of the mapping. A state Graph cannot show is
// reported so the caller can decide, instead of being written as busy.
func graphShowAs(state providers.FreeBusy) (graphmodels.FreeBusyStatus, bool) {
	switch providers.FreeBusyName(string(state)) {
	case providers.FreeBusyFree:
		return graphmodels.FREE_FREEBUSYSTATUS, true
	case providers.FreeBusyTentative:
		return graphmodels.TENTATIVE_FREEBUSYSTATUS, true
	case providers.FreeBusyBusy:
		return graphmodels.BUSY_FREEBUSYSTATUS, true
	case providers.FreeBusyOutOfOffice:
		return graphmodels.OOF_FREEBUSYSTATUS, true
	case providers.FreeBusyWorkingElsewhere:
		return graphmodels.WORKINGELSEWHERE_FREEBUSYSTATUS, true
	}
	return 0, false
}

// invalidCalendarIDError reports a calendar or event id this provider refuses to
// put into a request. It carries the sentinel the handlers map to 400.
func invalidCalendarIDError(kind, value string) error {
	return fmt.Errorf("%w: microsoft calendar %s %q", providers.ErrInvalidIdentifier, kind, value)
}

func boolValue(value *bool) bool {
	return value != nil && *value
}
