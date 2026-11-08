<script setup lang="ts">
// ActivityView — the auditable journal of every administrative task.
//
// A task is one execution of a provider-backed operation (see
// internal/services/management.go): the row keeps the request, the outcome and the
// summary counters, and every step the operation took is appended to a journal.
// The screen reads the history, opens one task's journal, cancels a running task
// and downloads the journal as a CSV, which is what an operator attaches to an
// audit trail.
import { Download, Eye, XCircle } from '@lucide/vue';
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';

import StatusBadge from '@/components/StatusBadge.vue';
import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import Dialog from '@/components/ui/Dialog.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Select from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import { accountLabel } from '@/lib/accounts';
import {
  accounts as accountsApi,
  management as managementApi,
  messageOf,
  type ConnectedAccount,
  type ManagementTask,
  type ManagementTaskDetail,
} from '@/lib/api';
import { formatBytes, formatDateTime, formatDuration, formatNumber } from '@/lib/format';

/** POLL_INTERVAL is how often the screen refreshes while a task is running. */
const POLL_INTERVAL = 2000;
/** LIST_LIMIT is how many tasks the history shows at once. */
const LIST_LIMIT = 50;

const { t, locale } = useI18n();

const accounts = ref<ConnectedAccount[]>([]);
const accountId = ref(0);
const tasks = ref<ManagementTask[]>([]);
const loading = ref(true);
const failure = ref('');

const detailOpen = ref(false);
const detail = ref<ManagementTaskDetail | null>(null);
const detailLoading = ref(false);
const detailFailure = ref('');

const cancellingId = ref(0);

let timer: ReturnType<typeof setInterval> | undefined;

const accountOptions = computed(() => [
  { value: 0, label: t('common.all') },
  ...accounts.value.map((account) => ({ value: account.id, label: accountLabel(account) })),
]);

/** kindTitle translates an operation kind. */
function kindTitle(kind: string): string {
  return t(`management.kinds.${kind}`);
}

/** exportHref is the browser URL of one task's journal CSV. */
function exportHref(id: number): string {
  return managementApi.exportHref(id);
}

async function loadAccounts(): Promise<void> {
  accounts.value = (await accountsApi.list()).accounts;
}

async function load(): Promise<void> {
  tasks.value = (await managementApi.list(accountId.value || undefined, LIST_LIMIT)).tasks;
}

async function refresh(): Promise<void> {
  failure.value = '';
  try {
    await load();
  } catch (error) {
    failure.value = messageOf(error);
  }
}

function stopPolling(): void {
  if (timer) {
    clearInterval(timer);
    timer = undefined;
  }
}

function ensurePolling(): void {
  if (timer) {
    return;
  }
  timer = setInterval(async () => {
    await load();
    if (!tasks.value.some((task) => task.status === 'running')) {
      stopPolling();
    }
  }, POLL_INTERVAL);
}

async function openDetail(id: number): Promise<void> {
  detailOpen.value = true;
  detail.value = null;
  detailFailure.value = '';
  detailLoading.value = true;
  try {
    detail.value = (await managementApi.get(id)).task;
  } catch (error) {
    detailFailure.value = messageOf(error);
  } finally {
    detailLoading.value = false;
  }
}

async function cancel(task: ManagementTask): Promise<void> {
  cancellingId.value = task.id;
  try {
    await managementApi.cancel(task.id);
    await load();
    if (detail.value?.id === task.id) {
      detail.value = (await managementApi.get(task.id)).task;
    }
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    cancellingId.value = 0;
  }
}

onMounted(async () => {
  try {
    await loadAccounts();
    await refresh();
    if (tasks.value.some((task) => task.status === 'running')) {
      ensurePolling();
    }
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
});

onBeforeUnmount(stopPolling);

watch(accountId, () => {
  void refresh();
});
</script>

<template>
  <div class="space-y-6">
    <Alert v-if="failure" tone="destructive" :title="t('management.loadError')" :message="failure" />

    <Card :title="t('management.history')">
      <template #actions>
        <Select v-model="accountId" :options="accountOptions" class="w-56" :aria-label="t('management.account')" />
      </template>
      <div class="p-5 pt-4">
        <div v-if="loading" class="flex justify-center py-8">
          <Spinner />
        </div>

        <EmptyState v-else-if="tasks.length === 0" :title="t('management.historyEmpty')" />

        <div v-else class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-border text-xs uppercase text-muted-foreground">
                <th class="px-2 py-2 text-start font-medium">{{ t('management.kind') }}</th>
                <th class="px-2 py-2 text-start font-medium">{{ t('management.account') }}</th>
                <th class="px-2 py-2 text-start font-medium">{{ t('management.status') }}</th>
                <th class="px-2 py-2 text-start font-medium">{{ t('management.started') }}</th>
                <th class="px-2 py-2 text-start font-medium">{{ t('management.duration') }}</th>
                <th class="px-2 py-2 text-start font-medium">{{ t('management.items') }}</th>
                <th class="px-2 py-2 text-start font-medium">{{ t('management.steps') }}</th>
                <th class="px-2 py-2 text-end font-medium">{{ t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="task in tasks" :key="task.id" class="border-b border-border/60">
                <td class="px-2 py-2 font-medium">{{ kindTitle(task.kind) }}</td>
                <td class="px-2 py-2 text-muted-foreground">{{ task.account_email }}</td>
                <td class="px-2 py-2"><StatusBadge :status="task.status" /></td>
                <td class="px-2 py-2 text-muted-foreground">{{ formatDateTime(task.started_at, locale) }}</td>
                <td class="px-2 py-2 text-muted-foreground">{{ formatDuration(task.duration_ms, t) }}</td>
                <td class="px-2 py-2 text-muted-foreground">{{ formatNumber(task.items_processed, locale) }}</td>
                <td class="px-2 py-2 text-muted-foreground">{{ formatNumber(task.step_count, locale) }}</td>
                <td class="px-2 py-2">
                  <div class="flex items-center justify-end gap-1">
                    <Button variant="ghost" size="icon" :aria-label="t('management.view')" @click="openDetail(task.id)">
                      <Eye class="h-4 w-4" aria-hidden="true" />
                    </Button>
                    <Button
                      v-if="task.running"
                      variant="ghost"
                      size="icon"
                      :aria-label="t('common.cancel')"
                      :disabled="cancellingId === task.id"
                      @click="cancel(task)"
                    >
                      <XCircle class="h-4 w-4" aria-hidden="true" />
                    </Button>
                    <a
                      :href="exportHref(task.id)"
                      :aria-label="t('management.export')"
                      class="inline-flex h-9 w-9 items-center justify-center rounded-md hover:bg-accent hover:text-accent-foreground"
                    >
                      <Download class="h-4 w-4" aria-hidden="true" />
                    </a>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </Card>


    <Dialog
      v-model:open="detailOpen"
      :title="detail ? kindTitle(detail.kind) : t('management.detail')"
      :close-label="t('common.close')"
      size="xl"
    >
      <div class="space-y-4">
        <Alert v-if="detailFailure" tone="destructive" :message="detailFailure" />
        <div v-if="detailLoading" class="flex justify-center py-6">
          <Spinner />
        </div>
        <template v-else-if="detail">
          <div class="flex flex-wrap items-center gap-2">
            <StatusBadge :status="detail.status" />
            <span class="text-sm text-muted-foreground">{{ detail.account_email }}</span>
            <span class="text-sm text-muted-foreground">{{ formatDateTime(detail.started_at, locale) }}</span>
          </div>

          <p v-if="detail.message" class="text-sm">{{ detail.message }}</p>

          <dl class="grid grid-cols-2 gap-2 text-sm sm:grid-cols-4">
            <div>
              <dt class="text-xs text-muted-foreground">{{ t('management.items') }}</dt>
              <dd>{{ formatNumber(detail.items_processed, locale) }}</dd>
            </div>
            <div>
              <dt class="text-xs text-muted-foreground">{{ t('management.bytes') }}</dt>
              <dd>{{ formatBytes(detail.bytes_moved, locale) }}</dd>
            </div>
            <div>
              <dt class="text-xs text-muted-foreground">{{ t('management.steps') }}</dt>
              <dd>{{ formatNumber(detail.step_count, locale) }}</dd>
            </div>
            <div>
              <dt class="text-xs text-muted-foreground">{{ t('management.duration') }}</dt>
              <dd>{{ formatDuration(detail.duration_ms, t) }}</dd>
            </div>
          </dl>

          <EmptyState v-if="detail.steps.length === 0" :title="t('management.stepsEmpty')" />
          <div v-else class="max-h-80 overflow-y-auto rounded-md border border-border">
            <table class="w-full text-sm">
              <thead class="sticky top-0 bg-muted">
                <tr class="text-xs uppercase text-muted-foreground">
                  <th class="px-2 py-2 text-start font-medium">#</th>
                  <th class="px-2 py-2 text-start font-medium">{{ t('management.steps') }}</th>
                  <th class="px-2 py-2 text-start font-medium">{{ t('management.status') }}</th>
                  <th class="px-2 py-2 text-start font-medium">{{ t('management.detail') }}</th>
                  <th class="px-2 py-2 text-start font-medium">{{ t('management.items') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="step in detail.steps" :key="step.id" class="border-t border-border/60">
                  <td class="px-2 py-1 text-muted-foreground">{{ step.seq }}</td>
                  <td class="px-2 py-1">{{ step.name }}</td>
                  <td class="px-2 py-1"><StatusBadge :status="step.status" /></td>
                  <td class="px-2 py-1 text-muted-foreground">{{ step.detail }}</td>
                  <td class="px-2 py-1 text-muted-foreground">{{ formatNumber(step.items, locale) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </template>
      </div>
    </Dialog>
  </div>
</template>

