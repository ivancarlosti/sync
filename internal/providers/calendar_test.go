package providers

import (
	"testing"
	"time"
)

// TestZoneNameAcceptsTheLabelsAProviderPublishes pins the three shapes a zone
// label reaches the engine in: an IANA name (a Google calendar), a Windows name (a
// Graph event or an Outlook export) and an RFC 5545 globally-unique TZID (a
// subscription exported by a desktop client). Every one of them has to leave as a
// name the other provider reads back, because that name travels inside the write.
func TestZoneNameAcceptsTheLabelsAProviderPublishes(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Berlin"); err != nil {
		t.Skipf("the runtime has no zone database: %v", err)
	}
	cases := map[string]string{
		"Europe/Berlin":                         "Europe/Berlin",
		"  Europe/Berlin  ":                     "Europe/Berlin",
		`"Europe/Paris"`:                        "Europe/Paris",
		"W. Europe Standard Time":               "Europe/Berlin",
		"w. europe standard time":               "Europe/Berlin",
		"Romance Standard Time":                 "Europe/Paris",
		"Eastern Standard Time":                 "America/New_York",
		"Tokyo Standard Time":                   "Asia/Tokyo",
		"India Standard Time":                   "Asia/Kolkata",
		"UTC":                                   "UTC",
		"utc":                                   "UTC",
		"/mozilla.org/20050126_1/Europe/Berlin": "Europe/Berlin",
		"/Europe/Berlin":                        "Europe/Berlin",
	}
	for label, want := range cases {
		if got := ZoneName(label); got != want {
			t.Errorf("ZoneName(%q) = %q, want %q", label, got, want)
		}
	}
}

// TestZoneNameRefusesWhatMustNotReachAPayload pins the empty answer. Google
// rejects the whole event with "Invalid time zone definition for start time" when
// the zone it is handed is not one it knows, so an unmappable label must never be
// echoed back into a payload.
func TestZoneNameRefusesWhatMustNotReachAPayload(t *testing.T) {
	for _, label := range []string{
		"",
		"   ",
		"Local",
		"local",
		"Mars/Olympus",
		"Customized Time Zone",
		"Europe/Berlin\r\nX-Injected: 1",
		"Europe/Berlin\x00",
		"/vendor/only/segment",
	} {
		if got := ZoneName(label); got != "" {
			t.Errorf("ZoneName(%q) = %q, want an empty answer", label, got)
		}
	}
}

// TestInZoneKeepsTheInstant is the property a write depends on: the wall clock may
// be rendered differently, the moment may not move. An unknown label falls back to
// UTC instead of being sent as a name the provider would refuse.
func TestInZoneKeepsTheInstant(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Berlin"); err != nil {
		t.Skipf("the runtime has no zone database: %v", err)
	}
	moment := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

	wall, zone := InZone(moment, "W. Europe Standard Time")
	if zone != "Europe/Berlin" {
		t.Fatalf("zone = %q, want Europe/Berlin", zone)
	}
	if !wall.Equal(moment) {
		t.Fatalf("the moment moved: %s", wall)
	}
	if hour, minute := wall.Hour(), wall.Minute(); hour != 11 || minute != 0 {
		t.Fatalf("wall clock = %02d:%02d, want 11:00", hour, minute)
	}

	fallback, fallbackZone := InZone(moment, "Mars/Olympus")
	if fallbackZone != "UTC" || !fallback.Equal(moment) {
		t.Fatalf("an unknown zone answered %s in %q", fallback, fallbackZone)
	}
	if fallback.Hour() != 10 {
		t.Fatalf("the UTC fallback rendered %02d:00, want 10:00", fallback.Hour())
	}
}

// TestFreeBusyNameNormalisesTheFreeBusyFlag keeps an unknown label from reaching
// a payload, where it would clear the provider default instead of reporting that
// the caller had nothing to say. The aliases are the spellings the providers and
// the stored jobs still use.
func TestFreeBusyNameNormalisesTheFreeBusyFlag(t *testing.T) {
	cases := map[string]FreeBusy{
		"opaque":            FreeBusyBusy,
		"OPAQUE":            FreeBusyBusy,
		" busy ":            FreeBusyBusy,
		" transparent  ":    FreeBusyFree,
		"Transparent":       FreeBusyFree,
		"free":              FreeBusyFree,
		"tentative":         FreeBusyTentative,
		"oof":               FreeBusyOutOfOffice,
		"out_of_office":     FreeBusyOutOfOffice,
		"workingElsewhere":  FreeBusyWorkingElsewhere,
		"working_elsewhere": FreeBusyWorkingElsewhere,
		"cancelled":         FreeBusyCancelled,
		"nonsense":          "",
		"":                  "",
	}
	for value, want := range cases {
		if got := FreeBusyName(value); got != want {
			t.Errorf("FreeBusyName(%q) = %q, want %q", value, got, want)
		}
	}
}

// TestEventInputFromCarriesTheFreeBusyAndTheSeries guards the hop from a listed
// event to the body of a write: anything that is not copied here is silently lost
// on every copy the engine makes.
func TestEventInputFromCarriesTheFreeBusyAndTheSeries(t *testing.T) {
	original := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	input := EventInputFrom(CalendarEvent{
		Summary:       "Standup",
		FreeBusy:      FreeBusyFree,
		Kind:          EventKindException,
		OriginalStart: original,
		Recurrence:    []string{"RRULE:FREQ=WEEKLY;BYDAY=MO"},
		Cancelled:     true,
	})
	if input.FreeBusy != FreeBusyFree {
		t.Fatalf("free/busy = %q, want %q", input.FreeBusy, FreeBusyFree)
	}
	if !input.Cancelled {
		t.Fatal("the cancellation was not carried into the write")
	}
	if !input.OriginalStart.Equal(original) {
		t.Fatalf("original start = %s", input.OriginalStart)
	}
	if len(input.Recurrence) != 1 || input.Recurrence[0] != "RRULE:FREQ=WEEKLY;BYDAY=MO" {
		t.Fatalf("recurrence = %v", input.Recurrence)
	}
}

// TestEventFingerprintCoversTheRuleOnlyWhenThereIsOne is what keeps the upgrade
// from rewriting every paired event: a fingerprint of an event without a rule and
// without an original start is the value it always had.
func TestEventFingerprintCoversTheRuleOnlyWhenThereIsOne(t *testing.T) {
	plain := CalendarEvent{Summary: "Standup", Start: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	base := EventFingerprint(plain)
	if got := EventFingerprint(plain); got != base {
		t.Fatalf("the fingerprint of an unchanged event moved: %q != %q", got, base)
	}
	reordered := plain
	reordered.Recurrence = []string{"RRULE:FREQ=WEEKLY;BYDAY=MO"}
	first := EventFingerprint(reordered)
	reordered.Recurrence = []string{" rrule:freq=weekly;byday=mo "}
	if got := EventFingerprint(reordered); got != first {
		t.Fatalf("a re-cased rule changed the fingerprint: %q != %q", got, first)
	}
	reordered.Recurrence = []string{"RRULE:FREQ=DAILY"}
	if got := EventFingerprint(reordered); got == first {
		t.Fatal("a different rule kept the fingerprint")
	}
	exception := plain
	exception.OriginalStart = time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	if got := EventFingerprint(exception); got == base {
		t.Fatal("an exception was not told apart from a plain event")
	}
}
