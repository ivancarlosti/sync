package services

import (
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// optionDestination is a destination side that reports itself writable without
// reaching a provider: the option rules only ask whether the side can be written
// to, so the rest of the interface is never called.
type optionDestination struct{ providers.CalendarProvider }

func newOptionDestination(calendar string) *calendarSide {
	side := &calendarSide{role: "destination", calendar: calendar}
	side.provider = optionDestination{}
	return side
}

func TestOptionsOfAJobWithoutOptionsAreEmpty(t *testing.T) {
	for _, job := range []*models.CalendarJob{
		nil,
		{},
		{TitlePrefix: "   ", TitleSuffix: " ", TransparencyOverride: "nonsense"},
		{FreeBusyMap: ""},
	} {
		options := optionsOf(job)
		if options.prefix != "" || options.suffix != "" || len(options.freeBusy) != 0 {
			t.Fatalf("options of %+v = %+v, want none", job, options)
		}
		if options.fingerprint != "" {
			t.Fatalf("a job without options answered the fingerprint %q", options.fingerprint)
		}
	}
}

// TestDecoratedSummaryRoundTripsThroughTheDestination pins the pair of functions
// the engine leans on: what a destination write adds, a destination read has to
// remove, so an edit made next to the copy does not carry the marker back into the
// source calendar.
func TestDecoratedSummaryRoundTripsThroughTheDestination(t *testing.T) {
	options := optionsOf(&models.CalendarJob{TitlePrefix: " [Work] ", TitleSuffix: "(shared)"})
	if got, want := options.decorate("Standup"), "[Work] Standup (shared)"; got != want {
		t.Fatalf("decorate() = %q, want %q", got, want)
	}
	if got := options.undecorate("[Work] Standup (shared)"); got != "Standup" {
		t.Fatalf("undecorate() = %q, want Standup", got)
	}
	// A summary that never carried the decoration is returned untouched.
	if got := options.undecorate("Planning"); got != "Planning" {
		t.Fatalf("undecorate() = %q, want Planning", got)
	}
	// An empty summary does not leave a dangling separator behind.
	if got, want := options.decorate("   "), "[Work] (shared)"; got != want {
		t.Fatalf("decorate(empty) = %q, want %q", got, want)
	}

	// The prefix alone is enough, and a suffix the operator typed is matched
	// literally.
	prefixOnly := optionsOf(&models.CalendarJob{TitlePrefix: "[Work]"})
	if got := prefixOnly.decorate("Standup"); got != "[Work] Standup" {
		t.Fatalf("decorate() = %q", got)
	}
	if got := prefixOnly.undecorate("[Work] Standup"); got != "Standup" {
		t.Fatalf("undecorate() = %q", got)
	}
}

func TestApplyToShapesTheDestinationWrite(t *testing.T) {
	options := optionsOf(&models.CalendarJob{
		TitlePrefix:          "[Work]",
		TransparencyOverride: string(models.CalendarFreeBusyFree),
	})
	input := options.applyTo(providers.CalendarEventInput{
		Summary:  "Standup",
		FreeBusy: providers.FreeBusyBusy,
	})
	if input.Summary != "[Work] Standup" {
		t.Fatalf("summary = %q", input.Summary)
	}
	if input.FreeBusy != providers.FreeBusyFree {
		t.Fatalf("free/busy = %q, want the mapped state", input.FreeBusy)
	}

	// Without a mapping the source state survives, which is what keeps the
	// free/busy state of the original authoritative.
	keep := optionsOf(&models.CalendarJob{TitlePrefix: "[Work]"})
	if got := keep.applyTo(providers.CalendarEventInput{FreeBusy: providers.FreeBusyFree}); got.FreeBusy != providers.FreeBusyFree {
		t.Fatalf("free/busy = %q, want it kept", got.FreeBusy)
	}

	busy := optionsOf(&models.CalendarJob{TransparencyOverride: string(models.CalendarFreeBusyBusy)})
	got := busy.applyTo(providers.CalendarEventInput{Summary: "Standup", FreeBusy: providers.FreeBusyFree})
	if got.FreeBusy != providers.FreeBusyBusy {
		t.Fatalf("free/busy = %q, want busy", got.FreeBusy)
	}
	if got.Summary != "Standup" {
		t.Fatalf("summary = %q, want it untouched", got.Summary)
	}
}

// TestOptionsFingerprintIdentifiesTheValues is what decides whether a pairing is
// rewritten, so it has to change when a value changes and stay put when nothing
// meaningful did.
func TestOptionsFingerprintIdentifiesTheValues(t *testing.T) {
	base := optionsOf(&models.CalendarJob{TitlePrefix: "A"}).fingerprint
	if base == "" {
		t.Fatal("an option set answered an empty fingerprint")
	}
	if got := optionsOf(&models.CalendarJob{TitlePrefix: "A "}).fingerprint; got != base {
		t.Fatalf("trailing space changed the fingerprint: %q != %q", got, base)
	}
	for _, other := range []*models.CalendarJob{
		{TitlePrefix: "B"},
		{TitleSuffix: "A"},
		{TransparencyOverride: string(models.CalendarFreeBusyFree)},
		{TitlePrefix: "A", TitleSuffix: "(shared)"},
	} {
		if got := optionsOf(other).fingerprint; got == base {
			t.Fatalf("options %+v answered the same fingerprint as a bare prefix", other)
		}
	}
	// Removing the options clears the column instead of recording a new value,
	// so a job that never used them looks exactly like one that stopped.
	if got := optionsOf(&models.CalendarJob{}).fingerprint; got != "" {
		t.Fatalf("an empty option set answered %q", got)
	}
}

// TestWriteInputDecoratesOneSideOnly is the asymmetry the whole feature hangs on:
// the marker belongs to the copy, and a write back to the source must not carry it.
func TestWriteInputDecoratesOneSideOnly(t *testing.T) {
	engine := &calendarEngine{job: &models.CalendarJob{TitlePrefix: "[Work]"}}
	from := providers.CalendarEvent{Summary: "Standup", FreeBusy: providers.FreeBusyBusy}
	uid := "event@example.com"

	if got := engine.writeInput(uid, newOptionDestination("target"), from); got.Summary != "[Work] Standup" {
		t.Fatalf("destination summary = %q", got.Summary)
	}

	source := &calendarSide{role: "source"}
	if got := engine.writeInput(uid, source, providers.CalendarEvent{Summary: "[Work] Standup"}); got.Summary != "Standup" {
		t.Fatalf("source summary = %q, want the decoration stripped", got.Summary)
	}
}

// TestStaleOptionsRewritesAnUnchangedPairOnce covers the reason the options
// fingerprint exists: an event both sides agree on is still redecorated when the
// options changed, and the recorded fingerprint makes the next run a no-op.
func TestStaleOptionsRewritesAnUnchangedPairOnce(t *testing.T) {
	start := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	engine := &calendarEngine{
		job:    &models.CalendarJob{TitlePrefix: "[Work]"},
		target: newOptionDestination("target"),
	}
	in := planInput{
		Link:        &models.CalendarEventLink{UID: "event@example.com"},
		Source:      &providers.CalendarEvent{UID: "event@example.com", Summary: "Standup", Start: start},
		Destination: &providers.CalendarEvent{UID: "event@example.com", ID: "A1", Summary: "[Work] Standup", Start: start},
	}

	decision := engine.rewriteForOptions(in, planDecision{Action: actionUnchanged})
	if decision.Action != actionUpdateDest {
		t.Fatalf("action = %q, want %q", decision.Action, actionUpdateDest)
	}
	if decision.Evidence == "" {
		t.Fatal("the rewrite did not explain itself")
	}

	// Once the copy was written under these options the rule stops firing, which
	// is what keeps a run from rewriting everything on every pass.
	in.Link.OptionsFingerprint = engine.options().fingerprint
	if got := engine.rewriteForOptions(in, planDecision{Action: actionUnchanged}); got.Action != actionUnchanged {
		t.Fatalf("action = %q, want %q", got.Action, actionUnchanged)
	}

	// A pairing that was never written (no destination id yet) is left to the
	// normal planning, which creates the copy with the options applied.
	in.Link.OptionsFingerprint = ""
	in.Destination.ID = ""
	if got := engine.rewriteForOptions(in, planDecision{Action: actionUnchanged}); got.Action != actionUnchanged {
		t.Fatalf("action = %q, want %q", got.Action, actionUnchanged)
	}

	// A side without a writable target (a feed destination, a preview of a
	// one-way job) never rewrites.
	engine.target = &calendarSide{role: "destination"}
	if got := engine.rewriteForOptions(in, planDecision{Action: actionUnchanged}); got.Action != actionUnchanged {
		t.Fatalf("action = %q, want %q", got.Action, actionUnchanged)
	}
}

func TestRewriteForOptionsLeavesARealDecisionAlone(t *testing.T) {
	start := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	engine := &calendarEngine{
		job:    &models.CalendarJob{TitlePrefix: "[Work]"},
		target: newOptionDestination("target"),
	}
	in := planInput{
		Link:        &models.CalendarEventLink{UID: "event@example.com"},
		Source:      &providers.CalendarEvent{UID: "event@example.com", Summary: "Standup", Start: start},
		Destination: &providers.CalendarEvent{UID: "event@example.com", ID: "A1", Summary: "[Work] Standup", Start: start},
	}
	for _, action := range []calendarAction{
		actionCreateDest, actionUpdateDest, actionUpdateSource, actionDeleteDest,
		actionDeleteSource, actionSkip, actionConflict,
	} {
		if got := engine.rewriteForOptions(in, planDecision{Action: action}); got.Action != action {
			t.Fatalf("action %q was turned into %q", action, got.Action)
		}
	}
}

// TestOptionsDriftedReopensAnUnmodifiedSubscription covers the case a conditional
// download would otherwise miss: the feed answers "not modified", yet the copies
// were written under older options and have to be re-listed to be rewritten.
func TestOptionsDriftedReopensAnUnmodifiedSubscription(t *testing.T) {
	window := providers.CalendarWindow{
		Start: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	inWindow := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	engine := &calendarEngine{
		job:    &models.CalendarJob{TitlePrefix: "[Work]"},
		target: newOptionDestination("target"),
		window: window,
		links: map[string]models.CalendarEventLink{
			"event@example.com": {DestinationEventID: "A1", DestinationStart: inWindow},
		},
	}
	if !engine.optionsDrifted() {
		t.Fatal("a copy written without the options did not reopen the listing")
	}

	// Recording the options of this run ends the drift.
	link := engine.links["event@example.com"]
	link.OptionsFingerprint = engine.options().fingerprint
	engine.links["event@example.com"] = link
	if engine.optionsDrifted() {
		t.Fatal("a copy written under the current options reopened the listing")
	}

	// A pairing whose copy sits outside the window cannot be rewritten by this
	// run: the destination event would not be listed either.
	engine.job = &models.CalendarJob{}
	engine.links["event@example.com"] = models.CalendarEventLink{
		DestinationEventID: "A1",
		DestinationStart:   time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC),
	}
	if engine.optionsDrifted() {
		t.Fatal("a copy outside the window reopened the listing")
	}

	// A link without a destination id is a pairing the run has not written yet;
	// its creation carries the options on its own.
	engine.job = &models.CalendarJob{TitlePrefix: "[Work]"}
	engine.links["event@example.com"] = models.CalendarEventLink{DestinationStart: inWindow}
	if engine.optionsDrifted() {
		t.Fatal("an unwritten pairing reopened the listing")
	}

	// A side without a writable target never reopens anything.
	engine.target = &calendarSide{role: "destination"}
	engine.links["event@example.com"] = models.CalendarEventLink{DestinationEventID: "A1", DestinationStart: inWindow}
	if engine.optionsDrifted() {
		t.Fatal("a job without a writable target reopened the listing")
	}
}

// TestFreeBusyMapRoundTripsThroughItsCanonicalForm pins the storage format: only
// the entries that change something are kept, the order is stable, and an
// identity mapping is not stored as "{}".
func TestFreeBusyMapRoundTripsThroughItsCanonicalForm(t *testing.T) {
	encoded, err := NormalizeFreeBusyMap(`{"busy":"free","tentative":"busy","free":"free"}`)
	if err != nil {
		t.Fatalf("NormalizeFreeBusyMap() error = %v", err)
	}
	if want := `{"busy":"free","tentative":"busy"}`; encoded != want {
		t.Fatalf("canonical form = %q, want %q", encoded, want)
	}
	if again, err := NormalizeFreeBusyMap(encoded); err != nil || again != encoded {
		t.Fatalf("the canonical form is not stable: %q (%v)", again, err)
	}
	if got, err := NormalizeFreeBusyMap(`{"free":"free"}`); err != nil || got != "" {
		t.Fatalf("an identity mapping was stored as %q (%v), want the empty string", got, err)
	}
	if got, err := NormalizeFreeBusyMap(""); err != nil || got != "" {
		t.Fatalf("an absent mapping answered %q (%v)", got, err)
	}
}

// TestFreeBusyMapRefusesWhatItCannotWrite is the guard between a payload and a
// provider: a label nobody knows must fail the save instead of silently clearing
// the destination state.
func TestFreeBusyMapRefusesWhatItCannotWrite(t *testing.T) {
	cases := []string{
		`{`,
		`{"holiday":"free"}`,
		`{"busy":"holiday"}`,
		`{"busy":""}`,
		`{"":"free"}`,
	}
	for _, raw := range cases {
		if _, err := NormalizeFreeBusyMap(raw); err == nil {
			t.Errorf("NormalizeFreeBusyMap(%q) accepted a value it cannot write", raw)
		}
	}
}

// TestFreeBusyMapOfExpandsTheLegacyOverride is what keeps an existing job doing
// what its operator configured: the old field forced one state on every copy, so
// it expands across the four show-as states and leaves cancellations alone.
func TestFreeBusyMapOfExpandsTheLegacyOverride(t *testing.T) {
	legacy := freeBusyMapOf(&models.CalendarJob{TransparencyOverride: string(models.CalendarFreeBusyFree)})
	for _, state := range []providers.FreeBusy{
		providers.FreeBusyFree,
		providers.FreeBusyTentative,
		providers.FreeBusyBusy,
		providers.FreeBusyOutOfOffice,
	} {
		if got, _ := legacy.apply(state); got != providers.FreeBusyFree {
			t.Errorf("apply(%q) = %q, want free", state, got)
		}
	}
	if _, changed := legacy.apply(providers.FreeBusyCancelled); changed {
		t.Fatal("the legacy override touched a cancelled occurrence")
	}

	// A saved map wins over the deprecated field.
	both := freeBusyMapOf(&models.CalendarJob{
		FreeBusyMap:          `{"busy":"free"}`,
		TransparencyOverride: string(models.CalendarFreeBusyBusy),
	})
	if got, _ := both.apply(providers.FreeBusyBusy); got != providers.FreeBusyFree {
		t.Fatalf("the deprecated override beat the saved mapping: %q", got)
	}
	if got, _ := both.apply(providers.FreeBusyTentative); got != providers.FreeBusyTentative {
		t.Fatalf("a state the map does not mention was changed to %q", got)
	}
}

// TestApplyToSendsACancelledOccurrenceToTheMappedState covers the row that makes
// the map useful for a cancelled occurrence: keeping it cancelled is the identity,
// and sending it to a show-as state restores the time in the destination.
func TestApplyToSendsACancelledOccurrenceToTheMappedState(t *testing.T) {
	keep := optionsOf(&models.CalendarJob{FreeBusyMap: `{"busy":"free"}`})
	cancelled := keep.applyTo(providers.CalendarEventInput{Cancelled: true, FreeBusy: providers.FreeBusyCancelled})
	if !cancelled.Cancelled || cancelled.FreeBusy != providers.FreeBusyCancelled {
		t.Fatalf("an unmapped cancellation was rewritten: %+v", cancelled)
	}

	visible := optionsOf(&models.CalendarJob{FreeBusyMap: `{"cancelled":"free"}`})
	restored := visible.applyTo(providers.CalendarEventInput{Cancelled: true, FreeBusy: providers.FreeBusyCancelled})
	if restored.Cancelled {
		t.Fatal("the mapped cancellation stayed cancelled")
	}
	if restored.FreeBusy != providers.FreeBusyFree {
		t.Fatalf("free/busy = %q, want free", restored.FreeBusy)
	}
}

// TestStaleFreeBusyRewritesOnlyWhenTheStateWasRecorded is what keeps the upgrade
// from rewriting every paired event once: a link written before the columns existed
// has no recorded state and is adopted silently.
func TestStaleFreeBusyRewritesOnlyWhenTheStateWasRecorded(t *testing.T) {
	start := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	engine := &calendarEngine{
		job:    &models.CalendarJob{FreeBusyMap: `{"busy":"free"}`},
		target: newOptionDestination("target"),
	}
	in := planInput{
		Link:        &models.CalendarEventLink{UID: "event@example.com"},
		Source:      &providers.CalendarEvent{UID: "event@example.com", Summary: "Standup", Start: start, FreeBusy: providers.FreeBusyBusy},
		Destination: &providers.CalendarEvent{UID: "event@example.com", ID: "A1", Summary: "Standup", Start: start, FreeBusy: providers.FreeBusyBusy},
	}

	// Nothing recorded: the mapping is adopted without a write.
	if stale, _ := engine.staleFreeBusy(in); stale {
		t.Fatal("a link without a recorded state was rewritten")
	}
	in.Link.DestinationFreeBusy = string(providers.FreeBusyBusy)
	stale, evidence := engine.staleFreeBusy(in)
	if !stale || evidence == "" {
		t.Fatalf("stale = %t, evidence = %q, want a rewrite", stale, evidence)
	}
	in.Link.DestinationFreeBusy = string(providers.FreeBusyFree)
	if stale, _ := engine.staleFreeBusy(in); stale {
		t.Fatal("a copy already in the mapped state was rewritten again")
	}
	// A job without a mapping never chases the state.
	engine.job = &models.CalendarJob{}
	if stale, _ := engine.staleFreeBusy(in); stale {
		t.Fatal("a job without a mapping rewrote an event")
	}
}
