<script setup lang="ts">
// AuditLogsView — the organisation-wide activity feed.
//
// The list is the *stored* copy of the vendor feeds (see docs/audit-logs.md), not
// a live query: the collector polls every ten minutes (or on demand from here),
// which is what keeps the history past the retention window of the providers and
// what makes the screen a single table instead of two provider-shaped ones.
//
// The cursor banner is the health of each feed: a tenant waiting for a consent
// still shows its directory activity and explains why the file feed is empty,
// instead of looking broken.
//
// The account filter is the same design seen from the operator's side: an account
// is turned into the organisation feed it reads, because two administrators of one
// tenant share a single feed, and that is the finest scope the stored events can
// honour.
import { Download, RefreshCw, ScrollText } from '@lucide/vue';
import { computed, onMounted, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';

import PageHeader from '@/components/PageHeader.vue';
import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Input from '@/components/ui/Input.vue';
import Select from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import { accountLabel } from '@/lib/accounts';
import {
  accounts as accountsApi,
  auditLogs as auditLogsApi,
  codeOf,
  type AuditLogAction,
  type AuditLogCursor,
  type AuditLogEvent,
  type AuditLogFilters,
  type AuditLogTarget,
  type ConnectedAccount,
} from '@/lib/api';
import { formatDateTime, formatNumber } from '@/lib/format';
import { useFeedbackStore } from '@/stores/feedback';

/** PAGE_SIZE is how many events one page of the table shows. */
const PAGE_SIZE = 50;
/** ACTIONS and TARGETS mirror the server vocabulary (models/enums.go). */
const ACTIONS: AuditLogAction[] = ['create', 'delete', 'update', 'move', 'share', 'login', 'other'];
const TARGETS: AuditLogTarget[] = ['file', 'folder', 'user', 'group', 'site', 'drive', 'other'];

const { t, locale } = useI18n();
const feedback = useFeedbackStore();

const events = ref<AuditLogEvent[]>([]);
const cursors = ref<AuditLogCursor[]>([]);
const total = ref(0);
const offset = ref(0);
const loading = ref(true);
const failure = ref('');
const collecting = ref(false);

// The applied filters, and the ones the form is editing: "Apply" is what copies
// one into the other, so a half-typed actor never narrows the table.
const applied = ref<AuditLogFilters>({});
const draft = ref<AuditLogFilters>({});

// The accounts the operator can narrow the list by, and the one the form has
// picked ("" reads every account). The feed is read once per organisation, so the
// pick is turned into the provider and the organisation of that account when the
// filters are applied — see apply().
const accounts = ref<ConnectedAccount[]>([]);
const accountId = ref<string | number>('');

/** selectedAccount is the account the form has picked, when it picked one. */
const selectedAccount = computed(() => accounts.value.find((account) => account.id === accountId.value));

/** selectedOrg is the organisation of that account, empty without one. */
const selectedOrg = computed(() => selectedAccount.value?.org_id ?? '');

/**
 * accountOptions are the accounts of the chosen provider, so the two fields of the
 * form can never disagree. An account without an organisation is offered disabled
 * with the reason: a personal account has no administrator feed, so filtering by it
 * could only ever list nothing.
 */
const accountOptions = computed(() => [
  { value: '', label: t('auditLogs.accountAll') },
  ...accounts.value
    .filter((account) => !draft.value.provider || account.provider === draft.value.provider)
    .map((account) => ({
      value: account.id,
      label: account.org_id
        ? accountLabel(account)
        : `${accountLabel(account)} — ${t('auditLogs.accountNoFeed')}`,
      disabled: !account.org_id,
    })),
]);

/**
 * readersByOrg names the accounts behind each organisation feed, so the table says
 * who an event belongs to instead of showing a tenant id alone. The key is the pair
 * the collector groups by (see internal/services/auditlog.go).
 */
const readersByOrg = computed(() => {
  const index = new Map<string, string[]>();
  for (const account of accounts.value) {
    if (!account.org_id) continue;
    const key = `${account.provider}\u0000${account.org_id}`;
    const addresses = index.get(key) ?? [];
    addresses.push(account.email || account.display_name || account.provider_account_id);
    index.set(key, addresses);
  }
  return index;
});

/** readers lists the accounts of the organisation of an event. */
function readers(event: AuditLogEvent): string {
  return (readersByOrg.value.get(`${event.provider}\u0000${event.org_id}`) ?? []).join(' · ');
}

// Picking an account chooses its provider, and choosing another provider drops a
// pick that no longer belongs to it: the two fields describe one decision.
watch(accountId, () => {
  if (selectedAccount.value) {
    draft.value.provider = selectedAccount.value.provider;
  }
});
watch(
  () => draft.value.provider,
  (provider) => {
    if (selectedAccount.value && selectedAccount.value.provider !== provider) {
      accountId.value = '';
    }
  },
);

const providerOptions = computed(() => [
  { value: '', label: t('common.all') },
  { value: 'microsoft', label: 'Microsoft 365' },
  { value: 'google', label: 'Google Workspace' },
]);

const actionOptions = computed(() => [
  { value: '', label: t('common.all') },
  ...ACTIONS.map((action) => ({ value: action, label: t(`auditLogs.action.${action}`) })),
]);

const targetOptions = computed(() => [
  { value: '', label: t('common.all') },
  ...TARGETS.map((target) => ({ value: target, label: t(`auditLogs.target.${target}`) })),
]);

/** staleFeeds are the feeds whose last pass failed: what the banner explains. */
const staleFeeds = computed(() => cursors.value.filter((cursor) => !cursor.healthy));

/** consentFeeds are the ones an operator can unblock with a grant. */
const consentFeeds = computed(() => cursors.value.filter((cursor) => cursor.needs_consent));

/** page counts the pages of the filtered set for the pager. */
const page = computed(() => Math.floor(offset.value / PAGE_SIZE) + 1);
const pages = computed(() => Math.max(1, Math.ceil(total.value / PAGE_SIZE)));
const hasPrevious = computed(() => offset.value > 0);
const hasNext = computed(() => offset.value + PAGE_SIZE < total.value);

/** errorText translates the machine-readable code of a failed request. */
function errorText(error: unknown): string {
  const key = `auditLogs.error_${codeOf(error)}`;
  const translated = t(key);
  return translated === key ? t('auditLogs.error_unknown') : translated;
}

/** when renders the vendor timestamp of an event. */
function when(value: string | undefined): string {
  return value ? formatDateTime(value, locale.value) : t('common.never');
}

/** count renders a counter with the grouping of the active locale. */
function count(value: number): string {
  return formatNumber(value, locale.value);
}

/** load reads one page with the applied filters. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const answer = await auditLogsApi.list(applied.value, PAGE_SIZE, offset.value);
    events.value = answer.events;
    cursors.value = answer.cursors;
    total.value = answer.total;
  } catch (error) {
    failure.value = errorText(error);
  } finally {
    loading.value = false;
  }
}

/**
 * loadAccounts fills the account filter with the connected accounts of the
 * instance — the same list the Accounts screen shows, whose organisation the API
 * reports as org_id. A failure only costs the filter: the table keeps working with
 * the provider alone, so it is not surfaced as the load error of the screen.
 */
async function loadAccounts(): Promise<void> {
  try {
    accounts.value = (await accountsApi.list()).accounts;
  } catch {
    accounts.value = [];
  }
}

/** apply copies the draft into the applied filters and jumps back to page one. */
function apply(): void {
  const filters: AuditLogFilters = { ...draft.value };
  const account = selectedAccount.value;
  if (account) {
    // The events do not name an account: the feed is read once per organisation,
    // so that is the finest scope the list can honour.
    filters.provider = account.provider;
    filters.org_id = account.org_id;
  }
  applied.value = filters;
  offset.value = 0;
  void load();
}

/** clear resets every filter. */
function clear(): void {
  draft.value = {};
  accountId.value = '';
  applied.value = {};
  offset.value = 0;
  void load();
}

/** goTo moves the pager by one page. */
function goTo(direction: -1 | 1): void {
  offset.value = Math.max(0, offset.value + direction * PAGE_SIZE);
  void load();
}

/**
 * collect asks the server to poll every feed now, then reloads: it is how an
 * operator verifies a grant without waiting for the next tick.
 */
async function collect(): Promise<void> {
  collecting.value = true;
  try {
    const result = await auditLogsApi.collect();
    if (result.failed > 0) {
      feedback.fail(
        t('auditLogs.collectedWithFailures', { failed: result.failed, streams: result.streams }),
        result.errors?.join('\n'),
      );
    } else {
      feedback.success(t('auditLogs.collected', { events: result.events, streams: result.streams }));
    }
    await load();
  } catch (error) {
    feedback.fail(errorText(error));
  } finally {
    collecting.value = false;
  }
}

/** exportHref is the download URL of the CSV of the current filters. */
const exportHref = computed(() => auditLogsApi.exportHref(applied.value));

onMounted(() => {
  void load();
  void loadAccounts();
});
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('auditLogs.title')" :subtitle="t('auditLogs.subtitle')">
      <template #actions>
        <Button variant="outline" :loading="collecting" :disabled="collecting" @click="collect">
          <RefreshCw v-if="!collecting" class="h-4 w-4" aria-hidden="true" />
          {{ collecting ? t('auditLogs.collecting') : t('auditLogs.collect') }}
        </Button>
        <Button as="a" :href="exportHref" variant="outline">
          <Download class="h-4 w-4" aria-hidden="true" />
          {{ t('auditLogs.export') }}
        </Button>
      </template>
    </PageHeader>

    <Alert v-if="failure" tone="destructive" :title="t('auditLogs.loadError')" :message="failure">
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <Alert
      v-if="consentFeeds.length > 0"
      tone="warning"
      :title="t('auditLogs.consentTitle')"
      :message="t('auditLogs.consentHint', { feeds: consentFeeds.length })"
    >
      <template #footer>
        <ul class="space-y-1 text-xs text-muted-foreground">
          <li v-for="feed in consentFeeds" :key="`${feed.provider}-${feed.org_id}-${feed.stream}`">
            <span class="font-medium text-foreground">{{ feed.provider }} · {{ feed.org_id }} · {{ feed.stream }}</span>
            <span class="block break-anywhere">{{ feed.last_error }}</span>
          </li>
        </ul>
      </template>
    </Alert>

    <Alert
      v-else-if="staleFeeds.length > 0"
      tone="warning"
      :title="t('auditLogs.staleTitle')"
      :message="t('auditLogs.staleHint', { feeds: staleFeeds.length })"
    >
      <template #footer>
        <ul class="space-y-1 text-xs text-muted-foreground">
          <li v-for="feed in staleFeeds" :key="`${feed.provider}-${feed.org_id}-${feed.stream}`">
            <span class="font-medium text-foreground">{{ feed.provider }} · {{ feed.org_id }} · {{ feed.stream }}</span>
            <span class="block break-anywhere">{{ feed.last_error }}</span>
          </li>
        </ul>
      </template>
    </Alert>

    <Card class="space-y-4 p-5">
      <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('auditLogs.provider') }}</p>
          <Select id="audit-logs-provider" v-model="draft.provider" :options="providerOptions" />
        </div>
        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('auditLogs.account') }}</p>
          <Select
            id="audit-logs-account"
            v-model="accountId"
            :options="accountOptions"
            :disabled="accounts.length === 0"
          />
          <p v-if="selectedOrg" class="text-xs text-muted-foreground">
            {{ t('auditLogs.accountHint', { org: selectedOrg }) }}
          </p>
        </div>
        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('auditLogs.actionLabel') }}</p>
          <Select id="audit-logs-action" v-model="draft.action" :options="actionOptions" />
        </div>
        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('auditLogs.targetLabel') }}</p>
          <Select id="audit-logs-target" v-model="draft.target" :options="targetOptions" />
        </div>
        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('auditLogs.search') }}</p>
          <Input
            id="audit-logs-search"
            v-model="draft.search"
            type="search"
            :placeholder="t('auditLogs.searchHint')"
            @keyup.enter="apply"
          />
        </div>
        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('auditLogs.from') }}</p>
          <Input id="audit-logs-from" v-model="draft.from" type="date" />
        </div>
        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('auditLogs.to') }}</p>
          <Input id="audit-logs-to" v-model="draft.to" type="date" />
        </div>
      </div>
      <div class="flex flex-wrap items-center justify-between gap-2">
        <p class="text-xs text-muted-foreground">
          {{ t('auditLogs.summary', { total: count(total), page, pages }) }}
        </p>
        <div class="flex items-center gap-2">
          <Button variant="ghost" size="sm" @click="clear">{{ t('auditLogs.clear') }}</Button>
          <Button @click="apply">{{ t('common.apply') }}</Button>
        </div>
      </div>
    </Card>

    <div v-if="loading && events.length === 0" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <Card v-else-if="events.length === 0" content-class="p-0">
      <EmptyState :icon="ScrollText" :title="t('auditLogs.empty')" :message="t('auditLogs.emptyHint')" />
    </Card>

    <Card v-else content-class="p-0">
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('auditLogs.when') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('auditLogs.actionLabel') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('auditLogs.object') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('auditLogs.actor') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('auditLogs.organisation') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-border">
            <tr v-for="event in events" :key="event.id" class="hover:bg-muted/40">
              <td class="whitespace-nowrap px-3 py-2 text-muted-foreground">
                <span class="numeric">{{ when(event.occurred_at) }}</span>
              </td>
              <td class="px-3 py-2">
                <span class="flex items-center gap-2">
                  <span class="rounded-md border border-border px-1.5 py-0.5 text-xs font-medium">
                    {{ t(`auditLogs.action.${event.action}`) }}
                  </span>
                  <span class="text-xs text-muted-foreground">
                    {{ t(`auditLogs.target.${event.target}`) }}
                  </span>
                </span>
              </td>
              <td class="break-anywhere px-3 py-2">
                <p>{{ event.target_name || event.target_id || t('common.notAvailable') }}</p>
                <p v-if="event.site_id" class="text-xs text-muted-foreground">{{ event.site_id }}</p>
              </td>
              <td class="break-anywhere px-3 py-2">
                <p>{{ event.actor_email || t('common.unknown') }}</p>
                <p v-if="event.actor_name" class="text-xs text-muted-foreground">{{ event.actor_name }}</p>
              </td>
              <td class="break-anywhere px-3 py-2 text-muted-foreground">
                <p>{{ event.org_id }}</p>
                <p class="text-xs">{{ event.provider }} · {{ event.source }}</p>
                <p v-if="readers(event)" class="text-xs">{{ readers(event) }}</p>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <div v-if="total > PAGE_SIZE" class="flex items-center justify-end gap-2">
      <Button variant="outline" size="sm" :disabled="!hasPrevious" @click="goTo(-1)">
        {{ t('common.previous') }}
      </Button>
      <span class="text-xs text-muted-foreground">{{ t('auditLogs.page', { page, pages }) }}</span>
      <Button variant="outline" size="sm" :disabled="!hasNext" @click="goTo(1)">
        {{ t('common.next') }}
      </Button>
    </div>
  </div>
</template>
