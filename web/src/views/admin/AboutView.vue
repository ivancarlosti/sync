<script setup lang="ts">
// AboutView — what this build is, and whether it is healthy.
//
// The instance answers two different questions here: "which binary am I talking
// to" (version, commit, build date, auth mode) and "is it working right now"
// (database connectivity, running jobs). The health card calls the same endpoint
// the container health check uses, so a broken deployment is visible here.
import { RefreshCw } from '@lucide/vue';
import { computed, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';

import StatusBadge from '@/components/StatusBadge.vue';
import Alert from '@/components/ui/Alert.vue';
import Badge from '@/components/ui/Badge.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import Spinner from '@/components/ui/Spinner.vue';
import { messageOf, system, type BuildInfo } from '@/lib/api';
import { formatDateTime, formatNumber } from '@/lib/format';
import { providerLabel } from '@/lib/providers';
import { useAuthStore } from '@/stores/auth';

/** AUTH_MODES maps the wire value of `auth_mode` to its label. */
const AUTH_MODES: Record<string, string> = {
  none: 'admin.authNone',
  account: 'admin.authAccount',
  keycloak: 'admin.authKeycloak',
};

const { t, te, locale } = useI18n();
const auth = useAuthStore();

const info = ref<BuildInfo | null>(null);
const mode = ref('');
const providers = ref<string[]>([]);
const locales = ref<string[]>([]);
const healthStatus = ref('');
const healthDetail = ref('');
const runningJobs = ref(0);
const loading = ref(true);
const failure = ref('');

/** version is the build the server answered with, or the one the session knows. */
const version = computed(() => info.value ?? auth.version);

/** buildDate renders the build timestamp, or a dash for a source build. */
const buildDate = computed(() =>
  version.value?.build_date
    ? formatDateTime(version.value.build_date, locale.value)
    : t('common.notAvailable'),
);

/** authModeLabel names the authentication mode of this instance. */
const authModeLabel = computed(() => {
  const key = AUTH_MODES[mode.value];
  return key && te(key) ? t(key) : mode.value;
});

/** providerItems names the providers this binary was compiled with. */
const providerItems = computed(() => providers.value.map((name) => providerLabel(name)));

/** localeItems names the shipped languages in their own language. */
const localeItems = computed(() =>
  locales.value.map((code) => (te(`language.${code}`) ? t(`language.${code}`) : code)),
);

/** healthText is the sentence under the health badge. */
const healthText = computed(() => {
  if (healthStatus.value === 'ok') {
    return t('admin.healthOk');
  }
  if (healthStatus.value === '') {
    return t('common.notAvailable');
  }
  return t('admin.healthDegraded', { detail: healthDetail.value });
});

/** load reads the build metadata and the health of the instance together. */
async function load(): Promise<void> {
  loading.value = true;
  failure.value = '';
  try {
    const [build, health] = await Promise.all([system.version(), system.health()]);
    info.value = build.info;
    mode.value = build.auth_mode;
    providers.value = build.providers ?? [];
    locales.value = build.locales ?? [];
    healthStatus.value = health.status;
    healthDetail.value = health.database;
    runningJobs.value = health.running_jobs ?? 0;
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loading.value = false;
  }
}

onMounted(() => {
  void load();
});
</script>

<template>
  <div class="space-y-6">
    <Alert v-if="failure" tone="destructive" :title="t('errors.genericTitle')" :message="failure">
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <div v-if="loading && !version" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <template v-else>
      <Card :title="t('admin.about')" :description="t('admin.aboutSubtitle')">
        <dl class="grid gap-4 sm:grid-cols-2">
          <div class="space-y-1">
            <dt class="text-xs uppercase tracking-wide text-muted-foreground">
              {{ t('admin.version') }}
            </dt>
            <dd class="text-sm font-medium">{{ version?.version || t('common.unknown') }}</dd>
          </div>
          <div class="space-y-1">
            <dt class="text-xs uppercase tracking-wide text-muted-foreground">
              {{ t('admin.commit') }}
            </dt>
            <dd class="break-anywhere font-mono text-xs">
              {{ version?.commit || t('common.unknown') }}
            </dd>
          </div>
          <div class="space-y-1">
            <dt class="text-xs uppercase tracking-wide text-muted-foreground">
              {{ t('admin.goVersion') }}
            </dt>
            <dd class="text-sm font-medium">{{ version?.go_version || t('common.unknown') }}</dd>
          </div>
          <div class="space-y-1">
            <dt class="text-xs uppercase tracking-wide text-muted-foreground">
              {{ t('admin.buildDate') }}
            </dt>
            <dd class="text-sm font-medium">{{ buildDate }}</dd>
          </div>
          <div class="space-y-1 sm:col-span-2">
            <dt class="text-xs uppercase tracking-wide text-muted-foreground">
              {{ t('admin.authMode') }}
            </dt>
            <dd class="text-sm font-medium">{{ authModeLabel }}</dd>
          </div>
        </dl>
      </Card>

      <Card :title="t('admin.providersAvailable')">
        <ul v-if="providerItems.length > 0" class="flex flex-wrap gap-2">
          <li v-for="item in providerItems" :key="item">
            <Badge tone="secondary">{{ item }}</Badge>
          </li>
        </ul>
        <p v-else class="text-sm text-muted-foreground">{{ t('common.none') }}</p>
      </Card>

      <Card :title="t('admin.localesAvailable')">
        <ul v-if="localeItems.length > 0" class="flex flex-wrap gap-2">
          <li v-for="item in localeItems" :key="item">
            <Badge tone="secondary">{{ item }}</Badge>
          </li>
        </ul>
        <p v-else class="text-sm text-muted-foreground">{{ t('common.none') }}</p>
      </Card>

      <Card :title="t('admin.health')">
        <template #actions>
          <Button variant="outline" size="sm" :loading="loading" @click="load">
            <RefreshCw class="h-4 w-4" aria-hidden="true" />
            {{ t('common.refresh') }}
          </Button>
        </template>
        <div v-if="healthStatus" class="space-y-4">
          <div class="flex flex-wrap items-center gap-3">
            <StatusBadge :status="healthStatus" />
            <p class="text-sm text-muted-foreground">{{ healthText }}</p>
          </div>
          <div class="flex flex-wrap items-center gap-3">
            <StatusBadge status="running" />
            <p class="text-sm text-muted-foreground">
              {{ t('dashboard.runningJobs') }}: {{ formatNumber(runningJobs, locale) }}
            </p>
          </div>
        </div>
        <p v-else class="text-sm text-muted-foreground">{{ t('common.notAvailable') }}</p>
      </Card>
    </template>
  </div>
</template>
