// Account display helpers.
//
// An account is named by the address the provider reports, never by the display
// name alone: a name is only unique inside one tenant, and two Microsoft 365
// organisations (or a Google and a Microsoft account) can hold the same person.
// The provider prefix says which service the row belongs to, because the same
// person often keeps both connected.
import type { ConnectedAccount } from '@/lib/api';
import { providerLabel } from '@/lib/providers';

/**
 * accountLabel names one account in a select, a table cell or a dialog: the
 * provider followed by the address, which is what tells two accounts of the same
 * name apart. It falls back to the display name, then to the raw provider id, so
 * a profile the provider returned without an address still renders something.
 */
export function accountLabel(account: ConnectedAccount): string {
  const address = account.email || account.display_name || account.provider_account_id;
  return `${providerLabel(account.provider)} · ${address}`;
}
