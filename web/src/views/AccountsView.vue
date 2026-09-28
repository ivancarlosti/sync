<script setup lang="ts">
// AccountsView — the authorisations Sync copies files with.
//
// An account is the precondition of every job: the engine reaches a folder
// through the token stored here (see internal/handlers/accounts.go). So the
// screen starts the OAuth flow of a provider and keeps an eye on what it already
// holds — `expires_at` is what makes a job fail weeks later, and verifying a token
// is cheaper than reading a failed run.
//
// The OAuth callback returns to this address with `connected`, `connect_error`
// and `detail` in the query (see redirectToAccounts in the Go handler); the codes
// are translated here like any other API failure, and the query is cleared so a
// reload does not repeat the message.
import { Plus, RefreshCw, ShieldCheck, Unplug } from '@lucide/vue';
import { computed, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';

import ConfirmDialog from '@/components/ConfirmDialog.vue';
import PageHeader from '@/components/PageHeader.vue';
import StatusBadge from '@/components/StatusBadge.vue';
import Alert from '@/components/ui/Alert.vue';
import Badge from '@/components/ui/Badge.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Spinner from '@/components/ui/Spinner.vue';
import { useAction } from '@/composables/useAction';
import {
  accounts,
  jobs as jobsApi,
  messageOf,
  oauth,
  type ConnectedAccount,
  type ProviderName,
  type SyncJob,
} from '@/lib/api';
import { formatDateTime } from '@/lib/format';
import { PROVIDERS, providerLabel } from '@/lib/providers';
import { useFeedbackStore } from '@/stores/feedback';

const route = useRoute();
const router = useRouter();
const { t, te, locale } = useI18n();
const feedback = useFeedbackStore();

const rows = ref<ConnectedAccount[]>([]);
const offered = ref<ProviderName[]>([]);
const usedJobs = ref<SyncJob[]>([]);
const loading = ref(true);
const failure = ref('');
/** connecting names the provider whose authorisation is being started. */
const connecting = ref<ProviderName | ''>('');
/** activeId is the row a long call belongs to, so one spinner is not all of them. */
const activeId = ref(0);
const removal = ref<ConnectedAccount | null>(null);

const { busy: verifying, run: runVerify } = useAction();
const { busy: removing, run: runRemove } = useAction();

/** confirmOpen drives the confirmation dialog from the row that was picked. */
const confirmOpen = computed({
  get: () => removal.value !== null,
  set: (open: boolean) => {
    if (!open) {
      removal.value = null;
    }
  },
});

/** messageKey maps a connect error code onto the catalog, with a fallback. */
function messageKey(code: string): string {
  const key = `accounts.error_${code}`;
  return te(key) ? key : 'accounts.error_unknown';
}

/** jobsOf counts the jobs that would be deleted with an account. */
function jobsOf(id: number): number {
  return usedJobs.value.filter(
    (job) => job.source_account_id === id || job.destination_account_id === id,
  ).length;
}

/** when renders a timestamp of the API, or an em dash when it is missing. */
function when(value: string | null | undefined): string {
  return formatDateTime(value, locale.value);
}

/** valueOf reads a single query parameter, tolerating the array form. */
function valueOf(value: unknown): string {
  const first = Array.isArray(value) ? value[0] : value;
  return typeof first === 'string' ? first : '';
}

/**
 * outcome is what the OAuth callback left in the URL. It is read once per
 * navigation: the effect that consumes it also clears the query.
 */
const outcome = computed(() => {
  const connected = valueOf(route.query.connected);
  if (connected !== '') {
    return { tone: 'success' as const, text: t('accounts.connectBanner', { provider: providerLabel(connected) }) };
  }
  const code = valueOf(route.query.connect_error);
  if (code !== '') {
    const detail = valueOf(route.query.detail);
    return { tone: 'error' as const, text: t(messageKey(code)), detail };
  }
  return null;
});

/** load reads the accounts, the providers this build can offer and the jobs. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const [list, available, all] = await Promise.all([
      accounts.list(),
      oauth.providers(),
      jobsApi.list(),
    ]);
    rows.value = list.accounts;
    usedJobs.value = all.jobs;
    // Kept in the display order of the provider list, not in the answer order.
    offered.value = PROVIDERS.filter((provider) => available.providers.includes(provider));
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/** connect asks the server for the provider URL and follows it. */
async function connect(provider: ProviderName): Promise<void> {
  connecting.value = provider;
  failure.value = '';
  try {
    const authorization = await accounts.connect(provider, '/accounts');
    // A full navigation: the provider page owns the rest of the exchange.
    window.location.assign(authorization.url);
  } catch (error) {
    feedback.fail(t('accounts.startError'), messageOf(error));
    connecting.value = '';
  }
}

/** verify refreshes the token and reports whether the provider still accepts it. */
async function verify(account: ConnectedAccount): Promise<void> {
  activeId.value = account.id;
  const answer = await runVerify(() => accounts.verify(account.id), {
    success: t('accounts.verified'),
    failure: t('accounts.verifyError'),
  });
  if (answer) {
    rows.value = rows.value.map((row) => (row.id === answer.id ? answer : row));
  }
  activeId.value = 0;
}

/** confirmRemoval deletes the stored authorisation and everything built on it. */
async function confirmRemoval(): Promise<void> {
  const account = removal.value;
  if (!account) {
    return;
  }
  const answer = await runRemove(() => accounts.remove(account.id), {
    failure: t('accounts.removeError'),
  });
  if (!answer) {
    return;
  }
  removal.value = null;
  await load();
  feedback.success(t('accounts.removed', { count: answer.deleted_jobs }));
}

onMounted(async () => {
  await load();
  if (outcome.value) {
    const shown = outcome.value;
    if (shown.tone === 'success') {
      feedback.success(shown.text);
    } else {
      feedback.fail(shown.text, shown.detail);
    }
    await router.replace({ query: {} });
  }
});
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('accounts.title')" :subtitle="t('accounts.subtitle')">
      <template #actions>
        <Button variant="outline" :loading="loading" @click="load">
          <RefreshCw class="h-4 w-4" aria-hidden="true" />
          {{ t('common.refresh') }}
        </Button>
        <Button
          v-for="provider in offered"
          :key="provider"
          :disabled="connecting !== '' && connecting !== provider"
          :loading="connecting === provider"
          @click="connect(provider)"
        >
          <Plus class="h-4 w-4" aria-hidden="true" />
          {{
            provider === 'google' ? t('accounts.connectGoogle') : t('accounts.connectMicrosoft')
          }}
        </Button>
      </template>
    </PageHeader>

    <Alert v-if="failure" tone="destructive" :title="t('accounts.loadError')" :message="failure">
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <div v-if="loading && rows.length === 0" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <Card v-else-if="rows.length === 0" content-class="p-0">
      <EmptyState :icon="Plus" :title="t('accounts.empty')" :message="t('accounts.emptyHint')">
        <Button
          v-for="provider in offered"
          :key="provider"
          variant="outline"
          size="sm"
          :loading="connecting === provider"
          @click="connect(provider)"
        >
          {{
            provider === 'google' ? t('accounts.connectGoogle') : t('accounts.connectMicrosoft')
          }}
        </Button>
      </EmptyState>
    </Card>

    <Card v-else content-class="p-0">
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('accounts.provider') }}</th>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('accounts.email') }}</th>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('accounts.status') }}</th>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('accounts.scopes') }}</th>
              <th scope="col" class="px-4 py-2 text-start font-medium">{{ t('accounts.expiresAt') }}</th>
              <th scope="col" class="px-4 py-2 text-start font-medium">
                {{ t('accounts.lastSyncedAt') }}
              </th>
              <th scope="col" class="px-4 py-2 text-end font-medium">{{ t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-border">
            <tr v-for="account in rows" :key="account.id" class="hover:bg-muted/40">
              <td class="px-4 py-3">
                <Badge tone="secondary">{{ providerLabel(account.provider) }}</Badge>
              </td>
              <td class="px-4 py-3">
                <p class="font-medium">{{ account.email || account.provider_account_id }}</p>
                <p v-if="account.display_name && account.display_name !== account.email" class="text-xs text-muted-foreground">
                  {{ account.display_name }}
                </p>
              </td>
              <td class="px-4 py-3">
                <StatusBadge :status="account.status" />
                <p v-if="account.last_error" class="mt-1 max-w-64 text-xs text-destructive">
                  {{ account.last_error }}
                </p>
              </td>
              <td class="px-4 py-3 text-muted-foreground" :title="account.scopes.join(', ')">
                {{ t('accounts.scopesCount', { count: account.scopes.length }) }}
              </td>
              <td class="px-4 py-3 text-muted-foreground">{{ when(account.expires_at) }}</td>
              <td class="px-4 py-3 text-muted-foreground">
                {{ when(account.last_synced_at) }}
                <p v-if="account.refreshed_at" class="text-xs">
                  {{ t('accounts.refreshedAt') }}: {{ when(account.refreshed_at) }}
                </p>
              </td>
              <td class="px-4 py-3">
                <div class="flex items-center justify-end gap-2">
                  <Button
                    variant="outline"
                    size="sm"
                    :loading="verifying && activeId === account.id"
                    @click="verify(account)"
                  >
                    <ShieldCheck class="h-4 w-4" aria-hidden="true" />
                    {{ verifying && activeId === account.id ? t('accounts.verifying') : t('accounts.verify') }}
                  </Button>
                  <Button variant="ghost" size="sm" @click="removal = account">
                    <Unplug class="h-4 w-4" aria-hidden="true" />
                    {{ t('accounts.remove') }}
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
      :title="t('accounts.removeTitle')"
      :message="removal ? t('accounts.removeMessage', { account: removal.email }) : ''"
      :confirm-label="t('accounts.remove')"
      :loading="removing"
      @confirm="confirmRemoval"
    >
      <p v-if="removal && jobsOf(removal.id) > 0">
        {{ t('accounts.removeJobsWarning', { count: jobsOf(removal.id) }) }}
      </p>
    </ConfirmDialog>
  </div>
</template>
