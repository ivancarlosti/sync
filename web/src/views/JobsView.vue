<script setup lang="ts">
// JobsView — the synchronisation jobs of this instance.
//
// A job is the unit the engine works on (see internal/handlers/jobs.go), so this
// screen answers three questions about each one: where does it copy from and to,
// when does it run, and what happened the last time. Everything destructive
// (delete) or long (run now) is confirmed or reported through the toast store,
// and a job that is currently copying is recognised from the `running` flag the
// API decorates the row with, so the buttons match the state instead of the
// operator having to guess.
import { CircleStop, FolderSync, Pencil, Play, Plus, RefreshCw, Trash2 } from '@lucide/vue';
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
import EmptyState from '@/components/ui/EmptyState.vue';
import Spinner from '@/components/ui/Spinner.vue';
import Switch from '@/components/ui/Switch.vue';
import { useAction } from '@/composables/useAction';
import { accountLabel as accountLabelOf } from '@/lib/accounts';
import {
  accounts as accountsApi,
  jobs as jobsApi,
  messageOf,
  type ConnectedAccount,
  type SyncJob,
} from '@/lib/api';
import { formatDateTime, formatRelative } from '@/lib/format';
import { payloadFromJob, scheduleLabel } from '@/lib/jobs';
import { useFeedbackStore } from '@/stores/feedback';

const router = useRouter();
const { t, locale } = useI18n();
const feedback = useFeedbackStore();

const rows = ref<SyncJob[]>([]);
const accounts = ref<ConnectedAccount[]>([]);
const loading = ref(true);
const failure = ref('');
/** activeId is the job a row-scoped long call belongs to, so only its spinner runs. */
const activeId = ref(0);
const removal = ref<SyncJob | null>(null);

const { busy: running, run: runNow } = useAction();
const { busy: cancelling, run: runCancel } = useAction();
const { busy: saving, run: runSave } = useAction();
const { busy: removing, run: runRemove } = useAction();

/** accountsById resolves the account ids of a job into readable labels. */
const accountsById = computed(() => {
  const map = new Map<number, ConnectedAccount>();
  for (const account of accounts.value) {
    map.set(account.id, account);
  }
  return map;
});

/** confirmOpen drives the confirmation dialog from the row that was picked. */
const confirmOpen = computed({
  get: () => removal.value !== null,
  set: (open: boolean) => {
    if (!open) {
      removal.value = null;
    }
  },
});

/** accountLabel names one side of a job, falling back to the raw id. */
function accountLabel(id: number): string {
  const account = accountsById.value.get(id);
  if (!account) {
    return t('common.unknown');
  }
  return accountLabelOf(account);
}

/** when renders a timestamp of the API, or an em dash when it is missing. */
function when(value: string | null | undefined): string {
  return formatDateTime(value, locale.value);
}

/** next renders the next planned execution in relative terms. */
function next(job: SyncJob): string {
  return job.next_run_at ? formatRelative(job.next_run_at, locale.value) : t('jobs.notScheduled');
}

/** load reads the jobs and the accounts their labels are built from. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const [list, connected] = await Promise.all([jobsApi.list(), accountsApi.list()]);
    rows.value = list.jobs;
    accounts.value = connected.accounts;
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/** run starts a manual execution of a job; the run itself continues in background. */
async function run(job: SyncJob): Promise<void> {
  activeId.value = job.id;
  const answer = await runNow(() => jobsApi.run(job.id), {
    success: t('jobs.started'),
    failure: t('jobs.runError'),
  });
  if (answer) {
    await load();
  }
  activeId.value = 0;
}

/** cancel stops the run of a job that is in flight. */
async function cancel(job: SyncJob): Promise<void> {
  activeId.value = job.id;
  const answer = await runCancel(() => jobsApi.cancel(job.id), {
    success: t('jobs.cancelled'),
    failure: t('jobs.cancelError'),
  });
  if (answer) {
    await load();
  }
  activeId.value = 0;
}

/**
 * toggle flips one switch. An update replaces the whole job, so the payload is
 * built from the row itself (`payloadFromJob`) with the single override — that is
 * what makes this safe to call from the list.
 */
async function toggle(job: SyncJob, enabled: boolean): Promise<void> {
  activeId.value = job.id;
  const answer = await runSave(() => jobsApi.update(job.id, payloadFromJob(job, { enabled })), {
    failure: t('jobs.saveError'),
  });
  if (answer) {
    rows.value = rows.value.map((row) => (row.id === answer.id ? answer : row));
  }
  activeId.value = 0;
}

/** confirmRemoval deletes the job and its history. */
async function confirmRemoval(): Promise<void> {
  const job = removal.value;
  if (!job) {
    return;
  }
  const answer = await runRemove(() => jobsApi.remove(job.id), {
    failure: t('jobs.removeError'),
  });
  if (!answer) {
    return;
  }
  removal.value = null;
  await load();
  feedback.success(t('jobs.removed'));
}

/** edit opens the editor of one job. */
function edit(job: SyncJob): void {
  void router.push({ name: 'job-edit', params: { id: String(job.id) } });
}

/** create opens the editor of a new job. */
function create(): void {
  void router.push({ name: 'job-new' });
}

/** history opens the run log filtered on one job. */
function history(job: SyncJob): void {
  void router.push({ name: 'runs', query: { job: String(job.id) } });
}

onMounted(load);
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('jobs.title')" :subtitle="t('jobs.subtitle')">
      <template #actions>
        <Button variant="outline" :loading="loading" @click="load">
          <RefreshCw class="h-4 w-4" aria-hidden="true" />
          {{ t('common.refresh') }}
        </Button>
        <Button @click="create">
          <Plus class="h-4 w-4" aria-hidden="true" />
          {{ t('jobs.new') }}
        </Button>
      </template>
    </PageHeader>

    <Alert v-if="failure" tone="destructive" :title="t('jobs.loadError')" :message="failure">
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <div v-if="loading && rows.length === 0" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <Card v-else-if="rows.length === 0" content-class="p-0">
      <EmptyState :icon="FolderSync" :title="t('jobs.empty')" :message="t('jobs.emptyHint')">
        <Button @click="create">
          <Plus class="h-4 w-4" aria-hidden="true" />
          {{ t('jobs.new') }}
        </Button>
      </EmptyState>
    </Card>

    <Card v-else content-class="p-0">
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('jobs.name') }}</th>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('jobs.source') }}</th>
              <th scope="col" class="px-4 py-2 text-start font-medium">
                {{ t('jobs.destination') }}
              </th>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('jobs.direction') }}</th>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('jobs.schedule') }}</th>
              <th scope="col" class="px-4 py-2 text-start font-medium">
                {{ t('jobs.lastStatus') }}
              </th>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('jobs.nextRun') }}</th>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('jobs.enabled') }}</th>
              <th scope="col" class="px-4 py-2 text-end font-medium">{{ t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-border">
            <tr v-for="job in rows" :key="job.id" class="align-top hover:bg-muted/40">
              <td class="min-w-0 px-4 py-3">
                <RouterLink
                  :to="{ name: 'job-edit', params: { id: String(job.id) } }"
                  class="break-anywhere font-medium text-primary underline-offset-4 hover:underline"
                >
                  {{ job.name }}
                </RouterLink>
                <p v-if="job.running" class="mt-1">
                  <StatusBadge status="running" />
                </p>
              </td>
              <td class="px-4 py-3">
                <p>{{ accountLabel(job.source_account_id) }}</p>
                <p class="text-xs text-muted-foreground">
                  {{ job.source_folder_path || t('browser.root') }}
                </p>
              </td>
              <td class="px-4 py-3">
                <p>{{ accountLabel(job.destination_account_id) }}</p>
                <p class="text-xs text-muted-foreground">
                  {{ job.destination_folder_path || t('browser.root') }}
                </p>
              </td>
              <td class="px-4 py-3">
                <Badge tone="outline">{{ t(`jobs.direction_${job.direction}`) }}</Badge>
              </td>
              <td class="px-4 py-3">
                <p>{{ scheduleLabel(t, job.interval_minutes) }}</p>
                <p class="text-xs text-muted-foreground">
                  {{ t('jobs.conflictPolicy') }}: {{ t(`jobs.conflict_${job.conflict_policy}`) }}
                </p>
              </td>
              <td class="px-4 py-3">
                <StatusBadge v-if="job.last_status" :status="job.last_status" />
                <span v-else class="text-muted-foreground">{{ t('jobs.noRuns') }}</span>
                <p v-if="job.last_run_at" class="mt-1 text-xs text-muted-foreground">
                  {{ when(job.last_run_at) }}
                </p>
                <p v-if="job.last_error" class="mt-1 max-w-64 text-xs text-destructive">
                  {{ job.last_error }}
                </p>
              </td>
              <td class="px-4 py-3 text-muted-foreground">{{ next(job) }}</td>
              <td class="px-4 py-3">
                <Switch
                  :model-value="job.enabled"
                  :disabled="saving && activeId === job.id"
                  :label="t('jobs.enabled')"
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
                    {{ cancelling && activeId === job.id ? t('jobs.cancelling') : t('jobs.cancel') }}
                  </Button>
                  <Button
                    v-else
                    variant="outline"
                    size="sm"
                    :loading="running && activeId === job.id"
                    @click="run(job)"
                  >
                    <Play class="h-4 w-4" aria-hidden="true" />
                    {{ running && activeId === job.id ? t('jobs.starting') : t('jobs.runNow') }}
                  </Button>
                  <Button variant="ghost" size="sm" @click="history(job)">
                    {{ t('jobs.history') }}
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

    <ConfirmDialog
      v-model:open="confirmOpen"
      :title="t('jobs.deleteTitle')"
      :message="t('jobs.deleteMessage')"
      :confirm-label="t('common.delete')"
      :loading="removing"
      @confirm="confirmRemoval"
    >
      {{ removal?.name ?? '' }}
    </ConfirmDialog>
  </div>
</template>
