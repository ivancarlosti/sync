<script setup lang="ts">
// LoginView — the only screen outside the shell.
//
// It renders whichever authentication the server offers (see
// internal/handlers/auth.go): nothing to do in `none` mode, a form in `account`
// mode (with reCAPTCHA when configured), and the realm button in `keycloak` mode.
// Failures arriving as query parameters (`auth_error`/`auth_detail` from the OIDC
// callback) and failures of the form itself go through the same translation.
import { LogIn, ShieldCheck } from '@lucide/vue';
import { computed, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';

import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Input from '@/components/ui/Input.vue';
import Label from '@/components/ui/Label.vue';
import { captchaToken } from '@/composables/useRecaptcha';
import { codeOf, messageOf } from '@/lib/api';
import { useAuthStore } from '@/stores/auth';

const auth = useAuthStore();
const route = useRoute();
const router = useRouter();
const { t, te } = useI18n();

const login = ref('');
const password = ref('');
const busy = ref(false);
const failure = ref<{ code: string; detail: string } | null>(null);

const name = computed(() => auth.version?.name ?? 'Sync');

/** redirectTarget only accepts same-origin paths; anything else lands home. */
const redirectTarget = computed(() => {
  const requested = route.query.redirect_to;
  const value = Array.isArray(requested) ? requested[0] : requested;
  if (typeof value === 'string' && value.startsWith('/') && !value.startsWith('//')) {
    return value;
  }
  return null;
});

/**
 * LOGIN_CODE_ALIASES adapts the codes of `POST /api/auth/login` to the login
 * catalog: the endpoint answers `unauthorized` for a wrong user or password,
 * which is the `invalid` wording here (everywhere else that code really does
 * mean "your session expired").
 */
const LOGIN_CODE_ALIASES: Record<string, string> = { unauthorized: 'invalid' };

/** messageKey maps an API code onto the login catalog, with a generic fallback. */
function messageKey(code: string): string {
  const key = `auth.error_${LOGIN_CODE_ALIASES[code] ?? code}`;
  return te(key) ? key : 'auth.error_unknown';
}

// reported is a failure the OIDC callback sent as a code instead of a message.
const reported = computed(() => {
  const code = route.query.auth_error;
  const detail = route.query.auth_detail;
  const value = Array.isArray(code) ? code[0] : code;
  if (typeof value !== 'string' || value === '') {
    return null;
  }
  const reason = Array.isArray(detail) ? detail[0] : detail;
  return { code: value, detail: typeof reason === 'string' ? reason : '' };
});

const problem = computed(() => failure.value ?? reported.value);

/** submit posts the credentials, with a captcha token when the server wants one. */
async function submit(): Promise<void> {
  failure.value = null;
  if (!login.value.trim() || !password.value) {
    failure.value = { code: 'validation', detail: t('auth.passwordRequired') };
    return;
  }
  busy.value = true;
  try {
    let token = '';
    if (auth.captcha.enabled && auth.captcha.site_key) {
      token = await captchaToken(auth.captcha.site_key, 'login');
    }
    await auth.signIn(login.value.trim(), password.value, { captchaToken: token });
    password.value = '';
    await router.replace(redirectTarget.value ?? { name: 'dashboard' });
  } catch (error) {
    failure.value = { code: codeOf(error), detail: messageOf(error) };
  } finally {
    busy.value = false;
  }
}

/** signInWithRealm asks the server for the realm URL and navigates to it. */
async function signInWithRealm(): Promise<void> {
  failure.value = null;
  busy.value = true;
  try {
    await auth.startKeycloak(redirectTarget.value ?? '/');
  } catch (error) {
    failure.value = { code: codeOf(error), detail: messageOf(error) };
  } finally {
    busy.value = false;
  }
}

/** enter leaves the open instance for the dashboard. */
async function enter(): Promise<void> {
  await router.replace(redirectTarget.value ?? { name: 'dashboard' });
}
</script>

<template>
  <div class="flex min-h-screen flex-col bg-muted/40">
    <main class="flex flex-1 items-center justify-center px-4 py-10">
      <div class="w-full max-w-md space-y-6">
        <div class="flex flex-col items-center gap-3 text-center">
          <img src="/logo.svg" alt="" class="h-12 w-12" />
          <div class="space-y-1">
            <h1 class="text-xl font-semibold tracking-tight">{{ name }}</h1>
            <p class="text-sm text-muted-foreground">{{ t('auth.subtitle') }}</p>
          </div>
        </div>

        <div class="rounded-xl border border-border bg-card p-6 shadow-sm">
          <Alert
            v-if="problem"
            tone="destructive"
            :title="t(messageKey(problem.code))"
            :message="problem.detail || t('errors.genericMessage')"
            class="mb-4"
          />

          <!-- Open mode: the server trusts every request, nothing to enter. -->
          <div v-if="auth.open" class="space-y-4">
            <div class="space-y-1">
              <p class="text-sm font-medium">{{ t('auth.openModeTitle') }}</p>
              <p class="text-sm text-muted-foreground">{{ t('auth.openModeHint') }}</p>
            </div>
            <Button class="w-full" @click="enter">{{ t('auth.enter') }}</Button>
          </div>

          <!-- Keycloak mode: the realm owns the credentials. -->
          <div v-else-if="auth.keycloak" class="space-y-4">
            <Button class="w-full" :loading="busy" @click="signInWithRealm">
              <ShieldCheck class="h-4 w-4" aria-hidden="true" />
              {{ t('auth.keycloakSignIn') }}
            </Button>
          </div>

          <!-- Account mode: the form. -->
          <form v-else class="space-y-4" novalidate @submit.prevent="submit">
            <div class="space-y-1.5">
              <Label for="login" required>{{ t('auth.loginLabel') }}</Label>
              <Input
                id="login"
                v-model="login"
                autocomplete="username"
                :placeholder="t('auth.loginLabel')"
              />
            </div>

            <div class="space-y-1.5">
              <Label for="password" required>{{ t('auth.passwordLabel') }}</Label>
              <Input
                id="password"
                v-model="password"
                type="password"
                autocomplete="current-password"
                :placeholder="t('auth.passwordLabel')"
              />
            </div>

            <p v-if="auth.captcha.enabled" class="text-xs text-muted-foreground">
              {{ t('auth.captchaHint') }}
            </p>

            <Button type="submit" class="w-full" :loading="busy">
              <LogIn class="h-4 w-4" aria-hidden="true" />
              {{ busy ? t('auth.signingIn') : t('auth.signIn') }}
            </Button>
          </form>
        </div>

        <p class="text-center text-xs text-muted-foreground">
          {{ name }} {{ auth.version?.version ?? '' }}
        </p>
      </div>
    </main>
  </div>
</template>
