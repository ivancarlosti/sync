// Form value helpers.
//
// The UI primitives keep raw strings (an `<input type="number">` holds `''` while
// it is empty, so a value the operator never touched is not silently submitted as
// zero). Every conversion of that raw text back into an API payload lives here so
// two screens cannot disagree about what an empty field means.

/** MASKED_SECRET is what the API returns in place of a stored secret. */
export const MASKED_SECRET = '********';

/** asNumber converts a bound field into a number, with a fallback for blanks. */
export function asNumber(value: string | number | null | undefined, fallback = 0): number {
  if (typeof value === 'number') {
    return Number.isFinite(value) ? value : fallback;
  }
  const text = (value ?? '').trim();
  if (text === '') {
    return fallback;
  }
  const parsed = Number(text);
  return Number.isFinite(parsed) ? parsed : fallback;
}

/** asText trims a bound field, turning a blank into an empty string. */
export function asText(value: string | number | null | undefined): string {
  if (value === null || value === undefined) {
    return '';
  }
  return String(value).trim();
}

/** splitLines turns a textarea into the list the API expects. */
export function splitLines(value: string): string[] {
  return value
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line !== '');
}

/** joinLines is the inverse of `splitLines`, for filling a textarea. */
export function joinLines(values: string[] | null | undefined): string {
  return (values ?? []).join('\n');
}

/** isMasked reports whether a value is the secret placeholder of the API. */
export function isMasked(value: unknown): boolean {
  return typeof value === 'string' && value.trim() === MASKED_SECRET;
}

/** JsonResult is the outcome of parsing a textarea that holds JSON. */
export type JsonResult =
  | { ok: true; value: Record<string, unknown> | null }
  | { ok: false; message: string };

/**
 * parseJsonObject parses an optional JSON object field (`headers` of a webhook,
 * for instance). An empty textarea is a valid "no object" and never an error.
 */
export function parseJsonObject(raw: string): JsonResult {
  const text = raw.trim();
  if (text === '') {
    return { ok: true, value: null };
  }
  try {
    const parsed: unknown = JSON.parse(text);
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return { ok: false, message: 'a JSON object is required' };
    }
    return { ok: true, value: parsed as Record<string, unknown> };
  } catch (error) {
    return { ok: false, message: error instanceof Error ? error.message : String(error) };
  }
}

/** formatJson renders a value as the indented JSON a textarea shows. */
export function formatJson(value: unknown): string {
  if (value === null || value === undefined) {
    return '';
  }
  if (typeof value === 'string') {
    return value;
  }
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return '';
  }
}
