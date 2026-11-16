package google

import (
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

// TestGoogleEventPayloadWritesAZoneGoogleAccepts covers the failure this guard
// exists for: a zone label the calendar API does not recognise ("Invalid time zone
// definition for start time"). The label the engine holds may come from a Graph
// event or a feed TZID, so it is canonicalised on the way out and the moment is
// kept intact.
func TestGoogleEventPayloadWritesAZoneGoogleAccepts(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Berlin"); err != nil {
		t.Skipf("the runtime has no zone database: %v", err)
	}
	start := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	payload := googleEventPayload(providers.CalendarEventInput{
		Summary:  "Standup",
		Start:    start,
		End:      start.Add(time.Hour),
		TimeZone: "W. Europe Standard Time",
	})
	if payload.Start == nil || payload.End == nil {
		t.Fatalf("payload without times: %+v", payload)
	}
	if payload.Start.TimeZone != "Europe/Berlin" || payload.End.TimeZone != "Europe/Berlin" {
		t.Fatalf("zones = %q/%q, want Europe/Berlin", payload.Start.TimeZone, payload.End.TimeZone)
	}
	if payload.Start.DateTime != "2026-03-01T11:00:00+01:00" {
		t.Fatalf("start = %q, want the Berlin wall clock", payload.Start.DateTime)
	}
	parsed, err := time.Parse(time.RFC3339, payload.Start.DateTime)
	if err != nil || !parsed.Equal(start) {
		t.Fatalf("start = %q (%v), want the same instant as %s", payload.Start.DateTime, err, start)
	}

	unknown := googleEventPayload(providers.CalendarEventInput{
		Summary:  "Standup",
		Start:    start,
		End:      start.Add(time.Hour),
		TimeZone: "Mars/Olympus",
	})
	if unknown.Start == nil || unknown.Start.TimeZone != "UTC" || unknown.Start.DateTime != "2026-03-01T10:00:00Z" {
		t.Fatalf("an unknown zone was not rendered as UTC: %+v", unknown.Start)
	}
}

// TestGoogleEventPayloadSendsAnAllDayEventAsADate pins the shape of an all-day
// write: a DATE, never a midnight instant, which a reader in another zone would
// show on the previous day.
func TestGoogleEventPayloadSendsAnAllDayEventAsADate(t *testing.T) {
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	payload := googleEventPayload(providers.CalendarEventInput{
		Summary: "Holiday",
		Start:   start,
		End:     start.AddDate(0, 0, 1),
		AllDay:  true,
		// The zone of an all-day event is irrelevant to the DATE it is sent as,
		// but a Windows label must not leak into the payload either.
		TimeZone: "W. Europe Standard Time",
	})
	if payload.Start == nil || payload.Start.Date != "2026-02-01" || payload.Start.DateTime != "" {
		t.Fatalf("start = %+v, want a DATE", payload.Start)
	}
	if payload.End == nil || payload.End.Date != "2026-02-02" {
		t.Fatalf("end = %+v, want the exclusive next day", payload.End)
	}
	if payload.Start.TimeZone != "" {
		t.Fatalf("an all-day event carried a zone: %q", payload.Start.TimeZone)
	}
}

// TestGoogleEventPayloadOnlyWritesTheFreeBusyStateWhenAsked keeps an unrelated
// update from clearing a marker the operator set on the destination calendar, and
// pins how each of the five states is expressed: Google spreads them over
// `transparency`, `status` and `eventType`.
func TestGoogleEventPayloadOnlyWritesTheFreeBusyStateWhenAsked(t *testing.T) {
	keep := googleEventPayload(providers.CalendarEventInput{Summary: "Standup"})
	if keep.Transparency != "" || keep.Status != "" || keep.EventType != "" {
		t.Fatalf("a write without a state carried %+v", keep)
	}
	free := googleEventPayload(providers.CalendarEventInput{Summary: "Standup", FreeBusy: providers.FreeBusyFree})
	if free.Transparency != "transparent" {
		t.Fatalf("transparency = %q, want transparent", free.Transparency)
	}
	busy := googleEventPayload(providers.CalendarEventInput{Summary: "Standup", FreeBusy: providers.FreeBusyBusy})
	if busy.Transparency != "opaque" {
		t.Fatalf("transparency = %q, want opaque", busy.Transparency)
	}
	tentative := googleEventPayload(providers.CalendarEventInput{Summary: "Standup", FreeBusy: providers.FreeBusyTentative})
	if tentative.Status != "tentative" {
		t.Fatalf("status = %q, want tentative", tentative.Status)
	}
	away := googleEventPayload(providers.CalendarEventInput{Summary: "Standup", FreeBusy: providers.FreeBusyOutOfOffice})
	if away.EventType != "outOfOffice" {
		t.Fatalf("event type = %q, want outOfOffice", away.EventType)
	}
	cancelled := googleEventPayload(providers.CalendarEventInput{Summary: "Standup", FreeBusy: providers.FreeBusyCancelled})
	if cancelled.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", cancelled.Status)
	}
	unknown := googleEventPayload(providers.CalendarEventInput{Summary: "Standup", FreeBusy: "nonsense"})
	if unknown.Transparency != "" || unknown.Status != "" {
		t.Fatalf("an unknown state reached the payload: %+v", unknown)
	}
}

// TestGoogleEventPayloadWritesTheRuleAsTheApiExpectsIt pins the two shapes the
// rule of a series reaches the API in: Google wants the property name in front,
// which a Graph master does not carry.
func TestGoogleEventPayloadWritesTheRuleAsTheApiExpectsIt(t *testing.T) {
	payload := googleEventPayload(providers.CalendarEventInput{
		Summary:    "Standup",
		Recurrence: []string{"FREQ=WEEKLY;BYDAY=MO", "RRULE:FREQ=DAILY"},
	})
	if len(payload.Recurrence) != 2 || payload.Recurrence[0] != "RRULE:FREQ=WEEKLY;BYDAY=MO" || payload.Recurrence[1] != "RRULE:FREQ=DAILY" {
		t.Fatalf("recurrence = %v", payload.Recurrence)
	}
	// A write that does not carry a rule leaves an existing one in place.
	if got := googleEventPayload(providers.CalendarEventInput{Summary: "Standup"}); got.Recurrence != nil {
		t.Fatalf("recurrence = %v, want none", got.Recurrence)
	}
}

// TestGoogleEventTimeFromReportsACanonicalZone is the other half of the guard: the
// zone a listing carries travels to the destination, so it must already be a name
// the destination accepts.
func TestGoogleEventTimeFromReportsACanonicalZone(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Berlin"); err != nil {
		t.Skipf("the runtime has no zone database: %v", err)
	}
	moment, allDay, zone, err := googleEventTimeFrom(&googleEventTime{
		DateTime: "2026-03-01T11:00:00+01:00",
		TimeZone: "W. Europe Standard Time",
	})
	if err != nil {
		t.Fatalf("googleEventTimeFrom() error = %v", err)
	}
	if allDay {
		t.Fatal("a date-time was reported as an all-day event")
	}
	if zone != "Europe/Berlin" {
		t.Fatalf("zone = %q, want Europe/Berlin", zone)
	}
	if !moment.Equal(time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("moment = %s", moment)
	}

	// An unmappable label is dropped instead of echoed: the instant is what
	// survives, and the destination decides the wall clock.
	_, _, unknown, err := googleEventTimeFrom(&googleEventTime{DateTime: "2026-03-01T10:00:00Z", TimeZone: "Mars/Olympus"})
	if err != nil {
		t.Fatalf("googleEventTimeFrom() error = %v", err)
	}
	if unknown != "" {
		t.Fatalf("zone = %q, want an empty answer", unknown)
	}
}

// TestFreeBusyFromGoogleReadsTheListedState pins the asymmetry of the API: it
// omits `transparency` for a normal event, so an absent value is busy rather than
// unknown, and the other states live on `status` and `eventType`.
func TestFreeBusyFromGoogleReadsTheListedState(t *testing.T) {
	cases := []struct {
		raw  googleEvent
		want providers.FreeBusy
	}{
		{googleEvent{}, providers.FreeBusyBusy},
		{googleEvent{Transparency: "opaque"}, providers.FreeBusyBusy},
		{googleEvent{Transparency: "transparent"}, providers.FreeBusyFree},
		{googleEvent{Transparency: "Transparent"}, providers.FreeBusyFree},
		{googleEvent{Transparency: "nonsense"}, providers.FreeBusyBusy},
		{googleEvent{Status: "tentative"}, providers.FreeBusyTentative},
		{googleEvent{Status: "cancelled"}, providers.FreeBusyCancelled},
		{googleEvent{EventType: "outOfOffice"}, providers.FreeBusyOutOfOffice},
		{googleEvent{EventType: "focusTime"}, providers.FreeBusyBusy},
	}
	for _, testCase := range cases {
		if got := freeBusyFromGoogle(testCase.raw); got != testCase.want {
			t.Errorf("freeBusyFromGoogle(%+v) = %q, want %q", testCase.raw, got, testCase.want)
		}
	}
}

// TestGoogleEventFromClassifiesASeries pins the read model the engine pairs on:
// Google returns the master and the occurrences somebody edited, and the start
// each exception replaced is what names it.
func TestGoogleEventFromClassifiesASeries(t *testing.T) {
	master, err := googleEventFrom(googleEvent{
		ID:         "master",
		ICalUID:    "series@example.com",
		Start:      &googleEventTime{DateTime: "2026-03-02T09:00:00Z", TimeZone: "UTC"},
		End:        &googleEventTime{DateTime: "2026-03-02T09:30:00Z", TimeZone: "UTC"},
		Recurrence: []string{"RRULE:FREQ=WEEKLY;BYDAY=MO"},
	})
	if err != nil {
		t.Fatalf("googleEventFrom() error = %v", err)
	}
	if master.Kind != providers.EventKindMaster || !master.Recurring {
		t.Fatalf("kind = %q recurring = %t, want a master", master.Kind, master.Recurring)
	}
	if len(master.Recurrence) != 1 {
		t.Fatalf("recurrence = %v", master.Recurrence)
	}

	exception, err := googleEventFrom(googleEvent{
		ID:               "master_20260309T090000Z",
		ICalUID:          "series@example.com",
		Status:           "cancelled",
		Start:            &googleEventTime{DateTime: "2026-03-09T09:00:00Z", TimeZone: "UTC"},
		End:              &googleEventTime{DateTime: "2026-03-09T09:30:00Z", TimeZone: "UTC"},
		RecurringEventID: "master",
		OriginalStart:    &googleEventTime{DateTime: "2026-03-09T09:00:00Z", TimeZone: "UTC"},
	})
	if err != nil {
		t.Fatalf("googleEventFrom() error = %v", err)
	}
	if exception.Kind != providers.EventKindException {
		t.Fatalf("kind = %q, want an exception", exception.Kind)
	}
	if exception.SeriesID != "master" || !exception.Cancelled {
		t.Fatalf("series = %q cancelled = %t", exception.SeriesID, exception.Cancelled)
	}
	if !exception.OriginalStart.Equal(time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("original start = %s", exception.OriginalStart)
	}
	if exception.FreeBusy != providers.FreeBusyCancelled {
		t.Fatalf("free/busy = %q, want cancelled", exception.FreeBusy)
	}
}
