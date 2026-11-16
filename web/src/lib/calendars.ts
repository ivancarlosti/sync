// Calendar vocabulary and payload helpers.
//
// The calendar engine has its own, smaller vocabulary than the file jobs (see
// internal/models/calendar.go): the source can be an .ics subscription, the
// direction is either "both ways" or "only into the destination", and the
// conflict policies are the same four values. Keeping the closed sets and the
// draft/payload conversion here means the list and the editor cannot disagree
// about what an empty field means.
import { type Composer } from 'vue-i18n';

import { type SelectOption } from '@/components/ui/Select.vue';
import type {
  CalendarFreeBusy,
  CalendarFreeBusyState,
  CalendarJob,
  CalendarPayload,
  CalendarSeriesExceptions,
} from '@/lib/api';
import { CONFLICT_POLICIES } from '@/lib/jobs';

/** SOURCE_KINDS is the closed set of `source_kind` values of the API. */
export const SOURCE_KINDS = ['google', 'microsoft', 'ics'] as const;

/** CALENDAR_DIRECTIONS is the closed set of `direction` values of the API. */
export const CALENDAR_DIRECTIONS = ['bidirectional', 'one_way'] as const;

/** FREE_BUSY_VALUES is the closed set of the deprecated `transparency_override` values. */
export const FREE_BUSY_VALUES = ['', 'free', 'busy'] as const;

/**
 * FREE_BUSY_STATES is the closed set of source states the free/busy mapping
 * names, in the order the editor lists them: what a calendar shows an event as,
 * plus `cancelled`, which is what a cancelled occurrence of a series reads as.
 */
export const FREE_BUSY_STATES: readonly CalendarFreeBusyState[] = [
  'free',
  'tentative',
  'busy',
  'out_of_office',
  'cancelled',
];

/** SERIES_EXCEPTIONS is the closed set of `series_exceptions` values. */
export const SERIES_EXCEPTIONS: readonly CalendarSeriesExceptions[] = ['copy', 'skip'];

/** FREE_BUSY_PRESETS are the one-click transformations of the mapping grid. */
export const FREE_BUSY_PRESETS = ['keep', 'all_free', 'all_busy', 'hide_cancelled'] as const;

/** FreeBusyPreset is one of the presets the editor offers. */
export type FreeBusyPreset = (typeof FREE_BUSY_PRESETS)[number];

/**
 * FreeBusyMapping is the per-source-state transformation the engine applies to
 * the destination copy. A state that is absent (or maps to itself) is copied
 * unchanged.
 */
export type FreeBusyMapping = Partial<Record<CalendarFreeBusyState, CalendarFreeBusyState>>;


/**
 * DECOR_WIDTH mirrors `calendarDecorWidth` of the calendars handler: a prefix or
 * suffix longer than this is clamped by the API.
 */
export const DECOR_WIDTH = 64;

/** PLAN_ACTIONS is the vocabulary of the preview and of the run history. */
export const PLAN_ACTIONS = [
  'create_destination',
  'update_destination',
  'delete_destination',
  'cancel_destination',
  'create_source',
  'update_source',
  'delete_source',
  'conflict',
  'skip',
] as const;

/** DEFAULT_CONFLICT_POLICY mirrors the default `applyCalendarInput` applies. */
export const DEFAULT_CONFLICT_POLICY = 'newest_wins';

/** DEFAULT_PAST_DAYS and DEFAULT_FUTURE_DAYS mirror the handler defaults. */
export const DEFAULT_PAST_DAYS = 30;
export const DEFAULT_FUTURE_DAYS = 365;

/** MAX_INTERVAL_MINUTES mirrors `maxIntervalMinutes` of the jobs handler. */
export const MAX_INTERVAL_MINUTES = 7 * 24 * 60;

/** ICS_MARKER is the synthetic source calendar id of a subscription. */
export const ICS_MARKER = 'ics';

/** sourceKindOptions lists the source kinds with their translated labels. */
export function sourceKindOptions(t: Composer['t']): SelectOption[] {
  return SOURCE_KINDS.map((value) => ({ value, label: t(`calendars.source_${value}`) }));
}

/** calendarDirectionOptions lists the directions of a calendar job. */
export function calendarDirectionOptions(t: Composer['t'], isFeed: boolean): SelectOption[] {
  return CALENDAR_DIRECTIONS.map((value) => ({
    value,
    label: t(`calendars.direction_${value}`),
    // A feed has no writable end, so the only honest choice is the one way.
    disabled: isFeed && value !== 'one_way',
  }));
}

/** calendarConflictOptions lists the conflict policies (shared with jobs). */
export function calendarConflictOptions(t: Composer['t']): SelectOption[] {
  return CONFLICT_POLICIES.map((value) => ({ value, label: t(`jobs.conflict_${value}`) }));
}

/** scheduleLabel renders the schedule of a job as a short sentence. */
export function calendarScheduleLabel(t: Composer['t'], minutes: number): string {
  return minutes > 0 ? t('calendars.everyMinutes', { count: minutes }) : t('calendars.manualOnly');
}

/** freeBusyOptions lists the values of the free/busy override. */
export function calendarFreeBusyOptions(t: Composer['t']): SelectOption[] {
  return FREE_BUSY_VALUES.map((value) => ({
    value,
    label: value === '' ? t('calendars.freeBusy_keep') : t(`calendars.freeBusy_${value}`),
  }));
}

/**
 * decoratedSummary mirrors `eventOptions.decorate` of the engine: the parts joined
 * by one space, with an empty summary dropping out instead of leaving a gap. The
 * editor shows the result so the marker is chosen by looking at it rather than by
 * guessing how it will read.
 */
export function decoratedSummary(prefix: string, suffix: string, summary: string): string {
  const parts: string[] = [];
  if (prefix.trim() !== '') parts.push(prefix.trim());
  if (summary.trim() !== '') parts.push(summary.trim());
  if (suffix.trim() !== '') parts.push(suffix.trim());
  return parts.join(' ');
}

/** freeBusyValue narrows an untrusted value onto the deprecated closed set. */
export function freeBusyValue(value: unknown): CalendarFreeBusy {
  const text = typeof value === 'string' ? value.trim().toLowerCase() : '';
  return text === 'free' || text === 'busy' ? text : '';
}

/** freeBusyState narrows an untrusted value onto the five states. */
export function freeBusyState(value: unknown): CalendarFreeBusyState | null {
  const text = typeof value === 'string' ? value.trim().toLowerCase() : '';
  return (FREE_BUSY_STATES as readonly string[]).includes(text)
    ? (text as CalendarFreeBusyState)
    : null;
}

/** seriesExceptionsValue narrows an untrusted value onto the closed set. */
export function seriesExceptionsValue(value: unknown): CalendarSeriesExceptions {
  return value === 'skip' ? 'skip' : 'copy';
}

/**
 * parseFreeBusyMap reads the canonical JSON a job stores. An entry that names a
 * state onto itself is dropped, which is how the engine stores the mapping too.
 */
export function parseFreeBusyMap(raw: string | undefined): FreeBusyMapping {
  if (!raw || raw.trim() === '') return {};
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    const mapping: FreeBusyMapping = {};
    for (const state of FREE_BUSY_STATES) {
      const target = freeBusyState(parsed[state]);
      if (target && target !== state) mapping[state] = target;
    }
    return mapping;
  } catch {
    return {};
  }
}

/**
 * encodeFreeBusyMap mirrors `encodeFreeBusyMap` of the engine: only the entries
 * that change something, keys sorted, so the payload the SPA sends is the string
 * the API stores.
 */
export function encodeFreeBusyMap(mapping: FreeBusyMapping): string {
  const encoded: Record<string, string> = {};
  for (const state of [...FREE_BUSY_STATES].sort()) {
    const target = mapping[state];
    if (!target || target === state) continue;
    encoded[state] = target;
  }
  return Object.keys(encoded).length === 0 ? '' : JSON.stringify(encoded);
}

/**
 * freeBusyMapFromJob reads the mapping of a job, expanding the deprecated single
 * override the way the engine does when no map was ever saved.
 */
export function freeBusyMapFromJob(
  job: Pick<CalendarJob, 'free_busy_map' | 'transparency_override'>,
): FreeBusyMapping {
  const stored = parseFreeBusyMap(job.free_busy_map);
  if (Object.keys(stored).length > 0) return stored;
  const legacy = freeBusyValue(job.transparency_override);
  if (!legacy) return {};
  const mapping: FreeBusyMapping = {};
  for (const state of FREE_BUSY_STATES) {
    if (state !== 'cancelled') mapping[state] = legacy;
  }
  return mapping;
}

/**
 * freeBusyPresetMapping is the mapping a preset produces. "Hide cancelled" sends
 * a cancelled occurrence to free time, which is what keeps a dropped meeting
 * visible as an opening in the other calendar instead of an invisible hole.
 */
export function freeBusyPresetMapping(preset: FreeBusyPreset): FreeBusyMapping {
  switch (preset) {
    case 'all_free':
      return {
        free: 'free',
        tentative: 'free',
        busy: 'free',
        out_of_office: 'free',
        cancelled: 'free',
      };
    case 'all_busy':
      return {
        free: 'busy',
        tentative: 'busy',
        busy: 'busy',
        out_of_office: 'busy',
        cancelled: 'busy',
      };
    case 'hide_cancelled':
      return { cancelled: 'free' };
    case 'keep':
    default:
      return {};
  }
}

/** calendarFreeBusyTargetOptions lists the destination state of one mapping row. */
export function calendarFreeBusyTargetOptions(t: Composer['t']): SelectOption[] {
  return [
    { value: 'same', label: t('calendars.freeBusy_same') },
    ...FREE_BUSY_STATES.map((state) => ({
      value: state,
      label: t(`calendars.freeBusyState_${state}`),
    })),
  ];
}

/** calendarFreeBusyPresetOptions lists the presets of the mapping grid. */
export function calendarFreeBusyPresetOptions(t: Composer['t']): SelectOption[] {
  return FREE_BUSY_PRESETS.map((preset) => ({
    value: preset,
    label: t(`calendars.freeBusyPreset_${preset}`),
  }));
}

/** calendarSeriesExceptionsOptions lists how much of a series travels. */
export function calendarSeriesExceptionsOptions(t: Composer['t']): SelectOption[] {
  return SERIES_EXCEPTIONS.map((value) => ({
    value,
    label: t(`calendars.seriesExceptions_${value}`),
  }));
}

/** actionLabel translates a plan action, falling back to the raw identifier. */
export function actionLabel(t: Composer['t'], te: Composer['te'], action: string): string {
  const key = `calendars.action_${action}`;
  return te(key) ? t(key) : action;
}

/**
 * payloadFromJob builds the update payload of a job. An update replaces the job
 * as a whole, so every field travels: that is what makes flipping `enabled` from
 * the list safe. The subscription password is never known by the SPA, so it goes
 * out empty (which the API reads as "keep the stored one").
 */
export function payloadFromJob(
  job: CalendarJob,
  overrides: Partial<CalendarPayload> = {},
): CalendarPayload {
  return {
    name: job.name,
    source_kind: job.source_kind,
    source_account_id: job.source_account_id,
    source_calendar_id: job.source_calendar_id,
    source_calendar_name: job.source_calendar_name,
    source_url: job.source_url ?? '',
    source_username: job.source_username ?? '',
    source_password: '',
    clear_source_password: false,
    destination_account_id: job.destination_account_id,
    destination_calendar_id: job.destination_calendar_id,
    destination_calendar_name: job.destination_calendar_name,
    direction: job.direction,
    conflict_policy: job.conflict_policy,
    delete_missing: job.delete_missing,
    past_days: job.past_days,
    future_days: job.future_days,
    interval_minutes: job.interval_minutes,
    enabled: job.enabled,
    title_prefix: job.title_prefix ?? '',
    title_suffix: job.title_suffix ?? '',
    transparency_override: freeBusyValue(job.transparency_override),
    free_busy_map: job.free_busy_map ?? '',
    series_exceptions: seriesExceptionsValue(job.series_exceptions),
    ...overrides,
  };
}

/** newCalendarPayload is the draft the editor starts a job with. */
export function newCalendarPayload(): CalendarPayload {
  return {
    name: '',
    source_kind: 'microsoft',
    source_account_id: 0,
    source_calendar_id: '',
    source_calendar_name: '',
    source_url: '',
    source_username: '',
    source_password: '',
    clear_source_password: false,
    destination_account_id: 0,
    destination_calendar_id: '',
    destination_calendar_name: '',
    direction: 'bidirectional',
    conflict_policy: DEFAULT_CONFLICT_POLICY,
    delete_missing: false,
    past_days: DEFAULT_PAST_DAYS,
    future_days: DEFAULT_FUTURE_DAYS,
    interval_minutes: 0,
    enabled: true,
    title_prefix: '',
    title_suffix: '',
    transparency_override: '',
    free_busy_map: '',
    series_exceptions: 'copy',
  };
}
