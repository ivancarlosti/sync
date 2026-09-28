// useAction — one mutation at a time, with the standard feedback.
//
// Every screen in the app runs the same three steps around a write: block the
// button, report the outcome, unblock. This composable is that pattern so a view
// never forgets the `finally` (which is what leaves a spinner spinning forever
// when a request fails).
import { ref } from 'vue';

import { failureDetail } from '@/lib/feedback';
import { useFeedbackStore } from '@/stores/feedback';

/** ActionMessages are the translated sentences of one mutation. */
export interface ActionMessages {
  /** success is shown as a toast when the call resolves. */
  success?: string;
  /** failure is shown when the call rejects. */
  failure: string;
}

export function useAction() {
  const busy = ref(false);
  const feedback = useFeedbackStore();

  /**
   * run executes `task` with the standard feedback and returns its result, or
   * `null` when it failed — so a caller can simply `if (!result) return;` and
   * never has to catch anything itself.
   */
  async function run<T>(task: () => Promise<T>, messages: ActionMessages): Promise<T | null> {
    busy.value = true;
    try {
      const result = await task();
      if (messages.success) {
        feedback.success(messages.success);
      }
      return result;
    } catch (failure) {
      feedback.fail(messages.failure, failureDetail(failure));
      return null;
    } finally {
      busy.value = false;
    }
  }

  return { busy, run };
}
