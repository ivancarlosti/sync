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

// TestTransparencyNameNormalisesTheFreeBusyFlag keeps an unknown label from
// reaching a payload, where it would clear the provider default instead of
// reporting that the caller had nothing to say.
func TestTransparencyNameNormalisesTheFreeBusyFlag(t *testing.T) {
	cases := map[string]string{
		"opaque":           TransparencyOpaque,
		"OPAQUE":           TransparencyOpaque,
		" transparent  ":   TransparencyTransparent,
		"Transparent":      TransparencyTransparent,
		"free":             "",
		"busy":             "",
		"workingElsewhere": "",
		"":                 "",
	}
	for value, want := range cases {
		if got := TransparencyName(value); got != want {
			t.Errorf("TransparencyName(%q) = %q, want %q", value, got, want)
		}
	}
}

// TestEventInputFromCarriesTheFreeBusyFlag guards the hop from a listed event to
// the body of a write: a flag that is not copied here is silently lost on every
// copy the engine makes.
func TestEventInputFromCarriesTheFreeBusyFlag(t *testing.T) {
	input := EventInputFrom(CalendarEvent{Summary: "Standup", Transparency: TransparencyTransparent})
	if input.Transparency != TransparencyTransparent {
		t.Fatalf("transparency = %q, want %q", input.Transparency, TransparencyTransparent)
	}
}
