package services

import (
	"context"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// seriesProvider records what the engine writes and answers a plausible event, so
// the series rules can be exercised without a provider.
type seriesProvider struct {
	providers.CalendarProvider
	created []providers.CalendarEventInput
	updated []string
}

func (p *seriesProvider) CreateCalendarEvent(_ context.Context, _ providers.Credentials, _ *providers.Tokens, _ string, input providers.CalendarEventInput) (*providers.CalendarEvent, error) {
	p.created = append(p.created, input)
	return &providers.CalendarEvent{
		ID:            "dest-" + input.UID,
		UID:           input.UID,
		Summary:       input.Summary,
		Start:         input.Start,
		End:           input.End,
		AllDay:        input.AllDay,
		TimeZone:      input.TimeZone,
		FreeBusy:      input.FreeBusy,
		Recurrence:    input.Recurrence,
		OriginalStart: input.OriginalStart,
		Cancelled:     input.Cancelled,
		Kind:          providers.EventKindSingle,
	}, nil
}

func (p *seriesProvider) UpdateCalendarEvent(_ context.Context, _ providers.Credentials, _ *providers.Tokens, _, eventID string, input providers.CalendarEventInput) (*providers.CalendarEvent, error) {
	p.updated = append(p.updated, eventID)
	return &providers.CalendarEvent{
		ID:       eventID,
		UID:      input.UID,
		Start:    input.Start,
		End:      input.End,
		FreeBusy: input.FreeBusy,
	}, nil
}

// newSeriesEngine builds an engine with both sides resolved and no store access on
// the paths the series rules take.
func newSeriesEngine(job *models.CalendarJob, source, target []providers.CalendarEvent) (*calendarEngine, *seriesProvider) {
	writer := &seriesProvider{}
	engine := &calendarEngine{
		service: &CalendarService{now: time.Now},
		job:     job,
		run:     &models.CalendarRun{JobID: job.ID},
		links:   map[string]models.CalendarEventLink{},
		pending: map[string]models.CalendarEventLink{},
		window: providers.CalendarWindow{
			Start: time.Now().AddDate(0, 0, -30),
			End:   time.Now().AddDate(1, 0, 0),
		},
	}
	engine.source = &calendarSide{role: "source", events: indexEvents(source)}
	engine.target = &calendarSide{role: "destination", calendar: "target", events: indexEvents(target)}
	engine.target.provider = writer
	return engine, writer
}

// seriesEvents is one weekly series with the second occurrence cancelled, as a
// provider lists it once the master is read alongside its exception.
func seriesEvents() []providers.CalendarEvent {
	first := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	second := first.AddDate(0, 0, 7)
	return []providers.CalendarEvent{
		{
			ID:         "master",
			UID:        "series@example.com",
			Summary:    "Standup",
			Start:      first,
			End:        first.Add(30 * time.Minute),
			TimeZone:   "UTC",
			FreeBusy:   providers.FreeBusyBusy,
			Recurring:  true,
			Kind:       providers.EventKindMaster,
			Recurrence: []string{"RRULE:FREQ=WEEKLY;BYDAY=MO"},
		},
		{
			ID:            "master_20260309T090000Z",
			UID:           "series@example.com",
			Summary:       "Standup",
			Start:         second,
			End:           second.Add(30 * time.Minute),
			TimeZone:      "UTC",
			FreeBusy:      providers.FreeBusyCancelled,
			Recurring:     true,
			Kind:          providers.EventKindException,
			SeriesID:      "master",
			OriginalStart: second,
			Cancelled:     true,
		},
	}
}

// TestASeriesIsCopiedWithItsOccurrences is the whole feature in one run: the
// master is created first, and the cancelled occurrence is written through the id
// the destination answered.
func TestASeriesIsCopiedWithItsOccurrences(t *testing.T) {
	engine, writer := newSeriesEngine(&models.CalendarJob{ID: 1}, seriesEvents(), nil)
	ctx := context.Background()
	if err := engine.reconcileEvents(ctx); err != nil {
		t.Fatalf("reconcileEvents() error = %v", err)
	}
	if err := engine.reconcileExceptions(ctx); err != nil {
		t.Fatalf("reconcileExceptions() error = %v", err)
	}

	if len(writer.created) != 2 {
		t.Fatalf("writes = %d, want the master and its occurrence", len(writer.created))
	}
	master := writer.created[0]
	if len(master.Recurrence) != 1 || master.Recurrence[0] != "RRULE:FREQ=WEEKLY;BYDAY=MO" {
		t.Fatalf("the master was written without its rule: %+v", master)
	}
	if master.SeriesID != "" {
		t.Fatalf("the master was written as an occurrence of %q", master.SeriesID)
	}
	occurrence := writer.created[1]
	if occurrence.SeriesID == "" || occurrence.SeriesID == "master" {
		t.Fatalf("the occurrence targets %q, want the destination's own master id", occurrence.SeriesID)
	}
	if !occurrence.OriginalStart.Equal(time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("original start = %s", occurrence.OriginalStart)
	}
	if !occurrence.Cancelled {
		t.Fatal("the cancellation was not carried into the copy")
	}

	// The occurrence is paired under the compound key, and the master under its
	// own, so a later run can tell the two apart.
	masterKey := "series@example.com"
	occurrenceKey := "series@example.com" + seriesKeySeparator + "2026-03-09T09:00:00Z"
	if _, ok := engine.pending[masterKey]; !ok {
		t.Fatalf("the master was not recorded under %q: %v", masterKey, engine.pending)
	}
	if _, ok := engine.pending[occurrenceKey]; !ok {
		t.Fatalf("the occurrence was not recorded under %q", occurrenceKey)
	}
	if got := engine.pending[occurrenceKey].LastAction; got != string(actionCancelDest) {
		t.Fatalf("action = %q, want %q", got, actionCancelDest)
	}
}

// TestSeriesMastersOnlySkipsTheOccurrences covers the option that turns the second
// phase off: the rule travels, the occurrences do not, and the run says so.
func TestSeriesMastersOnlySkipsTheOccurrences(t *testing.T) {
	job := &models.CalendarJob{ID: 1, SeriesExceptions: string(models.CalendarSeriesSkip)}
	engine, writer := newSeriesEngine(job, seriesEvents(), nil)
	ctx := context.Background()
	if err := engine.reconcileEvents(ctx); err != nil {
		t.Fatalf("reconcileEvents() error = %v", err)
	}
	if err := engine.reconcileExceptions(ctx); err != nil {
		t.Fatalf("reconcileExceptions() error = %v", err)
	}
	if len(writer.created) != 1 {
		t.Fatalf("writes = %d, want the master only", len(writer.created))
	}
	if writer.created[0].SeriesID != "" {
		t.Fatal("the only write was an occurrence")
	}
	if engine.run.EventsSkipped != 1 {
		t.Fatalf("skipped = %d, want the occurrence", engine.run.EventsSkipped)
	}
}

// TestAnUnchangedSeriesIsNotRewritten is the guard against a series copy that
// grows: both copies agree, the link says so, and the run writes nothing.
func TestAnUnchangedSeriesIsNotRewritten(t *testing.T) {
	events := seriesEvents()
	source := indexEvents(events)
	destination := indexEvents(events)
	engine, writer := newSeriesEngine(&models.CalendarJob{ID: 1}, events, events)
	for key, event := range source {
		engine.links[key] = models.CalendarEventLink{
			JobID:              1,
			UID:                key,
			SourceEventID:      event.ID,
			DestinationEventID: destination[key].ID,
			SourceFingerprint:  providers.EventFingerprint(event),
			DestFingerprint:    providers.EventFingerprint(destination[key]),
			SourceStart:        event.Start.UTC(),
			DestinationStart:   destination[key].Start.UTC(),
		}
	}
	ctx := context.Background()
	if err := engine.reconcileEvents(ctx); err != nil {
		t.Fatalf("reconcileEvents() error = %v", err)
	}
	if err := engine.reconcileExceptions(ctx); err != nil {
		t.Fatalf("reconcileExceptions() error = %v", err)
	}
	if len(writer.created) != 0 || len(writer.updated) != 0 {
		t.Fatalf("an unchanged series was written: created %d, updated %d", len(writer.created), len(writer.updated))
	}
}

// TestIndexEventsKeysAnOccurrenceByItsStart pins the pairing key: an expanded
// occurrence is dropped, and an exception is keyed by the UID of its master even
// when the provider publishes another one on the occurrence.
func TestIndexEventsKeysAnOccurrenceByItsStart(t *testing.T) {
	first := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	second := first.AddDate(0, 0, 7)
	indexed := indexEvents([]providers.CalendarEvent{
		{ID: "master", UID: "series@example.com", Kind: providers.EventKindMaster, Start: first, End: first.Add(time.Hour)},
		{ID: "instance", UID: "series@example.com", Kind: providers.EventKindOccurrence, SeriesID: "master", OriginalStart: second, Start: second, End: second.Add(time.Hour)},
		{ID: "exception", UID: "per-instance@example.com", Kind: providers.EventKindException, SeriesID: "master", OriginalStart: second, Start: second.Add(time.Hour), End: second.Add(2 * time.Hour)},
	})
	if len(indexed) != 2 {
		t.Fatalf("indexed %d events, want the master and the exception: %v", len(indexed), indexed)
	}
	if _, ok := indexed["series@example.com"]; !ok {
		t.Fatal("the master was not keyed by its UID")
	}
	key := "series@example.com" + seriesKeySeparator + "2026-03-09T09:00:00Z"
	event, ok := indexed[key]
	if !ok {
		t.Fatalf("the exception was not keyed by its series and original start: %v", indexed)
	}
	if !event.OriginalStart.Equal(second) {
		t.Fatalf("original start = %s", event.OriginalStart)
	}
}

// TestDestinationUIDStripsTheSeriesSuffix keeps the compound key out of the
// provider field: the UID of a copy is the UID of the series, never its pairing
// key.
func TestDestinationUIDStripsTheSeriesSuffix(t *testing.T) {
	key := "series@example.com" + seriesKeySeparator + "2026-03-09T09:00:00Z"
	if got := destinationUID(key); got != "series@example.com" {
		t.Fatalf("destinationUID() = %q", got)
	}
}
