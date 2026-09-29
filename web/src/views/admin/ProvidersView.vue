<script setup lang="ts">
// ProvidersView — the OAuth credentials of this instance.
//
// Two sources exist on the server (see internal/handlers/providers.go): the
// environment variables and the override stored in Sync. Every card says which
// one is in use, and an empty secret keeps the stored one, so an operator can
// correct a redirect URI without having the secret at hand.
import { Compass, RefreshCw, Save, Trash2 } from '@lucide/vue';
import { computed, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRouter } from 'vue-router';

import ConfirmDialog from '@/components/ConfirmDialog.vue';
import StatusBadge from '@/components/StatusBadge.vue';
import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Input from '@/components/ui/Input.vue';
import Label from '@/components/ui/Label.vue';
import Spinner from '@/components/ui/Spinner.vue';
import { useAction } from '@/composables/useAction';
import { messageOf, providers as providersApi, type ProviderInfo, type ProviderName } from '@/lib/api';
import { MASKED_SECRET } from '@/lib/forms';
import { providerLabel } from '@/lib/providers';

/** Form is the editable state of one provider. */
interface Form {
  client_id: string;
  client_secret: string;
  redirect_uri: string;
  tenant_id: string;
}

/** EMPTY is the form of a provider that was never loaded. */
const EMPTY: Form = { client_id: '', client_secret: '', redirect_uri: '', tenant_id: '' };

const { t } = useI18n();
const router = useRouter();

const rows = ref<ProviderInfo[]>([]);
const hints = ref<Record<string, string>>({});
const forms = ref<Record<string, Form>>({});
const loading = ref(true);
const failure = ref('');

/** activeId scopes the spinner of a card to the provider it belongs to. */
const activeId = ref<ProviderName | ''>('');
const clearing = ref<ProviderInfo | null>(null);

const { busy: saving, run: runSave } = useAction();
const { busy: removing, run: runClear } = useAction();

/** origin is the base the redirect hint of every provider is appended to. */
const origin = computed(() => window.location.origin);

/** clearOpen drives the confirmation dialog from the provider that was picked. */
const clearOpen = computed({
  get: () => clearing.value !== null,
  set: (open: boolean) => {
    if (!open) {
      clearing.value = null;
    }
  },
});

/** clearLabel is the provider whose stored override is about to be removed. */
const clearLabel = computed(() => (clearing.value ? providerLabel(clearing.value.provider) : ''));

/** formOf is the editable state of one provider, created on first use. */
function formOf(provider: ProviderName): Form {
  return forms.value[provider] ?? EMPTY;
}

/** setField writes one input of one provider. */
function setField(provider: ProviderName, key: keyof Form, value: string | number): void {
  forms.value = {
    ...forms.value,
    [provider]: { ...formOf(provider), [key]: String(value) },
  };
}

/** hintOf is the callback path of one provider, appended to the origin. */
function hintOf(provider: string): string {
  const path = hints.value[provider] ?? '';
  return path === '' ? origin.value : `${origin.value}${path}`;
}

/** load reads both providers together with the callback paths. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const answer = await providersApi.list();
    rows.value = answer.providers;
    hints.value = answer.redirect_hint ?? {};
    // The fields are filled from the server, except the secret: an empty input
    // means "keep what is stored", which is what makes the form safe to resubmit.
    const next: Record<string, Form> = {};
    for (const row of answer.providers) {
      next[row.provider] = {
        client_id: row.client_id ?? '',
        client_secret: '',
        redirect_uri: row.redirect_uri ?? '',
        tenant_id: row.tenant_id ?? '',
      };
    }
    forms.value = next;
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/** save stores the credentials of one provider. */
async function save(row: ProviderInfo): Promise<void> {
  const form = formOf(row.provider);
  activeId.value = row.provider;
  const answer = await runSave(
    () =>
      providersApi.update(row.provider, {
        client_id: form.client_id,
        client_secret: form.client_secret,
        redirect_uri: form.redirect_uri,
        tenant_id: form.tenant_id,
      }),
    { success: t('admin.saved'), failure: t('admin.saveError') },
  );
  activeId.value = '';
  if (answer) {
    // The secret is cleared from the form: the next save must be able to mean
    // "keep the stored one" again, which only an empty input can express.
    setField(row.provider, 'client_secret', '');
    rows.value = rows.value.map((item) => (item.provider === row.provider ? answer : item));
  }
}

/** askClear opens the confirmation for one provider. */
function askClear(row: ProviderInfo): void {
  clearing.value = row;
}

/** clear removes the stored override of one provider. */
async function clear(): Promise<void> {
  const row = clearing.value;
  if (!row) {
    return;
  }
  activeId.value = row.provider;
  const answer = await runClear(() => providersApi.clear(row.provider), {
    success: t('admin.cleared'),
    failure: t('admin.clearError'),
  });
  activeId.value = '';
  if (answer) {
    clearing.value = null;
    rows.value = rows.value.map((item) => (item.provider === row.provider ? answer : item));
    await load();
  }
}

/** secretPlaceholder masks a stored secret so it is visible but not readable. */
function secretPlaceholder(row: ProviderInfo): string {
  return row.secret_set ? MASKED_SECRET : t('admin.clientSecret');
}

/**
 * sourceStatus is the badge of where the credentials come from. The API answers
 * `none` when neither the environment nor the database holds them, which is the
 * "not configured" state rather than a source.
 */
function sourceStatus(row: ProviderInfo): string {
  return row.source === 'none' ? 'notConfigured' : row.source;
}

onMounted(() => {
  void load();
});

/** openGuide opens the guided app registration of one provider. */
function openGuide(provider: ProviderName): void {
  void router.push({ name: 'admin-guide', params: { provider } });
}
</script>

<template>
  <div class="space-y-6">
    <Alert
      v-if="failure"
      tone="destructive"
      :title="t('admin.loadError')"
      :message="failure"
    >
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <div class="flex items-center justify-end">
      <Button variant="outline" size="sm" :loading="loading" @click="load">
        <RefreshCw class="h-4 w-4" aria-hidden="true" />
        {{ t('common.refresh') }}
      </Button>
    </div>

    <div v-if="loading && rows.length === 0" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <EmptyState v-if="!loading && rows.length === 0" :title="t('common.noResults')" />

    <Card
      v-for="row in rows"
      :key="row.provider"
      :description="t('admin.providersSubtitle')"
      :header-class="'items-center'"
    >
      <template #title>
        <span class="flex items-center gap-2">
          {{ providerLabel(row.provider) }}
          <StatusBadge :status="row.configured ? 'configured' : 'notConfigured'" />
        </span>
      </template>
      <template #actions>
        <span class="flex items-center gap-2 text-xs text-muted-foreground">
          {{ t('admin.source') }}
          <StatusBadge :status="sourceStatus(row)" />
        </span>
      </template>

      <div class="space-y-4">
        <div class="grid gap-4 sm:grid-cols-2">
          <div class="space-y-1.5">
            <Label :for="`provider-${row.provider}-client-id`" required>
              {{ t('admin.clientId') }}
            </Label>
            <Input
              :id="`provider-${row.provider}-client-id`"
              :model-value="formOf(row.provider).client_id"
              autocomplete="off"
              @update:model-value="(value) => setField(row.provider, 'client_id', value)"
            />
          </div>

          <div class="space-y-1.5">
            <Label
              :for="`provider-${row.provider}-client-secret`"
              :hint="row.secret_set ? t('admin.clientSecretSet') : t('admin.clientSecretMissing')"
            >
              {{ t('admin.clientSecret') }}
            </Label>
            <Input
              :id="`provider-${row.provider}-client-secret`"
              type="password"
              autocomplete="new-password"
              :placeholder="secretPlaceholder(row)"
              :model-value="formOf(row.provider).client_secret"
              @update:model-value="(value) => setField(row.provider, 'client_secret', value)"
            />
          </div>

          <div class="space-y-1.5 sm:col-span-2">
            <Label
              :for="`provider-${row.provider}-redirect-uri`"
              :hint="t('admin.redirectUriHint', { origin: hintOf(row.provider) })"
            >
              {{ t('admin.redirectUri') }}
            </Label>
            <Input
              :id="`provider-${row.provider}-redirect-uri`"
              type="url"
              :model-value="formOf(row.provider).redirect_uri"
              @update:model-value="(value) => setField(row.provider, 'redirect_uri', value)"
            />
          </div>

          <div v-if="row.provider === 'microsoft'" class="space-y-1.5 sm:col-span-2">
            <Label
              :for="`provider-${row.provider}-tenant-id`"
              :hint="t('admin.tenantIdHint')"
            >
              {{ t('admin.tenantId') }}
            </Label>
            <Input
              :id="`provider-${row.provider}-tenant-id`"
              :model-value="formOf(row.provider).tenant_id"
              @update:model-value="(value) => setField(row.provider, 'tenant_id', value)"
            />
          </div>
        </div>

        <p class="text-xs text-muted-foreground">
          {{ t('admin.secret') }}: {{ row.secret_set ? t('admin.set') : t('admin.unset') }}
        </p>

        <p v-if="row.admin_consent" class="text-xs text-muted-foreground">
          {{ t('admin.guide.consentTitle') }}:
          {{
            row.admin_consent.granted
              ? t('admin.guide.consentGranted', {
                  tenant: row.admin_consent.tenant,
                  date: row.admin_consent.at,
                })
              : t('admin.guide.consentMissing')
          }}
        </p>
      </div>

      <template #footer>
        <Button variant="outline" :disabled="saving || removing" @click="openGuide(row.provider)">
          <Compass class="h-4 w-4" aria-hidden="true" />
          {{ t('admin.guide.title') }}
        </Button>
        <Button
          v-if="row.source === 'database'"
          variant="outline"
          :disabled="saving"
          @click="askClear(row)"
        >
          <Trash2 class="h-4 w-4" aria-hidden="true" />
          {{ t('admin.clearSecret') }}
        </Button>
        <Button
          :loading="saving && activeId === row.provider"
          :disabled="removing && activeId === row.provider"
          @click="save(row)"
        >
          <Save class="h-4 w-4" aria-hidden="true" />
          {{ saving && activeId === row.provider ? t('common.saving') : t('common.save') }}
        </Button>
      </template>
    </Card>

    <ConfirmDialog
      v-model:open="clearOpen"
      :title="t('admin.clearSecret')"
      :message="clearLabel"
      :confirm-label="t('admin.clearSecret')"
      :loading="removing"
      @confirm="clear"
    />
  </div>
</template>
