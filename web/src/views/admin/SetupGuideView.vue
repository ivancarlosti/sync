<script setup lang="ts">
// SetupGuideView — the guided app registration (Admin > Setup guide).
//
// Everything an operator has to do in the Google Cloud console or in the Entra
// admin center is one ordered list, and every value they have to paste in comes
// from this instance rather than from the documentation: the redirect URI, the
// exact permission list and the tenant-wide consent (Microsoft).
//
// The steps, their console links and the permission table come from the API
// (GET /api/providers/:provider/guide, see internal/services/guide.go), so the
// instructions can never describe a permission the code does not request. The
// wording is translated here: each step id maps onto
// `admin.guide.steps.<provider>.<id>.title` / `.body`.
//
// The tenant-wide consent returns to this page, so the query string carries the
// outcome (`consent`, `consent_error`, `detail`) exactly like the accounts screen
// does for a connection.
import { Compass, ExternalLink, KeyRound, RefreshCw, ShieldCheck } from '@lucide/vue';
import { computed, onMounted, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';

import CapabilityBadges from '@/components/CapabilityBadges.vue';
import CopyField from '@/components/CopyField.vue';
import PageHeader from '@/components/PageHeader.vue';
import Alert from '@/components/ui/Alert.vue';
import Badge from '@/components/ui/Badge.vue';
import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import Spinner from '@/components/ui/Spinner.vue';
import { useAction } from '@/composables/useAction';
import {
  accounts,
  messageOf,
  providers as providersApi,
  type GuideStep,
  type ProviderGuide,
  type ProviderName,
} from '@/lib/api';
import { formatDateTime } from '@/lib/format';
import { PROVIDERS, isProvider, providerLabel } from '@/lib/providers';
import { useFeedbackStore } from '@/stores/feedback';

const route = useRoute();
const router = useRouter();
const { t, te, locale } = useI18n();
const feedback = useFeedbackStore();

const guide = ref<ProviderGuide | null>(null);
const loading = ref(true);
const failure = ref('');

const { busy: consenting, run: runConsent } = useAction();

/** provider is the provider of the URL, defaulting to the first one shipped. */
const provider = computed<ProviderName>(() => {
  const raw = String(route.params.provider ?? '');
  return isProvider(raw) ? raw : PROVIDERS[0];
});

/** scopeText is the permission list as the console expects it: space separated. */
const scopeText = computed(() => (guide.value ? guide.value.scopes.join(' ') : ''));

/** stepKey builds the catalog key of one field of a step. */
function stepKey(step: GuideStep, field: 'title' | 'body'): string {
  return `admin.guide.steps.${provider.value}.${step.id}.${field}`;
}

/** warningKey is the catalog key of a caveat reported by the API, or empty. */
function warningKey(code: string): string {
  const key = `admin.guide.warnings.${code}`;
  return te(key) ? key : '';
}

/** capabilityLabel translates a capability, falling back to its identifier. */
function capabilityLabel(capability: string): string {
  const key = `admin.capability.${capability}`;
  return te(key) ? t(key) : capability;
}

/** when renders a timestamp of the API, or an empty string when it is missing. */
function when(value: string | undefined): string {
  return value ? formatDateTime(value, locale.value) : '';
}

/** valueOf reads a single query parameter, tolerating the array form. */
function valueOf(value: unknown): string {
  const first = Array.isArray(value) ? value[0] : value;
  return typeof first === 'string' ? first : '';
}

/** messageFor translates a consent error code, reusing the accounts wording. */
function messageFor(code: string): string {
  const key = `accounts.error_${code}`;
  return te(key) ? t(key) : t('admin.guide.consentError');
}

/**
 * outcome is what the consent redirect left in the URL. It is read once per
 * navigation: the effect that consumes it also clears the query.
 */
const outcome = computed(() => {
  const code = valueOf(route.query.consent_error);
  if (code !== '') {
    return { tone: 'error' as const, text: messageFor(code), detail: valueOf(route.query.detail) };
  }
  if (valueOf(route.query.consent) === 'granted') {
    return { tone: 'success' as const, text: t('admin.guide.consentDone'), detail: '' };
  }
  return null;
});

/** load reads the walkthrough of the provider of the URL. */
async function load(): Promise<void> {
  const wanted = provider.value;
  loading.value = true;
  failure.value = '';
  try {
    const answer = await providersApi.guide(wanted);
    // A quick switch between the tabs can leave an older answer in flight: only
    // the request that still matches the tab owns the screen.
    if (wanted !== provider.value) {
      return;
    }
    guide.value = answer;
  } catch (error) {
    if (wanted !== provider.value) {
      return;
    }
    failure.value = messageOf(error);
  } finally {
    if (wanted === provider.value) {
      loading.value = false;
    }
  }
}

/** openConsole opens a step page in a new tab. */
function openConsole(url: string | undefined): void {
  if (!url) {
    return;
  }
  window.open(url, '_blank', 'noopener,noreferrer');
}

/** grantConsent starts the tenant-wide consent and follows the provider URL. */
async function grantConsent(): Promise<void> {
  const answer = await runConsent(
    () => accounts.adminConsent(provider.value, `/admin/guide/${provider.value}`),
    { failure: t('admin.guide.consentError') },
  );
  if (answer) {
    window.location.assign(answer.url);
  }
}

/**
 * showOutcome turns the query string a consent redirect left behind into a
 * message and clears it, so a reload does not show the same banner again.
 */
async function showOutcome(): Promise<void> {
  const shown = outcome.value;
  if (!shown) {
    return;
  }
  if (shown.tone === 'success') {
    feedback.success(shown.text);
  } else {
    feedback.fail(shown.text, shown.detail);
  }
  await router.replace({ query: {} });
}

onMounted(async () => {
  await load();
  await showOutcome();
});

// The screen is one route record with a provider parameter, so Vue Router reuses
// this component between the tabs: without this watcher `onMounted` would not run
// again and the steps, the banner and the warnings of the previous provider would
// stay on screen (a Google step id rendered with a Microsoft i18n key, for one).
watch(provider, async () => {
  // Drop the previous provider before the new answer arrives: keeping it would
  // render its steps and its banner under the new tab.
  guide.value = null;
  await load();
  await showOutcome();
});
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('admin.guide.title')" :subtitle="t('admin.guide.subtitle')">
      <template #actions>
        <Button variant="outline" :loading="loading" @click="load">
          <RefreshCw class="h-4 w-4" aria-hidden="true" />
          {{ t('common.refresh') }}
        </Button>
      </template>
    </PageHeader>

    <div class="flex flex-wrap gap-1 rounded-lg bg-muted p-1" :aria-label="t('admin.guide.providers')">
      <RouterLink
        v-for="name in PROVIDERS"
        :key="name"
        :to="{ name: 'admin-guide', params: { provider: name } }"
        :aria-current="provider === name ? 'page' : undefined"
        class="inline-flex items-center gap-2 rounded-md px-3 py-1.5 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground"
        :class="provider === name && 'bg-background text-foreground shadow-sm'"
      >
        {{ providerLabel(name) }}
      </RouterLink>
    </div>

    <Alert v-if="failure" tone="destructive" :title="t('admin.guide.loadError')" :message="failure">
      <template #footer>
        <Button variant="outline" size="sm" @click="load">{{ t('common.retry') }}</Button>
      </template>
    </Alert>

    <div v-else-if="loading && !guide" class="flex items-center justify-center gap-2 py-10">
      <Spinner :label="t('common.loading')" />
      <span class="text-sm text-muted-foreground">{{ t('common.loading') }}</span>
    </div>

    <template v-else-if="guide">
      <Alert
        :tone="guide.configured ? 'success' : 'warning'"
        :title="guide.configured ? t('admin.guide.statusReady') : t('admin.guide.statusMissing')"
        :message="t(`admin.guide.intro.${guide.provider}`)"
      >
        <template #footer>
          <Button variant="outline" size="sm" @click="router.push({ name: 'admin-providers' })">
            <KeyRound class="h-4 w-4" aria-hidden="true" />
            {{ t('admin.guide.credentialsAction') }}
          </Button>
        </template>
      </Alert>
      <div class="grid gap-6 lg:grid-cols-3">
        <div class="space-y-4 lg:col-span-2">
          <Card content-class="space-y-4">
            <ol class="space-y-4" :aria-label="t('admin.guide.step')">
              <li
                v-for="(step, index) in guide.steps"
                :key="step.id"
                class="flex gap-3 rounded-lg border border-border p-4"
              >
                <span
                  class="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-semibold"
                >
                  {{ index + 1 }}
                </span>
                <div class="min-w-0 flex-1 space-y-2">
                  <div class="flex flex-wrap items-center gap-2">
                    <h3 class="text-sm font-semibold">{{ t(stepKey(step, 'title')) }}</h3>
                    <Badge v-if="step.optional" tone="outline">{{ t('admin.guide.optional') }}</Badge>
                    <Badge
                      v-if="step.action === 'admin_consent' && guide.admin_consent.granted"
                      tone="success"
                    >
                      {{ t('admin.guide.consentTitle') }}
                    </Badge>
                  </div>
                  <p class="text-sm text-muted-foreground">{{ t(stepKey(step, 'body')) }}</p>

                  <CopyField
                    v-if="step.copy === 'redirect_uri'"
                    :value="guide.redirect_uri"
                    class="max-w-2xl"
                  />
                  <CopyField v-else-if="step.copy === 'scopes'" :value="scopeText" multiline />

                  <div v-if="step.action === 'admin_consent'" class="space-y-2">
                    <Alert
                      v-if="guide.admin_consent.granted"
                      tone="success"
                      :message="
                        t('admin.guide.consentGranted', {
                          tenant: guide.admin_consent.tenant,
                          date: when(guide.admin_consent.at),
                        })
                      "
                    />
                    <Alert v-else tone="warning" :message="t('admin.guide.consentMissing')" />
                    <Button :loading="consenting" @click="grantConsent">
                      <ShieldCheck class="h-4 w-4" aria-hidden="true" />
                      {{ consenting ? t('common.loading') : t('admin.guide.consentAction') }}
                    </Button>
                  </div>

                  <Button v-if="step.url" variant="outline" size="sm" @click="openConsole(step.url)">
                    <ExternalLink class="h-4 w-4" aria-hidden="true" />
                    {{ t('admin.guide.console') }}
                  </Button>
                </div>
              </li>
            </ol>
          </Card>
        </div>

        <div class="space-y-4">
          <Card :title="t('admin.guide.redirectTitle')" :description="t('admin.guide.redirectHint')">
            <CopyField :value="guide.redirect_uri" />
          </Card>

          <Card :title="t('admin.guide.capabilitiesTitle')">
            <CapabilityBadges :capabilities="guide.capabilities" />
          </Card>

          <Card
            :title="t('admin.guide.permissionsTitle')"
            :description="t('admin.guide.permissionsHint', { count: guide.permissions.length })"
            content-class="space-y-3"
          >
            <CopyField :value="scopeText" multiline />
            <ul class="space-y-2">
              <li v-for="permission in guide.permissions" :key="permission.scope" class="space-y-1">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="text-xs font-medium">{{ capabilityLabel(permission.capability) }}</span>
                  <Badge v-if="permission.admin_consent" tone="warning">
                    {{ t('admin.guide.consentTitle') }}
                  </Badge>
                </div>
                <code class="block break-all font-mono text-xs text-muted-foreground">{{
                  permission.scope
                }}</code>
              </li>
            </ul>
          </Card>

          <Card v-if="guide.warnings.length" :title="t('admin.guide.warningsTitle')">
            <ul class="space-y-2 text-sm text-muted-foreground">
              <template v-for="code in guide.warnings" :key="code">
                <li v-if="warningKey(code)" class="flex gap-2">
                  <Compass class="mt-0.5 h-4 w-4 shrink-0 text-warning" aria-hidden="true" />
                  <span>{{ t(warningKey(code)) }}</span>
                </li>
              </template>
            </ul>
          </Card>

          <Card :title="t('admin.guide.connectTitle')" :description="t('admin.guide.connectHint')">
            <template #actions>
              <Button variant="outline" size="sm" @click="router.push({ name: 'accounts' })">
                {{ t('admin.guide.connectAction') }}
              </Button>
            </template>
            <Button
              variant="link"
              size="sm"
              as="a"
              href="docs/app-registration.md"
              target="_blank"
              rel="noopener noreferrer"
            >
              {{ t('admin.guide.docs') }}
            </Button>
          </Card>
        </div>
      </div>
    </template>
  </div>
</template>

