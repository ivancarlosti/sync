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

// TestGoogleEventPayloadOnlyOverridesTheFreeBusyFlagWhenAsked keeps an unrelated
// update from clearing a "free" marker the operator set on the destination.
func TestGoogleEventPayloadOnlyOverridesTheFreeBusyFlagWhenAsked(t *testing.T) {
	keep := googleEventPayload(providers.CalendarEventInput{Summary: "Standup"})
	if keep.Transparency != "" {
		t.Fatalf("transparency = %q, want an omitted value", keep.Transparency)
	}
	free := googleEventPayload(providers.CalendarEventInput{Summary: "Standup", Transparency: providers.TransparencyTransparent})
	if free.Transparency != "transparent" {
		t.Fatalf("transparency = %q, want transparent", free.Transparency)
	}
	busy := googleEventPayload(providers.CalendarEventInput{Summary: "Standup", Transparency: providers.TransparencyOpaque})
	if busy.Transparency != "opaque" {
		t.Fatalf("transparency = %q, want opaque", busy.Transparency)
	}
	unknown := googleEventPayload(providers.CalendarEventInput{Summary: "Standup", Transparency: "free"})
	if unknown.Transparency != "" {
		t.Fatalf("an unknown flag reached the payload: %q", unknown.Transparency)
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

// TestTransparencyFromGoogleReadsTheListedFlag pins the asymmetry of the API: it
// omits `transparency` for a normal event, so an absent value is busy rather than
// unknown.
func TestTransparencyFromGoogleReadsTheListedFlag(t *testing.T) {
	cases := map[string]string{
		"":            providers.TransparencyOpaque,
		"opaque":      providers.TransparencyOpaque,
		"transparent": providers.TransparencyTransparent,
		"Transparent": providers.TransparencyTransparent,
		"nonsense":    providers.TransparencyOpaque,
	}
	for value, want := range cases {
		if got := transparencyFromGoogle(value); got != want {
			t.Errorf("transparencyFromGoogle(%q) = %q, want %q", value, got, want)
		}
	}
}
