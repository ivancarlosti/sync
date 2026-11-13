package services

import (
	"fmt"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// The pairing decision is a pure function of what both sides look like and what
// the previous run recorded. Keeping it out of the executor is what makes the
// engine testable without a provider: every rule below has a unit test and the
// reason it reports is the evidence the UI shows.

// calendarAction is the outcome of one pairing decision. The names double as the
// values stored in calendar_run_items.action, so the activity feed can group by
// them without a translation table.
type calendarAction string

const (
	actionUnchanged    calendarAction = "unchanged"
	actionCreateDest   calendarAction = "create_destination"
	actionUpdateDest   calendarAction = "update_destination"
	actionDeleteDest   calendarAction = "delete_destination"
	actionCreateSource calendarAction = "create_source"
	actionUpdateSource calendarAction = "update_source"
	actionDeleteSource calendarAction = "delete_source"
	actionForgetLink   calendarAction = "forget_link"
	actionConflict     calendarAction = "conflict"
	actionSkip         calendarAction = "skip"
)

// mutating reports whether the action changes a calendar. It decides whether a
// decision is worth a run item and a counter.
func (a calendarAction) mutating() bool {
	switch a {
	case actionCreateDest, actionUpdateDest, actionDeleteDest,
		actionCreateSource, actionUpdateSource, actionDeleteSource:
		return true
	default:
		return false
	}
}

// planInput is everything the pairing rule may look at. The fingerprints are
// computed by the caller from the events it listed, so the rule stays free of
// provider knowledge.
type planInput struct {
	// Link is the recorded state, nil for an event the engine has not seen.
	Link *models.CalendarEventLink
	// Source and Destination are the listed events, nil when absent.
	Source      *providers.CalendarEvent
	Destination *providers.CalendarEvent
	// SourceFingerprint and DestFingerprint describe the listed events.
	SourceFingerprint string
	DestFingerprint   string
	// SourceInWindow and DestInWindow report whether the last known start the
	// link recorded is inside the listing window. A side that is missing while
	// its start is outside the window was not deleted: it simply was not listed.
	SourceInWindow bool
	DestInWindow   bool

	Backward      bool
	Policy        models.ConflictPolicy
	DeleteMissing bool
}

// planDecision is the action and the evidence for it.
type planDecision struct {
	Action   calendarAction
	Evidence string
}

// decide pairs one event. It is total: every combination of present sides, link
// age and policy answers something, and the answer says why.
func decide(in planInput) planDecision {
	switch {
	case in.Source != nil && in.Destination != nil:
		return decideBothPresent(in)
	case in.Source != nil:
		return decideSourceOnly(in)
	case in.Destination != nil:
		return decideDestinationOnly(in)
	default:
		return decideNeitherPresent(in)
	}
}

// decideBothPresent is the ordinary case: two copies of the same event, each with
// a fingerprint that may or may not match what the last run stored.
func decideBothPresent(in planInput) planDecision {
	link := in.Link
	if link == nil {
		// The same UID on both sides without a recorded link means the two
		// calendars already agree (a copy the engine made, or an event that
		// exists in both by coincidence), so the link is recorded and nothing
		// is written.
		return planDecision{Action: actionUnchanged, Evidence: "both sides already hold this event"}
	}
	sourceChanged := in.SourceFingerprint != link.SourceFingerprint
	destChanged := in.DestFingerprint != link.DestFingerprint

	switch {
	case !sourceChanged && !destChanged:
		return planDecision{Action: actionUnchanged, Evidence: "neither side changed since the last run"}
	case sourceChanged && !destChanged:
		return planDecision{Action: actionUpdateDest, Evidence: "the source changed since the last run"}
	case destChanged && !sourceChanged:
		if !in.Backward {
			return planDecision{Action: actionUnchanged, Evidence: "the destination changed; a one-way job does not write back"}
		}
		return planDecision{Action: actionUpdateSource, Evidence: "the destination changed since the last run"}
	}

	// Both sides changed. Identical content means the edits converged (or the
	// same change was made on both sides) and there is nothing to write.
	if in.SourceFingerprint == in.DestFingerprint {
		return planDecision{Action: actionUnchanged, Evidence: "both sides changed to the same content"}
	}
	return resolveConflict(in)
}

// resolveConflict applies the job policy. It answers unchanged only when the
// policy says the source keeps its version, which is also what a one-way job
// does with a destination edit: the destination is left alone rather than
// reverted, so a local annotation the user typed is not thrown away.
func resolveConflict(in planInput) planDecision {
	switch in.Policy {
	case models.ConflictSourceWins:
		return planDecision{Action: actionUpdateDest, Evidence: "both sides changed; the source wins by policy"}
	case models.ConflictDestinationWins:
		if !in.Backward {
			return planDecision{Action: actionUnchanged, Evidence: "both sides changed; the destination wins by policy and a one-way job does not write back"}
		}
		return planDecision{Action: actionUpdateSource, Evidence: "both sides changed; the destination wins by policy"}
	case models.ConflictNewestWins, "":
		sourceStamp := in.Source.UpdatedAt
		destStamp := in.Destination.UpdatedAt
		if sourceStamp.IsZero() && destStamp.IsZero() {
			// Neither provider published a modification time (Graph does not),
			// so "newest" cannot be decided. Skipping leaves both copies as
			// they are and reports it, which is better than a guess that
			// overwrites one of them.
			return planDecision{Action: actionConflict, Evidence: "both sides changed and neither publishes a modification time"}
		}
		if !sourceStamp.IsZero() && (destStamp.IsZero() || sourceStamp.After(destStamp)) {
			return planDecision{Action: actionUpdateDest, Evidence: fmt.Sprintf("both sides changed; the source is newer (%s)", describeAge(sourceStamp, destStamp))}
		}
		if destStamp.IsZero() || destStamp.After(sourceStamp) {
			if !in.Backward {
				return planDecision{Action: actionUnchanged, Evidence: "both sides changed; the destination is newer and a one-way job does not write back"}
			}
			return planDecision{Action: actionUpdateSource, Evidence: fmt.Sprintf("both sides changed; the destination is newer (%s)", describeAge(destStamp, sourceStamp))}
		}
		return planDecision{Action: actionConflict, Evidence: "both sides changed with the same modification time"}
	default:
		return planDecision{Action: actionConflict, Evidence: fmt.Sprintf("unknown conflict policy %q", in.Policy)}
	}
}

// describeAge renders how much newer one stamp is than the other, the way the
// file engine reports it, and says so when the other side is missing.
func describeAge(newer, older time.Time) string {
	if older.IsZero() {
		return "the other side has no modification time"
	}
	return "by " + newer.Sub(older).Round(time.Second).String()
}

// decideSourceOnly handles an event the source lists and the destination does
// not. The link's recorded start decides whether the destination lost the event
// or whether it merely sits outside the listing window.
func decideSourceOnly(in planInput) planDecision {
	if in.Link == nil {
		return planDecision{Action: actionCreateDest, Evidence: "the event is not in the destination calendar yet"}
	}
	if !in.DestInWindow {
		return planDecision{Action: actionUnchanged, Evidence: "the destination copy starts outside the listing window"}
	}
	if in.DeleteMissing {
		return planDecision{Action: actionDeleteSource, Evidence: "the destination copy was deleted; the policy removes the source too"}
	}
	return planDecision{Action: actionCreateDest, Evidence: "the destination copy was deleted; recreating it"}
}

// decideDestinationOnly handles the mirror case. An .ics subscription and a
// one-way job have no writable source, so a missing source either recreates the
// destination's event there (bidirectional) or is reported.
func decideDestinationOnly(in planInput) planDecision {
	if in.Link == nil {
		if !in.Backward {
			return planDecision{Action: actionSkip, Evidence: "the event exists only in the destination and a one-way job does not write back"}
		}
		return planDecision{Action: actionCreateSource, Evidence: "the event is not in the source calendar yet"}
	}
	if !in.SourceInWindow {
		return planDecision{Action: actionUnchanged, Evidence: "the source copy starts outside the listing window"}
	}
	if in.DeleteMissing {
		if !in.Backward {
			return planDecision{Action: actionUnchanged, Evidence: "the source copy is gone; a one-way job keeps the destination copy"}
		}
		return planDecision{Action: actionDeleteDest, Evidence: "the source copy was deleted; the policy removes the destination too"}
	}
	if !in.Backward {
		return planDecision{Action: actionUnchanged, Evidence: "the source copy is gone and a one-way job keeps the destination copy"}
	}
	return planDecision{Action: actionCreateSource, Evidence: "the source copy was deleted; recreating it"}
}

// decideNeitherPresent handles an event both sides stop listing. The link is only
// forgotten when both recorded starts are inside their windows: otherwise the
// event may still exist on both sides and only fell out of range.
func decideNeitherPresent(in planInput) planDecision {
	if in.Link == nil {
		return planDecision{Action: actionSkip, Evidence: "the event is on neither side"}
	}
	if !in.SourceInWindow || !in.DestInWindow {
		return planDecision{Action: actionUnchanged, Evidence: "both sides are outside the listing window"}
	}
	return planDecision{Action: actionForgetLink, Evidence: "the event is gone from both sides"}
}
