<script setup lang="ts">
// Badge — a small status chip. `tone` carries the meaning, so a "failed" label
// is red on every screen without each view picking a colour.
import { cva, type VariantProps } from 'class-variance-authority';
import { computed } from 'vue';

import { cn } from '@/lib/utils';

const badgeVariants = cva(
  'inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 text-xs font-medium',
  {
    variants: {
      tone: {
        default: 'border-transparent bg-primary/10 text-primary',
        secondary: 'border-transparent bg-secondary text-secondary-foreground',
        outline: 'border-border text-foreground',
        success: 'border-transparent bg-success/15 text-success',
        warning: 'border-transparent bg-warning/20 text-warning',
        destructive: 'border-transparent bg-destructive/15 text-destructive',
        muted: 'border-transparent bg-muted text-muted-foreground',
      },
    },
    defaultVariants: { tone: 'default' },
  },
);

type BadgeTone = VariantProps<typeof badgeVariants>;

const props = defineProps<{
  tone?: BadgeTone['tone'];
  class?: string;
}>();

const classes = computed(() => cn(badgeVariants({ tone: props.tone }), props.class));
</script>

<template>
  <span :class="classes">
    <slot />
  </span>
</template>
