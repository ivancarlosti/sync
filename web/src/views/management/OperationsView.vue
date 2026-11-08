<script setup lang="ts">
// OperationsView — the catalogue of administrative operations of an account.
//
// Every entry is backed by a real provider call (see internal/providers/*/
// management.go): moving a Drive into a Shared Drive, archiving a departing user,
// copying a mailbox, transferring calendars, reporting folder sizes. The screen
// starts a task and the server runs it in the background with an auditable
// journal; the run is followed on the Activity tab.
import { Play } from '@lucide/vue';
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';

import StatusBadge from '@/components/StatusBadge.vue';
import Alert from '@/components/ui/Alert.vue';
import Badge from '@/components/ui/Badge.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import Dialog from '@/components/ui/Dialog.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Input from '@/components/ui/Input.vue';
import Label from '@/components/ui/Label.vue';
import Select from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import Switch from '@/components/ui/Switch.vue';
import { accountLabel } from '@/lib/accounts';
import {
  accounts as accountsApi,
  management as managementApi,
  messageOf,
  type ConnectedAccount,
  type ManagementOperation,
  type ManagementTask,
} from '@/lib/api';
import { formatDateTime, formatDuration } from '@/lib/format';

/** POLL_INTERVAL is how often the screen refreshes while a task is running. */
const POLL_INTERVAL = 2000;
/** RECENT_LIMIT is how many of the latest tasks the screen summarises. */
const RECENT_LIMIT = 5;

const { t, locale } = useI18n();

const accounts = ref<ConnectedAccount[]>([]);
const accountId = ref(0);
const operations = ref<ManagementOperation[]>([]);
const recent = ref<ManagementTask[]>([]);
const loading = ref(true);
const failure = ref('');

const dialogOpen = ref(false);
const activeOp = ref<ManagementOperation | null>(null);
const form = ref<Record<string, string>>({});
const confirmed = ref(false);
const starting = ref(false);
const startError = ref('');

let timer: ReturnType<typeof setInterval> | undefined;

const accountOptions = computed(() => [
  { value: 0, label: t('management.selectAccount'), disabled: true },
  ...accounts.value.map((account) => ({ value: account.id, label: accountLabel(account) })),
]);

const actionOptions = computed(() => [
  { value: 'add', label: t('management.actions.add') },
  { value: 'remove', label: t('management.actions.remove') },
  { value: 'list', label: t('management.actions.list') },
]);

const editableParams = computed(() =>
  activeOp.value ? activeOp.value.params.filter((param) => param.key !== 'confirm') : [],
);

const unsupported = computed(
  () => !loading.value && operations.value.length === 0 && accountId.value > 0,
);

/** fieldLabel translates one parameter of an operation. */
function fieldLabel(key: string): string {
  return t(`management.params.${key}`);
}

/** capabilityLabel translates the capability an operation needs. */
function capabilityLabel(capability: string): string {
  return t(`admin.capability.${capability}`);
}

/** kindTitle translates an operation kind. */
function kindTitle(kind: string): string {
  return t(`management.kinds.${kind}`);
}

async function loadAccounts(): Promise<void> {
  accounts.value = (await accountsApi.list()).accounts;
  if (accountId.value === 0 && accounts.value.length > 0) {
    accountId.value = accounts.value[0].id;
  }
}

async function loadOperations(): Promise<void> {
  if (accountId.value === 0) {
    operations.value = [];
    return;
  }
  operations.value = (await managementApi.operations(accountId.value)).operations;
}

async function loadRecent(): Promise<void> {
  if (accountId.value === 0) {
    recent.value = [];
    return;
  }
  recent.value = (await managementApi.list(accountId.value, RECENT_LIMIT)).tasks;
}

async function refresh(): Promise<void> {
  failure.value = '';
  try {
    await Promise.all([loadOperations(), loadRecent()]);
  } catch (error) {
    failure.value = messageOf(error);
  }
}

function openDialog(operation: ManagementOperation): void {
  activeOp.value = operation;
  const next: Record<string, string> = {};
  for (const param of operation.params) {
    if (param.key !== 'confirm') {
      next[param.key] = '';
    }
  }
  form.value = next;
  confirmed.value = false;
  startError.value = '';
  dialogOpen.value = true;
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
    await loadRecent();
    if (!recent.value.some((task) => task.status === 'running')) {
      stopPolling();
    }
  }, POLL_INTERVAL);
}

async function submit(): Promise<void> {
  if (!activeOp.value) {
    return;
  }
  const operation = activeOp.value;
  const params: Record<string, string> = {};
  for (const [key, value] of Object.entries(form.value)) {
    if (value.trim() !== '') {
      params[key] = value.trim();
    }
  }
  for (const param of operation.params) {
    if (param.required && param.key !== 'confirm' && !params[param.key]) {
      startError.value = t('common.requiredField');
      return;
    }
  }
  if (operation.danger) {
    if (!confirmed.value) {
      startError.value = t('management.dangerHint');
      return;
    }
    params.confirm = 'true';
  }
  starting.value = true;
  startError.value = '';
  try {
    await managementApi.start({ account_id: accountId.value, kind: operation.kind, params });
    dialogOpen.value = false;
    await loadRecent();
    ensurePolling();
  } catch (error) {
    startError.value = messageOf(error);
  } finally {
    starting.value = false;
  }
}

onMounted(async () => {
  try {
    await loadAccounts();
    await refresh();
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

    <Card :title="t('management.account')">
      <div class="space-y-2 p-5 pt-4">
        <Label for="management-account">{{ t('management.account') }}</Label>
        <Select
          id="management-account"
          v-model="accountId"
          :options="accountOptions"
          class="max-w-sm"
        />
        <p v-if="accounts.length === 0" class="text-sm text-muted-foreground">
          {{ t('management.noAccounts') }}
        </p>
      </div>
    </Card>

    <div v-if="loading" class="flex justify-center py-8">
      <Spinner />
    </div>

    <EmptyState v-else-if="unsupported" :title="t('management.unsupported')" />

    <div v-else class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
      <Card v-for="operation in operations" :key="operation.kind">
        <div class="flex h-full flex-col gap-3 p-5">
          <div class="flex items-start justify-between gap-2">
            <h3 class="text-sm font-semibold">{{ kindTitle(operation.kind) }}</h3>
            <Badge tone="secondary">{{ capabilityLabel(operation.capability) }}</Badge>
          </div>
          <Badge v-if="operation.danger" tone="warning" class="self-start">
            {{ t('management.danger') }}
          </Badge>
          <p v-if="operation.missing_capability" class="text-xs text-muted-foreground">
            {{
              t('management.missingCapability', {
                capability: capabilityLabel(operation.capability),
              })
            }}
          </p>
          <Button
            class="mt-auto self-start"
            size="sm"
            :disabled="!operation.supported"
            @click="openDialog(operation)"
          >
            <Play class="me-2 h-4 w-4" aria-hidden="true" />
            {{ t('management.run') }}
          </Button>
        </div>
      </Card>
    </div>

    <Card :title="t('management.history')">
      <div class="p-5 pt-4">
        <EmptyState v-if="recent.length === 0" :title="t('management.historyEmpty')" />
        <ul v-else class="divide-y divide-border">
          <li
            v-for="task in recent"
            :key="task.id"
            class="flex flex-wrap items-center justify-between gap-2 py-2"
          >
            <div class="space-y-1">
              <p class="text-sm font-medium">{{ kindTitle(task.kind) }}</p>
              <p class="text-xs text-muted-foreground">
                {{ formatDateTime(task.started_at, locale) }}
              </p>
            </div>
            <div class="flex items-center gap-2">
              <StatusBadge :status="task.status" />
              <span class="text-xs text-muted-foreground">
                {{ formatDuration(task.duration_ms, t) }}
              </span>
            </div>
          </li>
        </ul>
        <RouterLink
          :to="{ name: 'management-activity' }"
          class="mt-3 inline-block text-sm text-primary hover:underline"
        >
          {{ t('management.activity') }}
        </RouterLink>
      </div>
    </Card>

    <Dialog
      v-model:open="dialogOpen"
      :title="activeOp ? kindTitle(activeOp.kind) : ''"
      :close-label="t('common.close')"
      size="md"
    >
      <div v-if="activeOp" class="space-y-4">
        <Alert v-if="startError" tone="destructive" :message="startError" />

        <div v-for="param in editableParams" :key="param.key" class="space-y-1.5">
          <Label :for="`management-param-${param.key}`" :required="param.required">
            {{ fieldLabel(param.key) }}
          </Label>
          <Select
            v-if="param.key === 'action'"
            :id="`management-param-${param.key}`"
            v-model="form[param.key]"
            :options="actionOptions"
          />
          <Input
            v-else-if="param.key === 'max'"
            :id="`management-param-${param.key}`"
            v-model="form[param.key]"
            type="number"
            min="1"
          />
          <Input
            v-else
            :id="`management-param-${param.key}`"
            v-model="form[param.key]"
            :type="param.key.includes('email') ? 'email' : 'text'"
          />
        </div>

        <div v-if="activeOp.danger" class="space-y-2 rounded-md border border-border p-3">
          <p class="text-sm text-muted-foreground">{{ t('management.dangerHint') }}</p>
          <div class="flex items-center gap-2">
            <Switch id="management-confirm" v-model="confirmed" :label="t('management.confirmRun')" />
            <Label for="management-confirm">{{ t('management.confirmRun') }}</Label>
          </div>
        </div>

        <div class="flex justify-end gap-2">
          <Button variant="outline" @click="dialogOpen = false">{{ t('common.cancel') }}</Button>
          <Button :disabled="starting" @click="submit">
            <Spinner v-if="starting" class="me-2" />
            {{ t('management.run') }}
          </Button>
        </div>
      </div>
    </Dialog>
  </div>
</template>

