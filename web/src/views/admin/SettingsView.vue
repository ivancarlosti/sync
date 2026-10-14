<script setup lang="ts">
// SettingsView — the defaults and the maintenance tasks of one instance.
//
// The defaults are what a new job starts from, so saving them keeps the session
// in step: the next job form offers the schedule that was just set. The
// maintenance card drives the same tasks the scheduler runs on a timer, which is
// what makes a "why did nothing run?" question answerable from the interface.
import { RefreshCw, Save, Trash2, Wrench } from '@lucide/vue';
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
import Switch from '@/components/ui/Switch.vue';
import Textarea from '@/components/ui/Textarea.vue';
import { useAction } from '@/composables/useAction';
import {
  maintenance,
  messageOf,
  settings as settingsApi,
  type AppSettings,
  type BillingSchema,
  type PurgeTarget,
} from '@/lib/api';
import { asNumber } from '@/lib/forms';
import { useAuthStore } from '@/stores/auth';
import { useFeedbackStore } from '@/stores/feedback';

/** DEFAULT_KEEP is how many runs of every job the trim button keeps. */
const DEFAULT_KEEP = 200;

/** DEFAULT_RETENTION mirrors the server default of an emptied retention field. */
const DEFAULT_RETENTION = 180;

const { t, te } = useI18n();
const auth = useAuthStore();
const feedback = useFeedbackStore();

const stored = ref<AppSettings | null>(null);
const locales = ref<string[]>([]);
const themes = ref<string[]>([]);
const schemas = ref<BillingSchema[]>([]);
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

/**
 * billingForm is the billing vocabulary: which schema labels the report, the
 * custom document behind the `custom` schema, and the price of one GiB in each
 * direction (0 leaves that direction unpriced and hides its cost column).
 */
const billingForm = reactive({
  schema: 'none',
  custom: '',
  rateIn: '' as string | number,
  rateOut: '' as string | number,
  currency: 'USD',
});

/**
 * purgeForm is the retention policy: whether the scheduler erases aged records on
 * its own, the age of each kind of record, and the target of a manual pass. The
 * manual age is applied once and saves nothing; zero applies the policy below.
 */
const purgeForm = reactive({
  enabled: false,
  jobsDays: '' as string | number,
  auditsDays: '' as string | number,
  billingDays: '' as string | number,
  target: 'all' as PurgeTarget,
  days: '' as string | number,
});

/** keep is how many runs the trim task leaves behind. */
const keep = ref<string | number>(DEFAULT_KEEP);

const { busy: saving, run: runSave } = useAction();
const { busy: scheduling, run: runScheduler } = useAction();
const { busy: refreshing, run: runTokens } = useAction();
const { busy: pruningStates, run: runPruneStates } = useAction();
const { busy: pruningRuns, run: runPruneRuns } = useAction();
const { busy: purging, run: runPurge } = useAction();

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

/**
 * schemaOptions names the billing schemas. The presets come from the server (so
 * the API and the SPA can never disagree about them); the label of each is
 * translated when the catalog has one.
 */
const schemaOptions = computed(() =>
  schemas.value.map((schema) => ({
    value: schema.id,
    label: te(`admin.billingSchema_${schema.id}`)
      ? t(`admin.billingSchema_${schema.id}`)
      : schema.label,
  })),
);

/** activeSchema is the description of the selected billing vocabulary. */
const activeSchema = computed(() =>
  schemas.value.find((schema) => schema.id === billingForm.schema),
);

/** hasSchemaDefaults tells whether the schema ships indicative prices to apply. */
const hasSchemaDefaults = computed(
  () =>
    (activeSchema.value?.default_rate_in_per_gib ?? 0) > 0 ||
    (activeSchema.value?.default_rate_out_per_gib ?? 0) > 0,
);

/** purgeOptions names the targets a manual pass can erase. */
const purgeOptions = computed(() => [
  { value: 'all', label: t('admin.purgeTargetAll') },
  { value: 'jobs', label: t('admin.purgeTargetJobs') },
  { value: 'audits', label: t('admin.purgeTargetAudits') },
  { value: 'billing', label: t('admin.purgeTargetBilling') },
]);

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
    schemas.value = answer.billing_schemas;
    form.default_locale = answer.settings.default_locale;
    form.default_theme = answer.settings.default_theme;
    form.sync_default_interval_minutes = answer.settings.sync_default_interval_minutes;
    form.sync_run_timeout_minutes = answer.settings.sync_run_timeout_minutes;
    billingForm.schema = answer.settings.billing_schema;
    billingForm.custom = answer.settings.billing_schema_custom || answer.billing_schema_default;
    billingForm.rateIn = answer.settings.billing_rate_in_per_gib;
    billingForm.rateOut = answer.settings.billing_rate_out_per_gib;
    billingForm.currency = answer.settings.billing_currency;
    purgeForm.enabled = answer.settings.purge_enabled;
    purgeForm.jobsDays = answer.settings.purge_jobs_days;
    purgeForm.auditsDays = answer.settings.purge_audits_days;
    purgeForm.billingDays = answer.settings.purge_billing_days;
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
    billing_schema: billingForm.schema,
    billing_schema_custom: billingForm.custom,
    billing_rate_in_per_gib: asNumber(billingForm.rateIn, 0),
    billing_rate_out_per_gib: asNumber(billingForm.rateOut, 0),
    billing_currency: billingForm.currency,
    purge_enabled: purgeForm.enabled,
    purge_jobs_days: asNumber(purgeForm.jobsDays, DEFAULT_RETENTION),
    purge_audits_days: asNumber(purgeForm.auditsDays, DEFAULT_RETENTION),
    purge_billing_days: asNumber(purgeForm.billingDays, DEFAULT_RETENTION),
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

/**
 * applySchemaRates fills the two prices with the indicative list prices of the
 * selected schema. They are never inherited automatically: the report only gets
 * priced when an operator applies a price list on purpose.
 */
function applySchemaRates(): void {
  const schema = activeSchema.value;
  if (!schema) {
    return;
  }
  billingForm.rateIn = schema.default_rate_in_per_gib;
  billingForm.rateOut = schema.default_rate_out_per_gib;
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

/**
 * purgeNow applies the retention right away, to one target or to all three: the
 * manual counterpart of the loop the scheduler runs. An explicit age is used as
 * typed; zero applies the policy saved above, which is what the button sends by
 * default.
 */
async function purgeNow(): Promise<void> {
  const answer = await runPurge(
    () => maintenance.purge(purgeForm.target, asNumber(purgeForm.days, 0)),
    { failure: t('admin.purgeError') },
  );
  if (answer) {
    feedback.success(
      t('admin.purged', {
        jobs: answer.removed.jobs,
        audits: answer.removed.audits,
        traffic: answer.removed.billing,
      }),
    );
    await loadRaw();
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

    <Card :title="t('admin.billing')" :description="t('admin.billingSubtitle')">
      <div class="space-y-4">
        <div class="space-y-1.5">
          <Label for="billing-schema">{{ t('admin.billingSchema') }}</Label>
          <Select
            id="billing-schema"
            :model-value="billingForm.schema"
            :options="schemaOptions"
            :aria-label="t('admin.billingSchema')"
            @update:model-value="(value) => { billingForm.schema = String(value); }"
          />
          <p v-if="activeSchema?.hint" class="text-sm text-muted-foreground">
            {{ activeSchema.hint }}
          </p>
        </div>

        <div v-if="billingForm.schema === 'custom'" class="space-y-1.5">
          <Label for="billing-schema-custom">{{ t('admin.billingSchemaCustom') }}</Label>
          <Textarea
            id="billing-schema-custom"
            v-model="billingForm.custom"
            :rows="8"
            :spellcheck="false"
            class="font-mono text-xs"
            :aria-label="t('admin.billingSchemaCustom')"
          />
          <p class="text-sm text-muted-foreground">{{ t('admin.billingSchemaCustomHint') }}</p>
        </div>

        <div class="grid gap-3 sm:grid-cols-3">
          <div class="space-y-1.5">
            <Label for="billing-rate-in">{{ t('admin.billingRateIn') }}</Label>
            <Input
              id="billing-rate-in"
              v-model="billingForm.rateIn"
              type="number"
              step="0.0001"
              min="0"
            />
          </div>
          <div class="space-y-1.5">
            <Label for="billing-rate-out">{{ t('admin.billingRateOut') }}</Label>
            <Input
              id="billing-rate-out"
              v-model="billingForm.rateOut"
              type="number"
              step="0.0001"
              min="0"
            />
          </div>
          <div class="space-y-1.5">
            <Label for="billing-currency">{{ t('admin.billingCurrency') }}</Label>
            <Input id="billing-currency" v-model="billingForm.currency" maxlength="3" />
          </div>
        </div>
        <p class="text-sm text-muted-foreground">{{ t('admin.billingRateHint') }}</p>
        <div v-if="hasSchemaDefaults">
          <Button variant="outline" size="sm" @click="applySchemaRates">
            {{ t('admin.billingRateDefaults') }}
          </Button>
        </div>
      </div>
      <template #footer>
        <Button :loading="saving" @click="save">
          <Save class="h-4 w-4" aria-hidden="true" />
          {{ saving ? t('common.saving') : t('common.save') }}
        </Button>
      </template>
    </Card>

    <Card :title="t('admin.retention')" :description="t('admin.retentionSubtitle')">
      <div class="space-y-4">
        <div class="flex items-start justify-between gap-4">
          <div class="space-y-1">
            <Label for="purge-enabled">{{ t('admin.purgeEnabled') }}</Label>
            <p class="text-xs text-muted-foreground">{{ t('admin.purgeEnabledHint') }}</p>
          </div>
          <Switch
            id="purge-enabled"
            v-model="purgeForm.enabled"
            :label="t('admin.purgeEnabled')"
          />
        </div>

        <div class="grid gap-3 sm:grid-cols-3">
          <div class="space-y-1.5">
            <Label for="purge-jobs">{{ t('admin.purgeJobsDays') }}</Label>
            <Input id="purge-jobs" v-model="purgeForm.jobsDays" type="number" numeric :min="1" />
          </div>
          <div class="space-y-1.5">
            <Label for="purge-audits">{{ t('admin.purgeAuditsDays') }}</Label>
            <Input
              id="purge-audits"
              v-model="purgeForm.auditsDays"
              type="number"
              numeric
              :min="1"
            />
          </div>
          <div class="space-y-1.5">
            <Label for="purge-billing">{{ t('admin.purgeBillingDays') }}</Label>
            <Input
              id="purge-billing"
              v-model="purgeForm.billingDays"
              type="number"
              numeric
              :min="1"
            />
          </div>
        </div>
        <p class="text-sm text-muted-foreground">{{ t('admin.purgeDaysHint') }}</p>

        <div class="flex flex-wrap items-end gap-2 border-t border-border pt-4">
          <div class="space-y-1.5">
            <Label for="purge-target">{{ t('admin.purgeTarget') }}</Label>
            <Select
              id="purge-target"
              :model-value="purgeForm.target"
              :options="purgeOptions"
              :aria-label="t('admin.purgeTarget')"
              @update:model-value="(value) => { purgeForm.target = value as PurgeTarget; }"
            />
          </div>
          <div class="space-y-1.5">
            <Label for="purge-days" :hint="t('admin.purgeAgeHint')">{{ t('admin.purgeAge') }}</Label>
            <Input id="purge-days" v-model="purgeForm.days" type="number" numeric :min="0" class="w-40" />
          </div>
          <Button variant="outline" :loading="purging" @click="purgeNow">
            <Trash2 class="h-4 w-4" aria-hidden="true" />
            {{ t('admin.purgeNow') }}
          </Button>
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
