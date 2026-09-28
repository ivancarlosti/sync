<script setup lang="ts">
// Alert — a static message block (load failures, hints, confirmations).
//
// It is deliberately presentational: the transient feedback of an action goes to
// the toast store instead, so a screen that already renders an error never also
// pops one.
import { CircleAlert, CircleCheck, CircleQuestionMark, Info, TriangleAlert } from '@lucide/vue';
import { computed } from 'vue';

import { cn } from '@/lib/utils';

type AlertTone = 'info' | 'success' | 'warning' | 'destructive' | 'muted';

const TONES: Record<AlertTone, { wrapper: string; icon: string }> = {
  info: { wrapper: 'border-primary/30 bg-primary/5 text-foreground', icon: 'text-primary' },
  success: { wrapper: 'border-success/30 bg-success/5 text-foreground', icon: 'text-success' },
  warning: { wrapper: 'border-warning/40 bg-warning/10 text-foreground', icon: 'text-warning' },
  destructive: {
    wrapper: 'border-destructive/30 bg-destructive/5 text-foreground',
    icon: 'text-destructive',
  },
  muted: { wrapper: 'border-border bg-muted/40 text-muted-foreground', icon: 'text-muted-foreground' },
};

const props = withDefaults(
  defineProps<{
    tone?: AlertTone;
    title?: string;
    /** message is the always-visible body text. */
    message?: string;
    class?: string;
  }>(),
  { tone: 'info' },
);

const ICONS = {
  info: Info,
  success: CircleCheck,
  warning: TriangleAlert,
  destructive: CircleAlert,
  muted: CircleQuestionMark,
} as const;

const icon = computed(() => ICONS[props.tone]);
const tones = computed(() => TONES[props.tone]);
</script>

<template>
  <div
    role="status"
    :class="cn('flex items-start gap-3 rounded-lg border p-3 text-sm', tones.wrapper, props.class)"
  >
    <component :is="icon" :class="cn('mt-0.5 h-4 w-4 shrink-0', tones.icon)" aria-hidden="true" />
    <div class="min-w-0 flex-1 space-y-1">
      <p v-if="title || $slots.title" class="font-medium">
        <slot name="title">{{ title }}</slot>
      </p>
      <p v-if="message || $slots.default" class="break-anywhere text-muted-foreground">
        <slot>{{ message }}</slot>
      </p>
      <slot name="footer" />
    </div>
  </div>
</template>
