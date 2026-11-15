package microsoft

import (
	"testing"
	"time"

	graphmodels "github.com/microsoftgraph/msgraph-sdk-go/models"

	"github.com/ivancarlosti/sync/internal/providers"
)

// TestGraphTimeCanonicalisesTheZone pins the label Graph is sent. A zone name read
// from a feed or from Google is not a Windows name, and Graph's DateTimeTimeZone
// only round trips when the name it wrote is one the runtime can load again — which
// is what the engine does on the next run.
func TestGraphTimeCanonicalisesTheZone(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Paris"); err != nil {
		t.Skipf("the runtime has no zone database: %v", err)
	}
	moment := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

	wall, zone := graphTime(moment, false, "Romance Standard Time")
	if zone != "Europe/Paris" {
		t.Fatalf("zone = %q, want Europe/Paris", zone)
	}
	if wall != "2026-03-01T11:00:00" {
		t.Fatalf("wall clock = %q, want 11:00 local", wall)
	}

	// UTC renders the same instant and is a name Graph accepts.
	utc, utcZone := graphTime(moment, false, "Europe/London")
	if utcZone != "Europe/London" || utc != "2026-03-01T10:00:00" {
		t.Fatalf("London = %q in %q", utc, utcZone)
	}

	// An all-day event is sent as UTC midnight: Graph reads the date part in the
	// named zone, so a zone shift would move the day.
	allDay, allDayZone := graphTime(moment, true, "America/New_York")
	if allDay != "2026-03-01T10:00:00" || allDayZone != "UTC" {
		t.Fatalf("all-day = %q in %q", allDay, allDayZone)
	}

	// A label nothing can resolve keeps the instant and names UTC, which is the
	// only honest answer Graph will read.
	fallback, fallbackZone := graphTime(moment, false, "Mars/Olympus")
	if fallback != "2026-03-01T10:00:00" || fallbackZone != "UTC" {
		t.Fatalf("unknown zone = %q in %q", fallback, fallbackZone)
	}
}

// TestEventPayloadWritesTheFreeBusyFlagOnlyWhenAsked keeps a plain copy from
// flattening a destination event the operator marked as out-of-office or
// tentative into a plain busy one.
func TestEventPayloadWritesTheFreeBusyFlagOnlyWhenAsked(t *testing.T) {
	start := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	input := providers.CalendarEventInput{
		Summary:  "Standup",
		Start:    start,
		End:      start.Add(time.Hour),
		TimeZone: "UTC",
	}
	if got := eventPayload(input).GetShowAs(); got != nil {
		t.Fatalf("showAs = %v, want it left out", *got)
	}

	input.Transparency = providers.TransparencyTransparent
	if got := eventPayload(input).GetShowAs(); got == nil || *got != graphmodels.FREE_FREEBUSYSTATUS {
		t.Fatalf("showAs = %v, want free", got)
	}

	input.Transparency = providers.TransparencyOpaque
	if got := eventPayload(input).GetShowAs(); got == nil || *got != graphmodels.BUSY_FREEBUSYSTATUS {
		t.Fatalf("showAs = %v, want busy", got)
	}

	input.Transparency = "free"
	if got := eventPayload(input).GetShowAs(); got != nil {
		t.Fatalf("an unknown flag reached the payload: %v", *got)
	}
}

// TestEventFromGraphReadsTheFreeBusyFlag pins how Graph's six-value showAs is
// collapsed onto the two values the engine shares with Google, and that the zone of
// a listing is canonicalised on the way in.
func TestEventFromGraphReadsTheFreeBusyFlag(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Paris"); err != nil {
		t.Skipf("the runtime has no zone database: %v", err)
	}
	build := func(showAs *graphmodels.FreeBusyStatus) graphmodels.Eventable {
		event := graphmodels.NewEvent()
		event.SetId(ptr("A1"))
		event.SetICalUId(ptr("event@example.com"))
		event.SetSubject(ptr("Standup"))
		event.SetIsAllDay(ptr(false))
		start := graphmodels.NewDateTimeTimeZone()
		start.SetDateTime(ptr("2026-03-01T11:00:00"))
		start.SetTimeZone(ptr("Romance Standard Time"))
		event.SetStart(start)
		end := graphmodels.NewDateTimeTimeZone()
		end.SetDateTime(ptr("2026-03-01T12:00:00"))
		end.SetTimeZone(ptr("Romance Standard Time"))
		event.SetEnd(end)
		event.SetShowAs(showAs)
		return event
	}

	free := graphmodels.FREE_FREEBUSYSTATUS
	got, err := eventFromGraph(build(&free))
	if err != nil {
		t.Fatalf("eventFromGraph() error = %v", err)
	}
	if got.Transparency != providers.TransparencyTransparent {
		t.Fatalf("transparency = %q, want transparent", got.Transparency)
	}
	if got.TimeZone != "Europe/Paris" {
		t.Fatalf("zone = %q, want Europe/Paris", got.TimeZone)
	}
	if !got.Start.Equal(time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("start = %s", got.Start)
	}

	for _, status := range []graphmodels.FreeBusyStatus{
		graphmodels.BUSY_FREEBUSYSTATUS,
		graphmodels.OOF_FREEBUSYSTATUS,
		graphmodels.TENTATIVE_FREEBUSYSTATUS,
		graphmodels.WORKINGELSEWHERE_FREEBUSYSTATUS,
	} {
		value := status
		event, err := eventFromGraph(build(&value))
		if err != nil {
			t.Fatalf("eventFromGraph(%s) error = %v", value, err)
		}
		if event.Transparency != providers.TransparencyOpaque {
			t.Errorf("showAs %s answered %q, want opaque", value, event.Transparency)
		}
	}

	// Graph leaves the property out for an event created without one; that is
	// still a busy event.
	unset, err := eventFromGraph(build(nil))
	if err != nil {
		t.Fatalf("eventFromGraph(nil) error = %v", err)
	}
	if unset.Transparency != providers.TransparencyOpaque {
		t.Fatalf("transparency = %q, want opaque", unset.Transparency)
	}
}
