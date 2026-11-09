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
   * realmLogout is the end-session URL of the realm, or '' when this instance is
   * not in keycloak mode or the realm cannot answer. A logout must never depend
   * on the identity provider being reachable: the local session always ends.
   */
  async function realmLogout(): Promise<string> {
    if (!keycloak.value) {
      return '';
    }
    try {
      const { url } = await auth.keycloakLogout();
      return url;
    } catch {
      return '';
    }
  }

  /**
   * signOut clears the cookie through the API and, in keycloak mode, navigates to
   * the realm end-session URL. Without that second step the realm keeps its
   * single sign-on cookie, so the next login silently returns the same account —
   * the dead end of an operator the allow-list refused.
   *
   * It resolves true when the browser is already on its way to the realm (the
   * caller must not navigate: it would race the redirect) and false when the
   * operator is left on the SPA. The request is idempotent, so a failure (expired
   * session, server restart) still ends with a clean local state: the operator
   * asked to leave.
   */
  async function signOut(): Promise<boolean> {
    const realm = await realmLogout();
    let failed: unknown = null;
    try {
      await auth.logout();
    } catch (failure) {
      failed = failure;
    } finally {
      session.value = null;
    }
    if (realm) {
      // The realm ends the SSO session and comes back to APP_URL, where the
      // router guard sends the operator to /login.
      window.location.assign(realm);
      return true;
    }
    await load();
    if (failed) {
      throw failed;
    }
    return false;
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
