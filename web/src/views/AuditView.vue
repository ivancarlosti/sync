<script setup lang="ts">
// AuditView — content audits of a provider folder tree.
//
// An audit is an async job (see internal/handlers/audits.go): the screen starts
// one against an account and folder, then polls the history while it runs. The
// report itself is read lazily when an audit is opened (a single report can hold
// thousands of nodes) and is downloadable as a CSV the server builds from the
// stored rows, so the provider is never scanned again.
import { ClipboardList, Download, Eye, Folder, Play, XCircle } from '@lucide/vue';
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';

import FolderPicker from '@/components/FolderPicker.vue';
import PageHeader from '@/components/PageHeader.vue';
import StatusBadge from '@/components/StatusBadge.vue';
import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import Dialog from '@/components/ui/Dialog.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Input from '@/components/ui/Input.vue';
import Select from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import {
  accounts as accountsApi,
  audits as auditsApi,
  codeOf,
  type AuditEntry,
  type AuditRun,
  type ConnectedAccount,
} from '@/lib/api';
import { ROOT_PATH, type FolderSelection } from '@/lib/folders';
import { formatBytes, formatDateTime, formatNumber } from '@/lib/format';

/** POLL_INTERVAL is how often the screen refreshes while an audit is running. */
const POLL_INTERVAL = 2000;
/** LIST_LIMIT is how many audits the history shows at once. */
const LIST_LIMIT = 50;
/** MAX_DEPTH mirrors the server clamp (services.AuditMaxDepth). */
const MAX_DEPTH = 25;

const { t, locale } = useI18n();

const rows = ref<AuditRun[]>([]);
const accounts = ref<ConnectedAccount[]>([]);
const accountId = ref(0);
const driveId = ref('');
const folderId = ref('');
const folderPath = ref(ROOT_PATH);
const depth = ref(0);

const loading = ref(true);
const failure = ref('');
const starting = ref(false);
const pickerOpen = ref(false);

const detail = ref<AuditRun | null>(null);
const entries = ref<AuditEntry[]>([]);
const entriesLoading = ref(false);
const entriesFailure = ref('');

/** timer drives the automatic refresh while an audit is in flight. */
let timer: ReturnType<typeof setInterval> | undefined;

const accountOptions = computed(() => [
  { value: 0, label: t('audit.selectAccount'), disabled: true },
  ...accounts.value.map((account) => ({
    value: account.id,
    label: account.display_name || account.email,
  })),
]);

const selectedAccount = computed(() =>
  accounts.value.find((account) => account.id === accountId.value),
);

/** running is true while an audit of the history is still in flight. */
const running = computed(() => rows.value.some((run) => run.status === 'running'));

/** canStart gates the start button. */
const canStart = computed(() => accountId.value > 0 && !starting.value && !running.value);

/** detailOpen drives the report dialog from the audit that was picked. */
const detailOpen = computed({
  get: () => detail.value !== null,
  set: (open: boolean) => {
    if (!open) {
      detail.value = null;
      entries.value = [];
      entriesFailure.value = '';
    }
  },
});

/** errorText translates the machine-readable code of a failed request. */
function errorText(error: unknown): string {
  const key = `audit.error_${codeOf(error)}`;
  const translated = t(key);
  return translated === key ? t('audit.error_unknown') : translated;
}

/** count renders a counter with the grouping of the active locale. */
function count(value: number): string {
  return formatNumber(value, locale.value);
}

/** size renders a byte total with the units of the active locale. */
function size(value: number): string {
  return formatBytes(value, locale.value);
}

/** when renders a timestamp of the API. */
function when(value: string | null | undefined): string {
  return formatDateTime(value, locale.value);
}

/** displaySize is the subtree total for a folder and the file size for a file. */
function displaySize(entry: AuditEntry): string {
  return size(entry.kind === 'folder' ? entry.total_size : entry.size);
}

/** indent mirrors the depth of a node so the report reads like a tree. */
function indent(entry: AuditEntry): Record<string, string> {
  return { paddingInlineStart: `${entry.depth}rem` };
}

/** loadAccounts fills the account select and picks the first one. */
async function loadAccounts(): Promise<void> {
  try {
    const answer = await accountsApi.list();
    accounts.value = answer.accounts;
    if (accountId.value === 0 && accounts.value.length > 0) {
      accountId.value = accounts.value[0].id;
    }
  } catch (error) {
    failure.value = errorText(error);
  }
}

/** load reads the audit history and (re)arms the poller. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const answer = await auditsApi.list(LIST_LIMIT);
    rows.value = answer.audits;
  } catch (error) {
    failure.value = errorText(error);
  } finally {
    loading.value = false;
    schedule();
  }
}

/** schedule polls again while an audit is in flight, and stops when none is. */
function schedule(): void {
  clearTimer();
  if (!running.value) {
    return;
  }
  timer = setInterval(() => {
    void load();
  }, POLL_INTERVAL);
}

/** clearTimer stops the poller; the view calls it when it unmounts. */
function clearTimer(): void {
  if (timer !== undefined) {
    clearInterval(timer);
    timer = undefined;
  }
}

/** applyAccount resets the folder selection when the account changes. */
function applyAccount(value: string | number): void {
  accountId.value = Number(value);
  driveId.value = '';
  folderId.value = '';
  folderPath.value = ROOT_PATH;
}

/** applyFolders stores the folder the browser returned. */
function applyFolders(selection: FolderSelection): void {
  driveId.value = selection.driveId;
  folderId.value = selection.folderId;
  folderPath.value = selection.folderPath;
}

/** start launches an audit and refreshes the history. */
async function start(): Promise<void> {
  if (!canStart.value) {
    return;
  }
  starting.value = true;
  failure.value = '';
  try {
    await auditsApi.start({
      account_id: accountId.value,
      drive_id: driveId.value,
      folder_id: folderId.value,
      folder_path: folderPath.value,
      depth: depth.value,
    });
    await load();
  } catch (error) {
    failure.value = errorText(error);
  } finally {
    starting.value = false;
  }
}

/** openReport reads the report of one audit. */
async function openReport(run: AuditRun): Promise<void> {
  detail.value = run;
  entries.value = [];
  entriesFailure.value = '';
  entriesLoading.value = true;
  try {
    const answer = await auditsApi.get(run.id);
    // The detail carries the row as it stands now, so an audit still in flight
    // updates its own row without a full reload of the history.
    detail.value = answer.audit;
    entries.value = answer.entries;
    rows.value = rows.value.map((row) => (row.id === answer.audit.id ? answer.audit : row));
  } catch (error) {
    entriesFailure.value = errorText(error);
  } finally {
    entriesLoading.value = false;
  }
}

/** download saves the CSV of an audit through the browser. */
function download(run: AuditRun): void {
  window.open(auditsApi.exportHref(run.id), '_blank');
}

/** cancel asks the server to stop a running audit. */
async function cancel(run: AuditRun): Promise<void> {
  try {
    await auditsApi.cancel(run.id);
    await load();
  } catch (error) {
    failure.value = errorText(error);
  }
}

onMounted(async () => {
  await loadAccounts();
  await load();
});

onBeforeUnmount(clearTimer);
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('audit.title')" :subtitle="t('audit.subtitle')" />

    <Alert v-if="failure" tone="destructive" :title="t('audit.loadError')" :message="failure">
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <Card class="space-y-4 p-5">
      <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('audit.account') }}</p>
          <Select
            id="audit-account"
            :model-value="accountId"
            :options="accountOptions"
            @update:model-value="applyAccount"
          />
        </div>

        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('audit.folder') }}</p>
          <div class="flex items-center gap-2">
            <Button variant="outline" :disabled="accountId === 0" @click="pickerOpen = true">
              <Folder class="h-4 w-4" aria-hidden="true" />
              {{ t('audit.browse') }}
            </Button>
            <span class="break-anywhere text-sm text-muted-foreground">{{ folderPath }}</span>
          </div>
        </div>

        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('audit.depth') }}</p>
          <Input id="audit-depth" v-model="depth" type="number" :min="0" :max="MAX_DEPTH" numeric />
          <p class="text-xs text-muted-foreground">{{ t('audit.depthHint') }}</p>
        </div>
      </div>

      <div class="flex justify-end">
        <Button :disabled="!canStart" :loading="starting" @click="start">
          <Play v-if="!starting" class="h-4 w-4" aria-hidden="true" />
          {{ starting ? t('audit.starting') : t('audit.start') }}
        </Button>
      </div>
    </Card>

    <FolderPicker
      v-if="accountId > 0"
      v-model:open="pickerOpen"
      :account-id="accountId"
      :provider="selectedAccount?.provider"
      purpose="source"
      :start-drive-id="driveId"
      :start-folder-id="folderId"
      :start-folder-path="folderPath"
      @select="applyFolders"
    />

    <Alert v-if="running" tone="info" :message="t('audit.runningNow')" />

    <PageHeader :title="t('audit.history')" />

    <div v-if="loading && rows.length === 0" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <Card v-else-if="rows.length === 0" content-class="p-0">
      <EmptyState :icon="ClipboardList" :title="t('audit.empty')" :message="t('audit.emptyHint')" />
    </Card>

    <Card v-else content-class="p-0">
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('audit.account') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('audit.status') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('audit.folder') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('audit.depth') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('audit.files') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('audit.folders') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('audit.totalSize') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('audit.startedAt') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-border">
            <tr v-for="run in rows" :key="run.id" class="hover:bg-muted/40">
              <td class="break-anywhere px-3 py-2">{{ run.account_email }}</td>
              <td class="px-3 py-2">
                <span class="flex items-center gap-2">
                  <StatusBadge :status="run.status" />
                  <span v-if="run.truncated" class="text-xs text-warning">
                    {{ t('audit.truncated') }}
                  </span>
                </span>
              </td>
              <td class="break-anywhere px-3 py-2 text-muted-foreground">{{ run.root_path }}</td>
              <td class="px-3 py-2 text-end">
                <span class="numeric">
                  {{ run.max_depth > 0 ? count(run.max_depth) : t('audit.depthUnlimited') }}
                </span>
              </td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ count(run.files) }}</span></td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ count(run.folders) }}</span></td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ size(run.total_size) }}</span></td>
              <td class="px-3 py-2 text-muted-foreground">
                <span class="numeric">{{ when(run.started_at) }}</span>
              </td>
              <td class="px-3 py-2">
                <div class="flex items-center justify-end gap-1">
                  <Button variant="ghost" size="sm" @click="openReport(run)">
                    <Eye class="h-4 w-4" aria-hidden="true" />
                    {{ t('audit.report') }}
                  </Button>
                  <Button variant="ghost" size="sm" @click="download(run)">
                    <Download class="h-4 w-4" aria-hidden="true" />
                    {{ t('audit.download') }}
                  </Button>
                  <Button v-if="run.status === 'running'" variant="ghost" size="sm" @click="cancel(run)">
                    <XCircle class="h-4 w-4" aria-hidden="true" />
                    {{ t('audit.cancel') }}
                  </Button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>


    <Dialog
      v-model:open="detailOpen"
      :title="t('audit.report')"
      :description="detail ? `${detail.account_email} · ${detail.root_path}` : ''"
      size="xl"
      :close-label="t('common.closeDialog')"
    >
      <Alert
        v-if="entriesFailure"
        tone="destructive"
        :title="t('audit.loadError')"
        :message="entriesFailure"
      />

      <div v-else-if="entriesLoading" class="flex items-center justify-center gap-2 py-6">
        <Spinner :label="t('common.loading')" />
        <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
      </div>

      <div v-else-if="detail" class="space-y-4">
        <dl class="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <div class="space-y-1 rounded-lg border border-border p-3">
            <dt class="text-xs text-muted-foreground">{{ t('audit.files') }}</dt>
            <dd class="text-sm font-medium"><span class="numeric">{{ count(detail.files) }}</span></dd>
          </div>
          <div class="space-y-1 rounded-lg border border-border p-3">
            <dt class="text-xs text-muted-foreground">{{ t('audit.folders') }}</dt>
            <dd class="text-sm font-medium"><span class="numeric">{{ count(detail.folders) }}</span></dd>
          </div>
          <div class="space-y-1 rounded-lg border border-border p-3">
            <dt class="text-xs text-muted-foreground">{{ t('audit.totalSize') }}</dt>
            <dd class="text-sm font-medium"><span class="numeric">{{ size(detail.total_size) }}</span></dd>
          </div>
          <div class="space-y-1 rounded-lg border border-border p-3">
            <dt class="text-xs text-muted-foreground">{{ t('audit.maxDepth') }}</dt>
            <dd class="text-sm font-medium">
              <span class="numeric">{{ count(detail.max_depth_reached) }}</span>
            </dd>
          </div>
          <div class="space-y-1 rounded-lg border border-border p-3">
            <dt class="text-xs text-muted-foreground">{{ t('audit.truncated') }}</dt>
            <dd class="text-sm font-medium">{{ detail.truncated ? t('audit.yes') : t('audit.no') }}</dd>
          </div>
        </dl>

        <Alert
          v-if="detail.truncated"
          tone="warning"
          :message="t('audit.truncatedHint', { count: count(detail.folders + detail.files) })"
        />

        <EmptyState v-if="entries.length === 0" :icon="ClipboardList" :title="t('audit.entriesEmpty')" />

        <div v-else class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
              <tr>
                <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('audit.path') }}</th>
                <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('audit.kind') }}</th>
                <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('audit.size') }}</th>
                <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('audit.modified') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-border">
              <tr v-for="entry in entries" :key="entry.id">
                <td class="break-anywhere px-3 py-2">
                  <span :style="indent(entry)">{{ entry.path || t('audit.root') }}</span>
                </td>
                <td class="px-3 py-2"><StatusBadge :status="entry.kind" /></td>
                <td class="px-3 py-2 text-end text-muted-foreground">
                  <span class="numeric">{{ displaySize(entry) }}</span>
                </td>
                <td class="px-3 py-2 text-muted-foreground">
                  <span class="numeric">{{ when(entry.modified_at) }}</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <template #footer>
        <Button variant="outline" @click="detailOpen = false">{{ t('common.close') }}</Button>
        <Button v-if="detail" @click="download(detail)">
          <Download class="h-4 w-4" aria-hidden="true" />
          {{ t('audit.download') }}
        </Button>
      </template>
    </Dialog>
  </div>
</template>

