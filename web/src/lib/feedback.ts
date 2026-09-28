// Failure helpers shared by every screen.
//
// The API answers with a machine-readable `code` plus an English technical
// message (see internal/handlers/respond.go). The UI shows a translated sentence
// per screen and keeps the message — with its code — as the detail, so an
// operator can paste something actionable into a report without the interface
// leaking untranslated prose as its main text.
import { codeOf, messageOf } from '@/lib/api';

/** failureDetail renders the technical half of a failure. */
export function failureDetail(failure: unknown): string {
  const message = messageOf(failure);
  const code = codeOf(failure);
  if (!code || message.includes(`[${code}]`)) {
    return message;
  }
  return `${message} [${code}]`;
}
