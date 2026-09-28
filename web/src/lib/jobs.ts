// Job vocabulary and payload helpers.
//
// The direction and conflict values are enums of the API (see
// internal/models/job.go and the validation in internal/handlers/jobs.go) and the
// editor is reused for create and update, so the mapping between a `SyncJob` and
// the payload PUT/POST expects lives in one place instead of twice in the view.
import { type Composer } from 'vue-i18n';

import { type SelectOption } from '@/components/ui/Select.vue';
import type { JobPayload, SyncJob } from '@/lib/api';

/** DIRECTIONS is the closed set of `direction` values of the API. */
export const DIRECTIONS = ['google_to_microsoft', 'microsoft_to_google', 'bidirectional'] as const;

/** CONFLICT_POLICIES is the closed set of `conflict_policy` values of the API. */
export const CONFLICT_POLICIES = ['newest_wins', 'source_wins', 'destination_wins', 'skip'] as const;

/** DEFAULT_CONFLICT_POLICY mirrors the default `applyJobInput` falls back to. */
export const DEFAULT_CONFLICT_POLICY = 'newest_wins';

/** MAX_INTERVAL_MINUTES mirrors `maxIntervalMinutes` of the jobs handler. */
export const MAX_INTERVAL_MINUTES = 7 * 24 * 60;

/** directionOptions lists the directions with their translated labels. */
export function directionOptions(t: Composer['t']): SelectOption[] {
  return DIRECTIONS.map((value) => ({ value, label: t(`jobs.direction_${value}`) }));
}

/** conflictOptions lists the conflict policies with their translated labels. */
export function conflictOptions(t: Composer['t']): SelectOption[] {
  return CONFLICT_POLICIES.map((value) => ({ value, label: t(`jobs.conflict_${value}`) }));
}

/**
 * directionHint explains the selected direction in one sentence. The editor shows
 * it under the select so the operator reads what the value means, and the wording
 * lives in the catalog next to the option label.
 */
export function directionHint(t: Composer['t'], value: string): string {
  return t(`jobs.direction_${value}`);
}

/** conflictHint explains the selected conflict policy in one sentence. */
export function conflictHint(t: Composer['t'], value: string): string {
  return t(`jobs.conflict_${value}`);
}

/** scheduleLabel renders the schedule of a job as a short sentence. */
export function scheduleLabel(t: Composer['t'], minutes: number): string {
  return minutes > 0 ? t('jobs.everyMinutes', { count: minutes }) : t('jobs.manualOnly');
}

/**
 * defaultDirection mirrors `defaultDirection` of internal/handlers/jobs.go: an
 * empty direction is filled in from the provider pair the operator picked, so the
 * editor can show a real selection instead of an empty list.
 */
export function defaultDirection(source?: string, destination?: string): string {
  if (source === 'microsoft' && destination === 'google') {
    return 'microsoft_to_google';
  }
  return 'google_to_microsoft';
}

/**
 * payloadFromJob builds the update payload of a job. An update replaces the job
 * as a whole, so every field travels — that is what makes flipping `enabled` from
 * the list safe.
 */
export function payloadFromJob(job: SyncJob, overrides: Partial<JobPayload> = {}): JobPayload {
  return {
    name: job.name,
    source_account_id: job.source_account_id,
    destination_account_id: job.destination_account_id,
    source_drive_id: job.source_drive_id,
    source_folder_id: job.source_folder_id,
    source_folder_path: job.source_folder_path,
    destination_drive_id: job.destination_drive_id,
    destination_folder_id: job.destination_folder_id,
    destination_folder_path: job.destination_folder_path,
    direction: job.direction,
    conflict_policy: job.conflict_policy,
    exclude_patterns: [...job.exclude_patterns],
    delete_missing: job.delete_missing,
    interval_minutes: job.interval_minutes,
    enabled: job.enabled,
    ...overrides,
  };
}

/** newJobPayload is the draft the editor starts a job with. */
export function newJobPayload(): JobPayload {
  return {
    name: '',
    source_account_id: 0,
    destination_account_id: 0,
    source_drive_id: '',
    source_folder_id: '',
    source_folder_path: '',
    destination_drive_id: '',
    destination_folder_id: '',
    destination_folder_path: '',
    direction: '',
    conflict_policy: '',
    exclude_patterns: [],
    delete_missing: false,
    interval_minutes: 0,
    enabled: true,
  };
}
