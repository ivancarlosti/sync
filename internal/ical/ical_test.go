package ical

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// sampleFeed exercises the forms the parser has to accept: a folded line, an
// escaped text value, a UTC time, an all-day date, a TZID, a duration, a
// recurrence and a cancelled event.
const sampleFeed = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example//EN
BEGIN:VEVENT
UID:one@example.com
SUMMARY:Standup with a very long
  title
DESCRIPTION:Line one\nLine two\, with a comma
LOCATION:Room 1\; floor 2
DTSTART:20260115T090000Z
DTEND:20260115T091500Z
LAST-MODIFIED:20260110T120000Z
ORGANIZER;CN=Ana:mailto:ana@example.com
ATTENDEE:mailto:bo@example.com
ATTENDEE:mailto:cy@example.com
END:VEVENT
BEGIN:VEVENT
UID:two@example.com
SUMMARY:Holiday
DTSTART;VALUE=DATE:20260201
DTEND;VALUE=DATE:20260202
END:VEVENT
BEGIN:VEVENT
UID:three@example.com
SUMMARY:Local meeting
DTSTART;TZID=Europe/Madrid:20260301T100000
DURATION:PT45M
END:VEVENT
BEGIN:VEVENT
UID:four@example.com
SUMMARY:Weekly series
DTSTART:20260401T080000Z
RRULE:FREQ=WEEKLY;COUNT=4
END:VEVENT
BEGIN:VEVENT
UID:five@example.com
SUMMARY:Cancelled thing
STATUS:CANCELLED
DTSTART:20260501T080000Z
DTEND:20260501T090000Z
END:VEVENT
END:VCALENDAR
`

func TestParseReadsTheEventShapesAFeedUses(t *testing.T) {
	events, err := Parse([]byte(sampleFeed), Options{})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("got %d events, want 5", len(events))
	}

	first := events[0]
	if first.UID != "one@example.com" {
		t.Fatalf("uid = %q", first.UID)
	}
	if first.Summary != "Standup with a very long title" {
		t.Fatalf("folded summary = %q", first.Summary)
	}
	if first.Description != "Line one\nLine two, with a comma" {
		t.Fatalf("description = %q", first.Description)
	}
	if first.Location != "Room 1; floor 2" {
		t.Fatalf("location = %q", first.Location)
	}
	if !first.Start.Equal(time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("start = %s", first.Start)
	}
	if !first.End.Equal(time.Date(2026, 1, 15, 9, 15, 0, 0, time.UTC)) {
		t.Fatalf("end = %s", first.End)
	}
	if !first.UpdatedAt.Equal(time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("updated = %s", first.UpdatedAt)
	}
	if first.Organizer != "ana@example.com" {
		t.Fatalf("organizer = %q", first.Organizer)
	}
	if strings.Join(first.Attendees, ",") != "bo@example.com,cy@example.com" {
		t.Fatalf("attendees = %v", first.Attendees)
	}

	allDay := events[1]
	if !allDay.AllDay {
		t.Fatal("an all-day event was not reported as one")
	}
	if !allDay.Start.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) || !allDay.End.Equal(time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("all-day range = %s .. %s", allDay.Start, allDay.End)
	}

	local := events[2]
	madrid, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Skipf("the runtime has no zone database: %v", err)
	}
	if local.TimeZone != "Europe/Madrid" {
		t.Fatalf("time zone = %q", local.TimeZone)
	}
	if !local.Start.Equal(time.Date(2026, 3, 1, 10, 0, 0, 0, madrid)) {
		t.Fatalf("tzid start = %s", local.Start)
	}
	if local.End.Sub(local.Start) != 45*time.Minute {
		t.Fatalf("duration = %s, want 45m", local.End.Sub(local.Start))
	}

	if !events[3].Recurring {
		t.Fatal("a series master was not marked recurring")
	}
	if !events[4].Cancelled {
		t.Fatal("STATUS:CANCELLED was not read")
	}
}

func TestParseDropsTheEventsOutsideTheWindow(t *testing.T) {
	events, err := Parse([]byte(sampleFeed), Options{
		WindowStart: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(events) != 1 || events[0].UID != "two@example.com" {
		t.Fatalf("got %+v, want the all-day event of February only", events)
	}
}

func TestParseRejectsAPayloadThatIsNotACalendar(t *testing.T) {
	if _, err := Parse([]byte("<html>nope</html>"), Options{}); !errors.Is(err, ErrInvalidFeed) {
		t.Fatalf("error = %v, want ErrInvalidFeed", err)
	}
}

func TestParseSkipsAnEventWithoutAUID(t *testing.T) {
	feed := "BEGIN:VCALENDAR\nBEGIN:VEVENT\nDTSTART:20260101T000000Z\nEND:VEVENT\nEND:VCALENDAR\n"
	events, err := Parse([]byte(feed), Options{})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %+v, want no event", events)
	}
}

func TestParseDurationHandlesTheSupportedUnits(t *testing.T) {
	cases := map[string]time.Duration{
		"PT45M":   45 * time.Minute,
		"PT1H30M": 90 * time.Minute,
		"P1D":     24 * time.Hour,
		"P2W":     14 * 24 * time.Hour,
		"P1DT2H":  26 * time.Hour,
	}
	for value, want := range cases {
		got, err := parseDuration(value)
		if err != nil || got != want {
			t.Fatalf("parseDuration(%q) = %s, %v; want %s", value, got, err, want)
		}
	}
	for _, value := range []string{"", "P1M", "1H", "PT"} {
		if _, err := parseDuration(value); err == nil {
			t.Fatalf("parseDuration(%q) accepted an invalid value", value)
		}
	}
}

func TestValidateURLRefusesWhatTheFetcherMustNotDial(t *testing.T) {
	if _, err := ValidateURL("https://calendar.example.com/feed.ics", false); err != nil {
		t.Fatalf("a public URL was refused: %v", err)
	}
	for _, target := range []string{
		"file:///etc/passwd",
		"https://user:secret@calendar.example.com/feed.ics",
		"https://127.0.0.1/feed.ics",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/feed.ics",
		"/relative.ics",
	} {
		if _, err := ValidateURL(target, false); err == nil {
			t.Fatalf("ValidateURL(%q) accepted a target the fetcher must refuse", target)
		}
	}
	if _, err := ValidateURL("http://127.0.0.1:8080/feed.ics", true); err != nil {
		t.Fatalf("an operator-allowed private target was refused: %v", err)
	}
}

// TestFeedURLPatternPinsTheShapeOfAStoredSubscription covers the allow-list the
// fetcher applies to the exact string it hands to the HTTP client: the scheme,
// the host, the port and the absence of credentials, spaces and control
// characters. ValidateURL answers the same questions for a different reason (a
// stored value has to be saveable), so both are asserted here.
func TestFeedURLPatternPinsTheShapeOfAStoredSubscription(t *testing.T) {
	accepted := []string{
		"https://calendar.example.com/feed.ics",
		"https://calendar.example.com:8443/feed.ics?token=abc&size=large",
		"http://127.0.0.1:8080/feed.ics",
		"https://[2001:db8::1]/feed.ics",
		"https://example.com",
	}
	for _, target := range accepted {
		if !feedURLPattern.MatchString(target) {
			t.Fatalf("feedURLPattern refused %q", target)
		}
	}
	refused := []string{
		"",
		"webcal://calendar.example.com/feed.ics",
		"file:///etc/passwd",
		"/relative.ics",
		"https://user:secret@calendar.example.com/feed.ics",
		"https://calendar.example.com/feed.ics\r\nX-Injected: 1",
		"https://calendar.example.com/fe ed.ics",
	}
	for _, target := range refused {
		if feedURLPattern.MatchString(target) {
			t.Fatalf("feedURLPattern accepted %q", target)
		}
	}
}

// TestFetchRefusesAShapeItMustNotDial keeps the guard honest at the call site: a
// URL that only the pattern refuses is reported instead of being dialled, and
// the save path (ValidateURL) refuses it for the same reason.
func TestFetchRefusesAShapeItMustNotDial(t *testing.T) {
	for _, target := range []string{
		"https://calendar.example.com/fe ed.ics",
		"https://calendar.example.com/feed.ics\r\nX-Injected: 1",
	} {
		if _, err := ValidateURL(target, false); !errors.Is(err, ErrInvalidURL) {
			t.Fatalf("ValidateURL(%q) error = %v, want %v", target, err, ErrInvalidURL)
		}
		if _, err := Fetch(context.Background(), Subscription{URL: target}); !errors.Is(err, ErrInvalidURL) {
			t.Fatalf("Fetch(%q) error = %v, want %v", target, err, ErrInvalidURL)
		}
	}
}
