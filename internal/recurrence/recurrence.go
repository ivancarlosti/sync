// Package recurrence translates the recurrence of a calendar series between the
// two vocabularies the engine meets: the RFC 5545 lines Google and .ics feeds
// exchange, and the `patternedRecurrence` object Microsoft Graph stores.
//
// The translation is deliberately strict. Graph can express six patterns, and an
// RRULE it cannot represent (BYSETPOS, BYYEARDAY, BYWEEKNO, an RDATE-only rule, an
// EXRULE) is refused with ErrUnsupported instead of being approximated into a
// series that quietly differs from the original. The engine turns that refusal
// into a reported skip, which is the behaviour an operator can act on.
package recurrence

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/microsoft/kiota-abstractions-go/serialization"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

// ErrUnsupported marks a rule that cannot be represented on the other side. It is
// wrapped with the reason, so the evidence a run reports names the property that
// made the series impossible to copy.
var ErrUnsupported = errors.New("recurrence: the rule cannot be represented by the destination")

// maxCount bounds COUNT and INTERVAL, matching Graph's own validation. A larger
// value is a malformed rule, not a series worth copying.
const maxCount = 999

// Rule is one RRULE, normalised. FREQ is one of DAILY, WEEKLY, MONTHLY, YEARLY.
type Rule struct {
	Freq     string
	Interval int
	Count    int
	Until    time.Time
	ByDay    []Weekday
	// ByMonthDay and ByMonth hold the values at the width the Graph pattern
	// stores (int32), so the translation never narrows them on the way out.
	// BYMONTHDAY is 1..31, or negative counting back from the end of the month;
	// BYMONTH is 1..12.
	ByMonthDay []int32
	ByMonth    []int32
	WeekStart  string
}

// Weekday is one BYDAY entry: a two-letter day with an optional ordinal, which is
// what makes "the second Tuesday of the month" expressible.
type Weekday struct {
	Day     string
	Ordinal int
}

// String renders the entry the way an RRULE spells it.
func (w Weekday) String() string {
	if w.Ordinal == 0 {
		return w.Day
	}
	return strconv.Itoa(w.Ordinal) + w.Day
}

// Parse reads the recurrence lines of a master. It answers the rule plus the set
// of EXDATE instants, and refuses anything Graph has no pattern for.
func Parse(lines []string) (*Rule, []time.Time, error) {
	var rule *Rule
	exdates := make([]time.Time, 0, 4)
	for _, line := range lines {
		value := strings.TrimSpace(line)
		if value == "" {
			continue
		}
		name, body := splitProperty(value)
		switch name {
		case "RRULE":
			if rule != nil {
				return nil, nil, fmt.Errorf("%w: more than one RRULE", ErrUnsupported)
			}
			parsed, err := parseRRULE(body)
			if err != nil {
				return nil, nil, err
			}
			rule = parsed
		case "EXDATE":
			moments, err := parseExDate(body)
			if err != nil {
				return nil, nil, err
			}
			exdates = append(exdates, moments...)
		case "RDATE":
			return nil, nil, fmt.Errorf("%w: an RDATE rule has no Graph pattern", ErrUnsupported)
		case "EXRULE":
			return nil, nil, fmt.Errorf("%w: an EXRULE has no Graph pattern", ErrUnsupported)
		default:
			return nil, nil, fmt.Errorf("%w: unknown recurrence property %q", ErrUnsupported, name)
		}
	}
	if rule == nil {
		return nil, nil, fmt.Errorf("%w: no RRULE", ErrUnsupported)
	}
	return rule, exdates, nil
}

// splitProperty separates a content line into its upper-cased name and its value,
// dropping parameters (the TZID of an EXDATE) the translation does not need.
func splitProperty(line string) (string, string) {
	if index := strings.IndexByte(line, ':'); index >= 0 {
		head, value := line[:index], line[index+1:]
		name := head
		if semi := strings.IndexByte(head, ';'); semi >= 0 {
			name = head[:semi]
		}
		return strings.ToUpper(strings.TrimSpace(name)), value
	}
	// A rule may already be written without the "RRULE" prefix (the value of an
	// .ics property), which is a FREQ= list.
	if strings.Contains(strings.ToUpper(line), "FREQ=") {
		return "RRULE", line
	}
	return strings.ToUpper(strings.TrimSpace(line)), ""
}

func parseRRULE(body string) (*Rule, error) {
	rule := &Rule{Interval: 1}
	seen := map[string]bool{}
	for _, part := range strings.Split(body, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("%w: %q is not a rule part", ErrUnsupported, part)
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if seen[key] {
			return nil, fmt.Errorf("%w: %s appears twice", ErrUnsupported, key)
		}
		seen[key] = true
		var err error
		switch key {
		case "FREQ":
			rule.Freq = strings.ToUpper(value)
		case "INTERVAL":
			rule.Interval, err = parseBoundedInt(value, "INTERVAL")
		case "COUNT":
			rule.Count, err = parseBoundedInt(value, "COUNT")
		case "UNTIL":
			rule.Until, err = parseUntil(value)
		case "BYDAY":
			rule.ByDay, err = parseByDay(value)
		case "BYMONTHDAY":
			rule.ByMonthDay, err = parseSmallInts(value, "BYMONTHDAY", -31, 31)
		case "BYMONTH":
			rule.ByMonth, err = parseSmallInts(value, "BYMONTH", 1, 12)
		case "WKST":
			rule.WeekStart = strings.ToUpper(value)
		case "BYSETPOS", "BYYEARDAY", "BYWEEKNO", "BYHOUR", "BYMINUTE", "BYSECOND", "RSCALE", "SKIP":
			return nil, fmt.Errorf("%w: %s has no Graph pattern", ErrUnsupported, key)
		default:
			return nil, fmt.Errorf("%w: unknown rule part %s", ErrUnsupported, key)
		}
		if err != nil {
			return nil, err
		}
	}
	switch rule.Freq {
	case "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
	case "":
		return nil, fmt.Errorf("%w: a rule without FREQ", ErrUnsupported)
	default:
		return nil, fmt.Errorf("%w: FREQ=%s has no Graph pattern", ErrUnsupported, rule.Freq)
	}
	if rule.Count > 0 && !rule.Until.IsZero() {
		return nil, fmt.Errorf("%w: COUNT and UNTIL are mutually exclusive", ErrUnsupported)
	}
	return rule, nil
}

func parseBoundedInt(value, name string) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 || number > maxCount {
		return 0, fmt.Errorf("%w: %s=%q is out of range", ErrUnsupported, name, value)
	}
	return number, nil
}

// parseSmallInts reads a comma-separated BYMONTH or BYMONTHDAY list. The values
// are parsed at the width the Graph pattern stores (int32) instead of at the
// width of an int: `strconv.Atoi` answers an architecture-dependent integer,
// which would have to be narrowed into the pattern and, on a 64-bit build, could
// wrap before the pattern is built.
func parseSmallInts(value, name string, min, max int32) ([]int32, error) {
	parts := strings.Split(value, ",")
	numbers := make([]int32, 0, len(parts))
	for _, part := range parts {
		number, err := strconv.ParseInt(strings.TrimSpace(part), 10, 32)
		if err != nil || number < int64(min) || number > int64(max) || number == 0 {
			return nil, fmt.Errorf("%w: %s=%q is out of range", ErrUnsupported, name, value)
		}
		numbers = append(numbers, int32(number))
	}
	return numbers, nil
}

func parseByDay(value string) ([]Weekday, error) {
	parts := strings.Split(value, ",")
	weekdays := make([]Weekday, 0, len(parts))
	for _, part := range parts {
		part = strings.ToUpper(strings.TrimSpace(part))
		if len(part) < 2 {
			return nil, fmt.Errorf("%w: BYDAY=%q is malformed", ErrUnsupported, value)
		}
		day := part[len(part)-2:]
		if _, ok := graphDay(day); !ok {
			return nil, fmt.Errorf("%w: BYDAY=%q names no weekday", ErrUnsupported, value)
		}
		ordinal := 0
		if prefix := part[:len(part)-2]; prefix != "" {
			parsed, err := strconv.Atoi(prefix)
			if err != nil || parsed == 0 || parsed < -1 || parsed > 4 {
				return nil, fmt.Errorf("%w: BYDAY=%q has an ordinal Graph cannot express", ErrUnsupported, value)
			}
			ordinal = parsed
		}
		weekdays = append(weekdays, Weekday{Day: day, Ordinal: ordinal})
	}
	return weekdays, nil
}

func parseUntil(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"20060102T150405Z", "20060102T150405", "20060102"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: UNTIL=%q is not a date", ErrUnsupported, value)
}

func parseExDate(body string) ([]time.Time, error) {
	moments := make([]time.Time, 0, 4)
	for _, part := range strings.Split(body, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parsed, err := parseUntil(part)
		if err != nil {
			return nil, err
		}
		moments = append(moments, parsed)
	}
	return moments, nil
}

// ToGraph builds the object a Graph create or update carries. `start` anchors the
// range and, for a weekly rule without BYDAY, names the weekday. `zone` is the
// zone Graph applies to the pattern.
//
// An EXDATE is refused: Graph expresses a skipped occurrence as an exception, not
// as part of the rule, and inventing exceptions for instants that may fall outside
// the listing window would grow the series on every run.
func ToGraph(lines []string, start time.Time, zone string) (models.PatternedRecurrenceable, error) {
	rule, exdates, err := Parse(lines)
	if err != nil {
		return nil, err
	}
	if len(exdates) > 0 {
		return nil, fmt.Errorf("%w: EXDATE has no Graph pattern", ErrUnsupported)
	}
	pattern, err := patternFor(rule, start)
	if err != nil {
		return nil, err
	}
	rangeValue := models.NewRecurrenceRange()
	if zone != "" {
		rangeValue.SetRecurrenceTimeZone(&zone)
	}
	switch {
	case !rule.Until.IsZero():
		rangeValue.SetTypeEscaped(ptr(models.ENDDATE_RECURRENCERANGETYPE))
		rangeValue.SetEndDate(serialization.NewDateOnly(rule.Until))
	case rule.Count > 0:
		count := int32(rule.Count)
		rangeValue.SetTypeEscaped(ptr(models.NUMBERED_RECURRENCERANGETYPE))
		rangeValue.SetNumberOfOccurrences(&count)
	default:
		rangeValue.SetTypeEscaped(ptr(models.NOEND_RECURRENCERANGETYPE))
	}
	rangeValue.SetStartDate(serialization.NewDateOnly(start))

	recurrence := models.NewPatternedRecurrence()
	recurrence.SetPattern(pattern)
	recurrence.SetRangeEscaped(rangeValue)
	return recurrence, nil
}

func patternFor(rule *Rule, start time.Time) (models.RecurrencePatternable, error) {
	pattern := models.NewRecurrencePattern()
	interval := int32(rule.Interval)
	if interval < 1 {
		interval = 1
	}
	pattern.SetInterval(&interval)
	switch rule.Freq {
	case "DAILY":
		if len(rule.ByDay) > 0 || len(rule.ByMonthDay) > 0 || len(rule.ByMonth) > 0 {
			return nil, fmt.Errorf("%w: a daily pattern carries no day selection", ErrUnsupported)
		}
		pattern.SetTypeEscaped(ptr(models.DAILY_RECURRENCEPATTERNTYPE))
		return pattern, nil
	case "WEEKLY":
		pattern.SetTypeEscaped(ptr(models.WEEKLY_RECURRENCEPATTERNTYPE))
		days := make([]models.DayOfWeek, 0, len(rule.ByDay))
		if len(rule.ByDay) == 0 {
			day, _ := graphDay(weekdayName(start.Weekday()))
			days = append(days, day)
		}
		for _, entry := range rule.ByDay {
			if entry.Ordinal != 0 {
				return nil, fmt.Errorf("%w: a weekly pattern cannot pin an occurrence to a week of the month", ErrUnsupported)
			}
			day, _ := graphDay(entry.Day)
			days = append(days, day)
		}
		if len(rule.ByMonthDay) > 0 || len(rule.ByMonth) > 0 {
			return nil, fmt.Errorf("%w: a weekly pattern carries no month selection", ErrUnsupported)
		}
		pattern.SetDaysOfWeek(days)
		firstDay := models.MONDAY_DAYOFWEEK
		if rule.WeekStart != "" {
			day, ok := graphDay(rule.WeekStart)
			if !ok {
				return nil, fmt.Errorf("%w: WKST=%q names no weekday", ErrUnsupported, rule.WeekStart)
			}
			firstDay = day
		}
		pattern.SetFirstDayOfWeek(&firstDay)
		return pattern, nil
	case "MONTHLY":
		if len(rule.ByMonth) > 0 {
			return nil, fmt.Errorf("%w: a monthly pattern carries no month selection", ErrUnsupported)
		}
		return monthlyPattern(pattern, rule)
	case "YEARLY":
		if len(rule.ByMonth) != 1 {
			return nil, fmt.Errorf("%w: a yearly pattern needs exactly one BYMONTH", ErrUnsupported)
		}
		month := rule.ByMonth[0]
		if month < 1 || month > 12 {
			return nil, fmt.Errorf("%w: BYMONTH=%d is out of range", ErrUnsupported, month)
		}
		pattern.SetMonth(&month)
		return monthlyPattern(pattern, rule)
	}
	return nil, fmt.Errorf("%w: unsupported frequency %q", ErrUnsupported, rule.Freq)
}

// monthlyPattern fills a monthly or yearly pattern: an ordinal BYDAY becomes a
// relative pattern, a BYMONTHDAY an absolute one.
func monthlyPattern(pattern *models.RecurrencePattern, rule *Rule) (models.RecurrencePatternable, error) {
	if len(rule.ByDay) > 0 {
		if len(rule.ByMonthDay) > 0 {
			return nil, fmt.Errorf("%w: BYDAY and BYMONTHDAY together have no Graph pattern", ErrUnsupported)
		}
		if len(rule.ByDay) != 1 || rule.ByDay[0].Ordinal == 0 {
			return nil, fmt.Errorf("%w: a relative pattern needs one BYDAY with an ordinal", ErrUnsupported)
		}
		day, _ := graphDay(rule.ByDay[0].Day)
		index, ok := graphIndex(rule.ByDay[0].Ordinal)
		if !ok {
			return nil, fmt.Errorf("%w: the ordinal %d of BYDAY has no Graph index", ErrUnsupported, rule.ByDay[0].Ordinal)
		}
		pattern.SetDaysOfWeek([]models.DayOfWeek{day})
		pattern.SetIndex(&index)
		if rule.Freq == "YEARLY" {
			pattern.SetTypeEscaped(ptr(models.RELATIVEYEARLY_RECURRENCEPATTERNTYPE))
			return pattern, nil
		}
		pattern.SetTypeEscaped(ptr(models.RELATIVEMONTHLY_RECURRENCEPATTERNTYPE))
		return pattern, nil
	}
	if len(rule.ByMonthDay) != 1 {
		return nil, fmt.Errorf("%w: an absolute pattern needs exactly one BYMONTHDAY", ErrUnsupported)
	}
	day := rule.ByMonthDay[0]
	if day == 0 || day < -31 || day > 31 {
		return nil, fmt.Errorf("%w: BYMONTHDAY=%d is out of range", ErrUnsupported, day)
	}
	pattern.SetDayOfMonth(&day)
	if rule.Freq == "YEARLY" {
		pattern.SetTypeEscaped(ptr(models.ABSOLUTEYEARLY_RECURRENCEPATTERNTYPE))
		return pattern, nil
	}
	pattern.SetTypeEscaped(ptr(models.ABSOLUTEMONTHLY_RECURRENCEPATTERNTYPE))
	return pattern, nil
}

// FromGraph renders a Graph recurrence as the RFC 5545 line Google and a feed
// would carry, so a series copied from Microsoft is read back with the same
// canonical rule the source had.
func FromGraph(value models.PatternedRecurrenceable) ([]string, error) {
	if value == nil || value.GetPattern() == nil {
		return nil, fmt.Errorf("%w: the source published no recurrence", ErrUnsupported)
	}
	pattern := value.GetPattern()
	kind := pattern.GetTypeEscaped()
	if kind == nil {
		return nil, fmt.Errorf("%w: the source pattern has no type", ErrUnsupported)
	}
	freq := ""
	switch *kind {
	case models.DAILY_RECURRENCEPATTERNTYPE:
		freq = "DAILY"
	case models.WEEKLY_RECURRENCEPATTERNTYPE:
		freq = "WEEKLY"
	case models.ABSOLUTEMONTHLY_RECURRENCEPATTERNTYPE, models.RELATIVEMONTHLY_RECURRENCEPATTERNTYPE:
		freq = "MONTHLY"
	case models.ABSOLUTEYEARLY_RECURRENCEPATTERNTYPE, models.RELATIVEYEARLY_RECURRENCEPATTERNTYPE:
		freq = "YEARLY"
	default:
		return nil, fmt.Errorf("%w: unknown pattern type", ErrUnsupported)
	}
	parts := []string{"FREQ=" + freq}
	if interval := pattern.GetInterval(); interval != nil && *interval > 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(int(*interval)))
	}
	if weeks := pattern.GetDaysOfWeek(); len(weeks) > 0 {
		ordinal := ordinalOf(pattern.GetIndex())
		days := make([]string, 0, len(weeks))
		for _, week := range weeks {
			name, ok := dayName(week)
			if !ok {
				return nil, fmt.Errorf("%w: unknown weekday", ErrUnsupported)
			}
			if ordinal != 0 && freq != "WEEKLY" {
				days = append(days, strconv.Itoa(ordinal)+name)
				continue
			}
			days = append(days, name)
		}
		parts = append(parts, "BYDAY="+strings.Join(days, ","))
	}
	if day := pattern.GetDayOfMonth(); day != nil && *day > 0 {
		parts = append(parts, "BYMONTHDAY="+strconv.Itoa(int(*day)))
	}
	if month := pattern.GetMonth(); month != nil && *month > 0 {
		parts = append(parts, "BYMONTH="+strconv.Itoa(int(*month)))
	}
	if limit := value.GetRangeEscaped(); limit != nil {
		if limitKind := limit.GetTypeEscaped(); limitKind != nil {
			switch *limitKind {
			case models.ENDDATE_RECURRENCERANGETYPE:
				if end := limit.GetEndDate(); end != nil {
					parts = append(parts, "UNTIL="+strings.ReplaceAll(end.String(), "-", "")+"T000000Z")
				}
			case models.NUMBERED_RECURRENCERANGETYPE:
				if count := limit.GetNumberOfOccurrences(); count != nil && *count > 0 {
					parts = append(parts, "COUNT="+strconv.Itoa(int(*count)))
				}
			}
		}
	}
	return []string{"RRULE:" + strings.Join(parts, ";")}, nil
}

func graphDay(day string) (models.DayOfWeek, bool) {
	switch strings.ToUpper(day) {
	case "SU":
		return models.SUNDAY_DAYOFWEEK, true
	case "MO":
		return models.MONDAY_DAYOFWEEK, true
	case "TU":
		return models.TUESDAY_DAYOFWEEK, true
	case "WE":
		return models.WEDNESDAY_DAYOFWEEK, true
	case "TH":
		return models.THURSDAY_DAYOFWEEK, true
	case "FR":
		return models.FRIDAY_DAYOFWEEK, true
	case "SA":
		return models.SATURDAY_DAYOFWEEK, true
	}
	return 0, false
}

func dayName(day models.DayOfWeek) (string, bool) {
	switch day {
	case models.SUNDAY_DAYOFWEEK:
		return "SU", true
	case models.MONDAY_DAYOFWEEK:
		return "MO", true
	case models.TUESDAY_DAYOFWEEK:
		return "TU", true
	case models.WEDNESDAY_DAYOFWEEK:
		return "WE", true
	case models.THURSDAY_DAYOFWEEK:
		return "TH", true
	case models.FRIDAY_DAYOFWEEK:
		return "FR", true
	case models.SATURDAY_DAYOFWEEK:
		return "SA", true
	}
	return "", false
}

func weekdayName(day time.Weekday) string {
	return [...]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}[day]
}

func graphIndex(ordinal int) (models.WeekIndex, bool) {
	switch ordinal {
	case 1:
		return models.FIRST_WEEKINDEX, true
	case 2:
		return models.SECOND_WEEKINDEX, true
	case 3:
		return models.THIRD_WEEKINDEX, true
	case 4:
		return models.FOURTH_WEEKINDEX, true
	case -1:
		return models.LAST_WEEKINDEX, true
	}
	return 0, false
}

func ordinalOf(index *models.WeekIndex) int {
	if index == nil {
		return 0
	}
	switch *index {
	case models.FIRST_WEEKINDEX:
		return 1
	case models.SECOND_WEEKINDEX:
		return 2
	case models.THIRD_WEEKINDEX:
		return 3
	case models.FOURTH_WEEKINDEX:
		return 4
	case models.LAST_WEEKINDEX:
		return -1
	}
	return 0
}

func ptr[T any](value T) *T { return &value }
