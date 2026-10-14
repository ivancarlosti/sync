<script setup lang="ts">
// ApiView — the API tokens of this instance (Admin > API).
//
// A token is a programmatic credential: `sync_<prefix>.<secret>`. The secret is
// shown exactly once, by the dialog that created or regenerated it — the server
// only ever stores a SHA-256 digest, so there is nothing to reveal later. The
// scope of a token is what the API enforces: `read` allows every GET and
// `write` everything else (see docs/authentication.md).
import { KeyRound, Plus, RefreshCw, RotateCcw, ShieldOff, Trash2 } from '@lucide/vue';
import { computed, onMounted, reactive, ref } from 'vue';
import { useI18n } from 'vue-i18n';

import ConfirmDialog from '@/components/ConfirmDialog.vue';
import CopyField from '@/components/CopyField.vue';
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
import { useAction } from '@/composables/useAction';
import { messageOf, tokens as tokensApi, type APITokenView } from '@/lib/api';
import { formatDateTime } from '@/lib/format';

const { t, locale } = useI18n();

const rows = ref<APITokenView[]>([]);
const loading = ref(true);
const failure = ref('');

/** formOpen drives the create dialog; secretOpen the one-time secret dialog. */
const formOpen = ref(false);
const secretOpen = ref(false);
const issued = ref('');
const issuedName = ref('');
const validationError = ref('');

/** draft is the create form. */
const draft = reactive<{ name: string; scope: 'read' | 'write'; expires_in_days: number | string }>({
  name: '',
  scope: 'write',
  expires_in_days: 0,
});

/** confirm holds the token waiting for the destructive confirmation. */
const confirm = reactive<{ open: boolean; id: number; action: 'revoke' | 'remove'; name: string }>({
  open: false,
  id: 0,
  action: 'remove',
  name: '',
});

const { busy: creating, run: runCreate } = useAction();
const { busy: working, run: runWork } = useAction();
const { busy: confirming, run: runConfirm } = useAction();

/** scopeOptions explains what each scope grants. */
const scopeOptions = computed(() => [
  { value: 'read', label: t('api.scope_read') },
  { value: 'write', label: t('api.scope_write') },
]);

/** expiryOptions bounds the life of a token (0 = never expires). */
const expiryOptions = computed(() => [
  { value: 0, label: t('api.neverExpires') },
  { value: 30, label: t('api.days30') },
  { value: 90, label: t('api.days90') },
  { value: 365, label: t('api.days365') },
]);

/** stateTone maps the lifecycle of a token onto a badge tone. */
function stateTone(state: APITokenView['state']): 'success' | 'warning' | 'muted' {
  switch (state) {
    case 'active':
      return 'success';
    case 'expired':
      return 'warning';
    default:
      return 'muted';
  }
}

/** when renders an optional timestamp. */
function when(value?: string): string {
  return value ? formatDateTime(value, locale.value) : t('common.never');
}

/** load reads the token list. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const answer = await tokensApi.list();
    rows.value = answer.tokens;
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/** openForm resets the create dialog. */
function openForm(): void {
  draft.name = '';
  draft.scope = 'write';
  draft.expires_in_days = 0;
  validationError.value = '';
  formOpen.value = true;
}

/** submit creates a token and reveals its secret once. */
async function submit(): Promise<void> {
  if (draft.name.trim() === '') {
    validationError.value = t('api.nameRequired');
    return;
  }
  const days = Number(draft.expires_in_days) || 0;
  const answer = await runCreate(
    () =>
      tokensApi.create({
        name: draft.name.trim(),
        scopes: [draft.scope],
        expires_in_days: days,
      }),
    { failure: t('api.actionError') },
  );
  if (answer) {
    formOpen.value = false;
    reveal(answer.secret, answer.token.name);
    await load();
  }
}

/** regenerate replaces the secret of a token, invalidating the old one. */
async function regenerate(row: APITokenView): Promise<void> {
  const answer = await runWork(() => tokensApi.regenerate(row.id), {
    success: t('api.regenerated', { name: row.name }),
    failure: t('api.actionError'),
  });
  if (answer) {
    reveal(answer.secret, answer.token.name);
    await load();
  }
}

/** reveal shows the one-time secret of a token. */
function reveal(secret: string, name: string): void {
  issued.value = secret;
  issuedName.value = name;
  secretOpen.value = true;
}

/** ask opens the confirmation for a destructive action. */
function ask(row: APITokenView, action: 'revoke' | 'remove'): void {
  confirm.id = row.id;
  confirm.action = action;
  confirm.name = row.name;
  confirm.open = true;
}

/** confirmAction revokes or deletes the token that was picked. */
async function confirmAction(): Promise<void> {
  const action = confirm.action;
  const ok = await runConfirm(
    async () => {
      if (action === 'revoke') {
        await tokensApi.revoke(confirm.id);
      } else {
        await tokensApi.remove(confirm.id);
      }
      return true;
    },
    {
      success: action === 'revoke' ? t('api.revoked') : t('api.deleted'),
      failure: t('api.actionError'),
    },
  );
  if (ok) {
    confirm.open = false;
    await load();
  }
}

onMounted(() => {
  void load();
});
</script>

<template>
  <div class="space-y-6">
    <Card :description="t('api.subtitle')">
      <template #title>
        <span class="flex items-center gap-2">
          <KeyRound class="h-4 w-4 text-muted-foreground" aria-hidden="true" />
          {{ t('api.title') }}
        </span>
      </template>
      <template #actions>
        <Button variant="outline" size="sm" :loading="loading" @click="load">
          <RefreshCw class="h-4 w-4" aria-hidden="true" />
          {{ t('common.refresh') }}
        </Button>
        <Button size="sm" @click="openForm">
          <Plus class="h-4 w-4" aria-hidden="true" />
          {{ t('api.create') }}
        </Button>
      </template>

      <Alert v-if="failure" tone="destructive" :title="t('api.loadError')" :message="failure" />

      <div v-else-if="loading" class="flex items-center justify-center gap-2 py-6">
        <Spinner :label="t('common.loading')" />
        <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
      </div>

      <EmptyState
        v-else-if="rows.length === 0"
        :icon="KeyRound"
        :title="t('api.empty')"
        :message="t('api.emptyHint')"
      />

      <div v-else class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('api.name') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('api.token') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('api.scopes') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('api.lastUsed') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-border">
            <tr v-for="row in rows" :key="row.id">
              <td class="px-3 py-2">
                <div class="font-medium">{{ row.name }}</div>
                <div v-if="row.created_by" class="text-xs text-muted-foreground">
                  {{ row.created_by }}
                </div>
              </td>
              <td class="px-3 py-2">
                <code class="font-mono text-xs">sync_{{ row.prefix }}.…</code>
                <div class="mt-1">
                  <Badge :tone="stateTone(row.state)">{{ t(`api.state_${row.state}`) }}</Badge>
                </div>
              </td>
              <td class="px-3 py-2">
                <div class="flex flex-wrap gap-1">
                  <Badge v-for="scope in row.scopes" :key="scope" tone="outline">
                    {{ t(`api.scope_${scope}`) }}
                  </Badge>
                </div>
              </td>
              <td class="px-3 py-2 text-muted-foreground">
                <div>{{ when(row.last_used_at) }}</div>
                <div class="text-xs">
                  {{ t('api.requests', { count: row.requests }) }}
                  <span v-if="row.last_used_ip"> · {{ row.last_used_ip }}</span>
                </div>
              </td>
              <td class="px-3 py-2">
                <div class="flex justify-end gap-1">
                  <Button
                    variant="ghost"
                    size="sm"
                    :loading="working"
                    :disabled="row.state === 'revoked'"
                    @click="regenerate(row)"
                  >
                    <RotateCcw class="h-4 w-4" aria-hidden="true" />
                    {{ t('api.regenerate') }}
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    :disabled="row.state === 'revoked'"
                    @click="ask(row, 'revoke')"
                  >
                    <ShieldOff class="h-4 w-4" aria-hidden="true" />
                    {{ t('api.revoke') }}
                  </Button>
                  <Button variant="ghost" size="sm" @click="ask(row, 'remove')">
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

    <Dialog
      v-model:open="formOpen"
      :title="t('api.create')"
      :description="t('api.createHint')"
      :close-label="t('common.closeDialog')"
    >
      <form class="space-y-4" @submit.prevent="submit">
        <p v-if="validationError" class="text-sm font-medium text-destructive">
          {{ validationError }}
        </p>
        <div class="space-y-1.5">
          <Label for="token-name" required>{{ t('api.name') }}</Label>
          <Input
            id="token-name"
            v-model="draft.name"
            :placeholder="t('api.namePlaceholder')"
            autocomplete="off"
          />
        </div>
        <div class="space-y-1.5">
          <Label for="token-scope">{{ t('api.scopes') }}</Label>
          <Select
            id="token-scope"
            :model-value="draft.scope"
            :options="scopeOptions"
            :aria-label="t('api.scopes')"
            @update:model-value="(value) => { draft.scope = value as 'read' | 'write'; }"
          />
        </div>
        <div class="space-y-1.5">
          <Label for="token-expiry">{{ t('api.expiresAt') }}</Label>
          <Select
            id="token-expiry"
            :model-value="draft.expires_in_days"
            :options="expiryOptions"
            :aria-label="t('api.expiresAt')"
            @update:model-value="(value) => { draft.expires_in_days = Number(value); }"
          />
        </div>
      </form>
      <template #footer>
        <Button variant="outline" @click="formOpen = false">{{ t('common.cancel') }}</Button>
        <Button :loading="creating" @click="submit">
          <Plus class="h-4 w-4" aria-hidden="true" />
          {{ t('api.create') }}
        </Button>
      </template>
    </Dialog>

    <Dialog
      v-model:open="secretOpen"
      :title="t('api.secretTitle', { name: issuedName })"
      :description="t('api.secretHint')"
      :close-label="t('common.closeDialog')"
    >
      <div class="space-y-3">
        <Alert tone="warning" :message="t('api.secretWarning')" />
        <CopyField :value="issued" />
      </div>
      <template #footer>
        <Button @click="secretOpen = false">{{ t('api.secretDone') }}</Button>
      </template>
    </Dialog>

    <ConfirmDialog
      v-model:open="confirm.open"
      :title="confirm.action === 'revoke' ? t('api.revokeTitle') : t('api.deleteTitle')"
      :message="
        confirm.action === 'revoke'
          ? t('api.revokeMessage', { name: confirm.name })
          : t('api.deleteMessage', { name: confirm.name })
      "
      :confirm-label="confirm.action === 'revoke' ? t('api.revoke') : t('common.delete')"
      :loading="confirming"
      @confirm="confirmAction"
    />
  </div>
</template>

