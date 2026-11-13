<script setup lang="ts">
// CalendarsView — the list of calendar pairings.
//
// A calendar job keeps two calendars aligned (Google and Microsoft in either
// combination) or subscribes the destination to an .ics feed, inside a window
// around today (see internal/services/calendarsync.go). The list answers what is
// paired with what, how often it runs and what happened last; the two dialogs
// answer the questions a pairing raises: what would a run do right now, and what
// did the last runs actually decide.
import {
  CalendarDays,
  CircleStop,
  Eye,
  History,
  Pencil,
  Play,
  Plus,
  RefreshCw,
  Trash2,
} from '@lucide/vue';
import { computed, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRouter } from 'vue-router';

import ConfirmDialog from '@/components/ConfirmDialog.vue';
import PageHeader from '@/components/PageHeader.vue';
import StatusBadge from '@/components/StatusBadge.vue';
import Alert from '@/components/ui/Alert.vue';
import Badge from '@/components/ui/Badge.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import Dialog from '@/components/ui/Dialog.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Spinner from '@/components/ui/Spinner.vue';
import Switch from '@/components/ui/Switch.vue';
import { useAction } from '@/composables/useAction';
import { accountLabel as accountLabelOf } from '@/lib/accounts';
import {
  accounts as accountsApi,
  calendars as calendarsApi,
  messageOf,
  type CalendarJob,
  type CalendarPreview,
  type CalendarRun,
  type CalendarRunItem,
  type ConnectedAccount,
} from '@/lib/api';
import { actionLabel, calendarScheduleLabel, payloadFromJob } from '@/lib/calendars';
import { formatDateTime, formatDuration, formatRelative } from '@/lib/format';
import { useFeedbackStore } from '@/stores/feedback';

const { t, te, locale } = useI18n();
const router = useRouter();
const feedback = useFeedbackStore();

const rows = ref<CalendarJob[]>([]);
const accounts = ref<ConnectedAccount[]>([]);
const loading = ref(true);
const failure = ref('');
/** activeId is the job a row-scoped long call belongs to, so only its spinner runs. */
const activeId = ref(0);
const removal = ref<CalendarJob | null>(null);

/** The preview of one job, and the history of another. */
const preview = ref<CalendarPreview | null>(null);
const previewJob = ref<CalendarJob | null>(null);
const previewOpen = ref(false);
const historyJob = ref<CalendarJob | null>(null);
const historyRuns = ref<CalendarRun[]>([]);
const historyOpen = ref(false);
const historyError = ref('');
const selectedRun = ref<CalendarRun | null>(null);
const selectedItems = ref<CalendarRunItem[]>([]);

const { busy: running, run: runNow } = useAction();
const { busy: cancelling, run: runCancel } = useAction();
const { busy: toggling, run: runToggle } = useAction();
const { busy: removing, run: runRemove } = useAction();
const { busy: previewing, run: runPreview } = useAction();
const { busy: historyLoading, run: runHistory } = useAction();
const { busy: runLoading, run: runRun } = useAction();

/** confirmOpen drives the confirmation dialog from the row that was picked. */
const confirmOpen = computed({
  get: () => removal.value !== null,
  set: (open: boolean) => {
    if (!open) {
      removal.value = null;
    }
  },
});

/** accountsById resolves the account ids of a job into readable labels. */
const accountsById = computed(() => {
  const map = new Map<number, ConnectedAccount>();
  for (const account of accounts.value) {
    map.set(account.id, account);
  }
  return map;
});

/** accountLabel names one side of a job, falling back to the raw id. */
function accountLabel(id: number): string {
  const account = accountsById.value.get(id);
  return account ? accountLabelOf(account) : t('common.unknown');
}

/** when renders a timestamp of the API, or an em dash when it is missing. */
function when(value: string | null | undefined): string {
  return formatDateTime(value, locale.value);
}

/** next renders the next planned execution in relative terms. */
function next(job: CalendarJob): string {
  return job.next_run_at
    ? formatRelative(job.next_run_at, locale.value)
    : t('calendars.notScheduled');
}

/**
 * sourceLabel names the source end of a row: the account and the calendar, or
 * the address of the feed. A subscription has no account, which is exactly why
 * its row cannot be read like a provider pair.
 */
function sourceLabel(job: CalendarJob): string {
  if (job.source_kind === 'ics') {
    return job.source_url ?? '';
  }
  const calendar = job.source_calendar_name || job.source_calendar_id;
  return `${accountLabel(job.source_account_id)} · ${calendar}`;
}

/** destinationLabel names the destination end of a row. */
function destinationLabel(job: CalendarJob): string {
  const calendar = job.destination_calendar_name || job.destination_calendar_id;
  return `${accountLabel(job.destination_account_id)} · ${calendar}`;
}

/** load reads the jobs and the accounts their labels are built from. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const [list, connected] = await Promise.all([calendarsApi.list(), accountsApi.list()]);
    rows.value = list.calendars;
    accounts.value = connected.accounts;
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/** run starts a manual execution; the run itself continues in background. */
async function run(job: CalendarJob): Promise<void> {
  activeId.value = job.id;
  const answer = await runNow(() => calendarsApi.run(job.id), {
    success: t('calendars.started'),
    failure: t('calendars.runError'),
  });
  if (answer) {
    await load();
  }
  activeId.value = 0;
}

/** cancel stops the run of a job that is in flight. */
async function cancel(job: CalendarJob): Promise<void> {
  activeId.value = job.id;
  const answer = await runCancel(() => calendarsApi.cancel(job.id), {
    success: t('calendars.cancelled'),
    failure: t('calendars.cancelError'),
  });
  if (answer) {
    await load();
  }
  activeId.value = 0;
}

/**
 * toggle flips one switch with a whole-job update: the API replaces a job as a
 * whole, and `payloadFromJob` is what makes the untouched fields travel along.
 */
async function toggle(job: CalendarJob, enabled: boolean): Promise<void> {
  activeId.value = job.id;
  const answer = await runToggle(
    () => calendarsApi.update(job.id, payloadFromJob(job, { enabled })),
    { failure: t('calendars.saveError') },
  );
  if (answer) {
    rows.value = rows.value.map((row) => (row.id === answer.id ? answer : row));
  }
  activeId.value = 0;
}

/** openPreview asks the server what a run would do, without writing anything. */
async function openPreview(job: CalendarJob): Promise<void> {
  activeId.value = job.id;
  previewJob.value = job;
  const answer = await runPreview(() => calendarsApi.preview(job.id), {
    failure: t('calendars.previewError'),
  });
  activeId.value = 0;
  if (!answer) {
    return;
  }
  preview.value = answer;
  previewOpen.value = true;
}

/** openHistory lists the runs of one job. */
async function openHistory(job: CalendarJob): Promise<void> {
  activeId.value = job.id;
  historyJob.value = job;
  historyRuns.value = [];
  historyError.value = '';
  selectedRun.value = null;
  selectedItems.value = [];
  const answer = await runHistory(() => calendarsApi.runs(job.id), {
    failure: t('calendars.historyError'),
  });
  activeId.value = 0;
  historyOpen.value = true;
  if (!answer) {
    historyError.value = t('calendars.historyError');
    return;
  }
  historyRuns.value = answer.runs;
}

/** openRun loads the per-event evidence of one run of the open history. */
async function openRun(entry: CalendarRun): Promise<void> {
  const job = historyJob.value;
  if (!job) {
    return;
  }
  const answer = await runRun(() => calendarsApi.runDetail(job.id, entry.id), {
    failure: t('calendars.historyError'),
  });
  if (!answer) {
    return;
  }
  selectedRun.value = answer.run;
  selectedItems.value = answer.items;
}

/** confirmRemoval deletes the job together with its run history. */
async function confirmRemoval(): Promise<void> {
  const job = removal.value;
  if (!job) {
    return;
  }
  const answer = await runRemove(() => calendarsApi.remove(job.id), {
    failure: t('calendars.removeError'),
  });
  if (!answer) {
    return;
  }
  removal.value = null;
  await load();
  feedback.success(t('calendars.removed'));
}

/** create and edit move to the editor, which owns the form as a route. */
function create(): void {
  void router.push({ name: 'calendar-new' });
}

function edit(job: CalendarJob): void {
  void router.push({ name: 'calendar-edit', params: { id: job.id } });
}

onMounted(load);
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('calendars.title')" :subtitle="t('calendars.subtitle')">
      <template #actions>
        <Button variant="outline" :loading="loading" @click="load">
          <RefreshCw class="h-4 w-4" aria-hidden="true" />
          {{ t('common.refresh') }}
        </Button>
        <Button :disabled="accounts.length === 0" @click="create">
          <Plus class="h-4 w-4" aria-hidden="true" />
          {{ t('calendars.new') }}
        </Button>
      </template>
    </PageHeader>

    <Alert v-if="failure" tone="destructive" :title="t('calendars.loadError')" :message="failure">
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <div v-if="loading && rows.length === 0" class="flex items-center justify-center py-16">
      <Spinner size="lg" :label="t('common.loading')" />
    </div>

    <Card v-else-if="rows.length === 0" content-class="p-0">
      <EmptyState
        :icon="CalendarDays"
        :title="t('calendars.empty')"
        :message="t('calendars.emptyHint')"
      >
        <Button :disabled="accounts.length === 0" @click="create">
          <Plus class="h-4 w-4" aria-hidden="true" />
          {{ t('calendars.new') }}
        </Button>
      </EmptyState>
    </Card>

    <Card v-else content-class="p-0">
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('calendars.name') }}</th>
              <th scope="col" class="px-4 py-2 text-start font-medium">
                {{ t('calendars.source') }}
              </th>
              <th scope="col" class="px-4 py-2 text-start font-medium">
                {{ t('calendars.destination') }}
              </th>
              <th scope="col" class="px-4 py-2 text-start font-medium">
                {{ t('calendars.direction') }}
              </th>
              <th scope="col" class="px-4 py-2 text-start font-medium">
                {{ t('calendars.interval') }}
              </th>
              <th scope="col" class="px-4 py-2 text-start font-medium">
                {{ t('calendars.lastRun') }}
              </th>
              <th scope="col" class="px-4 py-2 text-start font-medium">
                {{ t('calendars.nextRun') }}
              </th>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('common.enabled') }}</th>
              <th scope="col" class="px-4 py-2 text-end font-medium">{{ t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-border">
            <tr v-for="job in rows" :key="job.id" class="align-top">
              <td class="px-4 py-3">
                <p class="font-medium">{{ job.name }}</p>
                <div class="mt-1 flex flex-wrap items-center gap-1">
                  <Badge tone="outline">{{ t(`calendars.source_${job.source_kind}`) }}</Badge>
                  <Badge v-if="job.delete_missing" tone="secondary">
                    {{ t('calendars.deleteMissing') }}
                  </Badge>
                </div>
              </td>
              <td class="px-4 py-3">
                <p class="break-anywhere">{{ sourceLabel(job) }}</p>
                <p v-if="job.source_etag || job.source_fetched_at" class="text-xs text-muted-foreground">
                  {{ t('calendars.feedFetched') }}: {{ when(job.source_fetched_at) }}
                </p>
              </td>
              <td class="px-4 py-3 break-anywhere">{{ destinationLabel(job) }}</td>
              <td class="px-4 py-3">
                <Badge tone="outline">{{ t(`calendars.direction_${job.direction}`) }}</Badge>
                <p class="mt-1 text-xs text-muted-foreground">
                  {{ t(`jobs.conflict_${job.conflict_policy}`) }}
                </p>
              </td>
              <td class="px-4 py-3">
                <p>{{ calendarScheduleLabel(t, job.interval_minutes) }}</p>
                <p class="text-xs text-muted-foreground">
                  {{ t('calendars.window') }}: {{ job.past_days }} / {{ job.future_days }}
                </p>
              </td>
              <td class="px-4 py-3">
                <StatusBadge v-if="job.last_status" :status="job.last_status" />
                <span v-else class="text-muted-foreground">{{ t('calendars.noRuns') }}</span>
                <p v-if="job.last_run_at" class="mt-1 text-xs text-muted-foreground">
                  {{ when(job.last_run_at) }}
                </p>
                <p v-if="job.last_error" class="mt-1 max-w-64 break-anywhere text-xs text-destructive">
                  {{ job.last_error }}
                </p>
              </td>
              <td class="px-4 py-3 text-muted-foreground">{{ next(job) }}</td>
              <td class="px-4 py-3">
                <Switch
                  :model-value="job.enabled"
                  :disabled="toggling && activeId === job.id"
                  :label="t('calendars.enabled')"
                  @update:model-value="(value: boolean) => toggle(job, value)"
                />
              </td>
              <td class="px-4 py-3">
                <div class="flex flex-wrap items-center justify-end gap-1">
                  <Button
                    v-if="job.running"
                    variant="outline"
                    size="sm"
                    :loading="cancelling && activeId === job.id"
                    @click="cancel(job)"
                  >
                    <CircleStop class="h-4 w-4" aria-hidden="true" />
                    {{ t('calendars.cancel') }}
                  </Button>
                  <Button
                    v-else
                    variant="outline"
                    size="sm"
                    :loading="running && activeId === job.id"
                    @click="run(job)"
                  >
                    <Play class="h-4 w-4" aria-hidden="true" />
                    {{ t('calendars.runNow') }}
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    :loading="previewing && activeId === job.id"
                    @click="openPreview(job)"
                  >
                    <Eye class="h-4 w-4" aria-hidden="true" />
                    {{ t('calendars.preview') }}
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    :loading="historyLoading && activeId === job.id"
                    @click="openHistory(job)"
                  >
                    <History class="h-4 w-4" aria-hidden="true" />
                    {{ t('calendars.history') }}
                  </Button>
                  <Button variant="ghost" size="sm" @click="edit(job)">
                    <Pencil class="h-4 w-4" aria-hidden="true" />
                    {{ t('common.edit') }}
                  </Button>
                  <Button variant="ghost" size="sm" @click="removal = job">
                    <Trash2 class="h-4 w-4" aria-hidden="true" />
                    {{ t('common.delete') }}
                  </Button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- The preview is the plan of the stored job, computed by the server; it is
         the only honest answer to "what happens if I press Sync now". -->
    <Dialog
      v-model:open="previewOpen"
      size="xl"
      :title="t('calendars.previewTitle')"
      :description="previewJob?.name"
      :close-label="t('common.close')"
    >
      <div v-if="preview" class="space-y-4">
        <Alert tone="muted" :message="t('calendars.previewHint')" />
        <div class="space-y-1 text-sm">
          <p>
            {{ t('calendars.previewListed', { source: preview.source_listed, destination: preview.destination_listed }) }}
          </p>
          <p class="text-muted-foreground">
            {{ t('calendars.previewScanned', { count: preview.events_scanned }) }}
          </p>
          <p class="text-xs text-muted-foreground">
            {{ t('calendars.window') }}: {{ when(preview.window_start) }} — {{ when(preview.window_end) }}
          </p>
        </div>
        <p v-if="preview.plan.length === 0" class="text-sm text-muted-foreground">
          {{ t('calendars.previewEmpty') }}
        </p>
        <div v-else class="overflow-x-auto rounded-lg border border-border">
          <table class="w-full text-sm">
            <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
              <tr>
                <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('calendars.event') }}</th>
                <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('calendars.startAt') }}</th>
                <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('calendars.action') }}</th>
                <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('calendars.evidence') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-border">
              <tr v-for="decision in preview.plan" :key="`${decision.uid}-${decision.action}`">
                <td class="px-3 py-2">
                  <p>{{ decision.summary || decision.uid }}</p>
                  <p class="text-xs text-muted-foreground break-anywhere">{{ decision.uid }}</p>
                </td>
                <td class="px-3 py-2 text-muted-foreground">{{ when(decision.start_at) }}</td>
                <td class="px-3 py-2">
                  <Badge tone="outline">{{ actionLabel(t, te, decision.action) }}</Badge>
                </td>
                <td class="px-3 py-2 text-xs text-muted-foreground break-anywhere">
                  {{ decision.evidence }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
      <template #footer>
        <Button variant="outline" @click="previewOpen = false">{{ t('common.close') }}</Button>
      </template>
    </Dialog>

    <!-- History: the runs of one job, and the decisions of the run that is open.
         The evidence column is the point of the screen — "skipped because the
         destination changed after the source" is what explains a surprising run. -->
    <Dialog
      v-model:open="historyOpen"
      size="xl"
      :title="t('calendars.historyTitle')"
      :description="historyJob?.name"
      :close-label="t('common.close')"
    >
      <div class="space-y-4">
        <Alert v-if="historyError" tone="destructive" :message="historyError" />
        <p v-if="!historyLoading && historyRuns.length === 0 && !historyError" class="text-sm text-muted-foreground">
          {{ t('calendars.historyEmpty') }}
        </p>
        <div v-else class="grid gap-4 lg:grid-cols-5">
          <ul class="space-y-1 lg:col-span-2">
            <li v-for="entry in historyRuns" :key="entry.id">
              <button
                type="button"
                class="w-full rounded-md border border-border p-2 text-start text-sm transition-colors hover:bg-accent disabled:opacity-60"
                :class="selectedRun?.id === entry.id ? 'bg-accent' : ''"
                :disabled="runLoading"
                @click="openRun(entry)"
              >
                <span class="flex flex-wrap items-center gap-1">
                  <StatusBadge :status="entry.status" />
                  <Badge tone="secondary">{{ t(`calendars.trigger_${entry.trigger}`) }}</Badge>
                </span>
                <span class="mt-1 block text-xs text-muted-foreground">
                  {{ when(entry.started_at) }} · {{ formatDuration(entry.duration_ms, t) }}
                </span>
                <span class="mt-1 block text-xs text-muted-foreground">
                  {{ t('calendars.runCounts', {
                    created: entry.events_created,
                    updated: entry.events_updated,
                    deleted: entry.events_deleted,
                    skipped: entry.events_skipped,
                  }) }}
                </span>
              </button>
            </li>
          </ul>

          <div class="space-y-2 lg:col-span-3">
            <p v-if="runLoading" class="flex items-center gap-2 text-sm text-muted-foreground">
              <Spinner size="sm" />
              {{ t('common.loading') }}
            </p>
            <p v-else-if="!selectedRun" class="text-sm text-muted-foreground">
              {{ t('calendars.historyPick') }}
            </p>
            <template v-else>
              <p class="text-sm font-medium">{{ t('calendars.historyItems') }}</p>
              <p v-if="selectedItems.length === 0" class="text-sm text-muted-foreground">
                {{ t('calendars.historyItemsEmpty') }}
              </p>
              <ul v-else class="space-y-1">
                <li
                  v-for="item in selectedItems"
                  :key="item.id"
                  class="rounded-md border border-border p-2 text-sm"
                >
                  <div class="flex flex-wrap items-center gap-1">
                    <Badge tone="outline">{{ actionLabel(t, te, item.action) }}</Badge>
                    <span class="text-xs text-muted-foreground">{{ when(item.start_at) }}</span>
                  </div>
                  <p class="mt-1">{{ item.summary || item.uid }}</p>
                  <p v-if="item.evidence" class="text-xs text-muted-foreground break-anywhere">
                    {{ item.evidence }}
                  </p>
                </li>
              </ul>
            </template>
          </div>
        </div>
      </div>
      <template #footer>
        <Button variant="outline" @click="historyOpen = false">{{ t('common.close') }}</Button>
      </template>
    </Dialog>

    <ConfirmDialog
      v-model:open="confirmOpen"
      :title="t('calendars.deleteTitle')"
      :message="t('calendars.deleteMessage')"
      :confirm-label="t('common.delete')"
      :loading="removing"
      @confirm="confirmRemoval"
    >
      {{ removal?.name ?? '' }}
    </ConfirmDialog>
  </div>
</template>
