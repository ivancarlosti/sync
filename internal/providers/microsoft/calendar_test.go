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

// mustPayload builds the body of a write, failing the test when the rule cannot be
// represented.
func mustPayload(t *testing.T, input providers.CalendarEventInput) graphmodels.Eventable {
	t.Helper()
	payload, err := eventPayload(input)
	if err != nil {
		t.Fatalf("eventPayload() error = %v", err)
	}
	return payload
}

// TestEventPayloadWritesTheFreeBusyFlagOnlyWhenAsked keeps a plain copy from
// flattening a destination event the operator marked as out-of-office or
// tentative into a plain busy one, and pins the one-to-one mapping of the five
// states Graph can show.
func TestEventPayloadWritesTheFreeBusyFlagOnlyWhenAsked(t *testing.T) {
	start := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	input := providers.CalendarEventInput{
		Summary:  "Standup",
		Start:    start,
		End:      start.Add(time.Hour),
		TimeZone: "UTC",
	}
	if got := mustPayload(t, input).GetShowAs(); got != nil {
		t.Fatalf("showAs = %v, want it left out", *got)
	}

	cases := []struct {
		state providers.FreeBusy
		want  graphmodels.FreeBusyStatus
	}{
		{providers.FreeBusyFree, graphmodels.FREE_FREEBUSYSTATUS},
		{providers.FreeBusyTentative, graphmodels.TENTATIVE_FREEBUSYSTATUS},
		{providers.FreeBusyBusy, graphmodels.BUSY_FREEBUSYSTATUS},
		{providers.FreeBusyOutOfOffice, graphmodels.OOF_FREEBUSYSTATUS},
		{providers.FreeBusyWorkingElsewhere, graphmodels.WORKINGELSEWHERE_FREEBUSYSTATUS},
	}
	for _, testCase := range cases {
		input.FreeBusy = testCase.state
		if got := mustPayload(t, input).GetShowAs(); got == nil || *got != testCase.want {
			t.Errorf("showAs of %q = %v, want %v", testCase.state, got, testCase.want)
		}
	}

	input.FreeBusy = "nonsense"
	if got := mustPayload(t, input).GetShowAs(); got != nil {
		t.Fatalf("an unknown state reached the payload: %v", *got)
	}
}

// TestEventPayloadWritesTheRuleOrRefusesIt is the guard of a series copy: a rule
// Graph can express becomes a patterned recurrence, and a rule it cannot express
// fails the write instead of creating a single event where a series was meant.
func TestEventPayloadWritesTheRuleOrRefusesIt(t *testing.T) {
	input := providers.CalendarEventInput{
		Summary:    "Standup",
		Start:      time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
		End:        time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC),
		TimeZone:   "UTC",
		Recurrence: []string{"RRULE:FREQ=WEEKLY;BYDAY=MO,WE"},
	}
	recurrence := mustPayload(t, input).GetRecurrence()
	if recurrence == nil || recurrence.GetPattern() == nil {
		t.Fatal("a weekly rule was not written")
	}
	pattern := recurrence.GetPattern()
	if kind := pattern.GetTypeEscaped(); kind == nil || *kind != graphmodels.WEEKLY_RECURRENCEPATTERNTYPE {
		t.Fatalf("pattern type = %v, want weekly", kind)
	}
	if days := pattern.GetDaysOfWeek(); len(days) != 2 || days[0] != graphmodels.MONDAY_DAYOFWEEK || days[1] != graphmodels.WEDNESDAY_DAYOFWEEK {
		t.Fatalf("days = %v", days)
	}

	// A rule with a part Graph has no pattern for is refused, not approximated.
	input.Recurrence = []string{"RRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1"}
	if _, err := eventPayload(input); err == nil {
		t.Fatal("a rule Graph cannot express was written anyway")
	}

	// An empty rule leaves an existing series alone.
	input.Recurrence = nil
	if got := mustPayload(t, input).GetRecurrence(); got != nil {
		t.Fatalf("recurrence = %v, want none", got)
	}
}

// TestEventFromGraphReadsTheFreeBusyFlag pins the one-to-one mapping of Graph's
// six-value showAs, that an unset one is busy, and that the zone of a listing is
// canonicalised on the way in.
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
	if got.FreeBusy != providers.FreeBusyFree {
		t.Fatalf("free/busy = %q, want free", got.FreeBusy)
	}
	if got.TimeZone != "Europe/Paris" {
		t.Fatalf("zone = %q, want Europe/Paris", got.TimeZone)
	}
	if !got.Start.Equal(time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("start = %s", got.Start)
	}

	cases := []struct {
		status graphmodels.FreeBusyStatus
		want   providers.FreeBusy
	}{
		{graphmodels.BUSY_FREEBUSYSTATUS, providers.FreeBusyBusy},
		{graphmodels.OOF_FREEBUSYSTATUS, providers.FreeBusyOutOfOffice},
		{graphmodels.TENTATIVE_FREEBUSYSTATUS, providers.FreeBusyTentative},
		{graphmodels.WORKINGELSEWHERE_FREEBUSYSTATUS, providers.FreeBusyWorkingElsewhere},
	}
	for _, testCase := range cases {
		value := testCase.status
		event, err := eventFromGraph(build(&value))
		if err != nil {
			t.Fatalf("eventFromGraph(%s) error = %v", value, err)
		}
		if event.FreeBusy != testCase.want {
			t.Errorf("showAs %s answered %q, want %q", value, event.FreeBusy, testCase.want)
		}
	}

	// Graph leaves the property out for an event created without one; that is
	// still a busy event.
	unset, err := eventFromGraph(build(nil))
	if err != nil {
		t.Fatalf("eventFromGraph(nil) error = %v", err)
	}
	if unset.FreeBusy != providers.FreeBusyBusy {
		t.Fatalf("free/busy = %q, want busy", unset.FreeBusy)
	}
}
