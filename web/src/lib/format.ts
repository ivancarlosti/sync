// Locale aware formatting helpers.
//
// Every date, duration and byte size the SPA renders goes through this module so
// the numbers follow the active locale (including the RTL locales) and the
// screens never hand-roll a format string.
import { type Composer } from 'vue-i18n';

/** BYTE_UNITS keeps the scale of `formatBytes` in one place. */
const BYTE_UNITS = ['byte', 'kilobyte', 'megabyte', 'gigabyte', 'terabyte', 'petabyte'] as const;

/** formatBytes renders a size with the unit the locale expects (KB vs Ko). */
export function formatBytes(bytes: number, locale: string): string {
  if (!Number.isFinite(bytes) || bytes <= 0) {
    return '0';
  }
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  // Whole numbers stay clean; small values keep one decimal.
  const digits = value < 10 && unit > 0 ? 1 : 0;
  return `${new Intl.NumberFormat(locale, {
    maximumFractionDigits: digits,
    minimumFractionDigits: 0,
  }).format(value)} ${BYTE_UNITS[unit]}`;
}

/** formatDateTime renders a timestamp in the locale and the browser time zone. */
export function formatDateTime(value: string | null | undefined, locale: string): string {
  const date = parse(value);
  if (!date) {
    return '—';
  }
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(date);
}

/** formatDate renders a timestamp without the time of day. */
export function formatDate(value: string | null | undefined, locale: string): string {
  const date = parse(value);
  if (!date) {
    return '—';
  }
  return new Intl.DateTimeFormat(locale, { dateStyle: 'medium' }).format(date);
}

/**
 * formatRelative renders "in 5 minutes" / "2 hours ago" using the relative-time
 * format of the locale, which is what the dashboard shows for the next run.
 */
export function formatRelative(value: string | null | undefined, locale: string): string {
  const date = parse(value);
  if (!date) {
    return '—';
  }
  const deltaSeconds = (date.getTime() - Date.now()) / 1000;
  const formatter = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' });
  const steps: Array<[Intl.RelativeTimeFormatUnit, number]> = [
    ['year', 60 * 60 * 24 * 365],
    ['month', 60 * 60 * 24 * 30],
    ['day', 60 * 60 * 24],
    ['hour', 60 * 60],
    ['minute', 60],
    ['second', 1],
  ];
  for (const [unit, seconds] of steps) {
    if (Math.abs(deltaSeconds) >= seconds || unit === 'second') {
      return formatter.format(Math.round(deltaSeconds / seconds), unit);
    }
  }
  return '—';
}

/** formatDuration turns the millisecond durations of a run into a short text. */
export function formatDuration(milliseconds: number, t: Composer['t']): string {
  if (!milliseconds || milliseconds <= 0) {
    return t('common.notAvailable');
  }
  const totalSeconds = Math.round(milliseconds / 1000);
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  if (hours > 0) {
    return t('common.durationHours', { hours, minutes });
  }
  if (minutes > 0) {
    return t('common.durationMinutes', { minutes, seconds });
  }
  return t('common.durationSeconds', { seconds });
}

/** formatNumber renders a plain count with the locale grouping. */
export function formatNumber(value: number, locale: string): string {
  return new Intl.NumberFormat(locale).format(Number.isFinite(value) ? value : 0);
}

/** parse tolerates the empty strings the API omits through `omitempty`. */
function parse(value: string | null | undefined): Date | null {
  if (!value) {
    return null;
  }
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? null : date;
}
