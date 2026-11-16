# Copy recurring series and map free/busy per source state

## Context

The calendar engine of a job pairs single events only. Every event that belongs to
a recurring series is skipped with the evidence "the event belongs to a recurring
series, which the engine does not copy" (`internal/services/calendarsync.go`), so a
calendar whose meetings are mostly recurring copies almost nothing. The recurrence
data is already in the listings: `internal/ical` parses `RRULE`/`RDATE`/`EXDATE`/
`RECURRENCE-ID`, Google returns `recurrence`/`recurringEventId`/`originalStartTime`
under `singleEvents=false`, and Graph returns `type`/`seriesMasterId` per instance.

The free/busy handling is equally coarse. `providers` knows two values
(`opaque`/`transparent`), Microsoft collapses Graph's five `showAs` states onto them
on **read**, and the only control is `transparency_override`: one value applied to
every destination write, with no way to say "a tentative event of the source is busy
in the destination" or "a cancelled occurrence still blocks my time".

## Goal

1. Copy a recurring series: create the master (the rule) in the destination, then
   copy the modified and cancelled occurrences of the series.
2. Replace the single override with a per-source-state transformation map that is
   configured per job and applied to destination writes only:
   `free`, `tentative`, `busy`, `out_of_office`, `cancelled` → same as the source
   or one of the four show-as states.
3. Keep the engine's evidence vocabulary: everything the run cannot copy is a
   reported skip, never a silent partial copy.

## Steps

1. `internal/providers`: replace the two free/busy constants with a five-state
   `FreeBusy` vocabulary (plus `cancelled` as a source state), and add the series
   read model (`Kind`, `SeriesID`, `OriginalStart`, `Recurrence`) to
   `CalendarEvent`/`CalendarEventInput`. `EventFingerprint` gains the canonical
   recurrence and the original start, and only when they are non-empty, so no
   fingerprint stored before this change moves.
2. Providers: normalise and write all states (Google `transparency` + `status` +
   `eventType`, Graph `showAs` one to one, iCal `TRANSP` + `STATUS`); list series
   masters and exceptions (Graph cannot see a master through `calendarView`, so it
   is read per `seriesMasterId`); write a master with its rule and an exception
   through the provider's instance endpoint.
3. `internal/recurrence`: translate RFC 5545 `RRULE` to Graph `patternedRecurrence`
   and back for the six patterns Graph supports, refusing (typed error) everything
   it cannot represent instead of approximating it.
4. `internal/models` + `internal/services`: `CalendarJob.FreeBusyMap` (canonical
   JSON, identity entries omitted) and `SeriesExceptions`, the link columns that
   make free/busy drift detectable without rewriting every pairing, and the option
   fingerprint over the map.
5. Engine: a second write phase for exceptions (a master has to exist before its
   occurrences can be targeted), the per-state mapping applied to destination
   writes, and skip evidence for the series the destination cannot represent.
6. API, UI and docs: `free_busy_map`/`series_exceptions` in the job payload, a
   five-row mapping grid with presets in the job editor, `docs/calendars.md`,
   `docs/api.md`, `docs/database.md`, `docs/providers.md`.

## Risks

- Graph has no writable `isCancelled`: a copied cancellation deletes the
  occurrence, and a cancelled occurrence cannot be resurrected later. Changing the
  `cancelled` row from "same as the source" to a show-as state therefore rewrites
  the master and retries the affected occurrences.
- Google resolves an occurrence through `events.instances?originalStart=`; a
  just-created master may not list it yet, so a miss is skipped without a link and
  retried on the next run rather than guessed.
- The RRULE conversion is lossy at the edges (`BYSETPOS`, `BYYEARDAY`, `BYWEEKNO`,
  `RDATE`-only rules, `EXRULE`). Such a series is refused as a whole.
- Extra provider calls: one master read plus one instance read per series on a
  Microsoft source, one instance read per exception write on Google. The
  `series_exceptions` option turns the second phase off for operators who do not
  want the cost.
- `CalendarEventLink` gains columns; `AutoMigrate` adds them, and the free/busy
  columns stay empty on the first run so the upgrade itself rewrites nothing.
