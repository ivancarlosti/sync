package recurrence

import (
	"errors"
	"testing"
	"time"

	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

// TestParseReadsThePartsGraphCanCarry pins the accepted vocabulary and the
// defaults: an INTERVAL that is absent is one, which is what an RRULE means.
func TestParseReadsThePartsGraphCanCarry(t *testing.T) {
	rule, exdates, err := Parse([]string{"RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE;COUNT=10"})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if rule.Freq != "WEEKLY" || rule.Interval != 2 || rule.Count != 10 {
		t.Fatalf("rule = %+v", rule)
	}
	if len(rule.ByDay) != 2 || rule.ByDay[0].Day != "MO" || rule.ByDay[1].Day != "WE" {
		t.Fatalf("by day = %+v", rule.ByDay)
	}
	if len(exdates) != 0 {
		t.Fatalf("exdates = %v", exdates)
	}

	// The value of an .ics property arrives without the property name.
	plain, _, err := Parse([]string{"FREQ=DAILY"})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if plain.Interval != 1 || plain.Freq != "DAILY" {
		t.Fatalf("rule = %+v", plain)
	}
}

// TestParseRefusesWhatGraphHasNoPatternFor is the guard that keeps a series from
// being copied as something else: every refusal names the part that made it
// impossible.
func TestParseRefusesWhatGraphHasNoPatternFor(t *testing.T) {
	cases := []string{
		"RRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1",
		"RRULE:FREQ=YEARLY;BYYEARDAY=100",
		"RRULE:FREQ=WEEKLY;BYWEEKNO=20",
		"RDATE;TZID=Europe/Madrid:20260302T090000",
		"EXRULE:FREQ=WEEKLY",
		"RRULE:FREQ=HOURLY",
		"RRULE:FREQ=WEEKLY;COUNT=5;UNTIL=20260401T000000Z",
		"RRULE:FREQ=WEEKLY;BYDAY=XY",
		"RRULE:FREQ=WEEKLY;NONSENSE=1",
	}
	for _, line := range cases {
		if _, _, err := Parse([]string{line}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Parse(%q) error = %v, want ErrUnsupported", line, err)
		}
	}
}

// TestToGraphBuildsThePatternOfEachFrequency covers the six patterns Graph can
// express, which is the whole surface of a series copy towards Microsoft.
func TestToGraphBuildsThePatternOfEachFrequency(t *testing.T) {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		rule     string
		kind     models.RecurrencePatternType
		interval int32
	}{
		{"RRULE:FREQ=DAILY;INTERVAL=3", models.DAILY_RECURRENCEPATTERNTYPE, 3},
		{"RRULE:FREQ=WEEKLY;BYDAY=MO", models.WEEKLY_RECURRENCEPATTERNTYPE, 1},
		{"RRULE:FREQ=MONTHLY;BYMONTHDAY=15", models.ABSOLUTEMONTHLY_RECURRENCEPATTERNTYPE, 1},
		{"RRULE:FREQ=MONTHLY;BYDAY=-1FR", models.RELATIVEMONTHLY_RECURRENCEPATTERNTYPE, 1},
		{"RRULE:FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=15", models.ABSOLUTEYEARLY_RECURRENCEPATTERNTYPE, 1},
		{"RRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1TH", models.RELATIVEYEARLY_RECURRENCEPATTERNTYPE, 1},
	}
	for _, testCase := range cases {
		value, err := ToGraph([]string{testCase.rule}, start, "Europe/Madrid")
		if err != nil {
			t.Fatalf("ToGraph(%q) error = %v", testCase.rule, err)
		}
		pattern := value.GetPattern()
		if kind := pattern.GetTypeEscaped(); kind == nil || *kind != testCase.kind {
			t.Errorf("ToGraph(%q) type = %v, want %v", testCase.rule, kind, testCase.kind)
		}
		if interval := pattern.GetInterval(); interval == nil || *interval != testCase.interval {
			t.Errorf("ToGraph(%q) interval = %v", testCase.rule, interval)
		}
		if zone := value.GetRangeEscaped().GetRecurrenceTimeZone(); zone == nil || *zone != "Europe/Madrid" {
			t.Errorf("ToGraph(%q) zone = %v", testCase.rule, zone)
		}
	}
}

// TestToGraphCarriesTheEndOfTheSeries covers the three ranges: no end, a count and
// a date, which is what keeps a copied series from running longer than the
// original.
func TestToGraphCarriesTheEndOfTheSeries(t *testing.T) {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	unbounded, err := ToGraph([]string{"RRULE:FREQ=DAILY"}, start, "UTC")
	if err != nil {
		t.Fatalf("ToGraph() error = %v", err)
	}
	if kind := unbounded.GetRangeEscaped().GetTypeEscaped(); kind == nil || *kind != models.NOEND_RECURRENCERANGETYPE {
		t.Fatalf("range type = %v, want noEnd", kind)
	}
	numbered, err := ToGraph([]string{"RRULE:FREQ=DAILY;COUNT=7"}, start, "UTC")
	if err != nil {
		t.Fatalf("ToGraph() error = %v", err)
	}
	if count := numbered.GetRangeEscaped().GetNumberOfOccurrences(); count == nil || *count != 7 {
		t.Fatalf("count = %v, want 7", count)
	}
	bounded, err := ToGraph([]string{"RRULE:FREQ=DAILY;UNTIL=20260401"}, start, "UTC")
	if err != nil {
		t.Fatalf("ToGraph() error = %v", err)
	}
	if end := bounded.GetRangeEscaped().GetEndDate(); end == nil || end.String() != "2026-04-01" {
		t.Fatalf("end = %v, want 2026-04-01", end)
	}
}

// TestToGraphRefusesAnExceptionDate is the one lossy corner the engine must report
// rather than approximate: Graph expresses a skipped occurrence as an exception,
// not as part of the rule.
func TestToGraphRefusesAnExceptionDate(t *testing.T) {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	_, err := ToGraph([]string{"RRULE:FREQ=WEEKLY;BYDAY=MO", "EXDATE:20260316T090000Z"}, start, "UTC")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ToGraph() error = %v, want ErrUnsupported", err)
	}
}

// TestFromGraphRoundTripsTheRule is what makes the fingerprint of a series stable
// across a copy: the rule read from Graph is the canonical line the source had.
func TestFromGraphRoundTripsTheRule(t *testing.T) {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	for _, rule := range []string{
		"RRULE:FREQ=DAILY",
		"RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE",
		"RRULE:FREQ=MONTHLY;BYDAY=2TU",
		"RRULE:FREQ=MONTHLY;BYMONTHDAY=15;COUNT=5",
		"RRULE:FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=15;UNTIL=20290101T000000Z",
	} {
		value, err := ToGraph([]string{rule}, start, "UTC")
		if err != nil {
			t.Fatalf("ToGraph(%q) error = %v", rule, err)
		}
		lines, err := FromGraph(value)
		if err != nil {
			t.Fatalf("FromGraph(%q) error = %v", rule, err)
		}
		if len(lines) != 1 {
			t.Fatalf("FromGraph(%q) = %v", rule, lines)
		}
		back, _, err := Parse(lines)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", lines[0], err)
		}
		original, _, err := Parse([]string{rule})
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", rule, err)
		}
		if back.Freq != original.Freq || back.Interval != original.Interval || back.Count != original.Count {
			t.Errorf("round trip of %q answered %+v", rule, back)
		}
	}
}

// TestToGraphDefaultsAWeeklyRuleToTheStartDay covers the API shape a rule may
// omit: a weekly rule without BYDAY recurs on the weekday of its start.
func TestToGraphDefaultsAWeeklyRuleToTheStartDay(t *testing.T) {
	start := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC) // a Wednesday
	value, err := ToGraph([]string{"RRULE:FREQ=WEEKLY"}, start, "UTC")
	if err != nil {
		t.Fatalf("ToGraph() error = %v", err)
	}
	days := value.GetPattern().GetDaysOfWeek()
	if len(days) != 1 || days[0] != models.WEDNESDAY_DAYOFWEEK {
		t.Fatalf("days = %v, want Wednesday", days)
	}
}
