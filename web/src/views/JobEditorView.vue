<script setup lang="ts">
// JobEditorView — create and edit one synchronisation job.
//
// The four tabs are the four questions of a job (what it is called, where it
// reads, where it writes, how it behaves) and they edit a single payload, because
// an update replaces the job as a whole (see `payloadFromJob`). The folder browser
// is a dialog instead of a screen: changing the destination must not throw away a
// half-filled form. Saving always leaves for the list, which is where the result
// of the change (last status, next run) is visible.
import { ArrowLeft, FolderInput, FolderOutput, Info, Save, Settings2 } from '@lucide/vue';
import { computed, onMounted, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';

import FolderPicker from '@/components/FolderPicker.vue';
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
import Textarea from '@/components/ui/Textarea.vue';
import { useAction } from '@/composables/useAction';
import {
  accounts as accountsApi,
  jobs as jobsApi,
  messageOf,
  type ConnectedAccount,
  type JobPayload,
} from '@/lib/api';
import { ROOT_PATH, type FolderSelection } from '@/lib/folders';
import { asNumber, asText, joinLines, splitLines } from '@/lib/forms';
import { formatDateTime } from '@/lib/format';
import {
  DEFAULT_CONFLICT_POLICY,
  MAX_INTERVAL_MINUTES,
  conflictHint,
  conflictOptions,
  defaultDirection,
  directionHint,
  directionOptions,
  newJobPayload,
  payloadFromJob,
  scheduleLabel,
} from '@/lib/jobs';
import { providerLabel } from '@/lib/providers';
import { useAuthStore } from '@/stores/auth';
import { useFeedbackStore } from '@/stores/feedback';

const route = useRoute();
const router = useRouter();
const { t, locale } = useI18n();
const auth = useAuthStore();
const feedback = useFeedbackStore();

/** id is the job being edited; zero means the "new job" route. */
const id = computed(() => {
  const raw = route.params.id;
  const value = Array.isArray(raw) ? raw[0] : raw;
  const parsed = Number(value ?? 0);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
});

const editing = computed(() => id.value > 0);

const draft = ref<JobPayload>(newJobPayload());
const accounts = ref<ConnectedAccount[]>([]);
const loading = ref(true);
const failure = ref('');
const tab = ref('details');
const intervalText = ref<string | number>(0);
const excludeText = ref('');
const schedule = ref<string[]>([]);
const errors = ref<Record<string, string>>({});
const sourcePicker = ref(false);
const destinationPicker = ref(false);

const { busy: saving, run: save } = useAction();

const tabs = computed<TabItem[]>(() => [
  { value: 'details', label: t('jobs.steps.details'), icon: Info },
  { value: 'source', label: t('jobs.steps.source'), icon: FolderInput },
  { value: 'destination', label: t('jobs.steps.destination'), icon: FolderOutput },
  { value: 'options', label: t('jobs.steps.options'), icon: Settings2 },
]);

/** accountOptions are the accounts a side of the job can point at. */
const accountOptions = computed<SelectOption[]>(() =>
  accounts.value.map((account) => ({
    value: account.id,
    label: `${providerLabel(account.provider)} · ${account.email || account.provider_account_id}`,
  })),
);

const directions = computed<SelectOption[]>(() => directionOptions(t));
const policies = computed<SelectOption[]>(() => conflictOptions(t));

/** account returns the account behind one side of the draft. */
function account(which: 'source' | 'destination'): ConnectedAccount | undefined {
  const key = which === 'source' ? 'source_account_id' : 'destination_account_id';
  return accounts.value.find((item) => item.id === draft.value[key]);
}

/** folderPath renders the display path of one side, root included. */
function folderPath(which: 'source' | 'destination'): string {
  const key = which === 'source' ? 'source_folder_path' : 'destination_folder_path';
  const value = draft.value[key].trim();
  return value === '' ? ROOT_PATH : value;
}

/** applyFolders stores what the browser returned for one side. */
function applyFolders(which: 'source' | 'destination', selection: FolderSelection): void {
  if (which === 'source') {
    draft.value.source_drive_id = selection.driveId;
    draft.value.source_folder_id = selection.folderId;
    draft.value.source_folder_path = selection.folderPath;
    delete errors.value.source;
    return;
  }
  draft.value.destination_drive_id = selection.driveId;
  draft.value.destination_folder_id = selection.folderId;
  draft.value.destination_folder_path = selection.folderPath;
  delete errors.value.destination;
}

/** loadSchedule reads the computed plan of a stored job. */
async function loadSchedule(): Promise<void> {
  if (!editing.value) {
    return;
  }
  try {
    const answer = await jobsApi.schedule(id.value);
    schedule.value = answer.preview ?? [];
  } catch {
    // The preview is a convenience; the job itself already loaded.
    schedule.value = [];
  }
}

/** load reads the accounts and, when editing, the job itself. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const connected = await accountsApi.list();
    accounts.value = connected.accounts;
    if (editing.value) {
      const job = await jobsApi.get(id.value);
      draft.value = payloadFromJob(job);
      intervalText.value = job.interval_minutes;
      excludeText.value = joinLines(job.exclude_patterns);
      void loadSchedule();
    } else {
      // A new job starts from the schedule default of the instance.
      intervalText.value = auth.defaults?.sync_default_interval_minutes ?? 0;
    }
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/**
 * validate mirrors the contract of `applyJobInput` for the cases the operator can
 * fix on this screen. The server validates again; this only avoids a round trip.
 */
function validate(): boolean {
  const found: Record<string, string> = {};
  if (draft.value.name.trim() === '') {
    found.name = 'common.requiredField';
  }
  if (draft.value.source_account_id <= 0 || draft.value.destination_account_id <= 0) {
    found.accounts = 'validation.selectAccount';
  } else if (draft.value.source_account_id === draft.value.destination_account_id) {
    found.accounts = 'validation.differentAccounts';
  }
  if (draft.value.source_drive_id.trim() === '') {
    found.source = 'validation.selectFolder';
  }
  if (draft.value.destination_drive_id.trim() === '') {
    found.destination = 'validation.selectFolder';
  }
  errors.value = found;
  if (Object.keys(found).length === 0) {
    return true;
  }
  // Send the operator to the tab that holds the first problem.
  if (found.name) {
    tab.value = 'details';
  } else if (found.accounts || found.source) {
    tab.value = 'source';
  } else if (found.destination) {
    tab.value = 'destination';
  }
  return false;
}

/** submit creates or updates the job and returns to the list. */
async function submit(): Promise<void> {
  if (!validate()) {
    return;
  }
  const payload: JobPayload = {
    ...draft.value,
    name: draft.value.name.trim(),
    exclude_patterns: splitLines(excludeText.value),
    interval_minutes: asNumber(intervalText.value),
    direction:
      draft.value.direction ||
      defaultDirection(account('source')?.provider, account('destination')?.provider),
    conflict_policy: draft.value.conflict_policy || DEFAULT_CONFLICT_POLICY,
  };
  const answer = editing.value
    ? await save(() => jobsApi.update(id.value, payload), { failure: t('jobs.saveError') })
    : await save(() => jobsApi.create(payload), { failure: t('jobs.saveError') });
  if (!answer) {
    return;
  }
  feedback.success(editing.value ? t('jobs.updated') : t('jobs.created'));
  back();
}

/** back returns to the list without saving. */
function back(): void {
  void router.push({ name: 'jobs' });
}

// The direction follows the provider pair (the server default) until the operator
// chooses one on purpose, which is what makes a fresh job valid out of the box.
watch(
  () => [draft.value.source_account_id, draft.value.destination_account_id],
  () => {
    if (draft.value.direction !== '') {
      return;
    }
    draft.value.direction = defaultDirection(
      account('source')?.provider,
      account('destination')?.provider,
    );
  },
);

/**
 * The selects of the form write `string | number` while the payload holds numbers
 * and plain strings, so each one gets a small writable proxy that does the
 * conversion at the boundary instead of scattering casts through the template.
 */
const sourceAccountId = computed({
  get: () => draft.value.source_account_id,
  set: (value: string | number) => {
    draft.value.source_account_id = asNumber(value);
  },
});

const destinationAccountId = computed({
  get: () => draft.value.destination_account_id,
  set: (value: string | number) => {
    draft.value.destination_account_id = asNumber(value);
  },
});

const direction = computed({
  get: () => draft.value.direction,
  set: (value: string | number) => {
    draft.value.direction = asText(value);
  },
});

const conflictPolicy = computed({
  get: () => draft.value.conflict_policy,
  set: (value: string | number) => {
    draft.value.conflict_policy = asText(value);
  },
});

onMounted(load);
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="editing ? t('jobs.editTitle') : t('jobs.createTitle')" :subtitle="t('jobs.subtitle')">
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
      <!-- 1. What the job is called and how it behaves by default. -->
      <template #details>
        <Card :title="t('jobs.steps.details')" :description="t('jobs.stepsHint.details')">
          <div class="space-y-5">
            <div class="space-y-1.5">
              <Label for="job-name" required>{{ t('jobs.name') }}</Label>
              <Input
                id="job-name"
                v-model="draft.name"
                :placeholder="t('jobs.namePlaceholder')"
                :disabled="saving"
              />
              <p v-if="errors.name" class="text-xs text-destructive">{{ t(errors.name) }}</p>
            </div>

            <div class="flex items-start justify-between gap-4 rounded-md border border-border p-3">
              <div class="space-y-1">
                <Label for="job-enabled">{{ t('jobs.enabled') }}</Label>
                <p class="text-xs text-muted-foreground">{{ t('jobs.enabledHint') }}</p>
              </div>
              <Switch
                id="job-enabled"
                v-model="draft.enabled"
                :disabled="saving"
                :label="t('jobs.enabled')"
              />
            </div>

            <div class="grid gap-4 sm:grid-cols-2">
              <div class="space-y-1.5">
                <Label for="job-direction" required>{{ t('jobs.direction') }}</Label>
                <Select id="job-direction" v-model="direction" :options="directions" />
                <p class="text-xs text-muted-foreground">
                  {{ directionHint(t, direction) }}
                </p>
              </div>
              <div class="space-y-1.5">
                <Label for="job-conflict" required>{{ t('jobs.conflictPolicy') }}</Label>
                <Select id="job-conflict" v-model="conflictPolicy" :options="policies" />
                <p class="text-xs text-muted-foreground">
                  {{ conflictHint(t, conflictPolicy) }}
                </p>
              </div>
            </div>
          </div>
        </Card>
      </template>

      <!-- 2. Where the job reads from. -->
      <template #source>
        <Card :title="t('jobs.steps.source')" :description="t('jobs.stepsHint.source')">
          <div class="space-y-5">
            <div class="space-y-1.5">
              <Label for="job-source-account" required>{{ t('jobs.sourceAccount') }}</Label>
              <Select
                id="job-source-account"
                v-model="sourceAccountId"
                :options="accountOptions"
                :placeholder="t('validation.selectAccount')"
                :disabled="saving"
              />
              <p v-if="errors.accounts" class="text-xs text-destructive">
                {{ t(errors.accounts) }}
              </p>
            </div>

            <div class="space-y-1.5">
              <Label for="job-source-folder" required>{{ t('jobs.sourceFolder') }}</Label>
              <div class="flex flex-wrap items-center gap-2">
                <code
                  id="job-source-folder"
                  class="rounded-md bg-muted px-2 py-1 text-xs text-muted-foreground"
                >
                  {{ folderPath('source') }}
                </code>
                <Button
                  variant="outline"
                  size="sm"
                  :disabled="saving || draft.source_account_id <= 0"
                  @click="sourcePicker = true"
                >
                  <FolderInput class="h-4 w-4" aria-hidden="true" />
                  {{ t('browser.browse') }}
                </Button>
              </div>
              <p v-if="errors.source" class="text-xs text-destructive">{{ t(errors.source) }}</p>
            </div>
          </div>
        </Card>
      </template>

      <!-- 3. Where the job writes to. The two sides differ only in the payload
           fields they touch, which is why the panels stay mirror images. -->
      <template #destination>
        <Card :title="t('jobs.steps.destination')" :description="t('jobs.stepsHint.destination')">
          <div class="space-y-5">
            <div class="space-y-1.5">
              <Label for="job-destination-account" required>{{ t('jobs.destinationAccount') }}</Label>
              <Select
                id="job-destination-account"
                v-model="destinationAccountId"
                :options="accountOptions"
                :placeholder="t('validation.selectAccount')"
                :disabled="saving"
              />
              <p v-if="errors.accounts" class="text-xs text-destructive">
                {{ t(errors.accounts) }}
              </p>
            </div>

            <div class="space-y-1.5">
              <Label for="job-destination-folder" required>{{ t('jobs.destinationFolder') }}</Label>
              <div class="flex flex-wrap items-center gap-2">
                <code
                  id="job-destination-folder"
                  class="rounded-md bg-muted px-2 py-1 text-xs text-muted-foreground"
                >
                  {{ folderPath('destination') }}
                </code>
                <Button
                  variant="outline"
                  size="sm"
                  :disabled="saving || draft.destination_account_id <= 0"
                  @click="destinationPicker = true"
                >
                  <FolderOutput class="h-4 w-4" aria-hidden="true" />
                  {{ t('browser.browse') }}
                </Button>
              </div>
              <p v-if="errors.destination" class="text-xs text-destructive">
                {{ t(errors.destination) }}
              </p>
            </div>
          </div>
        </Card>
      </template>

      <!-- 4. When the job runs and what it must not touch. -->
      <template #options>
        <div class="space-y-4">
          <Card :title="t('jobs.steps.options')" :description="t('jobs.stepsHint.options')">
            <div class="space-y-5">
              <div class="space-y-1.5">
                <Label for="job-interval" :hint="t('jobs.intervalHint')">{{ t('jobs.interval') }}</Label>
                <Input
                  id="job-interval"
                  v-model="intervalText"
                  type="number"
                  min="0"
                  :max="MAX_INTERVAL_MINUTES"
                  step="5"
                  numeric
                  :disabled="saving"
                />
                <p class="text-xs text-muted-foreground">{{ scheduleLabel(t, asNumber(intervalText)) }}</p>
              </div>

              <div class="flex items-start justify-between gap-4 rounded-md border border-border p-3">
                <div class="space-y-1">
                  <Label for="job-delete-missing">{{ t('jobs.deleteMissing') }}</Label>
                  <p class="text-xs text-muted-foreground">{{ t('jobs.deleteMissingHint') }}</p>
                </div>
                <Switch
                  id="job-delete-missing"
                  v-model="draft.delete_missing"
                  :disabled="saving"
                  :label="t('jobs.deleteMissing')"
                />
              </div>

              <div class="space-y-1.5">
                <Label for="job-exclude" :hint="t('jobs.excludeHint')">
                  {{ t('jobs.excludePatterns') }}
                </Label>
                <Textarea
                  id="job-exclude"
                  v-model="excludeText"
                  mono
                  :rows="5"
                  :disabled="saving"
                />
              </div>
            </div>
          </Card>

          <!-- The preview comes from the server, so it always shows the plan the
               scheduler would produce for the stored job, not a guess. -->
          <Card :title="t('jobs.schedule')" :description="t('jobs.schedulePreview')">
            <ul v-if="schedule.length" class="space-y-1 text-xs text-muted-foreground">
              <li v-for="entry in schedule" :key="entry">{{ formatDateTime(entry, locale) }}</li>
            </ul>
            <p v-else class="text-xs text-muted-foreground">{{ t('jobs.scheduleEmpty') }}</p>
          </Card>
        </div>
      </template>
    </Tabs>

    <!-- The footer sits outside the tabs: saving is possible from every step. -->
    <div
      v-if="!loading"
      class="flex flex-wrap items-center justify-end gap-2 rounded-lg border border-border bg-card p-4"
    >
      <Button variant="ghost" :disabled="saving" @click="back">{{ t('common.cancel') }}</Button>
      <Button :loading="saving" @click="submit">
        <Save v-if="!saving" class="h-4 w-4" aria-hidden="true" />
        {{ editing ? t('common.save') : t('common.create') }}
      </Button>
    </div>

    <FolderPicker
      v-if="draft.source_account_id > 0"
      v-model:open="sourcePicker"
      :account-id="draft.source_account_id"
      :provider="account('source')?.provider"
      purpose="source"
      :start-drive-id="draft.source_drive_id"
      :start-folder-id="draft.source_folder_id"
      :start-folder-path="draft.source_folder_path"
      @select="(selection) => applyFolders('source', selection)"
    />

    <FolderPicker
      v-if="draft.destination_account_id > 0"
      v-model:open="destinationPicker"
      :account-id="draft.destination_account_id"
      :provider="account('destination')?.provider"
      purpose="destination"
      :start-drive-id="draft.destination_drive_id"
      :start-folder-id="draft.destination_folder_id"
      :start-folder-path="draft.destination_folder_path"
      @select="(selection) => applyFolders('destination', selection)"
    />
  </div>
</template>
