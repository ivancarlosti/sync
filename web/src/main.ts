// Application bootstrap.
//
// Order matters here:
//  1. the theme and the language are applied before the first paint, so a reload
//     never flashes the wrong colours (index.html paints the same class);
//  2. the session is loaded once and cached in the store, which is what the
//     router guard and the header both read;
//  3. the administrator defaults (DEFAULT_LOCALE / DEFAULT_THEME) are applied
//     only for a browser that never made a choice of its own.
import { createPinia } from 'pinia';
import { createApp } from 'vue';

import App from '@/App.vue';
import { i18n, initLocale, readStoredLocale } from '@/i18n';
import { setUnauthorizedHandler } from '@/lib/api';
import router from '@/router';
import { useAuthStore } from '@/stores/auth';
import { useFeedbackStore } from '@/stores/feedback';
import { isTheme, THEME_KEY, useThemeStore } from '@/stores/theme';

import '@/style.css';

const pinia = createPinia();
const app = createApp(App);

app.use(pinia);
app.use(i18n);
app.use(router);

const theme = useThemeStore(pinia);
theme.init();
initLocale();

// A 401 anywhere means the cookie is gone (server restarted, session expired).
// Drop the local session and send the operator to the login screen with an
// explanation instead of leaving a half-rendered page behind.
setUnauthorizedHandler(() => {
  const auth = useAuthStore(pinia);
  const feedback = useFeedbackStore(pinia);
  auth.invalidate();
  feedback.clear();
  if (router.currentRoute.value.name === 'login') {
    return;
  }
  void router.push({
    name: 'login',
    query: {
      redirect_to: router.currentRoute.value.fullPath,
      auth_error: 'unauthorized',
    },
  });
});

const auth = useAuthStore(pinia);
void auth.load().then((view) => {
  const defaults = view?.defaults;
  if (!defaults) {
    return;
  }
  if (!readStoredLocale()) {
    initLocale(defaults.default_locale);
  }
  let storedTheme: string | null = null;
  try {
    storedTheme = window.localStorage.getItem(THEME_KEY);
  } catch {
    // Storage unavailable: the theme keeps what the bootstrap resolved.
  }
  if (!isTheme(storedTheme)) {
    theme.init(defaults.default_theme);
  }
});

app.mount('#app');
