<script setup lang="ts">
// Dialog — a modal built on the reka-ui primitives (focus trap, scroll lock,
// Escape to close, `aria-modal`).
//
// The screen always owns the boolean (`v-model:open="show"`), which lets a form
// close the dialog after a successful save from the same handler that submitted
// it. The body scrolls inside the panel, so a long form never grows past the
// viewport.
import {
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogOverlay,
  DialogPortal,
  DialogRoot,
  DialogTitle,
  DialogTrigger,
} from 'reka-ui';
import { X } from '@lucide/vue';
import { computed } from 'vue';

import { cn } from '@/lib/utils';

const props = withDefaults(
  defineProps<{
    title: string;
    description?: string;
    size?: 'sm' | 'md' | 'lg' | 'xl';
    /** closeLabel is the accessible name of the corner button. */
    closeLabel?: string;
    class?: string;
    contentClass?: string;
  }>(),
  { size: 'md' },
);

const open = defineModel<boolean>('open', { default: false });

const SIZES = {
  sm: 'max-w-sm',
  md: 'max-w-lg',
  lg: 'max-w-2xl',
  xl: 'max-w-4xl',
} as const;

const width = computed(() => SIZES[props.size]);
</script>

<template>
  <DialogRoot v-model:open="open">
    <DialogTrigger v-if="$slots.trigger" as-child>
      <slot name="trigger" />
    </DialogTrigger>

    <DialogPortal>
      <DialogOverlay
        class="fixed inset-0 z-50 bg-black/60 backdrop-blur-[1px] data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0"
      />
      <DialogContent
        :class="
          cn(
            'fixed left-1/2 top-1/2 z-50 flex max-h-[90vh] w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col overflow-hidden rounded-xl border border-border bg-card p-0 shadow-lg',
            'data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 data-[state=closed]:zoom-out-95 data-[state=open]:zoom-in-95',
            width,
            props.class,
          )
        "
      >
        <header class="flex items-start justify-between gap-4 border-b border-border p-5">
          <div class="space-y-1">
            <DialogTitle class="text-base font-semibold leading-none"><slot name="title">{{ title }}</slot></DialogTitle>
            <DialogDescription v-if="description || $slots.description" class="text-sm text-muted-foreground">
              <slot name="description">{{ description }}</slot>
            </DialogDescription>
          </div>
          <DialogClose
            :aria-label="closeLabel"
            class="rounded-md p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
          >
            <X class="h-4 w-4" aria-hidden="true" />
          </DialogClose>
        </header>

        <div :class="cn('min-h-0 flex-1 overflow-y-auto p-5', contentClass)">
          <slot />
        </div>

        <footer
          v-if="$slots.footer"
          class="flex flex-wrap items-center justify-end gap-2 border-t border-border bg-muted/30 p-4"
        >
          <slot name="footer" />
        </footer>
      </DialogContent>
    </DialogPortal>
  </DialogRoot>
</template>
