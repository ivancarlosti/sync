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
import type { CalendarJob, CalendarPayload } from '@/lib/api';
import { CONFLICT_POLICIES } from '@/lib/jobs';

/** SOURCE_KINDS is the closed set of `source_kind` values of the API. */
export const SOURCE_KINDS = ['google', 'microsoft', 'ics'] as const;

/** CALENDAR_DIRECTIONS is the closed set of `direction` values of the API. */
export const CALENDAR_DIRECTIONS = ['bidirectional', 'one_way'] as const;

/** PLAN_ACTIONS is the vocabulary of the preview and of the run history. */
export const PLAN_ACTIONS = [
  'create_destination',
  'update_destination',
  'delete_destination',
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
  };
}
