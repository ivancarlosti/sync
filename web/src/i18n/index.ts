// vue-i18n setup: seven catalogs, one persisted language choice.
//
// The active locale lives in `localStorage['sync.locale']` so a reload keeps the
// operator's choice; when nothing is stored yet the browser languages are matched
// against the shipped catalogs (browser tags without a catalog of their own, like
// `zh-CN`, are mapped through ALIASES onto `h-CN`). Every catalog is a flat-key
// translation of `en-US`, which is also the fallback locale.
//
// `ar-SA` is the only right-to-left locale, so switching languages also mirrors
// the document through `dir` on `<html>`; the Tailwind `rtl:` variant keys off it.
import { createI18n } from 'vue-i18n';

import arSA from './locales/ar-SA.json';
import enUS from './locales/en-US.json';
import esMX from './locales/es-MX.json';
import frFR from './locales/fr-FR.json';
import hCN from './locales/h-CN.json';
import hiIN from './locales/hi-IN.json';
import ptBR from './locales/pt-BR.json';

/** LOCALE_KEY holds the persisted language choice. */
export const LOCALE_KEY = 'sync.locale';

/**
 * MESSAGES holds the seven catalogs under their locale code. Declaring it here
 * (instead of inline in `createI18n`) is what gives `LocaleCode` its literal
 * union: `keyof typeof MESSAGES` is the only list of the shipped languages, and
 * vue-i18n infers the same union from the object.
 */
const MESSAGES = {
  'en-US': enUS,
  'pt-BR': ptBR,
  'es-MX': esMX,
  'fr-FR': frFR,
  'h-CN': hCN,
  'hi-IN': hiIN,
  'ar-SA': arSA,
};

/** LocaleCode is the union of the shipped catalog codes. */
export type LocaleCode = keyof typeof MESSAGES;

/** DEFAULT_LOCALE mirrors `config.DefaultLocale`. */
export const DEFAULT_LOCALE: LocaleCode = 'en-US';

/** LocaleDescriptor is the minimum the UI needs to know about a catalog. */
export interface LocaleDescriptor {
  code: string;
  /** Native name; the switcher shows every language in its own script. */
  label: string;
  dir: 'ltr' | 'rtl';
}

/** LOCALES keeps the same order as `config.SupportedLocales`. */
export const LOCALES: LocaleDescriptor[] = [
  { code: 'en-US', label: 'English', dir: 'ltr' },
  { code: 'pt-BR', label: 'Português (Brasil)', dir: 'ltr' },
  { code: 'es-MX', label: 'Español (México)', dir: 'ltr' },
  { code: 'fr-FR', label: 'Français', dir: 'ltr' },
  { code: 'h-CN', label: '中文（简体）', dir: 'ltr' },
  { code: 'hi-IN', label: 'हिन्दी', dir: 'ltr' },
  { code: 'ar-SA', label: 'العربية', dir: 'rtl' },
];

const catalog: Record<string, LocaleDescriptor> = Object.fromEntries(
  LOCALES.map((locale) => [locale.code, locale]),
);

/**
 * ALIASES maps browser language tags onto a shipped catalog. The server calls the
 * Simplified Chinese catalog `h-CN`, but browsers report `zh-CN`/`zh-Hans`, so
 * both spellings have to resolve to it.
 */
const ALIASES: Record<string, string> = {
  en: 'en-US',
  pt: 'pt-BR',
  es: 'es-MX',
  fr: 'fr-FR',
  hi: 'hi-IN',
  ar: 'ar-SA',
  zh: 'h-CN',
  'zh-cn': 'h-CN',
  'zh-hans': 'h-CN',
  'zh-hans-cn': 'h-CN',
  'zh-sg': 'h-CN',
  'zh-tw': 'h-CN',
  'zh-hant': 'h-CN',
};

/** isLocale reports whether `value` names a shipped catalog. */
export function isLocale(value: unknown): value is LocaleCode {
  return typeof value === 'string' && value in catalog;
}

/** isRTL reports whether a catalog is written right to left. */
export function isRTL(code: string): boolean {
  return catalog[code]?.dir === 'rtl';
}

/** labelOf is the native name of a shipped locale (`h-CN` -> 中文（简体）). */
export function labelOf(code: string): string {
  return catalog[code]?.label ?? code;
}

export const i18n = createI18n({
  // Composition API mode: components use `useI18n()`, never `this.$t`.
  legacy: false,
  globalInjection: true,
  locale: DEFAULT_LOCALE,
  fallbackLocale: DEFAULT_LOCALE,
  /**
   * The catalogs are flat: `notifications.event_sync.success` and
   * `jobs.steps.details` are single keys whose name contains a dot, and only
   * `flatJson` makes the resolver look the literal key up instead of walking a
   * nested path that does not exist (which would render the key itself).
   */
  flatJson: true,
  messages: MESSAGES,
  // The catalogs are kept in parity by a checker script, so a missing key is a
  // bug worth seeing in the console; falling back to en-US is not worth a warning.
  missingWarn: true,
  fallbackWarn: false,
});

/** readStoredLocale returns the persisted choice, or null when unusable. */
export function readStoredLocale(): string | null {
  try {
    const stored = window.localStorage.getItem(LOCALE_KEY);
    return isLocale(stored) ? stored : null;
  } catch {
    // Private mode or a blocked storage partition: fall back to detection.
    return null;
  }
}

/** matchBrowser finds a catalog for the browser's preferred languages. */
function matchBrowser(): string | null {
  if (typeof navigator === 'undefined') {
    return null;
  }
  const candidates = navigator.languages?.length ? navigator.languages : [navigator.language];
  for (const candidate of candidates) {
    if (!candidate) {
      continue;
    }
    if (isLocale(candidate)) {
      return candidate;
    }
    const tag = candidate.toLowerCase();
    if (ALIASES[tag]) {
      return ALIASES[tag];
    }
    const base = tag.split('-')[0];
    if (ALIASES[base]) {
      return ALIASES[base];
    }
  }
  return null;
}

/** detectLocale prefers the stored choice, then the browser, then `fallback`. */
export function detectLocale(fallback: string = DEFAULT_LOCALE): string {
  const stored = readStoredLocale();
  if (stored) {
    return stored;
  }
  return matchBrowser() ?? (isLocale(fallback) ? fallback : DEFAULT_LOCALE);
}

/** applyDocumentLocale keeps `<html lang dir>` in sync with the UI language. */
export function applyDocumentLocale(code: string): void {
  const root = document.documentElement;
  root.setAttribute('lang', code);
  root.setAttribute('dir', isRTL(code) ? 'rtl' : 'ltr');
}

/**
 * applyLocale activates a locale and mirrors the document. Unknown codes degrade
 * to the default catalog instead of throwing, so a stale value in localStorage
 * or an unknown server default can never break the first render.
 */
function applyLocale(code: string, persist: boolean): string {
  const next = isLocale(code) ? code : DEFAULT_LOCALE;
  i18n.global.locale.value = next;
  if (persist) {
    try {
      window.localStorage.setItem(LOCALE_KEY, next);
    } catch {
      // Persisting is a nicety: the language still applies for this session.
    }
  }
  applyDocumentLocale(next);
  return next;
}

/**
 * setLocale records an explicit choice — the language switcher calls it — and
 * applies it right away.
 */
export function setLocale(code: string): string {
  return applyLocale(code, true);
}

/**
 * initLocale resolves the startup language: the stored choice, else
 * `serverDefault` (the administrator's `DEFAULT_LOCALE`), else the browser.
 *
 * Only a stored value is kept: a detection result is applied without being
 * persisted, because `main.ts` runs this once before the session arrives and
 * again with the server default. Writing the browser guess to localStorage on
 * the first pass would make the second pass see a "choice" that nobody made and
 * the administrator default could never take effect.
 */
export function initLocale(serverDefault?: string): string {
  const stored = readStoredLocale();
  if (stored) {
    return applyLocale(stored, false);
  }
  if (isLocale(serverDefault)) {
    return applyLocale(serverDefault, false);
  }
  return applyLocale(matchBrowser() ?? DEFAULT_LOCALE, false);
}
