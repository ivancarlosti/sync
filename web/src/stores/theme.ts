// Theme store.
//
// The preference is `light`, `dark` or `system` and lives in
// `localStorage['sync.theme']`, the key the pre-paint script of index.html reads
// (src/main.ts applies the same value before mount, so the two agree). `system`
// follows `prefers-color-scheme` live: the listener below keeps the `<html>`
// class correct when the operating system flips at dusk, and it is the default
// the very first visit uses.
import { defineStore } from 'pinia';
import { computed, ref } from 'vue';

import type { ThemePreference } from '@/lib/api';

/** THEME_KEY is shared with the inline script in index.html. */
export const THEME_KEY = 'sync.theme';

/** THEMES is the order the switcher cycles through. */
export const THEMES: ThemePreference[] = ['light', 'dark', 'system'];

/** isTheme guards values coming from storage or from the server settings. */
export function isTheme(value: unknown): value is ThemePreference {
  return value === 'light' || value === 'dark' || value === 'system';
}

/** readStoredTheme returns the persisted preference, or null when unusable. */
function readStoredTheme(): ThemePreference | null {
  try {
    const stored = window.localStorage.getItem(THEME_KEY);
    return isTheme(stored) ? stored : null;
  } catch {
    // Private mode: the theme still works for this session.
    return null;
  }
}

export const useThemeStore = defineStore('theme', () => {
  const preference = ref<ThemePreference>(readStoredTheme() ?? 'system');
  /** dark is the resolved mode; the `<html>` class and the store never drift. */
  const dark = ref(false);

  const resolved = computed<'light' | 'dark'>(() => (dark.value ? 'dark' : 'light'));

  /** prefersDark reads the operating system preference, defaulting to light. */
  function prefersDark(): boolean {
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ?? false;
  }

  /** paint applies the resolved theme to the document. */
  function paint(): void {
    const next = preference.value === 'system' ? prefersDark() : preference.value === 'dark';
    dark.value = next;
    document.documentElement.classList.toggle('dark', next);
    // `color-scheme` makes form controls and scrollbars follow the theme.
    document.documentElement.style.colorScheme = next ? 'dark' : 'light';
  }

  /** set stores and applies a preference; it is what the switcher calls. */
  function set(next: ThemePreference): void {
    preference.value = isTheme(next) ? next : 'system';
    try {
      window.localStorage.setItem(THEME_KEY, preference.value);
    } catch {
      // Persisting is a nicety; the theme applies either way.
    }
    paint();
  }

  /** cycle walks light -> dark -> system, the order of the switcher's menu. */
  function cycle(): void {
    const index = THEMES.indexOf(preference.value);
    set(THEMES[(index + 1) % THEMES.length]);
  }

  /**
   * init applies the stored preference (or `serverDefault`, the administrator's
   * `DEFAULT_THEME`, for a first visit) and starts following the system setting.
   */
  function init(serverDefault?: string): void {
    const stored = readStoredTheme();
    if (stored) {
      preference.value = stored;
    } else if (isTheme(serverDefault)) {
      preference.value = serverDefault;
    }
    paint();
    const query = window.matchMedia?.('(prefers-color-scheme: dark)');
    if (!query?.addEventListener) {
      return;
    }
    query.addEventListener('change', () => {
      if (preference.value === 'system') {
        paint();
      }
    });
  }

  return { preference, dark, resolved, set, cycle, init };
});
