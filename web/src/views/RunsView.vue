<script setup lang="ts">
// RunsView — the log of everything the engine copied.
//
// A run is one execution of a job (see internal/handlers/runs.go). This screen
// answers two questions: "what did the last runs change?" from the counters of
// the list, and "which files, exactly?" from the file operations, which are read
// lazily when a run is opened because a single run can hold thousands of them.
import { Eye, History, ListChecks, RefreshCw } from '@lucide/vue';
import { computed, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';

import PageHeader from '@/components/PageHeader.vue';
import StatusBadge from '@/components/StatusBadge.vue';
import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import Dialog from '@/components/ui/Dialog.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Select from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import {
  jobs as jobsApi,
  messageOf,
  runs as runsApi,
  type RunItem,
  type SyncJob,
  type SyncRun,
} from '@/lib/api';
import { formatBytes, formatDateTime, formatDuration, formatNumber } from '@/lib/format';

/** LIST_LIMIT is how many runs the log shows at once. */
const LIST_LIMIT = 50;
/** ITEMS_LIMIT is the page size of the file operations of one run. */
const ITEMS_LIMIT = 200;

const route = useRoute();
const router = useRouter();
const { t, locale } = useI18n();

const rows = ref<SyncRun[]>([]);
const jobs = ref<SyncJob[]>([]);
const jobId = ref(0);
const loading = ref(true);
const failure = ref('');

const detail = ref<SyncRun | null>(null);
const items = ref<RunItem[]>([]);
const itemsLoading = ref(false);
const itemsFailure = ref('');

/** jobOptions starts with "every job", which is the default filter. */
const jobOptions = computed(() => [
  { value: 0, label: t('runs.allJobs') },
  ...jobs.value.map((job) => ({ value: job.id, label: job.name })),
]);

/** running is true while a run of the log is still in flight. */
const running = computed(() => rows.value.some((run) => run.status === 'running'));

/** detailOpen drives the dialog from the run that was picked. */
const detailOpen = computed({
  get: () => detail.value !== null,
  set: (open: boolean) => {
    if (!open) {
      detail.value = null;
      items.value = [];
      itemsFailure.value = '';
    }
  },
});

/** counters is the summary of the opened run, label by label. */
const counters = computed(() => {
  const run = detail.value;
  if (!run) {
    return [];
  }
  return [
    { key: 'scanned', label: t('runs.scanned'), value: count(run.files_scanned) },
    { key: 'created', label: t('runs.created'), value: count(run.files_created) },
    { key: 'updated', label: t('runs.updated'), value: count(run.files_updated) },
    { key: 'deleted', label: t('runs.deleted'), value: count(run.files_deleted) },
    { key: 'skipped', label: t('runs.skipped'), value: count(run.files_skipped) },
    { key: 'folders', label: t('runs.folders'), value: count(run.folders_created) },
    { key: 'conflicts', label: t('runs.conflicts'), value: count(run.conflicts) },
    { key: 'errors', label: t('runs.errors'), value: count(run.errors) },
    { key: 'bytes', label: t('runs.bytes'), value: size(run.bytes_transferred) },
    { key: 'trigger', label: t('runs.trigger'), value: run.trigger },
  ];
});

/** when renders a timestamp of the API, or the em dash of the catalog. */
function when(value: string | null | undefined): string {
  return formatDateTime(value, locale.value);
}

/** elapsed renders how long a run took. */
function elapsed(run: SyncRun): string {
  return formatDuration(run.duration_ms, t);
}

/** count renders a counter with the grouping of the active locale. */
function count(value: number): string {
  return formatNumber(value, locale.value);
}

/** size renders a byte total with the units of the active locale. */
function size(value: number): string {
  return formatBytes(value, locale.value);
}

/** load reads the log of the selected job together with the filter options. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const [log, list] = await Promise.all([
      runsApi.list(LIST_LIMIT, jobId.value > 0 ? jobId.value : undefined),
      jobsApi.list(),
    ]);
    rows.value = log.runs;
    jobs.value = list.jobs;
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/** applyFilter reloads the log when the operator picks another job. */
function applyFilter(value: string | number): void {
  jobId.value = Number(value);
  void load();
}

/** open reads the file operations of one run. */
async function open(run: SyncRun): Promise<void> {
  detail.value = run;
  items.value = [];
  itemsFailure.value = '';
  itemsLoading.value = true;
  try {
    const answer = await runsApi.get(run.id);
    // The detail carries the counters as they stand now, so a run still in
    // flight updates its own row without a reload of the whole log.
    detail.value = answer.run;
    items.value = answer.items;
    rows.value = rows.value.map((row) => (row.id === answer.run.id ? answer.run : row));
  } catch (error) {
    itemsFailure.value = messageOf(error);
  } finally {
    itemsLoading.value = false;
  }
}

/** openJob leaves the log for the job that produced the run. */
function openJob(run: SyncRun): void {
  void router.push({ name: 'job-edit', params: { id: String(run.job_id) } });
}

// `/runs?job=12` is what the "history" action of the job list opens.
onMounted(() => {
  const requested = Number(route.query.job ?? 0);
  if (Number.isFinite(requested) && requested > 0) {
    jobId.value = requested;
  }
  void load();
});
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('runs.title')" :subtitle="t('runs.subtitle')">
      <template #actions>
        <div class="flex flex-wrap items-end gap-2">
          <div class="space-y-1">
            <p class="text-xs font-medium text-muted-foreground">{{ t('runs.job') }}</p>
            <Select
              id="runs-job"
              class="w-52"
              :model-value="jobId"
              :options="jobOptions"
              @update:model-value="applyFilter"
            />
          </div>
          <Button variant="outline" :loading="loading" @click="load">
            <RefreshCw class="h-4 w-4" aria-hidden="true" />
            {{ t('common.refresh') }}
          </Button>
        </div>
      </template>
    </PageHeader>

    <Alert v-if="failure" tone="destructive" :title="t('runs.loadError')" :message="failure">
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <Alert v-else-if="running" tone="info" :message="t('runs.runningNow')" />

    <div v-if="loading && rows.length === 0" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <Card v-else-if="rows.length === 0" content-class="p-0">
      <EmptyState :icon="History" :title="t('runs.empty')" :message="t('runs.subtitle')" />
    </Card>

    <Card v-else content-class="p-0">
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.job') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.status') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.trigger') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.startedAt') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.finishedAt') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('runs.duration') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('runs.conflicts') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('runs.errors') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('runs.bytes') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-border">
            <tr v-for="run in rows" :key="run.id" class="hover:bg-muted/40">
              <td class="min-w-0 px-3 py-2">
                <button
                  type="button"
                  class="break-anywhere text-start font-medium hover:underline"
                  @click="openJob(run)"
                >
                  {{ run.job_name }}
                </button>
              </td>
              <td class="px-3 py-2"><StatusBadge :status="run.status" /></td>
              <td class="px-3 py-2"><StatusBadge :status="run.trigger" /></td>
              <td class="px-3 py-2 text-muted-foreground"><span class="numeric">{{ when(run.started_at) }}</span></td>
              <td class="px-3 py-2 text-muted-foreground"><span class="numeric">{{ when(run.finished_at) }}</span></td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ elapsed(run) }}</span></td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ count(run.conflicts) }}</span></td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ count(run.errors) }}</span></td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ size(run.bytes_transferred) }}</span></td>
              <td class="px-3 py-2 text-end">
                <Button variant="ghost" size="sm" @click="open(run)">
                  <Eye class="h-4 w-4" aria-hidden="true" />
                  {{ t('runs.items') }}
                </Button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Dialog
      v-model:open="detailOpen"
      :title="t('runs.items')"
      :description="detail ? `${detail.job_name} · ${when(detail.started_at)}` : ''"
      size="xl"
      :close-label="t('common.closeDialog')"
    >
      <Alert
        v-if="itemsFailure"
        tone="destructive"
        :title="t('runs.itemsError')"
        :message="itemsFailure"
      />

      <div v-else-if="itemsLoading" class="flex items-center justify-center gap-2 py-6">
        <Spinner :label="t('common.loading')" />
        <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
      </div>

      <div v-else-if="detail" class="space-y-4">
        <dl class="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <div
            v-for="counter in counters"
            :key="counter.key"
            class="space-y-1 rounded-lg border border-border p-3"
          >
            <dt class="text-xs text-muted-foreground">{{ counter.label }}</dt>
            <dd class="text-sm font-medium"><span class="numeric">{{ counter.value }}</span></dd>
          </div>
        </dl>

        <p v-if="detail.message" class="break-anywhere text-sm text-muted-foreground">
          {{ detail.message }}
        </p>

        <EmptyState v-if="items.length === 0" :icon="ListChecks" :title="t('runs.itemsEmpty')" />

        <div v-else class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
              <tr>
                <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.action') }}</th>
                <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.path') }}</th>
                <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('runs.size') }}</th>
                <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.evidence') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-border">
              <tr v-for="item in items" :key="item.id" class="align-top">
                <td class="px-3 py-2"><StatusBadge :status="item.action" /></td>
                <td class="break-anywhere px-3 py-2">{{ item.path }}</td>
                <td class="px-3 py-2 text-end text-muted-foreground">
                  <span class="numeric">{{ size(item.size) }}</span>
                </td>
                <td class="break-anywhere px-3 py-2 text-xs text-muted-foreground">
                  {{ item.evidence || t('common.notAvailable') }}
                </td>
              </tr>
            </tbody>
          </table>
          <p v-if="items.length >= ITEMS_LIMIT" class="px-3 py-3 text-xs text-muted-foreground">
            {{ t('runs.limitReached', { count: count(items.length) }) }}
          </p>
        </div>
      </div>

      <template #footer>
        <Button variant="outline" @click="detailOpen = false">{{ t('common.close') }}</Button>
        <Button v-if="detail" @click="openJob(detail)">{{ t('runs.openJob') }}</Button>
      </template>
    </Dialog>
  </div>
</template>
