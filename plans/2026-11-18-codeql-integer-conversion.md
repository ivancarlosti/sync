# Plan — Close the two `go/incorrect-integer-conversion` alerts in `internal/recurrence`

- **Date:** 2026-11-18
- **Repo:** https://github.com/ivancarlosti/sync (branch `main`, base commit `d38b043`)
- **Alerts:** #5 (`recurrence.go:342`) and #6 (`recurrence.go:376`), rule
  `go/incorrect-integer-conversion` (CWE-190 / CWE-681), severity `warning`,
  both introduced by `d38b043` (*feat(calendars): copy recurring series and map
  free/busy per source state*).

## Context

`internal/recurrence` translates an RFC 5545 `RRULE` into the
`patternedRecurrence` object Microsoft Graph stores. The rule is read by
`Parse` → `parseRRULE` → `parseSmallInts`, which used `strconv.Atoi` and stored
the answer in `Rule.ByMonth` / `Rule.ByMonthDay` as `[]int`; the pattern fillers
then narrowed those values to `int32`, the width `RecurrencePattern.SetMonth`
and `SetDayOfMonth` take.

`strconv.Atoi` answers an `int`, whose width depends on the build architecture,
so the narrowing was flagged by CodeQL:

```
Incorrect conversion of an integer with architecture-dependent bit size
from strconv.Atoi to a lower bit size type int32 without an upper bound check.
```

The parser did bound the values (`parseSmallInts(value, "BYMONTH", 1, 12)`), but
it passed the bounds as **parameters**. CodeQL's
`IncorrectIntegerConversionLib.qll` only recognises a guard that compares the
value against a **constant** (`UpperBoundCheckGuard.isBoundFor`), which is also
why `int32(rule.Interval)` and `int32(rule.Count)` were never reported: their
parser compares against the constant `maxCount = 999`.

Two secondary facts shaped the fix:

1. `Rule` is exported inside the package, so any caller can build a rule that
   never passed through `Parse` (month 13, day 0) — `patternFor` is the last
   gate before the value becomes a Graph payload.
2. The alert must not be silenced: no `//nolint`, no exclusion in a CodeQL
   config, no blind cast. The remediation had to remove the unsafe narrowing.

## Goal

1. No conversion of a parsed, architecture-dependent integer into a narrower
   integer type in `internal/recurrence` — the day/month values are parsed at
   the width the Graph pattern stores.
2. Keep the accepted vocabulary identical: `BYMONTH` 1–12, `BYMONTHDAY` ±1–31,
   zero refused, everything else still `ErrUnsupported`.
3. Add regression tests for the numeric contract at both layers (the parser and
   the pattern filler), so the invariant is pinned by a test rather than by the
   analysis alone.

## Steps

1. `internal/recurrence/recurrence.go`
   - `Rule.ByMonthDay []int` → `[]int32`, `Rule.ByMonth []int` → `[]int32`,
     with a comment stating that this is the width of the Graph pattern and the
     accepted range.
   - `parseSmallInts(value, name string, min, max int32) ([]int32, error)`:
     `strconv.Atoi` → `strconv.ParseInt(…, 10, 32)`; the bound check compares
     against `int64(min)`/`int64(max)` and zero, so the value is within `int32`
     before it is stored. `ParseInt` with `bitSize = 32` makes the subsequent
     `int32(number)` a same-width conversion, which the rule treats as safe
     (source state 31 bits, `int32` sink adjusts to 31 bits).
   - `patternFor` (`YEARLY`): drop `int32(rule.ByMonth[0])`; validate the value
     against the literals 1 and 12 and refuse it with `ErrUnsupported`.
   - `monthlyPattern`: drop `int32(rule.ByMonthDay[0])`; validate against
     `0`, `-31`, `31` and refuse it with `ErrUnsupported`.
   - Untouched on purpose: `int32(rule.Count)` and `int32(rule.Interval)` — not
     reported, and already guarded by the constant `maxCount`.
2. `internal/recurrence/recurrence_test.go`
   - `TestParseRefusesValuesOutsideTheGraphRange` — `BYMONTH=13`, `BYMONTH=0`,
     `BYMONTHDAY=0`, `BYMONTHDAY=32`, `BYMONTHDAY=-32` are refused.
   - `TestPatternForRefusesARuleOutsideTheGraphRange` — a hand-built `Rule`
     outside the range is refused by the pattern filler itself.
3. Verification: `gofmt -l .`, `go build ./...`, `go vet ./...`,
   `go test ./... -count=1`, and a grep proving the flagged lines hold no
   conversion.

## Risks

- `Rule` changes field types (`[]int` → `[]int32`). Contained: a repository-wide
  grep found no consumer of `ByMonth`/`ByMonthDay` outside the package, and the
  exported surface of the package (`Parse`, `ToGraph`, `FromGraph`, `Rule`,
  `Weekday`, `ErrUnsupported`) keeps its shape.
- `strconv.ParseInt(…, 32)` refuses a value that does not fit `int32` before the
  range check, which is the intended behaviour (RFC 5545 numbers are tiny).
- The new guard in `patternFor`/`monthlyPattern` only rejects a value the parser
  already rejects; a rule that is accepted today keeps being accepted.
- Alert closure is confirmed by the CodeQL analysis GitHub runs on `main` after
  the push (default setup), not by a local run: the CodeQL CLI is no longer on
  this machine, only the query packs under `~/.codeql/packages`.

## Verification

| Check | Result |
|---|---|
| `gofmt -l .` | clean |
| `go build ./...` | ok |
| `go vet ./...` | ok |
| `go test ./internal/recurrence/ -count=1 -v` | 9 tests pass, including the 2 new ones |
| `go test ./... -count=1` | ok — every package with tests |
| `grep -n 'int32(' internal/recurrence/*.go` | `:202` (ParseInt 32 → int32, same width), `:286` `COUNT`, `:302` `INTERVAL` (both guarded by `maxCount`) |

## Current status

- **Done:** both alert sites fixed at the root, tests added, `gofmt`/`build`/
  `vet`/`test` green, committed as `04874de` and pushed to `main`.
- **Confirmed:** the CodeQL *Push on main* run (`44229589376`, all four
  languages green) re-analysed `04874de` and closed both alerts: #5 and #6
  (`go/incorrect-integer-conversion`) are now `fixed`. The same re-analysis also
  retired #1–#4, which had been left `open` against the 2026-09-29 commit even
  though their remedies were already in the tree — the repository now reports
  **0 open code-scanning alerts, 0 open Dependabot alerts, 0 open secret-scanning
  alerts and 0 open Dependabot pull requests**.
- **Residual risk:** none from the analysis side; the change is proven by the
  re-analysis on `main` rather than by a local CodeQL run (no CLI on the
  developer machine, only the query packs under `~/.codeql/packages`).
