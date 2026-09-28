// Transient feedback (toast) store.
//
// Screens never render a banner for themselves: they call `success`/`fail` and
// the single <AppToaster /> in the shell shows the result, so a message looks the
// same everywhere and survives a dialog closing on success.
import { defineStore } from 'pinia';
import { ref } from 'vue';

/** FeedbackTone selects the colour and the icon of a message. */
export type FeedbackTone = 'success' | 'error' | 'info';

/** Feedback is one message on screen. */
export interface Feedback {
  id: number;
  tone: FeedbackTone;
  /** message is an already translated sentence. */
  message: string;
  /** detail carries the technical reason of a failure, when there is one. */
  detail?: string;
}

/** AUTO_DISMISS_MS is how long a message stays before it fades out. */
const AUTO_DISMISS_MS: Record<FeedbackTone, number> = {
  success: 6000,
  info: 8000,
  // A failure carries a reason the operator may need to read carefully.
  error: 15000,
};

export const useFeedbackStore = defineStore('feedback', () => {
  const items = ref<Feedback[]>([]);
  let sequence = 0;

  /** dismiss removes one message. */
  function dismiss(id: number): void {
    items.value = items.value.filter((item) => item.id !== id);
  }

  /** push shows a message and schedules its removal. */
  function push(tone: FeedbackTone, message: string, detail?: string): number {
    sequence += 1;
    const id = sequence;
    items.value = [...items.value, { id, tone, message, detail: detail || undefined }];
    window.setTimeout(() => dismiss(id), AUTO_DISMISS_MS[tone]);
    return id;
  }

  const success = (message: string): number => push('success', message);
  const info = (message: string): number => push('info', message);
  const fail = (message: string, detail?: string): number => push('error', message, detail);

  /** clear drops every message (used when the session changes). */
  function clear(): void {
    items.value = [];
  }

  return { items, push, success, info, fail, dismiss, clear };
});
