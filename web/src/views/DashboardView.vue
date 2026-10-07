<script setup lang="ts">
// DashboardView — the state of the instance at a glance.
//
// Two questions are answered here: "what happened?" (the aggregates of the window
// the operator picked) and "is this instance healthy?" (the database, the build
// and the way operators sign in). Everything else is a link away: the dashboard
// is a summary, not a control panel.
import {
  Activity,
  Ban,
  BadgeCheck,
  CircleCheck,
  CircleDot,
  CircleX,
  Clock,
  FileMinus,
  FilePen,
  FilePlus,
  FolderSync,
  HardDrive,
  RefreshCw,
} from '@lucide/vue';
import { computed, onMounted, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRouter } from 'vue-router';

import PageHeader from '@/components/PageHeader.vue';
import StatCard from '@/components/StatCard.vue';
import StatusBadge from '@/components/StatusBadge.vue';
import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Select from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import { dashboard, messageOf, runs, system, type Stats, type SyncRun } from '@/lib/api';
import { formatBytes, formatDateTime, formatDuration, formatNumber } from '@/lib/format';
import { useAuthStore } from '@/stores/auth';

/** WINDOW_DAYS is the closed set the API accepts; `0` means "since ever". */
const WINDOW_DAYS = [1, 7, 30, 0] as const;

/** WINDOW_LABELS names each window in the catalog. */
const WINDOW_LABELS: Record<number, string> = {
  1: 'dashboard.last24h',
  7: 'dashboard.last7d',
  30: 'dashboard.last30d',
  0: 'dashboard.allTime',
};

/** RECENT_LIMIT is how many runs the summary lists. */
const RECENT_LIMIT = 5;

const auth = useAuthStore();
const router = useRouter();
const { t, locale } = useI18n();

const days = ref(30);
const stats = ref<Stats | null>(null);
const runningJobs = ref(0);
const recent = ref<SyncRun[]>([]);
const health = ref<{ status: string; database: string } | null>(null);
const loading = ref(true);
const failure = ref('');

const windowOptions = computed(() =>
  WINDOW_DAYS.map((value) => ({ value, label: t(WINDOW_LABELS[value]) })),
);
const windowLabel = computed(() => t(WINDOW_LABELS[days.value]));
/** windowModel keeps the `<select>` bound to a number, never to a string. */
const windowModel = computed({
  get: () => days.value,
  set: (value: string | number) => {
    days.value = Number(value);
  },
});
const subtitle = computed(() =>
  days.value > 0 ? t('dashboard.subtitle', { days: days.value }) : t('dashboard.allTime'),
);
const version = computed(() => auth.session?.version);
/** mode names the authentication of this instance for the health card. */
const mode = computed(() => auth.session?.mode ?? 'account');

/** count renders a counter with the grouping of the active locale. */
function count(value: number | undefined): string {
  return formatNumber(value ?? 0, locale.value);
}

/** size renders a byte total with the units of the active locale. */
function size(value: number | undefined): string {
  return formatBytes(value ?? 0, locale.value);
}

/** when renders a timestamp, or an em dash when the API omitted it. */
function when(value: string | null | undefined): string {
  return formatDateTime(value, locale.value);
}

/** duration renders the run duration in the short form of the catalog. */
function duration(milliseconds: number): string {
  return formatDuration(milliseconds, t);
}

/**
 * load reads the two answers the counters and the summary are built from. They
 * are requested together so a slow aggregate never leaves the recent runs half
 * rendered; the health probe has its own request (see `loadHealth`).
 */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const [aggregate, log] = await Promise.all([
      dashboard.stats(days.value),
      runs.list(RECENT_LIMIT),
    ]);
    stats.value = aggregate.stats;
    runningJobs.value = aggregate.running_jobs;
    recent.value = log.runs;
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/**
 * loadHealth probes the instance on its own so a degraded database (503) shows
 * up as a red row instead of hiding the counters that did load.
 */
async function loadHealth(): Promise<void> {
  try {
    const probe = await system.health();
    health.value = { status: probe.status, database: probe.database };
  } catch {
    health.value = { status: 'degraded', database: 'error' };
  }
}

onMounted(async () => {
  await Promise.all([load(), loadHealth()]);
});
watch(days, load);
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('dashboard.title')" :subtitle="subtitle">
      <template #actions>
        <div class="flex items-end gap-2">
          <div class="space-y-1">
            <p class="text-xs font-medium text-muted-foreground">{{ t('dashboard.window') }}</p>
            <Select
              id="dashboard-window"
              v-model="windowModel"
              :options="windowOptions"
              class="w-40"
            />
          </div>
          <Button variant="outline" :loading="loading" @click="load">
            <RefreshCw class="h-4 w-4" aria-hidden="true" />
            {{ t('common.refresh') }}
          </Button>
        </div>
      </template>
    </PageHeader>

    <Alert v-if="failure" tone="destructive" :title="t('dashboard.loadError')" :message="failure">
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <div v-if="loading && !stats" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <template v-else-if="stats">
      <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard
          :label="t('dashboard.runningJobs')"
          :value="count(runningJobs)"
          :hint="windowLabel"
          :icon="Clock"
          :tone="runningJobs > 0 ? 'default' : 'muted'"
        />
        <StatCard
          :label="t('dashboard.accounts')"
          :value="count(stats.accounts)"
          :icon="CircleCheck"
        />
        <StatCard
          :label="t('dashboard.jobs')"
          :value="count(stats.jobs)"
          :hint="`${t('dashboard.enabledJobs')}: ${count(stats.enabled_jobs)}`"
          :icon="FolderSync"
        />
        <StatCard
          :label="t('dashboard.runs')"
          :value="count(stats.runs)"
          :hint="windowLabel"
          :icon="Activity"
        />

        <StatCard
          :label="t('dashboard.succeeded')"
          :value="count(stats.succeeded)"
          :icon="BadgeCheck"
          tone="success"
        />
        <StatCard
          :label="t('dashboard.partial')"
          :value="count(stats.partial)"
          :icon="CircleDot"
          tone="warning"
        />
        <StatCard
          :label="t('dashboard.failed')"
          :value="count(stats.failed)"
          :icon="CircleX"
          tone="destructive"
        />
        <StatCard
          :label="t('dashboard.cancelled')"
          :value="count(stats.cancelled)"
          :icon="Ban"
          tone="muted"
        />
        <StatCard
          :label="t('dashboard.timedOut')"
          :value="count(stats.timed_out)"
          :icon="Clock"
          tone="warning"
        />

        <StatCard
          :label="t('dashboard.filesCreated')"
          :value="count(stats.files_created)"
          :icon="FilePlus"
        />
        <StatCard
          :label="t('dashboard.filesUpdated')"
          :value="count(stats.files_updated)"
          :icon="FilePen"
        />
        <StatCard
          :label="t('dashboard.filesDeleted')"
          :value="count(stats.files_deleted)"
          :icon="FileMinus"
          tone="muted"
        />
        <StatCard
          :label="t('dashboard.bytesTransferred')"
          :value="size(stats.bytes_transferred)"
          :icon="HardDrive"
        />
      </div>

      <div class="grid gap-4 lg:grid-cols-3">
        <Card class="lg:col-span-2" :title="t('dashboard.recentRuns')" :description="windowLabel">
          <template #actions>
            <RouterLink
              :to="{ name: 'runs' }"
              class="text-sm font-medium text-primary underline-offset-4 hover:underline"
            >
              {{ t('dashboard.viewAllRuns') }}
            </RouterLink>
          </template>

          <EmptyState
            v-if="recent.length === 0"
            :icon="Activity"
            :title="t('dashboard.noRuns')"
            :message="t('dashboard.noRunsHint')"
          >
            <Button variant="outline" size="sm" @click="router.push({ name: 'jobs' })">
              {{ t('dashboard.manageJobs') }}
            </Button>
          </EmptyState>

          <div v-else class="overflow-x-auto rounded-lg border border-border">
            <table class="w-full text-sm">
              <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
                <tr>
                  <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.job') }}</th>
                  <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.status') }}</th>
                  <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.trigger') }}</th>
                  <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('runs.startedAt') }}</th>
                  <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('runs.duration') }}</th>
                  <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('runs.created') }}</th>
                  <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('runs.updated') }}</th>
                  <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('runs.deleted') }}</th>
                  <th scope="col" class="px-3 py-2 text-end font-medium">{{ t('runs.errors') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-border">
                <tr v-for="run in recent" :key="run.id" class="hover:bg-muted/40">
                  <td class="px-3 py-2">
                    <RouterLink
                      :to="{ name: 'runs', query: { run: run.id } }"
                      class="font-medium text-primary underline-offset-4 hover:underline"
                    >
                      {{ run.job_name }}
                    </RouterLink>
                  </td>
                  <td class="px-3 py-2"><StatusBadge :status="run.status" /></td>
                  <td class="px-3 py-2"><StatusBadge :status="run.trigger" /></td>
                  <td class="px-3 py-2 text-muted-foreground">{{ when(run.started_at) }}</td>
                  <td class="px-3 py-2 text-end tabular-nums text-muted-foreground">
                    {{ duration(run.duration_ms) }}
                  </td>
                  <td class="px-3 py-2 text-end tabular-nums">{{ count(run.files_created) }}</td>
                  <td class="px-3 py-2 text-end tabular-nums">{{ count(run.files_updated) }}</td>
                  <td class="px-3 py-2 text-end tabular-nums">{{ count(run.files_deleted) }}</td>
                  <td
                    class="px-3 py-2 text-end tabular-nums"
                    :class="run.errors > 0 && 'font-medium text-destructive'"
                  >
                    {{ count(run.errors) }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </Card>

        <Card :title="t('dashboard.instanceHealth')">
          <dl class="space-y-3 text-sm">
            <div class="flex items-center justify-between gap-3">
              <dt class="text-muted-foreground">{{ t('status.ok') }}</dt>
              <dd><StatusBadge v-if="health" :status="health.status" /></dd>
            </div>
            <div class="flex items-center justify-between gap-3">
              <dt class="text-muted-foreground">{{ t('dashboard.database') }}</dt>
              <dd><StatusBadge v-if="health" :status="health.database" /></dd>
            </div>
            <div class="flex items-center justify-between gap-3">
              <dt class="text-muted-foreground">{{ t('dashboard.version') }}</dt>
              <dd class="numeric font-medium">{{ version?.version ?? t('common.unknown') }}</dd>
            </div>
            <div class="flex items-center justify-between gap-3">
              <dt class="text-muted-foreground">{{ t('dashboard.authMode') }}</dt>
              <dd><StatusBadge :status="mode === 'none' ? 'anonymous' : mode" /></dd>
            </div>
          </dl>

          <Alert
            v-if="health && health.status !== 'ok'"
            tone="destructive"
            :title="t('status.degraded')"
            :message="t('admin.healthDegraded', { detail: health.database })"
            class="mt-4"
          />
        </Card>
      </div>
    </template>
  </div>
</template>
