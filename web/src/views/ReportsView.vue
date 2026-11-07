<script setup lang="ts">
// ReportsView — directory reports of a Microsoft 365 tenant.
//
// A report is the read-only inventory of an organisation (users, guests, groups,
// distribution lists, sites, licences, domains) collected with the delegated
// permissions Sync already holds (see internal/providers/microsoft/report.go and
// internal/services/report.go). It is async: the screen starts one against an
// account, polls the history while it runs, then reads the stored tables one
// section at a time and downloads the whole workbook as a ZIP of CSVs.
import { BarChart3, Download, Eye, Play, XCircle } from '@lucide/vue';
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';

import PageHeader from '@/components/PageHeader.vue';
import StatusBadge from '@/components/StatusBadge.vue';
import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import Dialog from '@/components/ui/Dialog.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Select from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import Switch from '@/components/ui/Switch.vue';
import { accountLabel } from '@/lib/accounts';
import {
  accounts as accountsApi,
  codeOf,
  reports as reportsApi,
  type ConnectedAccount,
  type DirectoryReport,
  type ReportSection,
  type ReportSectionPage,
  type ReportSectionSummary,
  type ReportSchedule,
} from '@/lib/api';
import { formatDateTime, formatNumber } from '@/lib/format';
import { cn } from '@/lib/utils';

/** POLL_INTERVAL is how often the screen refreshes while a report is running. */
const POLL_INTERVAL = 2000;
/** LIST_LIMIT is how many reports the history shows at once. */
const LIST_LIMIT = 50;
/** PAGE_SIZE is how many rows of a section the dialog reads per request. */
const PAGE_SIZE = 200;
/** SCHEDULE_INTERVALS are the cadences the schedule offers, in minutes. */
const SCHEDULE_INTERVALS = [60, 360, 720, 1440, 10080];

const { t, locale } = useI18n();

const rows = ref<DirectoryReport[]>([]);
const accounts = ref<ConnectedAccount[]>([]);
const accountId = ref(0);
const availableSections = ref<ReportSection[]>([]);
const supported = ref(true);
const selectedSections = ref<ReportSection[]>([]);

const loading = ref(true);
const failure = ref('');
const starting = ref(false);

const schedule = ref<ReportSchedule | null>(null);
const scheduleEnabled = ref(false);
const scheduleInterval = ref(1440);
const scheduleSaving = ref(false);
const scheduleFailure = ref('');

const detail = ref<DirectoryReport | null>(null);
const detailSections = ref<ReportSectionSummary[]>([]);
const detailFailure = ref('');
const activeSection = ref('');
const page = ref<ReportSectionPage | null>(null);
const pageLoading = ref(false);
const pageFailure = ref('');
const pageOffset = ref(0);

/** timer drives the automatic refresh while a report is in flight. */
let timer: ReturnType<typeof setInterval> | undefined;

const accountOptions = computed(() => [
  { value: 0, label: t('reports.selectAccount'), disabled: true },
  ...accounts.value.map((account) => ({ value: account.id, label: accountLabel(account) })),
]);

const intervalOptions = computed(() =>
  SCHEDULE_INTERVALS.map((minutes) => ({
    value: minutes,
    label: t(`reports.interval_${minutes}`),
  })),
);

/** running is true while a report of the history is still in flight. */
const running = computed(() => rows.value.some((run) => run.status === 'running'));

/** canStart gates the start button. */
const canStart = computed(
  () => accountId.value > 0 && !starting.value && !running.value && supported.value,
);

/** detailOpen drives the report dialog from the report that was picked. */
const detailOpen = computed({
  get: () => detail.value !== null,
  set: (open: boolean) => {
    if (!open) {
      detail.value = null;
      detailSections.value = [];
      detailFailure.value = '';
      page.value = null;
      pageFailure.value = '';
    }
  },
});

/** errorText translates the machine-readable code of a failed request. */
function errorText(error: unknown): string {
  const key = `reports.error_${codeOf(error)}`;
  const translated = t(key);
  return translated === key ? t('reports.error_unknown') : translated;
}

/** count renders a counter with the grouping of the active locale. */
function count(value: number): string {
  return formatNumber(value, locale.value);
}

/** when renders a timestamp of the API. */
function when(value: string | null | undefined): string {
  return formatDateTime(value, locale.value);
}

/** sectionLabel translates one section name. */
function sectionLabel(section: string): string {
  return t(`reports.section_${section}`);
}

/** sectionSummary finds the table of contents entry of a section. */
function sectionSummary(section: string): ReportSectionSummary | undefined {
  return detailSections.value.find((entry) => entry.section === section);
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

/** load reads the report history and (re)arms the poller. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const answer = await reportsApi.list(LIST_LIMIT);
    rows.value = answer.reports;
  } catch (error) {
    failure.value = errorText(error);
  } finally {
    loading.value = false;
    armPoller();
  }
}

/** armPoller polls again while a report is in flight, and stops when none is. */
function armPoller(): void {
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

/** loadSections asks the server what the selected account can collect. */
async function loadSections(): Promise<void> {
  if (accountId.value === 0) {
    return;
  }
  try {
    const answer = await reportsApi.sections(accountId.value);
    availableSections.value = answer.sections;
    supported.value = answer.supported;
    selectedSections.value = answer.sections;
  } catch (error) {
    failure.value = errorText(error);
  }
}

/** loadSchedule reads the opt-in of the selected account. */
async function loadSchedule(): Promise<void> {
  scheduleFailure.value = '';
  if (accountId.value === 0) {
    return;
  }
  try {
    const answer = await reportsApi.schedules();
    const found = answer.schedules.find((entry) => entry.account_id === accountId.value) ?? null;
    schedule.value = found;
    scheduleEnabled.value = found?.enabled ?? false;
    scheduleInterval.value = found?.interval_minutes ?? 1440;
  } catch (error) {
    scheduleFailure.value = errorText(error);
  }
}

/** applyAccount reloads the section catalogue and the schedule of an account. */
async function applyAccount(value: string | number): Promise<void> {
  accountId.value = Number(value);
  await Promise.all([loadSections(), loadSchedule()]);
}

/** toggleSection adds or removes a section from the selection. */
function toggleSection(section: ReportSection, checked: boolean): void {
  const next = new Set(selectedSections.value);
  if (checked) {
    next.add(section);
  } else {
    next.delete(section);
  }
  // Keep the canonical order of the catalogue so the request is stable.
  selectedSections.value = availableSections.value.filter((entry) => next.has(entry));
}

/** start launches a report and refreshes the history. */
async function start(): Promise<void> {
  if (!canStart.value) {
    return;
  }
  starting.value = true;
  failure.value = '';
  try {
    await reportsApi.start({
      account_id: accountId.value,
      sections: selectedSections.value.length === availableSections.value.length
        ? []
        : [...selectedSections.value],
    });
    await load();
  } catch (error) {
    failure.value = errorText(error);
  } finally {
    starting.value = false;
  }
}

/** saveSchedule stores the opt-in of the selected account. */
async function saveSchedule(): Promise<void> {
  if (accountId.value === 0) {
    return;
  }
  scheduleSaving.value = true;
  scheduleFailure.value = '';
  try {
    const answer = await reportsApi.saveSchedule(accountId.value, {
      enabled: scheduleEnabled.value,
      interval_minutes: Number(scheduleInterval.value),
      sections: [],
    });
    schedule.value = answer.schedule;
    scheduleEnabled.value = answer.schedule.enabled;
    scheduleInterval.value = answer.schedule.interval_minutes;
  } catch (error) {
    scheduleFailure.value = errorText(error);
  } finally {
    scheduleSaving.value = false;
  }
}

/** deleteSchedule removes the opt-in of the selected account. */
async function deleteSchedule(): Promise<void> {
  if (accountId.value === 0) {
    return;
  }
  scheduleSaving.value = true;
  scheduleFailure.value = '';
  try {
    await reportsApi.deleteSchedule(accountId.value);
    schedule.value = null;
    scheduleEnabled.value = false;
    scheduleInterval.value = 1440;
  } catch (error) {
    scheduleFailure.value = errorText(error);
  } finally {
    scheduleSaving.value = false;
  }
}

/** openReport reads the table of contents of one report and its first section. */
async function openReport(run: DirectoryReport): Promise<void> {
  detail.value = run;
  detailSections.value = [];
  detailFailure.value = '';
  page.value = null;
  pageFailure.value = '';
  try {
    const answer = await reportsApi.get(run.id);
    detail.value = answer.report;
    detailSections.value = answer.sections;
    rows.value = rows.value.map((row) => (row.id === answer.report.id ? answer.report : row));
    const first = answer.sections.find((section) => section.row_count > 0) ?? answer.sections[0];
    if (first) {
      await selectSection(first.section);
    }
  } catch (error) {
    detailFailure.value = errorText(error);
  }
}

/** selectSection reads the first page of one stored table. */
async function selectSection(section: string): Promise<void> {
  activeSection.value = section;
  pageOffset.value = 0;
  await loadPage();
}

/** loadPage reads the current page of the active section. */
async function loadPage(): Promise<void> {
  if (!detail.value || activeSection.value === '') {
    return;
  }
  pageLoading.value = true;
  pageFailure.value = '';
  try {
    page.value = await reportsApi.section(detail.value.id, activeSection.value, pageOffset.value, PAGE_SIZE);
  } catch (error) {
    pageFailure.value = errorText(error);
    page.value = null;
  } finally {
    pageLoading.value = false;
  }
}

/** nextPage moves one page forward when there is one. */
async function nextPage(): Promise<void> {
  if (!page.value) {
    return;
  }
  if (pageOffset.value + PAGE_SIZE >= page.value.total) {
    return;
  }
  pageOffset.value += PAGE_SIZE;
  await loadPage();
}

/** previousPage moves one page back. */
async function previousPage(): Promise<void> {
  if (pageOffset.value === 0) {
    return;
  }
  pageOffset.value = Math.max(0, pageOffset.value - PAGE_SIZE);
  await loadPage();
}

/** download saves the ZIP of a report through the browser. */
function download(run: DirectoryReport): void {
  window.open(reportsApi.exportHref(run.id), '_blank');
}

/** cancelReport asks the server to stop a running report. */
async function cancelReport(run: DirectoryReport): Promise<void> {
  try {
    await reportsApi.cancel(run.id);
    await load();
  } catch (error) {
    failure.value = errorText(error);
  }
}

onMounted(async () => {
  await loadAccounts();
  await loadSections();
  await loadSchedule();
  await load();
});

onBeforeUnmount(clearTimer);
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('reports.title')" :subtitle="t('reports.subtitle')" />

    <Alert v-if="failure" tone="destructive" :title="t('reports.loadError')" :message="failure">
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <Card class="space-y-4 p-5">
      <div class="grid gap-4 sm:grid-cols-2">
        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('reports.account') }}</p>
          <Select
            id="report-account"
            :model-value="accountId"
            :options="accountOptions"
            @update:model-value="applyAccount"
          />
        </div>

        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('reports.sections') }}</p>
          <div v-if="supported" class="flex flex-wrap gap-2">
            <label
              v-for="section in availableSections"
              :key="section"
              class="flex items-center gap-2 rounded-md border border-border px-2 py-1 text-sm"
            >
              <input
                type="checkbox"
                class="accent-primary"
                :checked="selectedSections.includes(section)"
                @change="toggleSection(section, ($event.target as HTMLInputElement).checked)"
              />
              {{ sectionLabel(section) }}
            </label>
          </div>
          <p v-else class="text-sm text-muted-foreground">{{ t('reports.unsupported') }}</p>
          <p class="text-xs text-muted-foreground">{{ t('reports.sectionsHint') }}</p>
        </div>
      </div>

      <div class="flex justify-end">
        <Button :disabled="!canStart" :loading="starting" @click="start">
          <Play v-if="!starting" class="h-4 w-4" aria-hidden="true" />
          {{ starting ? t('reports.starting') : t('reports.start') }}
        </Button>
      </div>
    </Card>
    <Card class="space-y-4 p-5">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="space-y-1">
          <p class="text-sm font-medium">{{ t('reports.schedule') }}</p>
          <p class="text-xs text-muted-foreground">{{ t('reports.scheduleHint') }}</p>
        </div>
        <div class="flex items-center gap-2">
          <Switch
            id="report-schedule-enabled"
            v-model="scheduleEnabled"
            :label="t('reports.scheduleEnabled')"
            :disabled="accountId === 0 || !supported"
          />
          <span class="text-sm">
            {{ scheduleEnabled ? t('common.enabled') : t('common.disabled') }}
          </span>
        </div>
      </div>

      <Alert v-if="scheduleFailure" tone="destructive" :message="scheduleFailure" />
      <Alert v-if="schedule?.last_error" tone="warning" :message="schedule.last_error" />

      <div class="grid gap-4 sm:grid-cols-3">
        <div class="space-y-2">
          <p class="text-sm font-medium">{{ t('reports.scheduleInterval') }}</p>
          <Select
            id="report-schedule-interval"
            v-model="scheduleInterval"
            :options="intervalOptions"
            :disabled="!scheduleEnabled"
          />
        </div>
        <div class="space-y-1 text-sm">
          <p class="text-muted-foreground">{{ t('reports.scheduleNext') }}</p>
          <p class="numeric">
            {{ schedule?.next_run_at ? when(schedule.next_run_at) : t('common.never') }}
          </p>
        </div>
        <div class="space-y-1 text-sm">
          <p class="text-muted-foreground">{{ t('reports.scheduleLast') }}</p>
          <p class="numeric">
            {{ schedule?.last_run_at ? when(schedule.last_run_at) : t('common.never') }}
          </p>
        </div>
      </div>

      <div class="flex justify-end gap-2">
        <Button
          v-if="schedule"
          variant="outline"
          :loading="scheduleSaving"
          @click="deleteSchedule"
        >
          {{ t('reports.scheduleDelete') }}
        </Button>
        <Button
          :disabled="accountId === 0 || !supported"
          :loading="scheduleSaving"
          @click="saveSchedule"
        >
          {{ t('common.save') }}
        </Button>
      </div>
    </Card>

    <Alert v-if="running" tone="info" :message="t('reports.runningNow')" />

    <PageHeader :title="t('reports.history')" />

    <div v-if="loading && rows.length === 0" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <Card v-else-if="rows.length === 0" content-class="p-0">
      <EmptyState :icon="BarChart3" :title="t('reports.empty')" :message="t('reports.emptyHint')" />
    </Card>
    <Card v-else content-class="p-0">
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('reports.account') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('reports.status') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('reports.trigger') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('reports.sections') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('reports.rows') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('reports.startedAt') }}</th>
              <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-border">
            <tr v-for="run in rows" :key="run.id" class="hover:bg-muted/40">
              <td class="break-anywhere px-3 py-2">
                <p>{{ run.account_email }}</p>
                <p v-if="run.org_id" class="text-xs text-muted-foreground">{{ run.org_id }}</p>
              </td>
              <td class="px-3 py-2">
                <span class="flex items-center gap-2">
                  <StatusBadge :status="run.status" />
                  <span v-if="run.failed > 0" class="text-xs text-warning">
                    {{ t('reports.failedSections', { count: run.failed }) }}
                  </span>
                </span>
              </td>
              <td class="px-3 py-2 text-muted-foreground">{{ t(`reports.trigger_${run.trigger}`) }}</td>
              <td class="px-3 py-2 text-end">
                <span class="numeric">{{ count(run.section_count) }}</span>
              </td>
              <td class="px-3 py-2 text-end"><span class="numeric">{{ count(run.row_count) }}</span></td>
              <td class="px-3 py-2 text-muted-foreground">
                <span class="numeric">{{ when(run.started_at) }}</span>
              </td>
              <td class="px-3 py-2">
                <div class="flex items-center justify-end gap-1">
                  <Button variant="ghost" size="sm" @click="openReport(run)">
                    <Eye class="h-4 w-4" aria-hidden="true" />
                    {{ t('reports.open') }}
                  </Button>
                  <Button variant="ghost" size="sm" @click="download(run)">
                    <Download class="h-4 w-4" aria-hidden="true" />
                    {{ t('reports.download') }}
                  </Button>
                  <Button
                    v-if="run.status === 'running'"
                    variant="ghost"
                    size="sm"
                    @click="cancelReport(run)"
                  >
                    <XCircle class="h-4 w-4" aria-hidden="true" />
                    {{ t('common.cancel') }}
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
      :title="t('reports.reportTitle', { id: detail?.id ?? 0 })"
      :description="detail?.account_email ?? ''"
      size="xl"
      :close-label="t('common.closeDialog')"
    >
      <Alert
        v-if="detailFailure"
        tone="destructive"
        :title="t('reports.loadError')"
        :message="detailFailure"
      />

      <div v-else-if="detail" class="space-y-4">
        <dl class="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <div class="space-y-1 rounded-lg border border-border p-3">
            <dt class="text-xs text-muted-foreground">{{ t('reports.sections') }}</dt>
            <dd class="text-sm font-medium">
              <span class="numeric">{{ count(detail.section_count) }}</span>
            </dd>
          </div>
          <div class="space-y-1 rounded-lg border border-border p-3">
            <dt class="text-xs text-muted-foreground">{{ t('reports.rows') }}</dt>
            <dd class="text-sm font-medium">
              <span class="numeric">{{ count(detail.row_count) }}</span>
            </dd>
          </div>
          <div class="space-y-1 rounded-lg border border-border p-3">
            <dt class="text-xs text-muted-foreground">{{ t('reports.duration') }}</dt>
            <dd class="text-sm font-medium">
              <span class="numeric">{{ count(detail.duration_ms) }}</span> ms
            </dd>
          </div>
          <div class="space-y-1 rounded-lg border border-border p-3">
            <dt class="text-xs text-muted-foreground">{{ t('reports.truncated') }}</dt>
            <dd class="text-sm font-medium">
              {{ detail.truncated ? t('common.yes') : t('common.no') }}
            </dd>
          </div>
        </dl>

        <div class="flex flex-wrap gap-1 rounded-lg bg-muted p-1">
          <button
            v-for="entry in detailSections"
            :key="entry.section"
            type="button"
            :class="
              cn(
                'rounded-md px-3 py-1 text-sm font-medium transition-colors',
                activeSection === entry.section
                  ? 'bg-background text-foreground shadow-sm'
                  : 'text-muted-foreground hover:text-foreground',
              )
            "
            @click="selectSection(entry.section)"
          >
            {{ sectionLabel(entry.section) }}
            <span class="numeric ms-1 text-xs text-muted-foreground">
              {{ count(entry.row_count) }}
            </span>
          </button>
        </div>

        <Alert
          v-if="sectionSummary(activeSection)?.error"
          tone="warning"
          :title="t('reports.sectionFailed')"
          :message="sectionSummary(activeSection)?.error ?? ''"
        />
        <Alert
          v-if="sectionSummary(activeSection)?.truncated"
          tone="warning"
          :message="t('reports.truncatedHint')"
        />
        <div v-if="pageLoading" class="flex items-center justify-center gap-2 py-6">
          <Spinner :label="t('common.loading')" />
          <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
        </div>

        <Alert v-else-if="pageFailure" tone="destructive" :message="pageFailure" />

        <EmptyState
          v-else-if="!page || page.rows.length === 0"
          :icon="BarChart3"
          :title="t('reports.noRows')"
        />

        <template v-else>
          <div class="overflow-x-auto">
            <table class="w-full text-sm">
              <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
                <tr>
                  <th
                    v-for="column in page.columns"
                    :key="column.key"
                    scope="col"
                    class="whitespace-nowrap px-3 py-2 text-start font-medium"
                  >
                    {{ column.title }}
                  </th>
                </tr>
              </thead>
              <tbody class="divide-y divide-border">
                <tr
                  v-for="(row, index) in page.rows"
                  :key="`${activeSection}-${page.offset + index}`"
                >
                  <td v-for="(cell, column) in row" :key="column" class="break-anywhere px-3 py-2">
                    {{ cell }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <div class="flex flex-wrap items-center justify-between gap-2">
            <p class="text-xs text-muted-foreground">
              {{
                t('reports.pageInfo', {
                  from: page.offset + 1,
                  to: page.offset + page.rows.length,
                  total: page.total,
                })
              }}
            </p>
            <div class="flex gap-2">
              <Button
                variant="outline"
                size="sm"
                :disabled="page.offset === 0"
                @click="previousPage"
              >
                {{ t('common.previous') }}
              </Button>
              <Button
                variant="outline"
                size="sm"
                :disabled="page.offset + page.rows.length >= page.total"
                @click="nextPage"
              >
                {{ t('common.next') }}
              </Button>
            </div>
          </div>
        </template>
      </div>

      <template #footer>
        <Button variant="outline" @click="detailOpen = false">{{ t('common.close') }}</Button>
        <Button v-if="detail" @click="download(detail)">
          <Download class="h-4 w-4" aria-hidden="true" />
          {{ t('reports.download') }}
        </Button>
      </template>
    </Dialog>
  </div>
</template>
