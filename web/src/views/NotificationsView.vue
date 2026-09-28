<script setup lang="ts">
// NotificationsView — the channels a run reports to.
//
// The form is entirely server driven: GET /api/notifications returns the channel
// kinds with their field schema (see internal/handlers/notifications.go), so a
// sender added on the Go side shows up here without a change in the SPA. A secret
// is never returned by the API and an empty secret keeps the stored one, which is
// what the hint under every secret field says: editing a channel is safe.
import { Bell, Pencil, Plus, RefreshCw, Send, Trash2 } from '@lucide/vue';
import { computed, onMounted, reactive, ref } from 'vue';
import { useI18n } from 'vue-i18n';

import ConfirmDialog from '@/components/ConfirmDialog.vue';
import PageHeader from '@/components/PageHeader.vue';
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
import Textarea from '@/components/ui/Textarea.vue';
import { useAction } from '@/composables/useAction';
import {
  messageOf,
  notifications as notificationsApi,
  type ChannelField,
  type ChannelKind,
  type ChannelPayload,
  type NotificationChannel,
} from '@/lib/api';
import { formatDateTime } from '@/lib/format';
import { MASKED_SECRET, asNumber, asText, formatJson, parseJsonObject } from '@/lib/forms';

/** EVENT_PREFIX is how the catalog names the events of the API. */
const EVENT_PREFIX = 'notifications.event_';

/** ConfigValue is what one server driven field holds while it is edited. */
type ConfigValue = string | number | boolean;

const { t, te, locale } = useI18n();

const rows = ref<NotificationChannel[]>([]);
const kinds = ref<ChannelKind[]>([]);
const events = ref<string[]>([]);
const loading = ref(true);
const failure = ref('');

/** activeTest and activeToggle scope a row spinner to the channel it belongs to. */
const activeTest = ref(0);
const activeToggle = ref(0);
const removal = ref<NotificationChannel | null>(null);
const editingId = ref<number | null>(null);
const formOpen = ref(false);
const validationError = ref('');

/** draft is the channel being written; it lives outside the dialog on purpose. */
const draft = reactive({
  name: '',
  type: '',
  enabled: true,
  events: [] as string[],
  config: {} as Record<string, ConfigValue>,
});

const { busy: testing, run: runTest } = useAction();
const { busy: saving, run: runSave } = useAction();
const { busy: removing, run: runRemove } = useAction();
const { busy: toggling, run: runToggle } = useAction();

/** activeKind is the schema of the type picked in the form. */
const activeKind = computed(() => kinds.value.find((kind) => kind.kind === draft.type) ?? null);
const fields = computed<ChannelField[]>(() => activeKind.value?.fields ?? []);

/** typeOptions lists the kinds compiled into this build. */
const typeOptions = computed(() =>
  kinds.value.map((kind) => ({ value: kind.kind, label: kind.label })),
);

/** formTitle names the dialog after the action in progress. */
const formTitle = computed(() =>
  editingId.value === null ? t('notifications.createTitle') : t('notifications.editTitle'),
);

/** confirmOpen drives the deletion dialog from the channel that was picked. */
const confirmOpen = computed({
  get: () => removal.value !== null,
  set: (open: boolean) => {
    if (!open) {
      removal.value = null;
    }
  },
});

/** typeLabel names a channel kind, falling back to the raw identifier. */
function typeLabel(type: string): string {
  return kinds.value.find((kind) => kind.kind === type)?.label ?? type;
}

/** eventLabel translates an event identifier, falling back to the raw one. */
function eventLabel(event: string): string {
  const key = `${EVENT_PREFIX}${event}`;
  return te(key) ? t(key) : event;
}

/** channelKindFields is the schema of one kind, or an empty list when unknown. */
function channelKindFields(type: string): ChannelField[] {
  return kinds.value.find((kind) => kind.kind === type)?.fields ?? [];
}

/** defaultOf is the initial value of one field: the server default, or empty. */
function defaultOf(field: ChannelField): ConfigValue {
  const value = field.default;
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
    return value;
  }
  return '';
}

/** defaultConfig is the field set of one kind, filled with its defaults. */
function defaultConfig(type: string): Record<string, ConfigValue> {
  const config: Record<string, ConfigValue> = {};
  for (const field of channelKindFields(type)) {
    config[field.key] = defaultOf(field);
  }
  return config;
}

/** fieldId gives every server driven input a stable, labelable id. */
function fieldId(field: ChannelField): string {
  return `channel-${field.key}`;
}

/** htmlType maps a schema type onto the input type of the primitive. */
function htmlType(field: ChannelField): 'text' | 'password' | 'number' {
  if (field.type === 'password') {
    return 'password';
  }
  return field.type === 'int' ? 'number' : 'text';
}

/** fieldHint joins the schema hint with the "empty keeps the secret" reminder. */
function fieldHint(field: ChannelField): string {
  return [field.hint, field.secret ? t('notifications.secretKept') : ''].filter(Boolean).join(' · ');
}

/** fieldPlaceholder never reveals the length of a stored secret. */
function fieldPlaceholder(field: ChannelField): string {
  return field.secret ? MASKED_SECRET : '';
}

/** textValue is what a text-like field shows, whatever the stored type is. */
function textValue(field: ChannelField): string {
  const value = draft.config[field.key];
  if (value === undefined || typeof value === 'boolean') {
    return '';
  }
  return String(value);
}

/** boolValue is what a switch field shows. */
function boolValue(field: ChannelField): boolean {
  return draft.config[field.key] === true || draft.config[field.key] === 'true';
}

/** selectOptions turns the schema options into what the select expects. */
function selectOptions(field: ChannelField): Array<{ value: string; label: string }> {
  return (field.options ?? []).map((option) => ({ value: option, label: option }));
}

/** setValue stores one raw field value; it is converted at submit time. */
function setValue(key: string, value: ConfigValue): void {
  draft.config[key] = value;
}

/** storedToValue renders a stored configuration value into an editable one. */
function storedToValue(field: ChannelField, stored: unknown): ConfigValue {
  switch (typeof stored) {
    case 'boolean':
      return field.type === 'bool' ? stored : String(stored);
    case 'number':
      return stored;
    case 'string':
      return stored;
    default:
      return stored === null || stored === undefined ? defaultOf(field) : formatJson(stored);
  }
}

/** onTypeChange rebuilds the field values of the newly picked kind. */
function onTypeChange(value: string | number): void {
  draft.type = String(value);
  draft.config = defaultConfig(draft.type);
}

/** create opens an empty form for the first kind of this build. */
function create(): void {
  const first = kinds.value[0];
  if (!first) {
    return;
  }
  editingId.value = null;
  validationError.value = '';
  draft.name = '';
  draft.type = first.kind;
  draft.enabled = true;
  draft.events = [];
  draft.config = defaultConfig(first.kind);
  formOpen.value = true;
}

/** edit opens the form of one channel; its secrets are not returned at all. */
function edit(channel: NotificationChannel): void {
  editingId.value = channel.id;
  validationError.value = '';
  draft.name = channel.name;
  draft.type = channel.type;
  draft.enabled = channel.enabled;
  draft.events = [...channel.events];
  const config: Record<string, ConfigValue> = {};
  for (const field of channelKindFields(channel.type)) {
    config[field.key] = storedToValue(field, channel.config[field.key]);
  }
  draft.config = config;
  formOpen.value = true;
}

/** toggleEvent subscribes or unsubscribes the channel to one event. */
function toggleEvent(event: string, selected: boolean): void {
  if (selected) {
    draft.events = draft.events.includes(event) ? draft.events : [...draft.events, event];
    return;
  }
  draft.events = draft.events.filter((candidate) => candidate !== event);
}

/** validate reports the first problem of the form, or an empty string. */
function validate(): string {
  if (asText(draft.name) === '') {
    return `${t('notifications.name')}: ${t('common.requiredField')}`;
  }
  if (draft.events.length === 0) {
    return t('notifications.eventsRequired');
  }
  for (const field of fields.value) {
    if (!field.required || field.type === 'bool') {
      continue;
    }
    if (textValue(field).trim() === '') {
      return `${field.label}: ${t('common.requiredField')}`;
    }
  }
  return '';
}

/** payload converts the draft into the API payload, or null when a JSON fails. */
function payload(): ChannelPayload | null {
  const config: Record<string, unknown> = {};
  for (const field of fields.value) {
    const raw = draft.config[field.key];
    if (field.type === 'int') {
      config[field.key] = asNumber(
        typeof raw === 'number' ? raw : String(raw ?? ''),
        typeof field.default === 'number' ? field.default : 0,
      );
      continue;
    }
    if (field.type === 'bool') {
      config[field.key] = raw === true || raw === 'true';
      continue;
    }
    if (field.type === 'json') {
      const parsed = parseJsonObject(typeof raw === 'string' ? raw : '');
      if (!parsed.ok) {
        validationError.value = `${field.label}: ${parsed.message}`;
        return null;
      }
      if (parsed.value !== null) {
        config[field.key] = parsed.value;
      }
      continue;
    }
    config[field.key] = textValue(field).trim();
  }
  return {
    name: asText(draft.name),
    type: draft.type,
    config,
    events: draft.events,
    enabled: draft.enabled,
  };
}

/** load reads the channels, the kinds and the events a channel can subscribe to. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const answer = await notificationsApi.list();
    rows.value = answer.channels;
    kinds.value = answer.kinds;
    events.value = answer.events;
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/** submit validates the form, saves the channel and closes the dialog. */
async function submit(): Promise<void> {
  validationError.value = validate();
  if (validationError.value !== '') {
    return;
  }
  const body = payload();
  if (!body) {
    return;
  }
  const id = editingId.value;
  const answer = await runSave(
    () => (id === null ? notificationsApi.create(body) : notificationsApi.update(id, body)),
    {
      success: id === null ? t('notifications.created') : t('notifications.updated'),
      failure: t('notifications.saveError'),
    },
  );
  if (answer) {
    formOpen.value = false;
    await load();
  }
}

/** test delivers a test event synchronously, so a bad config fails here. */
async function test(channel: NotificationChannel): Promise<void> {
  activeTest.value = channel.id;
  const answer = await runTest(() => notificationsApi.test(channel.id), {
    success: t('notifications.testOk'),
    failure: t('notifications.testFailed'),
  });
  if (answer) {
    await load();
  }
  activeTest.value = 0;
}

/** toggle flips one switch through its own endpoint, so no config is rewritten. */
async function toggle(channel: NotificationChannel, enabled: boolean): Promise<void> {
  activeToggle.value = channel.id;
  const answer = await runToggle(() => notificationsApi.setEnabled(channel.id, enabled), {
    success: enabled ? t('notifications.enabledOn') : t('notifications.enabledOff'),
    failure: t('notifications.saveError'),
  });
  if (answer) {
    rows.value = rows.value.map((row) => (row.id === answer.id ? answer : row));
  }
  activeToggle.value = 0;
}

/** confirmRemoval deletes the channel that the dialog names. */
async function confirmRemoval(): Promise<void> {
  const channel = removal.value;
  if (!channel) {
    return;
  }
  const answer = await runRemove(() => notificationsApi.remove(channel.id), {
    success: t('notifications.removed'),
    failure: t('notifications.removeError'),
  });
  if (answer === null) {
    return;
  }
  removal.value = null;
  await load();
}

onMounted(load);
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('notifications.title')" :subtitle="t('notifications.subtitle')">
      <template #actions>
        <Button variant="outline" :loading="loading" @click="load">
          <RefreshCw class="h-4 w-4" aria-hidden="true" />
          {{ t('common.refresh') }}
        </Button>
        <Button :disabled="kinds.length === 0" @click="create">
          <Plus class="h-4 w-4" aria-hidden="true" />
          {{ t('notifications.new') }}
        </Button>
      </template>
    </PageHeader>

    <Alert
      v-if="failure"
      tone="destructive"
      :title="t('notifications.loadError')"
      :message="failure"
    >
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <div v-if="loading && rows.length === 0" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <Card v-else-if="rows.length === 0" content-class="p-0">
      <EmptyState
        :icon="Bell"
        :title="t('notifications.empty')"
        :message="t('notifications.emptyHint')"
      >
        <Button :disabled="kinds.length === 0" @click="create">{{ t('notifications.new') }}</Button>
      </EmptyState>
    </Card>

    <div v-else class="space-y-4">
      <Card v-for="channel in rows" :key="channel.id">
        <template #title>
          <span class="flex flex-wrap items-center gap-2">
            {{ channel.name }}
            <Badge tone="outline">{{ typeLabel(channel.type) }}</Badge>
            <StatusBadge v-if="channel.last_status" :status="channel.last_status" />
          </span>
        </template>
        <template #actions>
          <div class="flex items-center gap-2">
            <span class="text-sm text-muted-foreground">{{ t('notifications.enabled') }}</span>
            <Switch
              :id="`channel-enabled-${channel.id}`"
              :disabled="toggling && activeToggle === channel.id"
              :model-value="channel.enabled"
              :label="t('notifications.enabled')"
              @update:model-value="(value: boolean) => toggle(channel, value)"
            />
          </div>
        </template>
        <div class="space-y-3">
          <div class="space-y-1">
            <p class="text-xs font-medium text-muted-foreground">{{ t('notifications.events') }}</p>
            <div class="flex flex-wrap gap-2">
              <Badge v-for="event in channel.events" :key="event" tone="secondary">
                {{ eventLabel(event) }}
              </Badge>
              <span v-if="channel.events.length === 0" class="text-sm text-muted-foreground">
                {{ t('common.none') }}
              </span>
            </div>
          </div>

          <dl class="grid gap-2 text-sm sm:grid-cols-2">
            <div class="flex flex-wrap gap-2">
              <dt class="text-muted-foreground">{{ t('notifications.lastUsed') }}</dt>
              <dd class="numeric">{{ formatDateTime(channel.last_used_at, locale) }}</dd>
            </div>
            <div v-if="channel.last_error" class="flex flex-wrap gap-2">
              <dt class="text-muted-foreground">{{ t('notifications.lastError') }}</dt>
              <dd class="break-anywhere text-destructive">{{ channel.last_error }}</dd>
            </div>
          </dl>
        </div>
        <template #footer>
          <Button
            variant="outline"
            size="sm"
            :loading="testing && activeTest === channel.id"
            @click="test(channel)"
          >
            <Send class="h-4 w-4" aria-hidden="true" />
            {{ testing && activeTest === channel.id ? t('notifications.testing') : t('notifications.test') }}
          </Button>
          <Button variant="ghost" size="sm" @click="edit(channel)">
            <Pencil class="h-4 w-4" aria-hidden="true" />
            {{ t('common.edit') }}
          </Button>
          <Button variant="ghost" size="sm" @click="removal = channel">
            <Trash2 class="h-4 w-4" aria-hidden="true" />
            {{ t('common.delete') }}
          </Button>
        </template>
      </Card>
    </div>

    <Dialog
      v-model:open="formOpen"
      :title="formTitle"
      :description="t('notifications.subtitle')"
      size="lg"
      :close-label="t('common.closeDialog')"
    >
      <form class="space-y-4" @submit.prevent="submit">
        <p v-if="validationError" class="text-sm font-medium text-destructive">
          {{ validationError }}
        </p>

        <div class="space-y-1.5">
          <Label for="channel-name" required>{{ t('notifications.name') }}</Label>
          <Input
            id="channel-name"
            v-model="draft.name"
            :placeholder="t('notifications.namePlaceholder')"
          />
        </div>

        <div class="space-y-1.5">
          <Label for="channel-type">{{ t('notifications.type') }}</Label>
          <Select
            id="channel-type"
            :model-value="draft.type"
            :options="typeOptions"
            :disabled="editingId !== null"
            @update:model-value="onTypeChange"
          />
        </div>

        <div v-for="field in fields" :key="field.key" class="space-y-1.5">
          <div
            v-if="field.type === 'bool'"
            class="flex items-center justify-between gap-3 rounded-lg border border-border p-3"
          >
            <Label :for="fieldId(field)" :hint="field.hint">{{ field.label }}</Label>
            <Switch
              :id="fieldId(field)"
              :model-value="boolValue(field)"
              :label="field.label"
              @update:model-value="(value: boolean) => setValue(field.key, value)"
            />
          </div>
          <template v-else>
            <Label :for="fieldId(field)" :hint="fieldHint(field)" :required="field.required">
              {{ field.label }}
            </Label>
            <Select
              v-if="field.type === 'select'"
              :id="fieldId(field)"
              :model-value="textValue(field)"
              :options="selectOptions(field)"
              :required="field.required"
              @update:model-value="(value: string | number) => setValue(field.key, String(value))"
            />
            <Textarea
              v-else-if="field.type === 'json' || field.type === 'textarea'"
              :id="fieldId(field)"
              mono
              :rows="field.type === 'json' ? 5 : 4"
              :placeholder="fieldPlaceholder(field)"
              :model-value="textValue(field)"
              :required="field.required"
              @update:model-value="(value: string) => setValue(field.key, value)"
            />
            <Input
              v-else
              :id="fieldId(field)"
              :type="htmlType(field)"
              :numeric="field.type === 'int'"
              :placeholder="fieldPlaceholder(field)"
              :model-value="textValue(field)"
              :required="field.required"
              @update:model-value="(value: string | number) => setValue(field.key, value)"
            />
          </template>
        </div>

        <div class="space-y-1.5">
          <p class="text-sm font-medium leading-none">{{ t('notifications.events') }}</p>
          <div class="space-y-2 rounded-lg border border-border p-3">
            <div
              v-for="event in events"
              :key="event"
              class="flex items-center justify-between gap-3"
            >
              <span class="text-sm">{{ eventLabel(event) }}</span>
              <Switch
                :model-value="draft.events.includes(event)"
                :label="eventLabel(event)"
                @update:model-value="(value: boolean) => toggleEvent(event, value)"
              />
            </div>
            <p v-if="events.length === 0" class="text-sm text-muted-foreground">
              {{ t('common.noResults') }}
            </p>
          </div>
        </div>
      </form>

      <template #footer>
        <Button variant="outline" @click="formOpen = false">{{ t('common.cancel') }}</Button>
        <Button :loading="saving" @click="submit">
          {{ editingId === null ? t('common.create') : t('common.save') }}
        </Button>
      </template>
    </Dialog>

    <ConfirmDialog
      v-model:open="confirmOpen"
      :title="t('notifications.deleteTitle')"
      :message="t('notifications.deleteMessage')"
      :confirm-label="t('common.delete')"
      :loading="removing"
      @confirm="confirmRemoval"
    />
  </div>
</template>
