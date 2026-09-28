// Provider display helpers.
//
// `google` and `microsoft` are the stable identifiers of the API (see
// internal/models/enums.go); the names below are proper nouns, so they are not
// translated — the catalogs only carry the sentences around them.
import type { ProviderName } from '@/lib/api';

/** PROVIDERS is the display order of the provider list. */
export const PROVIDERS: ProviderName[] = ['google', 'microsoft'];

const LABELS: Record<ProviderName, string> = {
  google: 'Google Drive',
  microsoft: 'Microsoft 365',
};

/** providerLabel renders a provider identifier for the interface. */
export function providerLabel(provider: string): string {
  return LABELS[provider as ProviderName] ?? provider;
}

/** isProvider narrows an arbitrary string to a known provider. */
export function isProvider(value: string): value is ProviderName {
  return value === 'google' || value === 'microsoft';
}
