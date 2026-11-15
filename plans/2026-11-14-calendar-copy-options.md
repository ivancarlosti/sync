# Calendar copy options: title decoration, free/busy override, zone normalisation

## Context

A calendar job copies an event into another calendar, but it can only copy it
**verbatim**. Operators who sync a work calendar into a personal one want the
copy to say where it came from (`[Work] Dentist`), and a feed of all-day
reminders should not block every afternoon in the destination. Neither is
possible today, and the only workaround — editing the copy by hand — is undone by
the next run.

Zones are a second, quieter problem. Graph publishes Windows zone labels
(`W. Europe Standard Time`), Outlook exports and `.ics` feeds publish
globally-unique `TZID`s (`/mozilla.org/20070129_1/Europe/Berlin`), and Google
**rejects** a `start.timeZone` it does not know. A write built from any of those
labels either fails or silently lands in the wrong wall clock, and the three
providers disagreed about which of the four shapes they accepted.

## Goal

* Per-pairing options: `title_prefix`, `title_suffix` (each ≤ 64 runes) and
  `transparency_override` (`''|free|busy`), editable in the job editor's
  *options* tab with a live preview of the decorated title.
* The decoration applies to the **destination** write only, is stripped before
  anything is written back to the source, and converges in a single run when it
  is added or cleared — without being reported as a conflict and without
  re-syncing every existing pairing on upgrade.
* The free/busy flag is carried end to end: parsed from `TRANSP`, read from the
  Google/Microsoft payloads, written as the provider's own value, overridable per
  job.
* One canonical zone boundary (`ZoneName`, `LoadZone`, `InZone`) shared by the
  three providers: IANA names, Windows labels and globally-unique TZIDs all land
  on a loadable IANA name, and an unresolvable label yields UTC instead of an
  invalid name a provider would reject.
* The seven i18n catalogs, `docs/calendars.md`, `docs/api.md`,
  `docs/database.md` and `docs/providers.md` describe what the code does.

## Steps

1. `internal/models/calendar.go`: `CalendarFreeBusy` (`''|free|busy`) with
   `Valid()`/`Transparency()`; `CalendarJob.TitlePrefix`, `TitleSuffix`,
   `TransparencyOverride`; `CalendarEventLink.OptionsFingerprint`.
2. `internal/providers/calendar.go`: `CalendarEvent.Transparency`,
   `CalendarEventInput.Transparency` (excluded from `EventFingerprint`, so a
   stored fingerprint from before the field existed still matches),
   `TransparencyOpaque`/`TransparencyTransparent`/`TransparencyName`, and the
   zone trio `ZoneName`/`LoadZone`/`InZone` with a Windows → IANA alias table.
   `EventInputFrom` carries the flag.
3. Providers: Google (`transparency`, `start.timeZone`/`end.timeZone`
   canonicalised), Microsoft (Graph `showAs` read as free/busy and written only
   when the job sets a value; the zone canonicalised through `InZone`/`ZoneName`
   at the edges of the request), `internal/ical` (`TRANSP`, Windows TZID through
   `ZoneName`, unreadable zone → UTC).
4. `internal/services/calendar_options.go`: `eventOptions` (normalised prefix,
   suffix, transparency, fingerprint), `decorate`/`undecorate`,
   `applyTo`/`writeInput` (destination only), `staleOptions` and
   `optionsDrifted`. `calendar_options_test.go` pins each of them.
5. `internal/services/calendarsync.go`: `rewriteForOptions` turns an
   `unchanged` decision into one `update_destination` when the option fingerprint
   is stale; `remember` records the fingerprint after a destination write;
   `feedDrifted` re-fetches a feed whose `304` would otherwise hide a stale
   option.
6. `internal/handlers/calendars.go`: payload fields, trimming + `64`-rune clamp
   (`calendarDecorWidth`), `400 validation` for an unknown override, enforced
   `one_way` unchanged. New `TestCalendarCopyOptions` covers trim, clamp, echo and
   refusal.
7. Frontend: `lib/api.ts` (`CalendarFreeBusy`, new optional fields),
   `lib/calendars.ts` (`FREE_BUSY_VALUES`, `DECOR_WIDTH`, `decoratedSummary`,
   `freeBusyValue`, defaults/payload wiring), `views/CalendarEditorView.vue`
   (decoration preview, free/busy select), `components/ui/Input.vue`
   (`maxlength`), and the new keys in all seven catalogs.
8. Docs: a *Destination copy options* section in `docs/calendars.md`, the new
   payload fields and validation in `docs/api.md`, the columns in
   `docs/database.md`, the transparency/zone contract in `docs/providers.md`, the
   `TRANSP`/TZID behaviour in the parser table.
9. Validate: `gofmt -l internal/`, `go build ./...`, `go vet ./internal/...`,
   `go test ./internal/...`, then `npm run typecheck` and `npm run build` in
   `web/`.
10. Security review (`gh`): no open Dependabot PR, no code-scanning alert. The
   one open Dependabot alert (medium, #37) is the indirect
   `go.opentelemetry.io/otel/trace` `1.35.0–<1.47.0` advisory, pulled in by
   `kiota-abstractions-go`; the three otel modules are bumped to `v1.47.0` in the
   same change (plus the `otel/log` module the new version requires) and the
   whole suite is re-run against it. The alert closes on the next push.

## Risks

* **Mass re-sync on upgrade.** Adding a field to `EventFingerprint` would make
  every recorded pairing look edited on both sides, which `newest_wins` answers
  with a conflict or a rewrite of every event. `Transparency` is therefore kept
  out of the fingerprint; the trade-off is that a change of the *source's*
  free/busy flag alone does not trigger a write, which is why the override is
  tracked by the link's option fingerprint instead.
* **A decoration is one-sided by nature.** The destination fingerprint differs
  from the source by construction, so the decision must compare each side against
  what the previous run recorded (it does) and the option fingerprint must be
  written on every destination write (it is). A pairing whose options changed
  while the event is outside the window converges when it re-enters it.
* **Undecorate is best effort.** A summary the operator edited into the exact
  shape of the decoration cannot be told apart from the decoration; the strip
  removes one copy, at the matching end only, so the common cases (a marker in
  the middle, a doubled marker) stay intact.
* **Zone aliases can go stale** as tzdata renames zones; `ZoneName` tries the
  alias table, the label itself and the trailing TZID segments before giving up,
  and `LoadZone` never fails, so a stale alias degrades to UTC rather than to a
  rejected write.
* **The clamp is silent.** A prefix longer than 64 runes is shortened instead of
  refused: right for a typo, but it means the saved value can differ from what was
  typed — the editor mirrors the field width and the preview shows the result.
