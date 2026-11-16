package services

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// freeBusyStates is the order the mapping is presented and encoded in: the four
// states a calendar can show an event as, plus cancelled, which is what a
// cancelled occurrence of a series reads as.
var freeBusyStates = []providers.FreeBusy{
	providers.FreeBusyFree,
	providers.FreeBusyTentative,
	providers.FreeBusyBusy,
	providers.FreeBusyOutOfOffice,
	providers.FreeBusyCancelled,
}

// freeBusyMap is the transformation of a source state into the state the
// destination copy carries. A state the map does not mention is copied unchanged,
// which is why an empty map is the identity.
type freeBusyMap map[providers.FreeBusy]providers.FreeBusy

// knownFreeBusyState reports whether the value is one of the five states a mapping
// may name. It deliberately refuses the legacy `opaque`/`transparent` aliases:
// those are what FreeBusyName folds on read, not what an operator may configure.
func knownFreeBusyState(state providers.FreeBusy) bool {
	for _, candidate := range freeBusyStates {
		if state == candidate {
			return true
		}
	}
	return false
}

// decodeFreeBusyMap reads the canonical JSON a job stores. Every key and value is
// validated: a stored label the providers do not know would otherwise reach a
// payload and clear the destination's own state.
func decodeFreeBusyMap(raw string) (freeBusyMap, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var encoded map[string]string
	if err := json.Unmarshal([]byte(raw), &encoded); err != nil {
		return nil, fmt.Errorf("%w: the free/busy mapping is not valid JSON", ErrValidation)
	}
	decoded := make(freeBusyMap, len(encoded))
	for key, value := range encoded {
		source := providers.FreeBusyName(key)
		if source == "" || !knownFreeBusyState(source) {
			return nil, fmt.Errorf("%w: unknown free/busy state %q in the mapping", ErrValidation, key)
		}
		target := providers.FreeBusyName(value)
		if target == "" || !knownFreeBusyState(target) {
			return nil, fmt.Errorf("%w: the mapping of %q targets the unknown state %q", ErrValidation, key, value)
		}
		decoded[source] = target
	}
	return decoded, nil
}

// encodeFreeBusyMap renders the mapping canonically: only the entries that change
// something, in a stable order, so two jobs configured the same way store the same
// bytes and the fingerprint of the options is stable.
func encodeFreeBusyMap(mapping freeBusyMap) string {
	if len(mapping) == 0 {
		return ""
	}
	encoded := make(map[string]string, len(mapping))
	for _, state := range freeBusyStates {
		target, ok := mapping[state]
		if !ok || target == state {
			continue
		}
		encoded[string(state)] = string(target)
	}
	if len(encoded) == 0 {
		return ""
	}
	body, err := json.Marshal(encoded)
	if err != nil {
		return ""
	}
	return string(body)
}

// NormalizeFreeBusyMap validates the mapping an API payload carries and answers
// its canonical form. An empty mapping answers the empty string, which is what
// keeps a job without options from storing "{}".
func NormalizeFreeBusyMap(raw string) (string, error) {
	mapping, err := decodeFreeBusyMap(raw)
	if err != nil {
		return "", err
	}
	return encodeFreeBusyMap(mapping), nil
}

// apply answers the destination state of one source state. The second value
// reports whether the map changed it.
func (m freeBusyMap) apply(state providers.FreeBusy) (providers.FreeBusy, bool) {
	if len(m) == 0 {
		return state, false
	}
	target, ok := m[state]
	if !ok || target == "" || target == state {
		return state, false
	}
	return target, true
}

// freeBusyMapOf reads the mapping of a job, expanding the deprecated single
// override when no map was ever saved. The override forced every destination
// event to one state, so expanding it across the four show-as states keeps what
// an existing job does; a cancelled occurrence is deliberately left alone,
// because the override never touched it before.
func freeBusyMapOf(job *models.CalendarJob) freeBusyMap {
	if job == nil {
		return nil
	}
	if mapping, err := decodeFreeBusyMap(job.FreeBusyMap); err == nil && len(mapping) > 0 {
		return mapping
	}
	legacy := models.CalendarFreeBusy(strings.TrimSpace(job.TransparencyOverride))
	target := providers.FreeBusyName(legacy.Target())
	if target == "" {
		return nil
	}
	mapping := make(freeBusyMap, 4)
	for _, state := range freeBusyStates {
		if state == providers.FreeBusyCancelled {
			continue
		}
		mapping[state] = target
	}
	return mapping
}

// eventOptions is the normalised option set of one job.
type eventOptions struct {
	prefix   string
	suffix   string
	freeBusy freeBusyMap
	// seriesExceptions is "copy" (the default) or "skip".
	seriesExceptions models.CalendarSeriesExceptions
	// fingerprint identifies the values and is empty when no option is set, so a
	// job that never used them keeps the empty column it always had.
	fingerprint string
}

// optionsOf reads and normalises the options of a job.
func optionsOf(job *models.CalendarJob) eventOptions {
	if job == nil {
		return eventOptions{seriesExceptions: models.CalendarSeriesCopy}
	}
	series := models.CalendarSeriesExceptions(strings.TrimSpace(job.SeriesExceptions))
	if !series.Valid() {
		series = models.CalendarSeriesCopy
	}
	options := eventOptions{
		prefix:           strings.TrimSpace(job.TitlePrefix),
		suffix:           strings.TrimSpace(job.TitleSuffix),
		freeBusy:         freeBusyMapOf(job),
		seriesExceptions: series,
	}
	if options.prefix == "" && options.suffix == "" && len(options.freeBusy) == 0 {
		return options
	}
	joined := strings.Join([]string{options.prefix, options.suffix, encodeFreeBusyMap(options.freeBusy)}, "\x1f")
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
// the job maps it, the free/busy state replaced. A cancelled occurrence the map
// sends to a show-as state stops being a cancellation, which is how an operator
// keeps a cancelled meeting visible as free time in the other calendar.
func (o eventOptions) applyTo(input providers.CalendarEventInput) providers.CalendarEventInput {
	if o.prefix != "" || o.suffix != "" {
		input.Summary = o.decorate(input.Summary)
	}
	if target, changed := o.freeBusy.apply(input.FreeBusy); changed {
		input.FreeBusy = target
		if input.Cancelled {
			input.Cancelled = target == providers.FreeBusyCancelled
		}
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

// staleFreeBusy reports whether the destination state a pairing recorded is not
// what the mapping of the job asks for now, plus the evidence for the rewrite. A
// state that was never recorded is adopted silently: that is what keeps an upgrade
// from rewriting every event once just because the columns are new.
func (e *calendarEngine) staleFreeBusy(in planInput) (bool, string) {
	options := e.options()
	if len(options.freeBusy) == 0 || in.Link == nil || in.Source == nil || in.Destination == nil {
		return false, ""
	}
	if e.target == nil || !e.target.writable() || strings.TrimSpace(in.Destination.ID) == "" {
		return false, ""
	}
	recorded := providers.FreeBusy(strings.TrimSpace(in.Link.DestinationFreeBusy))
	if recorded == "" {
		return false, ""
	}
	wanted, _ := options.freeBusy.apply(in.Source.FreeBusy)
	if recorded == wanted {
		return false, ""
	}
	return true, "the free/busy mapping of the pairing changed since the destination was written"
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
		if strings.TrimSpace(link.DestinationEventID) == "" {
			continue
		}
		if !withinWindow(link.DestinationStart, e.window) {
			continue
		}
		if link.OptionsFingerprint != options.fingerprint {
			return true
		}
		if len(options.freeBusy) == 0 {
			continue
		}
		source := providers.FreeBusy(strings.TrimSpace(link.SourceFreeBusy))
		recorded := providers.FreeBusy(strings.TrimSpace(link.DestinationFreeBusy))
		if source == "" || recorded == "" {
			continue
		}
		if wanted, _ := options.freeBusy.apply(source); wanted != recorded {
			return true
		}
	}
	return false
}
