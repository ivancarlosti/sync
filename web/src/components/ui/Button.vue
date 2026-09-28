<script setup lang="ts">
// Button — the single action primitive of the app.
//
// Variants live in a `cva` map so every screen gets the same hover, focus and
// disabled behaviour, and `class` is merged last so a caller can still widen a
// button (`class="w-full"`) without fighting the defaults.
import { cva, type VariantProps } from 'class-variance-authority';
import { LoaderCircle } from '@lucide/vue';
import { computed } from 'vue';

import { cn } from '@/lib/utils';

const buttonVariants = cva(
  'inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-medium transition-colors focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background disabled:pointer-events-none disabled:opacity-50',
  {
    variants: {
      variant: {
        default: 'bg-primary text-primary-foreground shadow-sm hover:bg-primary/90',
        secondary: 'bg-secondary text-secondary-foreground hover:bg-secondary/80',
        outline: 'border border-input bg-background shadow-sm hover:bg-accent hover:text-accent-foreground',
        ghost: 'hover:bg-accent hover:text-accent-foreground',
        destructive: 'bg-destructive text-destructive-foreground shadow-sm hover:bg-destructive/90',
        link: 'text-primary underline-offset-4 hover:underline',
      },
      size: {
        sm: 'h-8 px-3 text-xs',
        default: 'h-9 px-4',
        lg: 'h-11 px-6 text-base',
        icon: 'h-9 w-9',
      },
    },
    defaultVariants: { variant: 'default', size: 'default' },
  },
);

type ButtonVariant = VariantProps<typeof buttonVariants>;

const props = withDefaults(
  defineProps<{
    variant?: ButtonVariant['variant'];
    size?: ButtonVariant['size'];
    /** as renders the control as another element (`a`, `label`, ...). */
    as?: string;
    type?: 'button' | 'submit' | 'reset';
    disabled?: boolean;
    /** loading swaps the leading icon for a spinner and blocks clicks. */
    loading?: boolean;
    class?: string;
  }>(),
  { as: 'button', type: 'button' },
);

const classes = computed(() =>
  cn(buttonVariants({ variant: props.variant, size: props.size }), props.class),
);
</script>

<template>
  <component
    :is="as"
    :type="as === 'button' ? type : undefined"
    :class="classes"
    :disabled="as === 'button' ? disabled || loading : undefined"
    :aria-busy="loading || undefined"
    :aria-disabled="as !== 'button' && (disabled || loading) ? 'true' : undefined"
  >
    <LoaderCircle v-if="loading" class="h-4 w-4 shrink-0 animate-spin" aria-hidden="true" />
    <slot />
  </component>
</template>
