<script setup lang="ts">
// SettingsView — the defaults and the maintenance tasks of one instance.
//
// The defaults are what a new job starts from, so saving them keeps the session
// in step: the next job form offers the schedule that was just set. The
// maintenance card drives the same tasks the scheduler runs on a timer, which is
// what makes a "why did nothing run?" question answerable from the interface.
import { RefreshCw, Save, Wrench } from '@lucide/vue';
import { computed, onMounted, reactive, ref } from 'vue';
import { useI18n } from 'vue-i18n';

import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import Input from '@/components/ui/Input.vue';
import Label from '@/components/ui/Label.vue';
import Select from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import { useAction } from '@/composables/useAction';
import { maintenance, messageOf, settings as settingsApi, type AppSettings } from '@/lib/api';
import { asNumber } from '@/lib/forms';
import { useAuthStore } from '@/stores/auth';
import { useFeedbackStore } from '@/stores/feedback';

/** DEFAULT_KEEP is how many runs of every job the trim button keeps. */
const DEFAULT_KEEP = 200;

const { t, te } = useI18n();
const auth = useAuthStore();
const feedback = useFeedbackStore();

const stored = ref<AppSettings | null>(null);
const locales = ref<string[]>([]);
const themes = ref<string[]>([]);
const raw = ref<Record<string, string>>({});
const loading = ref(true);
const failure = ref('');
const rawLoading = ref(false);
const rawFailure = ref('');

/** form holds the four defaults while they are being edited. */
const form = reactive({
  default_locale: '',
  default_theme: '',
  sync_default_interval_minutes: '' as string | number,
  sync_run_timeout_minutes: '' as string | number,
});

/** keep is how many runs the trim task leaves behind. */
const keep = ref<string | number>(DEFAULT_KEEP);

const { busy: saving, run: runSave } = useAction();
const { busy: scheduling, run: runScheduler } = useAction();
const { busy: refreshing, run: runTokens } = useAction();
const { busy: pruningStates, run: runPruneStates } = useAction();
const { busy: pruningRuns, run: runPruneRuns } = useAction();

/** localeOptions names every shipped language in its own language. */
const localeOptions = computed(() =>
  locales.value.map((code) => ({
    value: code,
    label: te(`language.${code}`) ? t(`language.${code}`) : code,
  })),
);

/** themeOptions names the themes this build ships. */
const themeOptions = computed(() =>
  themes.value.map((name) => ({
    value: name,
    label: te(`theme.${name}`) ? t(`theme.${name}`) : name,
  })),
);

/** rawRows is the stored key/value list, sorted once for display. */
const rawRows = computed(() =>
  Object.entries(raw.value)
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([key, value]) => ({ key, value })),
);

/** load reads the defaults together with their option sets. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const answer = await settingsApi.get();
    stored.value = answer.settings;
    locales.value = answer.locales;
    themes.value = answer.themes;
    form.default_locale = answer.settings.default_locale;
    form.default_theme = answer.settings.default_theme;
    form.sync_default_interval_minutes = answer.settings.sync_default_interval_minutes;
    form.sync_run_timeout_minutes = answer.settings.sync_run_timeout_minutes;
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

/** loadRaw reads every stored key, the diagnostic behind the form. */
async function loadRaw(): Promise<void> {
  rawLoading.value = true;
  rawFailure.value = '';
  try {
    const answer = await settingsApi.raw();
    raw.value = answer.values;
  } catch (error) {
    rawFailure.value = messageOf(error);
  } finally {
    rawLoading.value = false;
  }
}

/** save stores the defaults and refreshes the session they are also part of. */
async function save(): Promise<void> {
  const body: AppSettings = {
    default_locale: form.default_locale,
    default_theme: form.default_theme,
    sync_default_interval_minutes: asNumber(form.sync_default_interval_minutes, 0),
    sync_run_timeout_minutes: asNumber(form.sync_run_timeout_minutes, 0),
  };
  const answer = await runSave(() => settingsApi.update(body), {
    success: t('admin.savedSettings'),
    failure: t('admin.settingsError'),
  });
  if (answer) {
    stored.value = answer.settings;
    // The defaults also live in the session, so reloading it keeps the theme and
    // the default schedule offered by the job editor in step with the save.
    await auth.load();
    await loadRaw();
  }
}

/** scheduleNow runs the scheduler once and reports how many jobs it started. */
async function scheduleNow(): Promise<void> {
  const answer = await runScheduler(() => maintenance.runScheduler(), {
    failure: t('admin.maintenanceError'),
  });
  if (answer) {
    feedback.success(t('admin.schedulerStarted', { count: answer.started.length }));
  }
}

/** refreshTokens renews the OAuth tokens that are due. */
async function refreshTokens(): Promise<void> {
  const answer = await runTokens(() => maintenance.refreshTokens(), {
    failure: t('admin.maintenanceError'),
  });
  if (answer) {
    feedback.success(t('admin.tokensRefreshed', { count: answer.refreshed }));
  }
}

/** pruneStates deletes the authorisation states that expired unfinished. */
async function pruneStates(): Promise<void> {
  const answer = await runPruneStates(() => maintenance.pruneStates(), {
    failure: t('admin.maintenanceError'),
  });
  if (answer) {
    feedback.success(t('admin.statesRemoved', { count: answer.removed }));
  }
}

/** pruneRuns trims the run history down to the number of runs to keep. */
async function pruneRuns(): Promise<void> {
  const answer = await runPruneRuns(
    () => maintenance.pruneRuns(asNumber(keep.value, DEFAULT_KEEP)),
    { success: t('admin.runsPruned'), failure: t('admin.maintenanceError') },
  );
  if (answer) {
    keep.value = answer.keep;
  }
}

onMounted(() => {
  void load();
  void loadRaw();
});
</script>

<template>
  <div class="space-y-6">
    <Alert
      v-if="failure"
      tone="destructive"
      :title="t('admin.settingsLoadError')"
      :message="failure"
    >
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <div v-if="loading && !stored" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <Card v-else-if="stored" :title="t('admin.settings')" :description="t('admin.settingsSubtitle')">
      <div class="grid gap-4 sm:grid-cols-2">
        <div class="space-y-1.5">
          <Label for="setting-locale">{{ t('admin.defaultLocale') }}</Label>
          <Select id="setting-locale" v-model="form.default_locale" :options="localeOptions" />
        </div>
        <div class="space-y-1.5">
          <Label for="setting-theme">{{ t('admin.defaultTheme') }}</Label>
          <Select id="setting-theme" v-model="form.default_theme" :options="themeOptions" />
        </div>
        <div class="space-y-1.5">
          <Label for="setting-interval" :hint="t('admin.defaultIntervalHint')" required>
            {{ t('admin.defaultInterval') }}
          </Label>
          <Input
            id="setting-interval"
            v-model="form.sync_default_interval_minutes"
            type="number"
            numeric
            :min="0"
          />
        </div>
        <div class="space-y-1.5">
          <Label for="setting-timeout" :hint="t('admin.runTimeoutHint')" required>
            {{ t('admin.runTimeout') }}
          </Label>
          <Input
            id="setting-timeout"
            v-model="form.sync_run_timeout_minutes"
            type="number"
            numeric
            :min="1"
          />
        </div>
      </div>
      <template #footer>
        <Button :loading="saving" @click="save">
          <Save class="h-4 w-4" aria-hidden="true" />
          {{ saving ? t('common.saving') : t('common.save') }}
        </Button>
      </template>
    </Card>

    <Card :title="t('admin.rawSettings')" :description="t('admin.rawHint')">
      <template #actions>
        <Button variant="outline" size="sm" :loading="rawLoading" @click="loadRaw">
          <RefreshCw class="h-4 w-4" aria-hidden="true" />
          {{ t('common.refresh') }}
        </Button>
      </template>
      <Alert
        v-if="rawFailure"
        tone="destructive"
        :title="t('admin.settingsLoadError')"
        :message="rawFailure"
      />
      <div
        v-else-if="rawLoading && rawRows.length === 0"
        class="flex items-center justify-center gap-2 py-6"
      >
        <Spinner :label="t('common.loading')" />
        <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
      </div>
      <EmptyState v-else-if="rawRows.length === 0" :title="t('common.noResults')" />
      <div v-else class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead class="bg-muted/50 text-xs uppercase tracking-wide text-muted-foreground">
            <tr>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('admin.key') }}</th>
              <th scope="col" class="px-3 py-2 text-start font-medium">{{ t('admin.value') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-border">
            <tr v-for="row in rawRows" :key="row.key">
              <td class="break-anywhere px-3 py-2 font-mono text-xs">{{ row.key }}</td>
              <td class="break-anywhere px-3 py-2 font-mono text-xs text-muted-foreground">
                {{ row.value }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Card :description="t('admin.maintenanceSubtitle')">
      <template #title>
        <span class="flex items-center gap-2">
          <Wrench class="h-4 w-4 text-muted-foreground" aria-hidden="true" />
          {{ t('admin.maintenance') }}
        </span>
      </template>
      <div class="space-y-4">
        <div class="flex flex-wrap items-center gap-2">
          <Button variant="outline" :loading="scheduling" @click="scheduleNow">
            {{ t('admin.runScheduler') }}
          </Button>
          <Button variant="outline" :loading="refreshing" @click="refreshTokens">
            {{ t('admin.refreshTokens') }}
          </Button>
          <Button variant="outline" :loading="pruningStates" @click="pruneStates">
            {{ t('admin.pruneStates') }}
          </Button>
        </div>
        <div class="flex flex-wrap items-end gap-2">
          <div class="space-y-1.5">
            <Label for="setting-keep">{{ t('admin.keep') }}</Label>
            <Input id="setting-keep" v-model="keep" type="number" numeric :min="1" class="w-40" />
          </div>
          <Button variant="outline" :loading="pruningRuns" @click="pruneRuns">
            {{ t('admin.pruneRuns') }}
          </Button>
        </div>
      </div>
    </Card>
  </div>
</template>
