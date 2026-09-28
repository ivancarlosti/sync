// Session store.
//
// The SPA never sees a token: the session is an HttpOnly cookie the browser
// sends on every same-origin request. This store only mirrors what
// `GET /api/auth/session` says, so the router guard and the header can render
// without each screen fetching the session again.
import { defineStore } from 'pinia';
import { computed, ref } from 'vue';

import { auth, messageOf, type SessionView } from '@/lib/api';

export const useAuthStore = defineStore('auth', () => {
  const session = ref<SessionView | null>(null);
  /** loading is true while a session request is in flight. */
  const loading = ref(false);
  /** loaded marks a completed (successful or failed) first load. */
  const loaded = ref(false);
  const error = ref('');

  const authenticated = computed(() => session.value?.authenticated === true);
  const operator = computed(() => session.value?.operator ?? null);
  const mode = computed(() => session.value?.mode ?? 'account');
  const open = computed(() => session.value?.open === true);
  const defaults = computed(() => session.value?.defaults ?? null);
  const version = computed(() => session.value?.version ?? null);
  const captcha = computed(() => session.value?.captcha ?? { enabled: false, site_key: undefined });
  const keycloak = computed(() => session.value?.keycloak?.enabled === true);

  /** displayName is what the header shows for the signed-in operator. */
  const displayName = computed(
    () => operator.value?.name || operator.value?.email || operator.value?.subject || '',
  );

  /** inflight deduplicates the session request of the guard and the header. */
  let inflight: Promise<SessionView | null> | null = null;

  /**
   * load refreshes the session view. It never throws (the shell has to render
   * whatever the answer is) and concurrent callers share one request.
   */
  function load(): Promise<SessionView | null> {
    inflight ??= refresh().finally(() => {
      inflight = null;
    });
    return inflight;
  }

  async function refresh(): Promise<SessionView | null> {
    loading.value = true;
    try {
      const view = await auth.session();
      session.value = view;
      error.value = '';
      return view;
    } catch (failure) {
      error.value = messageOf(failure);
      session.value = null;
      return null;
    } finally {
      loading.value = false;
      loaded.value = true;
    }
  }

  /** signIn exchanges the credentials for a session cookie. */
  async function signIn(
    login: string,
    password: string,
    options: { captchaToken?: string; redirectTo?: string } = {},
  ): Promise<void> {
    const view = await auth.login({
      login,
      password,
      captcha_token: options.captchaToken ?? '',
      redirect_to: options.redirectTo ?? '',
    });
    session.value = view;
    error.value = '';
  }

  /** startKeycloak navigates to the realm; the callback returns to the SPA. */
  async function startKeycloak(redirectTo: string): Promise<void> {
    const authorization = await auth.keycloak(redirectTo);
    window.location.assign(authorization.url);
  }

  /**
   * signOut clears the cookie through the API. The request is idempotent, so a
   * failure (expired session, server restart) still ends with a clean local
   * state: the operator asked to leave.
   */
  async function signOut(): Promise<void> {
    try {
      await auth.logout();
    } finally {
      session.value = null;
      await load();
    }
  }

  /** invalidate is what the API client calls when a request answers 401. */
  function invalidate(): void {
    const current = session.value;
    if (current) {
      session.value = {
        ...current,
        authenticated: false,
        operator: { ...current.operator, authenticated: false },
      };
    }
  }

  return {
    session,
    loading,
    loaded,
    error,
    authenticated,
    operator,
    mode,
    open,
    defaults,
    version,
    captcha,
    keycloak,
    displayName,
    load,
    signIn,
    startKeycloak,
    signOut,
    invalidate,
  };
});
