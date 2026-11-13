package services

import (
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

func planEvent(summary string, start time.Time, updated time.Time) *providers.CalendarEvent {
	return &providers.CalendarEvent{
		UID:       "event@example.com",
		Summary:   summary,
		Start:     start,
		End:       start.Add(time.Hour),
		UpdatedAt: updated,
	}
}

func planLink(sourceFingerprint, destFingerprint string) *models.CalendarEventLink {
	return &models.CalendarEventLink{
		UID:               "event@example.com",
		SourceFingerprint: sourceFingerprint,
		DestFingerprint:   destFingerprint,
	}
}

func TestDecideCreatesTheDestinationOfANewSourceEvent(t *testing.T) {
	decision := decide(planInput{
		Source:            planEvent("Standup", time.Now(), time.Time{}),
		SourceFingerprint: "src",
	})
	if decision.Action != actionCreateDest {
		t.Fatalf("action = %s, want %s", decision.Action, actionCreateDest)
	}
}

func TestDecideLeavesAnUnchangedPairAlone(t *testing.T) {
	decision := decide(planInput{
		Link:              planLink("same", "same"),
		Source:            planEvent("Standup", time.Now(), time.Time{}),
		Destination:       planEvent("Standup", time.Now(), time.Time{}),
		SourceFingerprint: "same",
		DestFingerprint:   "same",
	})
	if decision.Action != actionUnchanged {
		t.Fatalf("action = %s, want %s", decision.Action, actionUnchanged)
	}
}

func TestDecideWritesASourceEditToTheDestination(t *testing.T) {
	decision := decide(planInput{
		Link:              planLink("before", "same"),
		Source:            planEvent("Standup v2", time.Now(), time.Time{}),
		Destination:       planEvent("Standup", time.Now(), time.Time{}),
		SourceFingerprint: "after",
		DestFingerprint:   "same",
		Backward:          true,
	})
	if decision.Action != actionUpdateDest {
		t.Fatalf("action = %s, want %s", decision.Action, actionUpdateDest)
	}
}

func TestDecideIgnoresADestinationEditOfAOneWayJob(t *testing.T) {
	decision := decide(planInput{
		Link:              planLink("same", "before"),
		Source:            planEvent("Standup", time.Now(), time.Time{}),
		Destination:       planEvent("Standup", time.Now(), time.Time{}),
		SourceFingerprint: "same",
		DestFingerprint:   "after",
	})
	if decision.Action != actionUnchanged {
		t.Fatalf("action = %s, want %s (a one-way job must not revert a destination edit)", decision.Action, actionUnchanged)
	}
}

func TestDecideWritesADestinationEditBackWhenBidirectional(t *testing.T) {
	decision := decide(planInput{
		Link:              planLink("same", "before"),
		Source:            planEvent("Standup", time.Now(), time.Time{}),
		Destination:       planEvent("Standup", time.Now(), time.Time{}),
		SourceFingerprint: "same",
		DestFingerprint:   "after",
		Backward:          true,
	})
	if decision.Action != actionUpdateSource {
		t.Fatalf("action = %s, want %s", decision.Action, actionUpdateSource)
	}
}

func TestDecideStopsOnAnIdenticalEditOnBothSides(t *testing.T) {
	decision := decide(planInput{
		Link:              planLink("before", "before"),
		Source:            planEvent("Standup", time.Now(), time.Time{}),
		Destination:       planEvent("Standup", time.Now(), time.Time{}),
		SourceFingerprint: "after",
		DestFingerprint:   "after",
	})
	if decision.Action != actionUnchanged {
		t.Fatalf("action = %s, want %s", decision.Action, actionUnchanged)
	}
}

func TestDecideResolvesNewestWinsByTheModificationStamp(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	older := start.Add(-time.Hour)
	newer := start.Add(time.Hour)

	decision := decide(planInput{
		Link:              planLink("before", "before"),
		Source:            planEvent("Source", start, newer),
		Destination:       planEvent("Destination", start, older),
		SourceFingerprint: "source-after",
		DestFingerprint:   "dest-after",
		Policy:            models.ConflictNewestWins,
		Backward:          true,
	})
	if decision.Action != actionUpdateDest {
		t.Fatalf("action = %s, want %s", decision.Action, actionUpdateDest)
	}

	decision = decide(planInput{
		Link:              planLink("before", "before"),
		Source:            planEvent("Source", start, older),
		Destination:       planEvent("Destination", start, newer),
		SourceFingerprint: "source-after",
		DestFingerprint:   "dest-after",
		Policy:            models.ConflictNewestWins,
		Backward:          true,
	})
	if decision.Action != actionUpdateSource {
		t.Fatalf("action = %s, want %s", decision.Action, actionUpdateSource)
	}
}

func TestDecideReportsAConflictWhenNoStampCanDecideIt(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	decision := decide(planInput{
		Link:              planLink("before", "before"),
		Source:            planEvent("Source", start, time.Time{}),
		Destination:       planEvent("Destination", start, time.Time{}),
		SourceFingerprint: "source-after",
		DestFingerprint:   "dest-after",
		Policy:            models.ConflictNewestWins,
		Backward:          true,
	})
	if decision.Action != actionConflict {
		t.Fatalf("action = %s, want %s", decision.Action, actionConflict)
	}
}

func TestDecideHonoursSourceWinsAndSkip(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	base := planInput{
		Link:              planLink("before", "before"),
		Source:            planEvent("Source", start, start),
		Destination:       planEvent("Destination", start, start),
		SourceFingerprint: "source-after",
		DestFingerprint:   "dest-after",
		Backward:          true,
	}

	sourceWins := base
	sourceWins.Policy = models.ConflictSourceWins
	if got := decide(sourceWins).Action; got != actionUpdateDest {
		t.Fatalf("source_wins action = %s, want %s", got, actionUpdateDest)
	}

	skip := base
	skip.Policy = models.ConflictSkip
	if got := decide(skip).Action; got != actionConflict {
		t.Fatalf("skip action = %s, want %s", got, actionConflict)
	}
}

func TestDecidePropagatesADestinationDeletion(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	link := planLink("same", "same")
	link.DestinationStart = start

	withDelete := planInput{
		Link:              link,
		Source:            planEvent("Standup", start, start),
		SourceFingerprint: "same",
		DestInWindow:      true,
		Backward:          true,
		DeleteMissing:     true,
	}
	if got := decide(withDelete).Action; got != actionDeleteSource {
		t.Fatalf("deleting action = %s, want %s", got, actionDeleteSource)
	}

	withRecreate := withDelete
	withRecreate.DeleteMissing = false
	if got := decide(withRecreate).Action; got != actionCreateDest {
		t.Fatalf("recreating action = %s, want %s", got, actionCreateDest)
	}
}

func TestDecideDoesNotMistakeAWindowEdgeForADeletion(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	link := planLink("same", "same")
	link.DestinationStart = start

	decision := decide(planInput{
		Link:              link,
		Source:            planEvent("Standup", start, start),
		SourceFingerprint: "same",
		DestInWindow:      false,
		Backward:          true,
		DeleteMissing:     true,
	})
	if decision.Action != actionUnchanged {
		t.Fatalf("action = %s, want %s (a copy outside the window is not a deletion)", decision.Action, actionUnchanged)
	}
}

func TestDecideForgetsAnEventGoneFromBothSides(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	link := planLink("same", "same")
	link.SourceStart = start
	link.DestinationStart = start

	decision := decide(planInput{Link: link, SourceInWindow: true, DestInWindow: true})
	if decision.Action != actionForgetLink {
		t.Fatalf("action = %s, want %s", decision.Action, actionForgetLink)
	}
}

func TestDecideAdoptsADestinationOnlyEventWhenBidirectional(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	event := planEvent("Local", start, start)

	oneWay := decide(planInput{Destination: event, DestFingerprint: "dst"})
	if oneWay.Action != actionSkip {
		t.Fatalf("one-way action = %s, want %s", oneWay.Action, actionSkip)
	}

	bidirectional := decide(planInput{Destination: event, DestFingerprint: "dst", Backward: true})
	if bidirectional.Action != actionCreateSource {
		t.Fatalf("bidirectional action = %s, want %s", bidirectional.Action, actionCreateSource)
	}
}

func TestCalendarActionReportsWhetherItMutates(t *testing.T) {
	for _, action := range []calendarAction{actionCreateDest, actionUpdateDest, actionDeleteDest, actionCreateSource, actionUpdateSource, actionDeleteSource} {
		if !action.mutating() {
			t.Fatalf("%s must be reported as mutating", action)
		}
	}
	for _, action := range []calendarAction{actionUnchanged, actionForgetLink, actionConflict, actionSkip} {
		if action.mutating() {
			t.Fatalf("%s must not be reported as mutating", action)
		}
	}
}
