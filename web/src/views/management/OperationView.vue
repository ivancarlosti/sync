<script setup lang="ts">
// OperationView — the full page of one administrative operation.
//
// The catalogue card opens this page instead of a small dialog: the drive,
// mailbox and group operations need room for a source select, a target select,
// a folder browser and the help that says what each address means. The form is
// data-driven from the operation's params, with the live users/groups/drives of
// the account fetched from /api/management/options so an operator selects an
// existing object instead of typing an address a typo can turn into a new one.
import { ArrowLeft, FolderOpen, RefreshCw, Trash2 } from '@lucide/vue';
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';

import FolderPicker from '@/components/FolderPicker.vue';
import Alert from '@/components/ui/Alert.vue';
import Badge from '@/components/ui/Badge.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Input from '@/components/ui/Input.vue';
import Label from '@/components/ui/Label.vue';
import Select, { type SelectOption } from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import Switch from '@/components/ui/Switch.vue';
import { accountLabel } from '@/lib/accounts';
import {
  accounts as accountsApi,
  management as managementApi,
  messageOf,
  type ConnectedAccount,
  type ManagementDelegate,
  type ManagementOperation,
  type ManagementOptions,
} from '@/lib/api';
import type { FolderSelection } from '@/lib/folders';

/** DELEGATE_KIND is the one operation with a live list instead of a plain form. */
const DELEGATE_KIND = 'google_manage_delegate';

const { t } = useI18n();
const route = useRoute();
const router = useRouter();

const kind = computed(() => String(route.params.kind ?? ''));
const accounts = ref<ConnectedAccount[]>([]);
const accountId = ref(0);
const operation = ref<ManagementOperation | null>(null);
const options = ref<ManagementOptions>({ users: [], groups: [], drives: [] });
const form = ref<Record<string, string>>({});
const confirmed = ref(false);
const loading = ref(true);
const failure = ref('');
const saving = ref(false);
const saveError = ref('');
const folderOpen = ref(false);
const folderPath = ref('');

const delegates = ref<ManagementDelegate[]>([]);
const delegatesLoading = ref(false);
const delegatesError = ref('');
const delegateChoice = ref('');
const delegateSaving = ref(false);
const delegateRemoving = ref('');

let timer: ReturnType<typeof setInterval> | undefined;

const selectedAccount = computed(
  () => accounts.value.find((account) => account.id === accountId.value) ?? null,
);
const provider = computed(() => selectedAccount.value?.provider ?? '');
const isDelegate = computed(() => kind.value === DELEGATE_KIND);

const accountOptions = computed(() => [
  { value: 0, label: t('management.selectAccount'), disabled: true },
  ...accounts.value.map((account) => ({ value: account.id, label: accountLabel(account) })),
]);

/** editableParams hides the confirmation flag: it is the switch at the bottom. */
const editableParams = computed(() =>
  operation.value ? operation.value.params.filter((param) => param.key !== 'confirm') : [],
);

/** kindTitle translates an operation kind. */
function kindTitle(value: string): string {
  return t(`management.kinds.${value}`);
}

/** capabilityLabel translates the capability an operation needs. */
function capabilityLabel(value: string): string {
  return t(`admin.capability.${value}`);
}

/** fieldLabel translates one parameter of an operation. */
function fieldLabel(key: string): string {
  return t(`management.params.${key}`);
}

/** fieldHelp translates the optional explanation under a field. */
function fieldHelp(key: string): string {
  const translated = t(`management.help.${key}`);
  return translated === `management.help.${key}` ? '' : translated;
}

function driveOptions(target: boolean): SelectOption[] {
  const drives = target
    ? options.value.drives.filter((drive) => drive.kind === 'shared')
    : options.value.drives;
  return drives.map((drive) => ({
    value: drive.id,
    label: [drive.name, drive.kind].filter(Boolean).join(' · '),
  }));
}

const userOptions = computed<SelectOption[]>(() =>
  options.value.users.map((user) => ({
    value: user.email,
    label: [user.name, user.email, user.guest ? `(${t('management.guest')})` : '']
      .filter(Boolean)
      .join(' · '),
  })),
);

const groupOptions = computed(() => options.value.groups);

/** isDriveParam says which params render as a drive select. */
function isDriveParam(key: string): boolean {
  return key === 'source_drive_id' || key === 'target_drive_id' || key === 'drive_id';
}

/** isUserParam says which params render as a user select. */
function isUserParam(key: string): boolean {
  return ['source', 'target', 'user_email', 'member_email', 'mailbox', 'delegate'].includes(key);
}

/** driveParamIsSharedOnly marks the target of a Shared Drive move. */
function driveParamIsSharedOnly(key: string): boolean {
  return key === 'target_drive_id' && kind.value === 'google_drive_to_shared_drive';
}

async function loadAccounts(): Promise<void> {
  accounts.value = (await accountsApi.list()).accounts;
  const requested = Number(route.query.account ?? 0);
  if (requested > 0 && accounts.value.some((account) => account.id === requested)) {
    accountId.value = requested;
  } else if (accounts.value.length > 0) {
    accountId.value = accounts.value[0].id;
  }
}

/** reset prepares the form of the loaded operation. */
function resetForm(): void {
  const next: Record<string, string> = {};
  for (const param of operation.value?.params ?? []) {
    if (param.key !== 'confirm') {
      next[param.key] = '';
    }
  }
  if ('action' in next) {
    next.action = 'add';
  }
  if ('user_email' in next && provider.value === 'google') {
    // The connected account is the administrator running the archive; the
    // departing user is the one the operator picks below.
  }
  if ('mailbox' in next) {
    next.mailbox = selectedAccount.value?.email ?? '';
  }
  if ('source_drive_id' in next) {
    next.source_drive_id = options.value.drives[0]?.id ?? '';
  }
  if ('drive_id' in next) {
    next.drive_id = options.value.drives[0]?.id ?? '';
  }
  if ('target_drive_id' in next && kind.value === 'google_drive_to_shared_drive') {
    next.target_drive_id = options.value.drives.find((drive) => drive.kind === 'shared')?.id ?? '';
  }
  form.value = next;
}

async function load(): Promise<void> {
  if (accountId.value === 0) {
    return;
  }
  failure.value = '';
  loading.value = true;
  try {
    const answer = await managementApi.operations(accountId.value);
    operation.value = answer.operations.find((entry) => entry.kind === kind.value) ?? null;
    if (!operation.value) {
      failure.value = t('management.unknownOperation');
      return;
    }
    options.value = await managementApi.options(accountId.value, kind.value);
    resetForm();
    if (isDelegate.value) {
      await loadDelegates();
    }
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/** loadDelegates refreshes the live list of the selected mailbox. */
async function loadDelegates(): Promise<void> {
  const mailbox = form.value.mailbox?.trim() ?? '';
  if (!isDelegate.value || mailbox === '' || accountId.value === 0) {
    delegates.value = [];
    return;
  }
  delegatesLoading.value = true;
  delegatesError.value = '';
  try {
    delegates.value = (await managementApi.delegates(accountId.value, mailbox)).delegates;
  } catch (error) {
    delegatesError.value = messageOf(error);
  } finally {
    delegatesLoading.value = false;
  }
}

/** waitForTask polls one task until it leaves `running`, then answers it. */
async function waitForTask(id: number): Promise<void> {
  if (timer) {
    clearInterval(timer);
    timer = undefined;
  }
  await new Promise<void>((resolve) => {
    timer = setInterval(async () => {
      try {
        const answer = await managementApi.get(id);
        if (!answer.task.running) {
          clearInterval(timer);
          timer = undefined;
          resolve();
        }
      } catch {
        clearInterval(timer);
        timer = undefined;
        resolve();
      }
    }, 1500);
  });
}

/** submit starts the generic form operation. */
async function submit(): Promise<void> {
  if (!operation.value) {
    return;
  }
  const params: Record<string, string> = {};
  for (const [key, value] of Object.entries(form.value)) {
    if (String(value).trim() !== '') {
      params[key] = String(value).trim();
    }
  }
  for (const param of operation.value.params) {
    if (param.required && param.key !== 'confirm' && !params[param.key]) {
      saveError.value = t('common.requiredField');
      return;
    }
  }
  if (operation.value.danger) {
    if (!confirmed.value) {
      saveError.value = t('management.dangerHint');
      return;
    }
    params.confirm = 'true';
  }
  saving.value = true;
  saveError.value = '';
  try {
    const answer = await managementApi.start({
      account_id: accountId.value,
      kind: operation.value.kind,
      params,
    });
    await router.push({ name: 'management-activity' });
    void answer;
  } catch (error) {
    saveError.value = messageOf(error);
  } finally {
    saving.value = false;
  }
}

/** submitDelegate starts one add/remove and refreshes the list when it ends. */
async function submitDelegate(action: 'add' | 'remove', delegate: string): Promise<void> {
  if (!isDelegate.value || accountId.value === 0) {
    return;
  }
  const mailbox = form.value.mailbox?.trim() ?? '';
  if (mailbox === '' || delegate === '') {
    delegatesError.value = t('common.requiredField');
    return;
  }
  if (action === 'add') {
    delegateSaving.value = true;
  } else {
    delegateRemoving.value = delegate;
  }
  delegatesError.value = '';
  try {
    const answer = await managementApi.start({
      account_id: accountId.value,
      kind: DELEGATE_KIND,
      params: { mailbox, delegate, action, confirm: 'true' },
    });
    await waitForTask(answer.task.id);
    await loadDelegates();
    delegateChoice.value = '';
  } catch (error) {
    delegatesError.value = messageOf(error);
  } finally {
    delegateSaving.value = false;
    delegateRemoving.value = '';
  }
}

/** onFolder keeps the root of a drive as an empty folder id. */
function onFolder(selection: FolderSelection): void {
  if (kind.value === 'google_drive_to_shared_drive' && selection.drive.kind !== 'shared') {
    saveError.value = t('management.sharedDriveOnly');
    return;
  }
  form.value.target_drive_id = selection.driveId;
  form.value.target_folder_id = selection.folderId;
  folderPath.value = selection.folderPath;
  folderOpen.value = false;
}

function clearFolder(): void {
  form.value.target_folder_id = '';
  folderPath.value = '';
}

onMounted(async () => {
  try {
    await loadAccounts();
    await load();
  } catch (error) {
    failure.value = messageOf(error);
    loading.value = false;
  }
});

watch(accountId, () => {
  void load();
});

watch(
  () => form.value.mailbox,
  () => {
    if (isDelegate.value) {
      void loadDelegates();
    }
  },
);

onBeforeUnmount(() => {
  if (timer) {
    clearInterval(timer);
    timer = undefined;
  }
});
</script>

<template>
  <div class="space-y-6">
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div class="space-y-1">
        <RouterLink
          :to="{ name: 'management-operations' }"
          class="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
        >
          <ArrowLeft class="h-4 w-4" aria-hidden="true" />
          {{ t('management.backToOperations') }}
        </RouterLink>
        <h1 class="text-xl font-semibold tracking-tight">
          {{ operation ? kindTitle(operation.kind) : t('management.operationFallback') }}
        </h1>
        <p v-if="operation" class="max-w-2xl text-sm text-muted-foreground">
          {{ t(`management.descriptions.${operation.kind}`) }}
        </p>
      </div>
      <div class="flex items-center gap-2">
        <Badge v-if="operation" tone="secondary">{{ capabilityLabel(operation.capability) }}</Badge>
        <Badge v-if="operation?.danger" tone="warning">{{ t('management.danger') }}</Badge>
      </div>
    </div>

    <Alert v-if="failure" tone="destructive" :title="t('management.loadError')" :message="failure" />

    <div v-if="loading" class="flex justify-center py-12">
      <Spinner />
    </div>

    <template v-else-if="operation">
      <Card :title="t('management.account')">
        <div class="space-y-2 p-5 pt-4">
          <Label for="operation-account">{{ t('management.account') }}</Label>
          <Select
            id="operation-account"
            v-model="accountId"
            :options="accountOptions"
            class="w-full max-w-md"
          />
          <p class="text-xs text-muted-foreground">{{ t('management.accountHint') }}</p>
        </div>
      </Card>

      <Alert
        v-if="kind === 'google_archive_user_group'"
        tone="info"
        :title="t('management.archiveTitle')"
        :message="t('management.archiveHint')"
      />

      <!-- Mailbox delegates: live list plus add/remove, no generic action field. -->
      <template v-if="isDelegate">
        <Card :title="t('management.delegates')">
          <div class="space-y-4 p-5 pt-4">
            <div class="max-w-md space-y-1.5">
              <Label for="delegate-mailbox" required>{{ fieldLabel('mailbox') }}</Label>
              <Select
                id="delegate-mailbox"
                v-model="form.mailbox"
                :options="userOptions"
                :placeholder="t('management.selectMailbox')"
              />
            </div>

            <Alert v-if="delegatesError" tone="destructive" :message="delegatesError" />

            <div class="flex items-center justify-between gap-2">
              <p class="text-sm font-medium">{{ t('management.currentDelegates') }}</p>
              <Button variant="ghost" size="sm" :disabled="delegatesLoading" @click="loadDelegates">
                <RefreshCw class="me-2 h-4 w-4" :class="{ 'animate-spin': delegatesLoading }" />
                {{ t('management.refresh') }}
              </Button>
            </div>
            <EmptyState
              v-if="!delegatesLoading && delegates.length === 0"
              :title="t('management.noDelegates')"
            />
            <div v-else-if="delegatesLoading" class="flex justify-center py-4"><Spinner /></div>
            <ul v-else class="divide-y divide-border rounded-md border border-border">
              <li
                v-for="delegate in delegates"
                :key="delegate.email"
                class="flex flex-wrap items-center justify-between gap-2 px-3 py-2"
              >
                <div class="space-y-0.5">
                  <p class="text-sm font-medium">{{ delegate.email }}</p>
                  <p class="text-xs text-muted-foreground">{{ delegate.status }}</p>
                </div>
                <Button
                  variant="ghost"
                  size="sm"
                  :disabled="delegateRemoving === delegate.email"
                  @click="submitDelegate('remove', delegate.email)"
                >
                  <Trash2 class="me-2 h-4 w-4" />
                  {{ t('management.actions.remove') }}
                </Button>
              </li>
            </ul>

            <div class="grid gap-3 rounded-md border border-border p-3 sm:grid-cols-[1fr_auto]">
              <div class="space-y-1.5">
                <Label for="delegate-add">{{ t('management.addDelegate') }}</Label>
                <Select
                  id="delegate-add"
                  v-model="delegateChoice"
                  :options="userOptions"
                  :placeholder="t('management.selectUser')"
                />
              </div>
              <div class="flex items-end">
                <Button :disabled="delegateSaving || delegateChoice === ''" @click="submitDelegate('add', delegateChoice)">
                  <Spinner v-if="delegateSaving" class="me-2" />
                  {{ t('management.actions.add') }}
                </Button>
              </div>
            </div>
          </div>
        </Card>
      </template>

      <!-- Generic form: every other operation. -->
      <Card v-else :title="t('management.form')">
        <div class="space-y-5 p-5 pt-4">
          <Alert v-if="saveError" tone="destructive" :message="saveError" />

          <div class="grid gap-5 md:grid-cols-2">
            <div v-for="param in editableParams" :key="param.key" class="space-y-1.5">
              <Label :for="`operation-${param.key}`" :required="param.required" :hint="fieldHelp(param.key)">
                {{ fieldLabel(param.key) }}
              </Label>

              <Select
                v-if="param.key === 'action'"
                :id="`operation-${param.key}`"
                v-model="form[param.key]"
                :options="[
                  { value: 'add', label: t('management.actions.add') },
                  { value: 'remove', label: t('management.actions.remove') },
                  { value: 'list', label: t('management.actions.list') },
                ]"
              />

              <Select
                v-else-if="isDriveParam(param.key)"
                :id="`operation-${param.key}`"
                v-model="form[param.key]"
                :options="driveOptions(driveParamIsSharedOnly(param.key))"
                :placeholder="t('management.selectDrive')"
                @update:model-value="clearFolder"
              />

              <Select
                v-else-if="isUserParam(param.key)"
                :id="`operation-${param.key}`"
                v-model="form[param.key]"
                :options="userOptions"
                :placeholder="t('management.selectUser')"
              />

              <template v-else-if="param.key === 'group_email'">
                <Input
                  :id="`operation-${param.key}`"
                  v-model="form[param.key]"
                  list="management-group-emails"
                  :placeholder="t('management.groupPlaceholder')"
                />
                <datalist id="management-group-emails">
                  <option v-for="group in groupOptions" :key="group.email" :value="group.email">
                    {{ group.name }}
                  </option>
                </datalist>
              </template>

              <template v-else-if="param.key === 'target_folder_id'">
                <div class="flex flex-wrap items-center gap-2">
                  <Button
                    variant="outline"
                    :disabled="!form.target_drive_id"
                    @click="folderOpen = true"
                  >
                    <FolderOpen class="me-2 h-4 w-4" />
                    {{ t('management.selectFolder') }}
                  </Button>
                  <Button v-if="form.target_folder_id" variant="ghost" @click="clearFolder">
                    {{ t('common.clear') }}
                  </Button>
                  <span v-if="folderPath" class="text-xs text-muted-foreground">{{ folderPath }}</span>
                </div>
              </template>

              <Input
                v-else
                :id="`operation-${param.key}`"
                v-model="form[param.key]"
                :type="param.key === 'max' ? 'number' : param.key.includes('email') ? 'email' : 'text'"
                :min="param.key === 'max' ? 1 : undefined"
              />
            </div>
          </div>

          <div v-if="operation.danger" class="space-y-2 rounded-md border border-border p-3">
            <p class="text-sm text-muted-foreground">{{ t('management.dangerHint') }}</p>
            <div class="flex items-center gap-2">
              <Switch id="operation-confirm" v-model="confirmed" :label="t('management.confirmRun')" />
              <Label for="operation-confirm">{{ t('management.confirmRun') }}</Label>
            </div>
          </div>

          <div class="flex flex-wrap justify-end gap-2">
            <Button variant="outline" @click="router.push({ name: 'management-operations' })">
              {{ t('common.cancel') }}
            </Button>
            <Button :disabled="saving" @click="submit">
              <Spinner v-if="saving" class="me-2" />
              {{ t('management.run') }}
            </Button>
          </div>
        </div>
      </Card>

      <FolderPicker
        v-if="form.target_drive_id"
        v-model:open="folderOpen"
        :account-id="accountId"
        :provider="provider"
        purpose="destination"
        :start-drive-id="form.target_drive_id"
        :start-folder-id="form.target_folder_id"
        @select="onFolder"
      />
    </template>
  </div>
</template>
