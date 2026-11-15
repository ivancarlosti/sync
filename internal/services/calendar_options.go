package services

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// A calendar job can shape the copy it writes into the destination: a title
// decoration (a marker such as "[Work] " in front of the summary) and a free/busy
// override. Both are one-sided by nature — the decoration exists only next to the
// original, and the override only on the copy — so they cannot be part of the
// event content that both sides agree on.
//
// That is what the options fingerprint on the link is for: a run that finds both
// sides unchanged but the recorded options stale rewrites the destination copy
// once. Without it a job whose prefix was just set would decorate the events of
// its next new event only, which is worse than not offering the option at all.

// eventOptions is the normalised option set of one job.
type eventOptions struct {
	prefix       string
	suffix       string
	transparency string
	// fingerprint identifies the values and is empty when no option is set, so a
	// job that never used them keeps the empty column it always had.
	fingerprint string
}

// optionsOf reads and normalises the options of a job.
func optionsOf(job *models.CalendarJob) eventOptions {
	if job == nil {
		return eventOptions{}
	}
	options := eventOptions{
		prefix:       strings.TrimSpace(job.TitlePrefix),
		suffix:       strings.TrimSpace(job.TitleSuffix),
		transparency: models.CalendarFreeBusy(job.TransparencyOverride).Transparency(),
	}
	if options.prefix == "" && options.suffix == "" && options.transparency == "" {
		return options
	}
	joined := strings.Join([]string{options.prefix, options.suffix, options.transparency}, "\x1f")
	sum := sha256.Sum256([]byte(joined))
	options.fingerprint = hex.EncodeToString(sum[:])[:16]
	return options
}

// decorate builds the summary the destination receives: prefix, summary, suffix,
// joined by one space each. A single space is the only separator the engine
// invents, and undecorate removes exactly that.
func (o eventOptions) decorate(summary string) string {
	parts := make([]string, 0, 3)
	if o.prefix != "" {
		parts = append(parts, o.prefix)
	}
	if trimmed := strings.TrimSpace(summary); trimmed != "" {
		parts = append(parts, trimmed)
	}
	if o.suffix != "" {
		parts = append(parts, o.suffix)
	}
	return strings.Join(parts, " ")
}

// undecorate removes the decoration from a summary read back from the
// destination, so an edit made next to the copy does not silently carry the
// marker into the source calendar. It is best effort: a summary the operator
// edited into the same shape as the decoration cannot be told apart.
func (o eventOptions) undecorate(summary string) string {
	trimmed := summary
	if o.prefix != "" && strings.HasPrefix(trimmed, o.prefix) {
		trimmed = strings.TrimPrefix(strings.TrimPrefix(trimmed, o.prefix), " ")
	}
	if o.suffix != "" && strings.HasSuffix(trimmed, o.suffix) {
		trimmed = strings.TrimSuffix(strings.TrimSuffix(trimmed, o.suffix), " ")
	}
	return trimmed
}

// applyTo returns the body of a destination write: the summary decorated and, when
// the job forces it, the free/busy flag replaced.
func (o eventOptions) applyTo(input providers.CalendarEventInput) providers.CalendarEventInput {
	if o.prefix != "" || o.suffix != "" {
		input.Summary = o.decorate(input.Summary)
	}
	if o.transparency != "" {
		input.Transparency = o.transparency
	}
	return input
}

// options are the destination-only options of this job.
func (e *calendarEngine) options() eventOptions {
	return optionsOf(e.job)
}

// writeInput builds the body of a write to side from an event of the other side. A
// destination write carries the options; a source write strips the decoration, so
// the pairing's marker never lands in the calendar people read.
func (e *calendarEngine) writeInput(uid string, side *calendarSide, from providers.CalendarEvent) providers.CalendarEventInput {
	input := providers.EventInputFrom(from)
	input.UID = destinationUID(uid)
	options := e.options()
	if side.role == "destination" {
		return options.applyTo(input)
	}
	input.Summary = options.undecorate(input.Summary)
	return input
}

// staleOptions reports whether the destination copy was written under other
// options than the ones the job holds now, plus the evidence for the rewrite. It
// is only ever asked about a pairing whose two sides already agree, so the
// difference cannot be content.
func (e *calendarEngine) staleOptions(in planInput) (bool, string) {
	options := e.options()
	if in.Link == nil || in.Source == nil || in.Destination == nil {
		return false, ""
	}
	if e.target == nil || !e.target.writable() || strings.TrimSpace(in.Destination.ID) == "" {
		return false, ""
	}
	if in.Link.OptionsFingerprint == options.fingerprint {
		return false, ""
	}
	return true, "the options of the pairing changed since the destination was written"
}

// optionsDrifted reports whether some recorded pairing was written under other
// options than the ones the job holds now. A subscription answered "not modified"
// is not the end of the work in that case: the events have to be re-listed for the
// destination to be rewritten.
func (e *calendarEngine) optionsDrifted() bool {
	options := e.options()
	if e.target == nil || !e.target.writable() {
		return false
	}
	for _, link := range e.links {
		if link.OptionsFingerprint == options.fingerprint {
			continue
		}
		if strings.TrimSpace(link.DestinationEventID) == "" {
			continue
		}
		if withinWindow(link.DestinationStart, e.window) {
			return true
		}
	}
	return false
}
