package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Calendar is one calendar of a connected account, as the calendar picker and
// the job editor list it.
type Calendar struct {
	ID   string
	Name string
	// Primary marks the calendar the provider considers the account's own.
	Primary bool
	// TimeZone is the IANA zone the calendar is configured with, when known.
	// Events coming from an .ics feed inherit it as their default.
	TimeZone string
	// ReadOnly marks a calendar the account may read but not write (a holiday
	// calendar, a shared calendar without write access). The engine refuses to
	// pick one as a destination.
	ReadOnly bool
}

// CalendarWindow is the half-open range a calendar listing covers. It is
// exclusive at the end, like every provider range query.
type CalendarWindow struct {
	Start time.Time
	End   time.Time
}

// CalendarEvent is one event as a provider returns it, normalised so the engine
// never reads a provider payload itself.
//
// Start and End are absolute instants. An all-day event is reported at midnight
// of its first day in TimeZone and its End is the exclusive midnight after the
// last day, which is how Google and Microsoft both model it; the engine writes
// it back the same way so the round trip does not shift the day.
type CalendarEvent struct {
	ID          string
	UID         string
	Summary     string
	Description string
	Location    string
	Start       time.Time
	End         time.Time
	AllDay      bool
	TimeZone    string
	// FreeBusy is the free/busy state the calendar shows the event with. It is
	// deliberately not part of EventFingerprint: adding a component would make
	// every fingerprint stored before this field existed look like an edit on
	// both sides, which the engine would answer with a rewrite of every paired
	// event. Drift is detected through the link's recorded state instead.
	FreeBusy FreeBusy
	// UpdatedAt is the provider's last modification stamp. It feeds the
	// newest_wins conflict rule, so it must be the event stamp, not the sync
	// time.
	UpdatedAt time.Time
	// Recurring marks a series master or an instance of one.
	Recurring bool
	// Kind tells a single event apart from the master of a series and from the
	// occurrences of one. The engine copies a master and its exceptions, and
	// ignores the plain occurrences a listing expands.
	Kind EventKind
	// SeriesID is the provider id of the master a listing expanded this event
	// from (Google's recurringEventId, Graph's seriesMasterId). It is empty for
	// a single event and for a master.
	SeriesID string
	// OriginalStart is the start the master gave the occurrence before it was
	// moved. It is the identity of an exception: the copy has to target the
	// same occurrence of the destination series.
	OriginalStart time.Time
	// Recurrence is the rule of a series master, as the RFC 5545 lines the
	// providers exchange (RRULE, RDATE, EXDATE).
	Recurrence []string
	// Cancelled marks a cancelled event that is still listed.
	Cancelled bool
	Organizer string
	Attendees []string
}

// CalendarEventInput is what the engine writes to a provider. ID, UID and the
// timestamps of the remote event are not part of it: a create has no ID yet and
// an update never moves an event to another identity.
type CalendarEventInput struct {
	// UID is the iCalendar UID the event should carry. Providers that honour it
	// (Google) keep the pairing stable across both sides; Graph stores it but
	// does not accept it on create, which is why the engine also tracks the
	// returned event id.
	UID         string
	Summary     string
	Description string
	Location    string
	Start       time.Time
	End         time.Time
	AllDay      bool
	TimeZone    string
	// FreeBusy is the state to write. An empty value leaves the provider
	// default in place, which is what keeps an unrelated update from clearing a
	// marker the operator set on the destination calendar.
	FreeBusy FreeBusy
	// Recurrence carries the rule of a series master (RRULE, RDATE, EXDATE).
	// It is empty for a single event, and a provider that is asked to write an
	// empty rule leaves an existing one in place.
	Recurrence []string
	// SeriesID is the provider id of the destination master when this write
	// targets one occurrence of a copied series; the provider resolves the
	// occurrence from it and OriginalStart.
	SeriesID string
	// OriginalStart is the occurrence this write targets inside SeriesID.
	OriginalStart time.Time
	// Cancelled asks the provider to cancel the occurrence instead of moving
	// it. Graph has no writable isCancelled, so it deletes the occurrence,
	// which is the same statement in the series.
	Cancelled bool
	// TransactionID is an idempotency token for a create. A retried create with
	// the same token does not mint a second event or a second series.
	TransactionID string
	Attendees     []string
}

// FreeBusy is the free/busy state of an event, in one vocabulary for both
// providers and for .ics feeds. The five values are what Graph calls `showAs`;
// Google expresses three of them through `transparency` and `status`, and a feed
// through `TRANSP` and `STATUS`.
type FreeBusy string

const (
	// FreeBusyFree does not block the calendar.
	FreeBusyFree FreeBusy = "free"
	// FreeBusyTentative is a maybe: it blocks the calendar but says so.
	FreeBusyTentative FreeBusy = "tentative"
	// FreeBusyBusy is a normal event.
	FreeBusyBusy FreeBusy = "busy"
	// FreeBusyOutOfOffice is away time.
	FreeBusyOutOfOffice FreeBusy = "out_of_office"
	// FreeBusyWorkingElsewhere is busy but somewhere else.
	FreeBusyWorkingElsewhere FreeBusy = "working_elsewhere"
	// FreeBusyCancelled is not a show-as state: it is what a cancelled
	// occurrence of a series reads as, and the transformation map lets the
	// operator decide what the destination should show for it.
	FreeBusyCancelled FreeBusy = "cancelled"
)

// FreeBusyName normalises a free/busy value, answering the empty string for
// anything it does not recognise: an unknown label must not reach a payload,
// where it would clear the provider's own default. The aliases are the spellings
// a provider or a stored job may still carry.
func FreeBusyName(value string) FreeBusy {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "free", "transparent":
		return FreeBusyFree
	case "tentative":
		return FreeBusyTentative
	case "busy", "opaque":
		return FreeBusyBusy
	case "out_of_office", "out-of-office", "oof":
		return FreeBusyOutOfOffice
	case "working_elsewhere", "workingelsewhere", "working-elsewhere":
		return FreeBusyWorkingElsewhere
	case "cancelled", "canceled":
		return FreeBusyCancelled
	default:
		return ""
	}
}

// ShowAs reports whether the state is one a calendar can be shown as. A
// cancelled occurrence has no show-as of its own: it is a series statement.
func (f FreeBusy) ShowAs() bool {
	switch f {
	case FreeBusyFree, FreeBusyTentative, FreeBusyBusy, FreeBusyOutOfOffice, FreeBusyWorkingElsewhere:
		return true
	default:
		return false
	}
}

// EventKind classifies a listed event by its place in a series.
type EventKind string

const (
	// EventKindSingle is an ordinary event without a series.
	EventKindSingle EventKind = "single"
	// EventKindMaster is the event that carries the recurrence rule.
	EventKindMaster EventKind = "master"
	// EventKindOccurrence is an unmodified occurrence a listing expanded.
	EventKindOccurrence EventKind = "occurrence"
	// EventKindException is an occurrence the operator moved, edited or
	// cancelled.
	EventKindException EventKind = "exception"
)

// CalendarProvider is the calendar half of a provider. It is a separate
// interface from Provider on purpose: the sync engine only needs the file half,
// the calendar engine only needs this one, and a provider that cannot do
// calendars simply does not implement it (see registry.go).
type CalendarProvider interface {
	// Calendars lists the calendars the account can read.
	Calendars(ctx context.Context, creds Credentials, tokens *Tokens) ([]Calendar, error)

	// CalendarEvents lists the events of one calendar inside a window.
	CalendarEvents(ctx context.Context, creds Credentials, tokens *Tokens, calendarID string, window CalendarWindow) ([]CalendarEvent, error)

	// CreateCalendarEvent creates an event and returns it as stored.
	CreateCalendarEvent(ctx context.Context, creds Credentials, tokens *Tokens, calendarID string, input CalendarEventInput) (*CalendarEvent, error)

	// UpdateCalendarEvent replaces the fields of an existing event.
	UpdateCalendarEvent(ctx context.Context, creds Credentials, tokens *Tokens, calendarID, eventID string, input CalendarEventInput) (*CalendarEvent, error)

	// DeleteCalendarEvent removes an event. A missing event must be reported as
	// ErrNotFound so the engine treats it as already done.
	DeleteCalendarEvent(ctx context.Context, creds Credentials, tokens *Tokens, calendarID, eventID string) error
}

// EventFingerprint is the content identity the engine compares between runs: two
// listings of the same unchanged event produce the same value, an edit on either
// side changes it. It deliberately ignores the provider id and the modification
// stamp (providers bump those on no-op writes) and covers everything the engine
// would copy.
func EventFingerprint(event CalendarEvent) string {
	parts := []string{
		strings.TrimSpace(event.Summary),
		strings.TrimSpace(event.Description),
		strings.TrimSpace(event.Location),
		event.Start.UTC().Format(time.RFC3339),
		event.End.UTC().Format(time.RFC3339),
		fmt.Sprintf("all_day=%t", event.AllDay),
		strings.Join(sortedLower(event.Attendees), ","),
		fmt.Sprintf("recurring=%t", event.Recurring),
	}
	// The rule of a master and the occurrence an exception targets are appended
	// only when they exist, so every fingerprint stored before they were read
	// keeps its value and the upgrade itself rewrites nothing.
	if rule := CanonicalRecurrence(event.Recurrence); rule != "" {
		parts = append(parts, "recurrence="+rule)
	}
	if !event.OriginalStart.IsZero() {
		parts = append(parts, "original_start="+event.OriginalStart.UTC().Format(time.RFC3339))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// CanonicalRecurrence renders the recurrence lines of a master the way the
// fingerprint compares them: providers reorder EXDATE values and re-case the
// property name between two listings of an unchanged series, and neither is an
// edit. An empty list answers the empty string, which is what keeps the
// component out of the fingerprints of non-recurring events.
func CanonicalRecurrence(lines []string) string {
	normalized := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.Fields(strings.ToUpper(strings.TrimSpace(line))), "")
		if line != "" {
			normalized = append(normalized, line)
		}
	}
	if len(normalized) == 0 {
		return ""
	}
	sort.Strings(normalized)
	return strings.Join(normalized, "\x1e")
}

// InputFingerprint is EventFingerprint for a value the engine is about to write.
// It is what a freshly written event is expected to list as, so a provider that
// normalises a value (Graph trims a description, Google rewrites an attendee
// list) does not look like a conflict on the next run.
func InputFingerprint(input CalendarEventInput) string {
	return EventFingerprint(CalendarEvent{
		Summary:       input.Summary,
		Description:   input.Description,
		Location:      input.Location,
		Start:         input.Start,
		End:           input.End,
		AllDay:        input.AllDay,
		Attendees:     input.Attendees,
		Recurrence:    input.Recurrence,
		OriginalStart: input.OriginalStart,
		Recurring:     len(input.Recurrence) > 0 || input.SeriesID != "",
	})
}

// EventInputFrom converts a listed event into the input that writes it back
// unchanged.
func EventInputFrom(event CalendarEvent) CalendarEventInput {
	return CalendarEventInput{
		UID:           event.UID,
		Summary:       event.Summary,
		Description:   event.Description,
		Location:      event.Location,
		Start:         event.Start,
		End:           event.End,
		AllDay:        event.AllDay,
		TimeZone:      event.TimeZone,
		FreeBusy:      event.FreeBusy,
		Recurrence:    append([]string(nil), event.Recurrence...),
		OriginalStart: event.OriginalStart,
		Cancelled:     event.Cancelled,
		Attendees:     append([]string(nil), event.Attendees...),
	}
}

// CalendarEventUID derives the iCalendar UID of an event the engine creates from
// a stable seed (the UID of the event on the source side, or a hash of its
// identity). Deriving it instead of letting each provider mint its own is what
// keeps a Google-created copy and a Microsoft-created copy of the same event
// recognisable as the same event when a later run pairs the two calendars in the
// opposite direction.
//
// The result is a compliance-shaped UID: a hex digest and a domain, no spaces,
// no control characters, no colon.
func CalendarEventUID(seed string) string {
	sum := sha256.Sum256([]byte("sync-calendar\x00" + strings.TrimSpace(seed)))
	return hex.EncodeToString(sum[:20]) + "@sync.local"
}

// CalendarsReadable reports whether a registered provider can answer calendar
// calls at all. The handler uses it to explain a provider without support.
func CalendarsReadable(provider any) bool {
	_, ok := provider.(CalendarProvider)
	return ok
}

// sortedLower normalises an attendee list before it is hashed: the order of an
// attendee list is not content, and a provider may return it in any order.
func sortedLower(values []string) []string {
	lowered := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			lowered = append(lowered, value)
		}
	}
	sort.Strings(lowered)
	return lowered
}

// LoadZone resolves a calendar zone without failing. Calendar payloads carry a
// zone name next to a wall-clock time; when the runtime has no zone database (a
// scratch container) or the name cannot be resolved, an explicit UTC offset still
// stays correct, so callers fall back to UTC instead of failing the listing.
func LoadZone(name string) *time.Location {
	canonical := ZoneName(name)
	if canonical == "" {
		return time.UTC
	}
	location, err := time.LoadLocation(canonical)
	if err != nil {
		return time.UTC
	}
	return location
}

// InZone renders an instant in a zone a provider will accept. The returned name
// is always loadable by the runtime and is therefore always a name Google and
// Microsoft can read: an unknown label (a Windows zone name Graph published, a
// TZID of a feed) falls back to UTC, which keeps the instant right even though the
// wall clock is rendered differently.
func InZone(moment time.Time, name string) (time.Time, string) {
	location := LoadZone(name)
	if location == time.UTC {
		return moment.UTC(), "UTC"
	}
	return moment.In(location), location.String()
}

// ZoneName normalises a zone label into an IANA name. It is the guard between a
// stored or parsed label and a provider payload: Google refuses an event whose
// start.timeZone it does not know ("Invalid time zone definition for start
// time"), so nothing but a name the runtime can load may reach a write.
//
// Three shapes are accepted: an IANA name, a Windows zone name (what Microsoft
// Graph and Outlook publish), and an RFC 5545 globally-unique TZID (a leading
// `/` plus vendor path segments, as Mozilla exports). The empty string answers an
// unresolvable label, and callers fall back to UTC.
func ZoneName(name string) string {
	label := strings.Trim(strings.TrimSpace(name), `"`)
	if label == "" || strings.EqualFold(label, "local") {
		// "Local" is a Go alias for the machine zone, not a zone a provider can
		// read, and it changes meaning with the deployment.
		return ""
	}
	for _, char := range label {
		if char < 0x20 || char == 0x7f {
			return ""
		}
	}
	for _, candidate := range zoneCandidates(label) {
		if location, err := time.LoadLocation(candidate); err == nil {
			return location.String()
		}
	}
	return ""
}

// zoneCandidates lists the names worth trying for one label, most specific first:
// the alias table, the label itself, then the trailing path segments of a
// globally-unique TZID.
func zoneCandidates(label string) []string {
	candidates := make([]string, 0, 4)
	if mapped, ok := zoneAliases[strings.ToLower(label)]; ok {
		candidates = append(candidates, mapped)
	}
	candidates = append(candidates, label)
	if strings.HasPrefix(label, "/") {
		segments := strings.Split(strings.TrimPrefix(label, "/"), "/")
		for index := 0; index < len(segments); index++ {
			candidates = append(candidates, strings.Join(segments[index:], "/"))
		}
	}
	return candidates
}

// zoneAliases maps the Windows zone names Graph, Outlook and Exchange publish to
// the IANA zone with the same rules. It is a deliberate subset: the zones an event
// realistically carries, not every entry of the CLDR table.
var zoneAliases = map[string]string{
	"dateline standard time":          "Etc/GMT+12",
	"utc-11":                          "Etc/GMT+11",
	"hawaiian standard time":          "Pacific/Honolulu",
	"alaskan standard time":           "America/Anchorage",
	"pacific standard time":           "America/Los_Angeles",
	"pacific standard time (mexico)":  "America/Tijuana",
	"us mountain standard time":       "America/Phoenix",
	"mountain standard time":          "America/Denver",
	"mountain standard time (mexico)": "America/Chihuahua",
	"central america standard time":   "America/Guatemala",
	"central standard time":           "America/Chicago",
	"central standard time (mexico)":  "America/Mexico_City",
	"canada central standard time":    "America/Regina",
	"sa pacific standard time":        "America/Bogota",
	"eastern standard time":           "America/New_York",
	"us eastern standard time":        "America/Indiana/Indianapolis",
	"venezuela standard time":         "America/Caracas",
	"atlantic standard time":          "America/Halifax",
	"sa western standard time":        "America/La_Paz",
	"pacific sa standard time":        "America/Santiago",
	"newfoundland standard time":      "America/St_Johns",
	"e. south america standard time":  "America/Sao_Paulo",
	"sa eastern standard time":        "America/Cayenne",
	"argentina standard time":         "America/Argentina/Buenos_Aires",
	"greenwich standard time":         "Atlantic/Reykjavik",
	"gmt standard time":               "Europe/London",
	"azores standard time":            "Atlantic/Azores",
	"cape verde standard time":        "Atlantic/Cape_Verde",
	"morocco standard time":           "Africa/Casablanca",
	"co-ordinated universal time":     "UTC",
	"utc":                             "UTC",
	"w. europe standard time":         "Europe/Berlin",
	"central europe standard time":    "Europe/Budapest",
	"romance standard time":           "Europe/Paris",
	"central european standard time":  "Europe/Warsaw",
	"gtb standard time":               "Europe/Bucharest",
	"e. europe standard time":         "Europe/Chisinau",
	"turkey standard time":            "Europe/Istanbul",
	"israel standard time":            "Asia/Jerusalem",
	"middle east standard time":       "Asia/Beirut",
	"egypt standard time":             "Africa/Cairo",
	"syria standard time":             "Asia/Damascus",
	"west bank standard time":         "Asia/Hebron",
	"russian standard time":           "Europe/Moscow",
	"arabic standard time":            "Asia/Baghdad",
	"arab standard time":              "Asia/Riyadh",
	"arabian standard time":           "Asia/Dubai",
	"iran standard time":              "Asia/Tehran",
	"georgian standard time":          "Asia/Tbilisi",
	"caucasus standard time":          "Asia/Yerevan",
	"afghanistan standard time":       "Asia/Kabul",
	"west asia standard time":         "Asia/Tashkent",
	"pakistan standard time":          "Asia/Karachi",
	"india standard time":             "Asia/Kolkata",
	"sri lanka standard time":         "Asia/Colombo",
	"nepal standard time":             "Asia/Kathmandu",
	"central asia standard time":      "Asia/Almaty",
	"central asian standard time":     "Asia/Almaty",
	"bangladesh standard time":        "Asia/Dhaka",
	"myanmar standard time":           "Asia/Yangon",
	"se asia standard time":           "Asia/Bangkok",
	"north asia standard time":        "Asia/Krasnoyarsk",
	"n. central asia standard time":   "Asia/Novosibirsk",
	"china standard time":             "Asia/Shanghai",
	"singapore standard time":         "Asia/Singapore",
	"taipei standard time":            "Asia/Taipei",
	"w. australia standard time":      "Australia/Perth",
	"cen. australia standard time":    "Australia/Adelaide",
	"aus central standard time":       "Australia/Adelaide",
	"aus central w. standard time":    "Australia/Eucla",
	"aus eastern standard time":       "Australia/Sydney",
	"e. australia standard time":      "Australia/Brisbane",
	"tasmania standard time":          "Australia/Hobart",
	"lord howe standard time":         "Australia/Lord_Howe",
	"tokyo standard time":             "Asia/Tokyo",
	"korea standard time":             "Asia/Seoul",
	"west pacific standard time":      "Pacific/Port_Moresby",
	"new zealand standard time":       "Pacific/Auckland",
	"fiji standard time":              "Pacific/Fiji",
	"tonga standard time":             "Pacific/Tongatapu",
	"samoa standard time":             "Pacific/Apia",
	"chatham islands standard time":   "Pacific/Chatham",
	"norfolk standard time":           "Pacific/Norfolk",
	"bougainville standard time":      "Pacific/Bougainville",
	"line islands standard time":      "Pacific/Kiritimati",
	"kamchatka standard time":         "Asia/Kamchatka",
	"magadan standard time":           "Asia/Magadan",
	"sakhalin standard time":          "Asia/Sakhalin",
	"vladivostok standard time":       "Asia/Vladivostok",
	"yalta standard time":             "Europe/Simferopol",
	"kaliningrad standard time":       "Europe/Kaliningrad",
	"saratov standard time":           "Europe/Saratov",
	"volgograd standard time":         "Europe/Volgograd",
	"astrakhan standard time":         "Europe/Astrakhan",
	"omsk standard time":              "Asia/Omsk",
	"tomsk standard time":             "Asia/Tomsk",
	"transbaikal standard time":       "Asia/Chita",
	"w. mongolia standard time":       "Asia/Hovd",
	"ulaanbaatar standard time":       "Asia/Ulaanbaatar",
	"utc-02":                          "Etc/GMT+2",
	"utc-08":                          "Etc/GMT+8",
	"utc-09":                          "Etc/GMT+9",
	"utc-10":                          "Etc/GMT+10",
	"utc+02":                          "Etc/GMT-2",
	"utc+08":                          "Asia/Shanghai",
	"utc+09":                          "Asia/Tokyo",
	"utc+12":                          "Etc/GMT-12",
	"utc+13":                          "Etc/GMT-13",
}
