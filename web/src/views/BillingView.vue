<script setup lang="ts">
// BillingView — the traffic the instance moved through its accounts.
//
// The report is aggregated in SQL by the server (see internal/handlers/billing.go):
// this screen only chooses the window, the grouping and the vocabulary. Each
// direction carries its own rate, so the cost columns appear when a price is
// configured in Admin > Settings: an instance that does not bill anything shows
// plain bytes.
import { Download, RefreshCw, Wallet } from '@lucide/vue';
import { computed, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';

import PageHeader from '@/components/PageHeader.vue';
import StatCard from '@/components/StatCard.vue';
import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Input from '@/components/ui/Input.vue';
import Label from '@/components/ui/Label.vue';
import Select from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import {
  billing as billingApi,
  messageOf,
  type BillingGroupBy,
  type BillingRow,
  type BillingSchema,
} from '@/lib/api';
import { formatBytes, formatDate, formatNumber } from '@/lib/format';

/** GiB is the unit a priced report is expressed in (cloud invoices price GiB). */
const GIB = 1024 * 1024 * 1024;

/** DEFAULT_DAYS is the window the screen opens on: the last 90 days. */
const DEFAULT_DAYS = 90;

const { t, locale } = useI18n();

const rows = ref<BillingRow[]>([]);
const schema = ref<BillingSchema | null>(null);
const groupBy = ref<BillingGroupBy>('day');
const from = ref('');
const to = ref('');
const loading = ref(true);
const failure = ref('');

/** groupOptions is the grouping offered by the report. */
const groupOptions = computed(() => [
  { value: 'day', label: t('billing.groupDay') },
  { value: 'month', label: t('billing.groupMonth') },
  { value: 'account', label: t('billing.groupAccount') },
  { value: 'run', label: t('billing.groupRun') },
]);

/** rates are the configured prices of one GiB, one per direction. */
const rates = computed(() => ({
  in: schema.value?.rate_in_per_gib ?? 0,
  out: schema.value?.rate_out_per_gib ?? 0,
}));

/** priced is true when a rate is configured, which is what shows the cost columns. */
const priced = computed(() => rates.value.in > 0 || rates.value.out > 0);

/** costInShown and costOutShown show a column only when its own direction is priced. */
const costInShown = computed(() => rates.value.in > 0);
const costOutShown = computed(() => rates.value.out > 0);

/**
 * windowTotals adds the lines of the report per direction, with the cost of each
 * of them. The two directions are summed here rather than read from the answer
 * because a report line only carries its own split.
 */
const windowTotals = computed(() => {
  const sum = { inbound: 0, outbound: 0, operations: 0, costIn: 0, costOut: 0 };
  for (const row of rows.value) {
    sum.inbound += row.inbound.bytes;
    sum.outbound += row.outbound.bytes;
    sum.operations += row.total.operations;
    sum.costIn += priceOf(row.inbound.bytes, rates.value.in);
    sum.costOut += priceOf(row.outbound.bytes, rates.value.out);
  }
  return sum;
});

/** count renders a counter with the grouping of the active locale. */
function count(value: number): string {
  return formatNumber(value, locale.value);
}

/** bytes renders a byte total with the units of the active locale. */
function bytes(value: number): string {
  return formatBytes(value, locale.value);
}

/** bucket names one report line: the value of the asked-for grouping. */
function bucket(row: BillingRow): string {
  if (row.day) {
    return formatDate(row.day, locale.value);
  }
  if (row.month) {
    return row.month;
  }
  if (row.run_id) {
    return t('billing.runBucket', { id: row.run_id });
  }
  if (row.account_id) {
    return row.account_email || t('billing.accountBucket', { id: row.account_id });
  }
  return t('billing.allBuckets');
}

/** priceOf prices a byte total with a rate per GiB. */
function priceOf(bytes: number, rate: number): number {
  return (bytes / GIB) * rate;
}

/** money renders an amount in the currency of the schema. */
function money(amount: number): string {
  const currency = schema.value?.currency || 'USD';
  try {
    return new Intl.NumberFormat(locale.value, { style: 'currency', currency }).format(amount);
  } catch {
    return `${amount.toFixed(2)} ${currency}`;
  }
}

/** costInOf and costOutOf price one report line in a single direction. */
function costInOf(row: BillingRow): string {
  return money(priceOf(row.inbound.bytes, rates.value.in));
}

function costOutOf(row: BillingRow): string {
  return money(priceOf(row.outbound.bytes, rates.value.out));
}

/** costOf prices both directions of a line, the sum the CSV `cost` column carries. */
function costOf(row: BillingRow): string {
  return money(
    priceOf(row.inbound.bytes, rates.value.in) + priceOf(row.outbound.bytes, rates.value.out),
  );
}

/** windowFrom is the inclusive start of the window the report asks for. */
function defaultFrom(): string {
  const start = new Date();
  start.setUTCDate(start.getUTCDate() - DEFAULT_DAYS);
  return start.toISOString().slice(0, 10);
}

/** load reads the report for the current filters. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const answer = await billingApi.summary({
      group_by: groupBy.value,
      from: from.value,
      to: to.value,
    });
    rows.value = answer.rows;
    schema.value = answer.schema;
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/** download opens the CSV export of the same window in a new navigation. */
function download(): void {
  window.location.assign(
    billingApi.exportHref({ group_by: groupBy.value, from: from.value, to: to.value }),
  );
}

onMounted(() => {
  from.value = defaultFrom();
  void load();
});
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('billing.title')" :subtitle="t('billing.subtitle')">
      <template #actions>
        <Button variant="outline" :loading="loading" @click="load">
          <RefreshCw class="h-4 w-4" aria-hidden="true" />
          {{ t('common.refresh') }}
        </Button>
        <Button :disabled="rows.length === 0" @click="download">
          <Download class="h-4 w-4" aria-hidden="true" />
          {{ t('billing.download') }}
        </Button>
      </template>
    </PageHeader>

    <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
      <StatCard
        :label="schema?.inbound_label ?? t('billing.inbound')"
        :value="bytes(windowTotals.inbound)"
        :hint="t('billing.windowHint')"
        :icon="Wallet"
        tone="muted"
      />
      <StatCard
        :label="schema?.outbound_label ?? t('billing.outbound')"
        :value="bytes(windowTotals.outbound)"
        :hint="t('billing.windowHint')"
        :icon="Wallet"
      />
      <StatCard
        :label="schema?.operations_label ?? t('billing.operations')"
        :value="count(windowTotals.operations)"
        :hint="t('billing.windowHint')"
        tone="muted"
      />
      <StatCard
        v-if="priced"
        :label="schema?.cost_label ?? t('billing.cost')"
        :value="money(windowTotals.costIn + windowTotals.costOut)"
        :hint="t('billing.windowHint')"
        tone="warning"
      />
      <StatCard v-else :label="t('billing.total')" :value="bytes(windowTotals.inbound + windowTotals.outbound)" />
    </div>

    <Card>
      <div class="flex flex-wrap items-end gap-3">
        <div class="space-y-1.5">
          <Label for="billing-group">{{ t('billing.groupBy') }}</Label>
          <Select
            id="billing-group"
            :model-value="groupBy"
            :options="groupOptions"
            :aria-label="t('billing.groupBy')"
            @update:model-value="(value) => { groupBy = value as BillingGroupBy; void load(); }"
          />
        </div>
        <div class="space-y-1.5">
          <Label for="billing-from">{{ t('billing.from') }}</Label>
          <Input id="billing-from" v-model="from" type="date" @change="load" />
        </div>
        <div class="space-y-1.5">
          <Label for="billing-to">{{ t('billing.to') }}</Label>
          <Input id="billing-to" v-model="to" type="date" @change="load" />
        </div>
      </div>
    </Card>

    <Alert v-if="failure" tone="destructive" :title="t('billing.loadError')" :message="failure" />

    <Card v-else>
      <div v-if="loading" class="flex items-center justify-center gap-2 py-8">
        <Spinner :label="t('common.loading')" />
        <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
      </div>

      <EmptyState
        v-else-if="rows.length === 0"
        :icon="Wallet"
        :title="t('billing.empty')"
        :message="t('billing.emptyHint')"
      />

      <div v-else class="overflow-x-auto">
        <p v-if="schema" class="mb-3 text-sm text-muted-foreground">{{ schema.hint ?? schema.label }}</p>
        <table class="w-full text-sm">
          <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('billing.bucket') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">
                {{ schema?.inbound_label ?? t('billing.inbound') }}
              </th>
              <th scope="col" class="px-3 py-2 text-end font-medium">
                {{ schema?.outbound_label ?? t('billing.outbound') }}
              </th>
              <th scope="col" class="px-3 py-2 text-end font-medium">
                {{ schema?.operations_label ?? t('billing.operations') }}
              </th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('billing.total') }}</th>
              <th v-if="priced" scope="col" class="px-3 py-2 text-end font-medium">
                {{ schema?.cost_label ?? t('billing.cost') }}
              </th>
              <th v-if="costInShown" scope="col" class="px-3 py-2 text-end font-medium">
                {{ t('billing.costIn') }}
              </th>
              <th v-if="costOutShown" scope="col" class="px-3 py-2 text-end font-medium">
                {{ t('billing.costOut') }}
              </th>
            </tr>
          </thead>
          <tbody class="divide-y divide-border">
            <tr v-for="row in rows" :key="bucket(row)">
              <td class="px-3 py-2 font-medium">{{ bucket(row) }}</td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ bytes(row.inbound.bytes) }}</span></td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ bytes(row.outbound.bytes) }}</span></td>
              <td class="px-3 py-2 text-end text-muted-foreground">
                <span class="numeric">{{ count(row.total.operations) }}</span>
              </td>
              <td class="px-3 py-2 text-end font-medium"><span class="numeric">{{ bytes(row.total.bytes) }}</span></td>
              <td v-if="priced" class="px-3 py-2 text-end"><span class="numeric">{{ costOf(row) }}</span></td>
              <td v-if="costInShown" class="px-3 py-2 text-end text-muted-foreground">
                <span class="numeric">{{ costInOf(row) }}</span>
              </td>
              <td v-if="costOutShown" class="px-3 py-2 text-end text-muted-foreground">
                <span class="numeric">{{ costOutOf(row) }}</span>
              </td>
            </tr>
          </tbody>
          <tfoot class="border-t border-border bg-muted/30 font-medium">
            <tr>
              <td class="px-3 py-2">{{ t('billing.windowTotal') }}</td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ bytes(windowTotals.inbound) }}</span></td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ bytes(windowTotals.outbound) }}</span></td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ count(windowTotals.operations) }}</span></td>
              <td class="px-3 py-2 text-end">
                <span class="numeric">{{ bytes(windowTotals.inbound + windowTotals.outbound) }}</span>
              </td>
              <td v-if="priced" class="px-3 py-2 text-end">
                <span class="numeric">{{ money(windowTotals.costIn + windowTotals.costOut) }}</span>
              </td>
              <td v-if="costInShown" class="px-3 py-2 text-end">
                <span class="numeric">{{ money(windowTotals.costIn) }}</span>
              </td>
              <td v-if="costOutShown" class="px-3 py-2 text-end">
                <span class="numeric">{{ money(windowTotals.costOut) }}</span>
              </td>
            </tr>
          </tfoot>
        </table>
      </div>
    </Card>
  </div>
</template>

