<script setup lang="ts">
// CalendarEditorView — create and edit one calendar job.
//
// The four tabs are the four questions of the pairing (what it is called, what
// it reads, what it writes, how it behaves). A calendar job is either a provider
// pair (Google and Microsoft in either combination) or an .ics subscription: the
// source tab swaps between the two, and the subscription adds the one thing a
// feed needs that a provider does not — a way to check the URL before saving it.
import {
  ArrowLeft,
  ArrowRight,
  CalendarArrowDown,
  CalendarArrowUp,
  Info,
  RefreshCw,
  Save,
  Settings2,
} from '@lucide/vue';
import { computed, onMounted, reactive, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';

import PageHeader from '@/components/PageHeader.vue';
import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import Input from '@/components/ui/Input.vue';
import Label from '@/components/ui/Label.vue';
import Select, { type SelectOption } from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import Switch from '@/components/ui/Switch.vue';
import Tabs, { type TabItem } from '@/components/ui/Tabs.vue';
import { useAction } from '@/composables/useAction';
import {
  accounts as accountsApi,
  calendars as calendarsApi,
  messageOf,
  type CalendarOption,
  type CalendarPayload,
  type ConnectedAccount,
  type FeedPreview,
} from '@/lib/api';
import { accountLabel } from '@/lib/accounts';
import {
  ICS_MARKER,
  MAX_INTERVAL_MINUTES,
  calendarConflictOptions,
  calendarDirectionOptions,
  calendarScheduleLabel,
  newCalendarPayload,
  payloadFromJob,
  sourceKindOptions,
} from '@/lib/calendars';
import { asNumber, asText } from '@/lib/forms';
import { formatDateTime } from '@/lib/format';
import { useFeedbackStore } from '@/stores/feedback';

const route = useRoute();
const router = useRouter();
const { t, locale } = useI18n();
const feedback = useFeedbackStore();

/** id is the job being edited; zero means the "new calendar" route. */
const id = computed(() => {
  const raw = route.params.id;
  const value = Array.isArray(raw) ? raw[0] : raw;
  const parsed = Number(value ?? 0);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
});

const editing = computed(() => id.value > 0);

const draft = reactive<CalendarPayload>(newCalendarPayload());
const accounts = ref<ConnectedAccount[]>([]);
const loading = ref(true);
const failure = ref('');
const tab = ref('details');
const errors = ref<Record<string, string>>({});

/** The two pickers and their own loading state. */
const sourceCalendars = ref<CalendarOption[]>([]);
const destinationCalendars = ref<CalendarOption[]>([]);
const sourceCalendarsError = ref('');
const destinationCalendarsError = ref('');
const calendarsLoading = ref(false);

/** The "check this subscription" answer, which is only ever a preview. */
const feedCheck = ref<FeedPreview | null>(null);
const feedCheckError = ref('');

const { busy: saving, run: save } = useAction();
const { busy: checking, run: check } = useAction();

/** isFeed reports whether the draft reads an .ics subscription. */
const isFeed = computed(() => draft.source_kind === 'ics');

const tabs = computed<TabItem[]>(() => [
  { value: 'details', label: t('calendars.steps.details'), icon: Info },
  { value: 'source', label: t('calendars.steps.source'), icon: CalendarArrowDown },
  { value: 'destination', label: t('calendars.steps.destination'), icon: CalendarArrowUp },
  { value: 'options', label: t('calendars.steps.options'), icon: Settings2 },
]);

/** STEP_ORDER is the four tabs in the order the operator walks them. */
const STEP_ORDER = ['details', 'source', 'destination', 'options'] as const;

/** STEP_ERROR_KEYS maps a step to the messages it owns. */
const STEP_ERROR_KEYS: Record<string, string[]> = {
  details: ['name'],
  source: ['sourceAccount', 'sourceCalendar', 'feed'],
  destination: ['destinationAccount', 'destinationCalendar', 'destination'],
  options: [],
};

const stepIndex = computed(() => Math.max(0, (STEP_ORDER as readonly string[]).indexOf(tab.value)));

const kinds = computed<SelectOption[]>(() => sourceKindOptions(t));
const directions = computed<SelectOption[]>(() => calendarDirectionOptions(t, isFeed.value));
const policies = computed<SelectOption[]>(() => calendarConflictOptions(t));

/** A provider source can only point at an account of that provider. */
const sourceAccountOptions = computed<SelectOption[]>(() =>
  accounts.value
    .filter((account) => isFeed.value || account.provider === draft.source_kind)
    .map((account) => ({ value: account.id, label: accountLabel(account) })),
);

/** The destination may be either provider: both can hold a calendar. */
const destinationAccountOptions = computed<SelectOption[]>(() =>
  accounts.value.map((account) => ({ value: account.id, label: accountLabel(account) })),
);

/**
 * calendarChoices lists a picker's calendars. A calendar the provider no longer
 * returns stays selectable under its stored name: dropping it would silently
 * move a job to another calendar on the next save.
 *
 * `writable` marks the destination picker, where a calendar that refuses writes
 * is shown but not offered, because the engine would fail on every event.
 */
function calendarChoices(
  list: CalendarOption[],
  currentId: string,
  currentName: string,
  writable: boolean,
): SelectOption[] {
  const options: SelectOption[] = list.map((calendar) => {
    const parts = [calendar.name];
    if (calendar.primary) {
      parts.push(t('calendars.primary'));
    }
    if (writable && calendar.read_only) {
      parts.push(t('calendars.readOnly'));
    }
    return {
      value: calendar.id,
      label: parts.join(' · '),
      disabled: writable && calendar.read_only,
    };
  });
  if (currentId !== '' && currentId !== ICS_MARKER && !list.some((c) => c.id === currentId)) {
    options.push({ value: currentId, label: currentName || currentId });
  }
  return options;
}

const sourceCalendarOptions = computed(() =>
  calendarChoices(sourceCalendars.value, draft.source_calendar_id, draft.source_calendar_name, false),
);

const destinationCalendarOptions = computed(() =>
  calendarChoices(
    destinationCalendars.value,
    draft.destination_calendar_id,
    draft.destination_calendar_name,
    true,
  ),
);

/** loadCalendars fills one picker from the account it belongs to. */
async function loadCalendars(which: 'source' | 'destination'): Promise<void> {
  const accountId = which === 'source' ? draft.source_account_id : draft.destination_account_id;
  const target = which === 'source' ? sourceCalendars : destinationCalendars;
  const error = which === 'source' ? sourceCalendarsError : destinationCalendarsError;
  error.value = '';
  if (!accountId) {
    target.value = [];
    return;
  }
  calendarsLoading.value = true;
  try {
    const answer = await accountsApi.calendarOptions(accountId);
    target.value = answer.calendars;
  } catch (problem) {
    target.value = [];
    error.value = messageOf(problem);
  } finally {
    calendarsLoading.value = false;
  }
}

/** applyCalendar remembers the display name of the picked calendar. */
function applyCalendar(which: 'source' | 'destination', value: string | number): void {
  const calendarId = asText(value);
  const list = which === 'source' ? sourceCalendars.value : destinationCalendars.value;
  const picked = list.find((calendar) => calendar.id === calendarId);
  if (which === 'source') {
    draft.source_calendar_id = calendarId;
    draft.source_calendar_name = picked?.name ?? '';
    delete errors.value.sourceCalendar;
    return;
  }
  draft.destination_calendar_id = calendarId;
  draft.destination_calendar_name = picked?.name ?? '';
  delete errors.value.destinationCalendar;
  delete errors.value.destination;
}

/** load reads the accounts and, when editing, the job itself. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const connected = await accountsApi.list();
    accounts.value = connected.accounts;
    if (!editing.value) {
      // A fresh job starts on the first provider there is an account for, with
      // the same account on both ends: the common case is one person moving
      // events between two of their own calendars.
      const first = connected.accounts[0];
      if (first) {
        draft.source_kind = first.provider;
        draft.source_account_id = first.id;
        draft.destination_account_id = first.id;
        await loadCalendars('source');
        await loadCalendars('destination');
      }
      return;
    }
    const job = await calendarsApi.get(id.value);
    Object.assign(draft, payloadFromJob(job));
    draft.source_password = '';
    draft.clear_source_password = false;
    if (job.source_account_id) {
      await loadCalendars('source');
    }
    if (job.destination_account_id) {
      await loadCalendars('destination');
    }
  } catch (problem) {
    failure.value = messageOf(problem);
  } finally {
    loading.value = false;
  }
}

// A picker answers the account it is bound to, so a new account clears the
// calendar of that side and reloads its list. That reset lives in the setters of
// the account proxies below: a watcher on the draft would also fire while an
// existing job is loaded into it, and would quietly erase its calendars.

/** applySourceKind rewrites the source half when the kind of source changes. */
function applySourceKind(kind: string): void {
  feedCheck.value = null;
  feedCheckError.value = '';
  delete errors.value.feed;
  delete errors.value.sourceAccount;
  delete errors.value.sourceCalendar;
  if (kind === 'ics') {
    draft.source_account_id = 0;
    draft.source_calendar_id = ICS_MARKER;
    draft.source_calendar_name = t('calendars.source_ics');
    draft.direction = 'one_way';
    sourceCalendars.value = [];
    return;
  }
  draft.source_url = '';
  draft.source_username = '';
  draft.source_password = '';
  draft.clear_source_password = false;
  if (draft.source_calendar_id === ICS_MARKER) {
    draft.source_calendar_id = '';
    draft.source_calendar_name = '';
  }
  if (draft.direction === 'one_way') {
    draft.direction = 'bidirectional';
  }
  // The account picked for the previous kind cannot serve the new one.
  const account = accounts.value.find((item) => item.id === draft.source_account_id);
  if (account && account.provider !== kind) {
    draft.source_account_id = 0;
    draft.source_calendar_id = '';
    draft.source_calendar_name = '';
  }
  void loadCalendars('source');
}

/** runFeedCheck reads the subscription of the draft, so a typo fails here. */
async function runFeedCheck(): Promise<void> {
  feedCheck.value = null;
  feedCheckError.value = asText(draft.source_url) === '' ? t('validation.feedUrl') : '';
  if (feedCheckError.value !== '') {
    return;
  }
  const answer = await check(
    () =>
      calendarsApi.feedPreview({
        url: asText(draft.source_url),
        username: asText(draft.source_username),
        password: draft.source_password,
        past_days: asNumber(draft.past_days, 30),
        future_days: asNumber(draft.future_days, 365),
      }),
    { failure: t('calendars.feedCheckError') },
  );
  if (answer) {
    feedCheck.value = answer;
  }
}

/** sameCalendar reports the pairing the server refuses: one calendar with itself. */
function sameCalendar(): boolean {
  return (
    draft.source_account_id === draft.destination_account_id &&
    asText(draft.source_calendar_id) !== '' &&
    asText(draft.source_calendar_id) === asText(draft.destination_calendar_id)
  );
}

/** collectErrors checks the whole form and returns the messages keyed by field. */
function collectErrors(): Record<string, string> {
  const found: Record<string, string> = {};
  if (asText(draft.name) === '') {
    found.name = 'common.requiredField';
  }
  if (isFeed.value) {
    if (asText(draft.source_url) === '') {
      found.feed = 'validation.feedUrl';
    }
  } else {
    if (draft.source_account_id <= 0) {
      found.sourceAccount = 'validation.selectSourceAccount';
    }
    if (asText(draft.source_calendar_id) === '') {
      found.sourceCalendar = 'validation.selectSourceCalendar';
    }
  }
  if (draft.destination_account_id <= 0) {
    found.destinationAccount = 'validation.selectDestinationAccount';
  }
  if (asText(draft.destination_calendar_id) === '') {
    found.destinationCalendar = 'validation.selectDestinationCalendar';
  }
  // The pair check only makes sense once both ends are filled in; blaming it
  // earlier would put "pick a different calendar" under an empty select.
  const endsKnown = !found.sourceAccount && !found.sourceCalendar;
  if (endsKnown && !found.destinationAccount && !found.destinationCalendar && sameCalendar()) {
    found.destination = 'validation.differentCalendars';
  }
  return found;
}

/** complete reports whether every required field of the whole form is filled in. */
const complete = computed(() => Object.keys(collectErrors()).length === 0);

/** validateStep checks one step alone, so "Next" only blames the current tab. */
function validateStep(step: string): boolean {
  const found = collectErrors();
  const keys = STEP_ERROR_KEYS[step] ?? [];
  for (const key of keys) {
    delete errors.value[key];
  }
  let ok = true;
  for (const key of keys) {
    if (found[key]) {
      errors.value[key] = found[key];
      ok = false;
    }
  }
  return ok;
}

/** validate checks the whole form and points the operator at the first problem. */
function validate(): boolean {
  const found = collectErrors();
  errors.value = found;
  if (Object.keys(found).length === 0) {
    return true;
  }
  if (found.name) {
    tab.value = 'details';
  } else if (found.sourceAccount || found.sourceCalendar || found.feed) {
    tab.value = 'source';
  } else if (found.destinationAccount || found.destinationCalendar || found.destination) {
    tab.value = 'destination';
  }
  return false;
}

/** nextStep advances one step after checking it. */
function nextStep(): void {
  if (stepIndex.value < STEP_ORDER.length - 1) {
    if (!validateStep(tab.value)) {
      return;
    }
    tab.value = STEP_ORDER[stepIndex.value + 1];
    return;
  }
  validate();
}

/** previousStep moves one step back. */
function previousStep(): void {
  if (stepIndex.value > 0) {
    tab.value = STEP_ORDER[stepIndex.value - 1];
  }
}

/** primaryLabel reads "Next" while the form is incomplete and Create/Save once it is. */
const primaryLabel = computed(() =>
  complete.value ? (editing.value ? t('common.save') : t('common.create')) : t('common.next'),
);

/** stepLabel is the "Step x of N" counter of the footer. */
const stepLabel = computed(() =>
  t('calendars.stepOf', { current: stepIndex.value + 1, total: STEP_ORDER.length }),
);

/** primaryAction advances while the form is incomplete and saves once it is. */
function primaryAction(): void {
  if (complete.value) {
    void submit();
    return;
  }
  nextStep();
}

/**
 * payload converts the draft into what the API accepts. The numbers are clamped
 * here to the same bounds the server applies (see `clampInt`), so what the form
 * shows after a save is what the job holds instead of what was typed.
 */
function payload(): CalendarPayload {
  return {
    ...draft,
    name: asText(draft.name),
    source_url: asText(draft.source_url),
    source_username: asText(draft.source_username),
    source_calendar_id: asText(draft.source_calendar_id),
    source_calendar_name: asText(draft.source_calendar_name),
    destination_calendar_id: asText(draft.destination_calendar_id),
    destination_calendar_name: asText(draft.destination_calendar_name),
    source_account_id: isFeed.value ? 0 : asNumber(draft.source_account_id, 0),
    direction: isFeed.value ? 'one_way' : draft.direction,
    past_days: Math.min(Math.max(asNumber(draft.past_days, 30), 0), 730),
    future_days: Math.min(Math.max(asNumber(draft.future_days, 365), 1), 1825),
    interval_minutes: Math.min(Math.max(asNumber(draft.interval_minutes, 0), 0), MAX_INTERVAL_MINUTES),
  };
}

/** submit creates or updates the job and returns to the list. */
async function submit(): Promise<void> {
  if (!validate()) {
    return;
  }
  const body = payload();
  const answer = editing.value
    ? await save(() => calendarsApi.update(id.value, body), { failure: t('calendars.saveError') })
    : await save(() => calendarsApi.create(body), { failure: t('calendars.saveError') });
  if (!answer) {
    return;
  }
  feedback.success(editing.value ? t('calendars.updated') : t('calendars.created'));
  back();
}

/** back returns to the list without saving. */
function back(): void {
  void router.push({ name: 'calendars' });
}

/**
 * The selects write `string | number` while the payload holds numbers and plain
 * strings, so each one gets a writable proxy that converts at the boundary.
 */
const sourceKind = computed({
  get: () => draft.source_kind,
  set: (value: string | number) => {
    const kind = asText(value);
    if (kind === draft.source_kind) {
      return;
    }
    draft.source_kind = kind;
    applySourceKind(kind);
  },
});

const sourceAccountId = computed({
  get: () => draft.source_account_id,
  set: (value: string | number) => {
    const id = asNumber(value);
    if (id !== draft.source_account_id) {
      draft.source_calendar_id = '';
      draft.source_calendar_name = '';
      delete errors.value.sourceCalendar;
    }
    delete errors.value.sourceAccount;
    draft.source_account_id = id;
    void loadCalendars('source');
  },
});

const destinationAccountId = computed({
  get: () => draft.destination_account_id,
  set: (value: string | number) => {
    const id = asNumber(value);
    if (id !== draft.destination_account_id) {
      draft.destination_calendar_id = '';
      draft.destination_calendar_name = '';
      delete errors.value.destinationCalendar;
      delete errors.value.destination;
    }
    delete errors.value.destinationAccount;
    draft.destination_account_id = id;
    void loadCalendars('destination');
  },
});

const sourceCalendarId = computed({
  get: () => draft.source_calendar_id,
  set: (value: string | number) => {
    applyCalendar('source', value);
  },
});

const destinationCalendarId = computed({
  get: () => draft.destination_calendar_id,
  set: (value: string | number) => {
    applyCalendar('destination', value);
  },
});

const direction = computed({
  get: () => draft.direction,
  set: (value: string | number) => {
    draft.direction = asText(value);
  },
});

const conflictPolicy = computed({
  get: () => draft.conflict_policy,
  set: (value: string | number) => {
    draft.conflict_policy = asText(value);
  },
});

// A message about a field goes away as soon as the operator fixes that field.
watch(
  () => draft.name,
  () => {
    delete errors.value.name;
  },
);
watch(
  () => draft.source_url,
  () => {
    delete errors.value.feed;
  },
);

onMounted(load);
</script>

<template>
  <div class="space-y-6">
    <PageHeader
      :title="editing ? t('calendars.editTitle') : t('calendars.createTitle')"
      :subtitle="t('calendars.editorSubtitle')"
    >
      <template #actions>
        <Button variant="ghost" @click="back">
          <ArrowLeft class="h-4 w-4" aria-hidden="true" />
          {{ t('common.back') }}
        </Button>
      </template>
    </PageHeader>

    <Alert v-if="failure" tone="destructive" :message="failure" />

    <div v-if="loading" class="flex items-center justify-center py-16">
      <Spinner size="lg" :label="t('common.loading')" />
    </div>

    <Tabs v-else v-model="tab" :items="tabs">
      <!-- 1. What the job is called and how it resolves differences. -->
      <template #details>
        <Card :title="t('calendars.steps.details')" :description="t('calendars.stepsHint.details')">
          <div class="space-y-5">
            <div class="space-y-1.5">
              <Label for="calendar-name" required>{{ t('calendars.name') }}</Label>
              <Input
                id="calendar-name"
                v-model="draft.name"
                :placeholder="t('calendars.namePlaceholder')"
                :disabled="saving"
              />
              <p v-if="errors.name" class="text-xs text-destructive">{{ t(errors.name) }}</p>
            </div>

            <div class="flex items-start justify-between gap-4 rounded-md border border-border p-3">
              <div class="space-y-1">
                <Label for="calendar-enabled">{{ t('calendars.enabled') }}</Label>
                <p class="text-xs text-muted-foreground">{{ t('calendars.enabledHint') }}</p>
              </div>
              <Switch
                id="calendar-enabled"
                v-model="draft.enabled"
                :disabled="saving"
                :label="t('calendars.enabled')"
              />
            </div>

            <div class="grid gap-4 sm:grid-cols-2">
              <div class="space-y-1.5">
                <Label for="calendar-direction" required>{{ t('calendars.direction') }}</Label>
                <Select
                  id="calendar-direction"
                  v-model="direction"
                  :options="directions"
                  :disabled="saving || isFeed"
                />
                <p class="text-xs text-muted-foreground">
                  {{ isFeed ? t('calendars.directionHintFeed') : t('calendars.directionHint') }}
                </p>
              </div>
              <div class="space-y-1.5">
                <Label for="calendar-conflict" required>{{ t('calendars.conflictPolicy') }}</Label>
                <Select
                  id="calendar-conflict"
                  v-model="conflictPolicy"
                  :options="policies"
                  :disabled="saving"
                />
                <p class="text-xs text-muted-foreground">{{ t('calendars.conflictHint') }}</p>
              </div>
            </div>
          </div>
        </Card>
      </template>

      <!-- 2. Where the job reads from: a provider calendar or an .ics feed. -->
      <template #source>
        <Card :title="t('calendars.steps.source')" :description="t('calendars.stepsHint.source')">
          <div class="space-y-5">
            <div class="space-y-1.5">
              <Label for="calendar-source-kind" required>{{ t('calendars.sourceKind') }}</Label>
              <Select
                id="calendar-source-kind"
                v-model="sourceKind"
                :options="kinds"
                :disabled="saving"
              />
            </div>

            <template v-if="isFeed">
              <div class="space-y-1.5">
                <Label for="calendar-feed-url" required :hint="t('calendars.feedUrlHint')">
                  {{ t('calendars.feedUrl') }}
                </Label>
                <Input
                  id="calendar-feed-url"
                  v-model="draft.source_url"
                  type="url"
                  placeholder="https://example.com/calendar.ics"
                  :disabled="saving"
                />
                <p v-if="errors.feed" class="text-xs text-destructive">{{ t(errors.feed) }}</p>
              </div>

              <div class="grid gap-4 sm:grid-cols-2">
                <div class="space-y-1.5">
                  <Label for="calendar-feed-user">{{ t('calendars.feedUsername') }}</Label>
                  <Input
                    id="calendar-feed-user"
                    v-model="draft.source_username"
                    autocomplete="off"
                    :disabled="saving"
                  />
                </div>
                <div class="space-y-1.5">
                  <Label for="calendar-feed-password">{{ t('calendars.feedPassword') }}</Label>
                  <Input
                    id="calendar-feed-password"
                    v-model="draft.source_password"
                    type="password"
                    autocomplete="new-password"
                    :disabled="saving || draft.clear_source_password"
                  />
                  <p v-if="editing" class="text-xs text-muted-foreground">
                    {{ t('calendars.feedPasswordKept') }}
                  </p>
                </div>
              </div>

              <div
                v-if="editing"
                class="flex items-start justify-between gap-4 rounded-md border border-border p-3"
              >
                <div class="space-y-1">
                  <Label for="calendar-feed-clear">{{ t('calendars.clearSourcePassword') }}</Label>
                  <p class="text-xs text-muted-foreground">
                    {{ t('calendars.clearSourcePasswordHint') }}
                  </p>
                </div>
                <Switch
                  id="calendar-feed-clear"
                  v-model="draft.clear_source_password"
                  :disabled="saving"
                  :label="t('calendars.clearSourcePassword')"
                />
              </div>

              <!-- Checking the feed before saving turns a typo into one request
                   instead of a job that fails on its first run. -->
              <div class="space-y-3 rounded-md border border-border p-3">
                <div class="flex flex-wrap items-center justify-between gap-2">
                  <div class="space-y-1">
                    <p class="text-sm font-medium">{{ t('calendars.checkFeed') }}</p>
                    <p class="text-xs text-muted-foreground">{{ t('calendars.checkFeedHint') }}</p>
                  </div>
                  <Button
                    variant="outline"
                    size="sm"
                    :loading="checking"
                    :disabled="saving || draft.source_url.trim() === ''"
                    @click="runFeedCheck"
                  >
                    <RefreshCw class="h-4 w-4" aria-hidden="true" />
                    {{ t('calendars.checkFeedAction') }}
                  </Button>
                </div>

                <Alert v-if="feedCheckError" tone="destructive" :message="feedCheckError" />

                <template v-if="feedCheck">
                  <p class="text-xs text-muted-foreground">
                    {{
                      t('calendars.feedCounts', {
                        events: feedCheck.events,
                        recurring: feedCheck.recurring,
                        cancelled: feedCheck.cancelled,
                      })
                    }}
                  </p>
                  <p class="text-xs text-muted-foreground">
                    {{ t('calendars.window') }}: {{ formatDateTime(feedCheck.window_start, locale) }} —
                    {{ formatDateTime(feedCheck.window_end, locale) }}
                  </p>
                  <ul v-if="feedCheck.sample.length" class="space-y-1 text-xs">
                    <li
                      v-for="event in feedCheck.sample"
                      :key="event.uid"
                      class="flex flex-wrap items-baseline gap-2"
                    >
                      <span class="truncate text-foreground">{{ event.summary || event.uid }}</span>
                      <span class="text-muted-foreground">
                        {{ formatDateTime(event.start_at, locale) }}
                      </span>
                    </li>
                  </ul>
                  <p v-else class="text-xs text-muted-foreground">
                    {{ t('calendars.previewEmpty') }}
                  </p>
                </template>
              </div>
            </template>

            <template v-else>
              <div class="space-y-1.5">
                <Label for="calendar-source-account" required>
                  {{ t('calendars.sourceAccount') }}
                </Label>
                <Select
                  id="calendar-source-account"
                  v-model="sourceAccountId"
                  :options="sourceAccountOptions"
                  :placeholder="t('validation.selectSourceAccount')"
                  :disabled="saving"
                />
                <p v-if="errors.sourceAccount" class="text-xs text-destructive">
                  {{ t(errors.sourceAccount) }}
                </p>
              </div>

              <div class="space-y-1.5">
                <Label for="calendar-source-calendar" required>
                  {{ t('calendars.sourceCalendar') }}
                </Label>
                <Select
                  id="calendar-source-calendar"
                  v-model="sourceCalendarId"
                  :options="sourceCalendarOptions"
                  :placeholder="t('validation.selectSourceCalendar')"
                  :disabled="saving || calendarsLoading || draft.source_account_id <= 0"
                />
                <p v-if="calendarsLoading" class="text-xs text-muted-foreground">
                  {{ t('common.loading') }}
                </p>
                <p v-if="sourceCalendarsError" class="text-xs text-destructive">
                  {{ sourceCalendarsError }}
                </p>
                <p v-if="errors.sourceCalendar" class="text-xs text-destructive">
                  {{ t(errors.sourceCalendar) }}
                </p>
              </div>
            </template>
          </div>
        </Card>
      </template>

      <!-- 3. Where the job writes to. A subscription has no writable end of its
           own, so this is the only place its events ever land. -->
      <template #destination>
        <Card
          :title="t('calendars.steps.destination')"
          :description="t('calendars.stepsHint.destination')"
        >
          <div class="space-y-5">
            <div class="space-y-1.5">
              <Label for="calendar-destination-account" required>
                {{ t('calendars.destinationAccount') }}
              </Label>
              <Select
                id="calendar-destination-account"
                v-model="destinationAccountId"
                :options="destinationAccountOptions"
                :placeholder="t('validation.selectDestinationAccount')"
                :disabled="saving"
              />
              <p v-if="errors.destinationAccount" class="text-xs text-destructive">
                {{ t(errors.destinationAccount) }}
              </p>
            </div>

            <div class="space-y-1.5">
              <Label for="calendar-destination-calendar" required>
                {{ t('calendars.destinationCalendar') }}
              </Label>
              <Select
                id="calendar-destination-calendar"
                v-model="destinationCalendarId"
                :options="destinationCalendarOptions"
                :placeholder="t('validation.selectDestinationCalendar')"
                :disabled="saving || calendarsLoading || draft.destination_account_id <= 0"
              />
              <p v-if="calendarsLoading" class="text-xs text-muted-foreground">
                {{ t('common.loading') }}
              </p>
              <p v-if="destinationCalendarsError" class="text-xs text-destructive">
                {{ destinationCalendarsError }}
              </p>
              <p v-if="errors.destinationCalendar" class="text-xs text-destructive">
                {{ t(errors.destinationCalendar) }}
              </p>
              <p v-if="errors.destination" class="text-xs text-destructive">
                {{ t(errors.destination) }}
              </p>
              <p class="text-xs text-muted-foreground">{{ t('calendars.destinationHint') }}</p>
            </div>
          </div>
        </Card>
      </template>

      <!-- 4. The window the job reads and how often it runs. -->
      <template #options>
        <Card :title="t('calendars.steps.options')" :description="t('calendars.stepsHint.options')">
          <div class="space-y-5">
            <div class="grid gap-4 sm:grid-cols-2">
              <div class="space-y-1.5">
                <Label for="calendar-past-days" :hint="t('calendars.pastDaysHint')">
                  {{ t('calendars.pastDays') }}
                </Label>
                <Input
                  id="calendar-past-days"
                  v-model="draft.past_days"
                  type="number"
                  min="0"
                  max="730"
                  numeric
                  :disabled="saving"
                />
              </div>
              <div class="space-y-1.5">
                <Label for="calendar-future-days" :hint="t('calendars.futureDaysHint')">
                  {{ t('calendars.futureDays') }}
                </Label>
                <Input
                  id="calendar-future-days"
                  v-model="draft.future_days"
                  type="number"
                  min="1"
                  max="1825"
                  numeric
                  :disabled="saving"
                />
              </div>
            </div>
            <p class="text-xs text-muted-foreground">{{ t('calendars.windowHint') }}</p>

            <div class="space-y-1.5">
              <Label for="calendar-interval" :hint="t('calendars.intervalHint')">
                {{ t('calendars.interval') }}
              </Label>
              <Input
                id="calendar-interval"
                v-model="draft.interval_minutes"
                type="number"
                min="0"
                :max="MAX_INTERVAL_MINUTES"
                step="5"
                numeric
                :disabled="saving"
              />
              <p class="text-xs text-muted-foreground">
                {{ calendarScheduleLabel(t, asNumber(draft.interval_minutes)) }}
              </p>
            </div>

            <div class="flex items-start justify-between gap-4 rounded-md border border-border p-3">
              <div class="space-y-1">
                <Label for="calendar-delete-missing">{{ t('calendars.deleteMissing') }}</Label>
                <p class="text-xs text-muted-foreground">{{ t('calendars.deleteMissingHint') }}</p>
              </div>
              <Switch
                id="calendar-delete-missing"
                v-model="draft.delete_missing"
                :disabled="saving"
                :label="t('calendars.deleteMissing')"
              />
            </div>
          </div>
        </Card>
      </template>
    </Tabs>

    <div
      v-if="!loading"
      class="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-border bg-card p-4"
    >
      <span class="text-xs text-muted-foreground">{{ stepLabel }}</span>
      <div class="flex flex-wrap items-center gap-2">
        <Button variant="ghost" :disabled="saving" @click="back">{{ t('common.cancel') }}</Button>
        <Button variant="outline" :disabled="saving || stepIndex === 0" @click="previousStep">
          <ArrowLeft class="h-4 w-4" aria-hidden="true" />
          {{ t('common.previous') }}
        </Button>
        <Button :loading="saving" @click="primaryAction">
          <Save v-if="!saving && complete" class="h-4 w-4" aria-hidden="true" />
          <ArrowRight v-else-if="!saving" class="h-4 w-4" aria-hidden="true" />
          {{ primaryLabel }}
        </Button>
      </div>
    </div>
  </div>
</template>
