<script setup lang="ts">
// AppToaster — the single place where the result of an action appears.
//
// Messages are pushed to the feedback store by the screens (or by `useAction`)
// and rendered here, stacked bottom-end so they follow the reading direction of
// the active locale. The container is `aria-live="polite"`: a screen reader
// announces the outcome without stealing focus from the form.
import { X } from '@lucide/vue';
import { useI18n } from 'vue-i18n';

import Alert from '@/components/ui/Alert.vue';
import { useFeedbackStore, type FeedbackTone } from '@/stores/feedback';

const feedback = useFeedbackStore();
const { t } = useI18n();

const TONES: Record<FeedbackTone, 'success' | 'destructive' | 'info'> = {
  success: 'success',
  error: 'destructive',
  info: 'info',
};
</script>

<template>
  <div
    class="pointer-events-none fixed bottom-4 end-4 z-[60] flex w-[min(26rem,calc(100vw-2rem))] flex-col gap-2"
    aria-live="polite"
  >
    <Alert
      v-for="item in feedback.items"
      :key="item.id"
      :tone="TONES[item.tone]"
      :title="item.message"
      class="pointer-events-auto bg-card shadow-lg"
    >
      <template #footer>
        <div class="flex items-end justify-between gap-3">
          <p v-if="item.detail" class="break-anywhere numeric text-xs text-muted-foreground">
            {{ item.detail }}
          </p>
          <button
            type="button"
            class="ms-auto rounded p-0.5 text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
            :aria-label="t('common.close')"
            @click="feedback.dismiss(item.id)"
          >
            <X class="h-3.5 w-3.5" aria-hidden="true" />
          </button>
        </div>
      </template>
    </Alert>
  </div>
</template>
