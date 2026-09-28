<script setup lang="ts">
// Card — the panel every screen is built from. The named slots keep the markup
// of a section identical everywhere: a title row (with room for actions) and a
// body, plus an optional footer for form buttons.
import { cn } from '@/lib/utils';

defineProps<{
  title?: string;
  description?: string;
  class?: string;
  headerClass?: string;
  contentClass?: string;
  footerClass?: string;
}>();
</script>

<template>
  <section :class="cn('rounded-xl border border-border bg-card text-card-foreground shadow-sm', $props.class)">
    <header
      v-if="title || description || $slots.title || $slots.actions"
      :class="cn('flex flex-wrap items-start justify-between gap-3 p-5 pb-0', headerClass)"
    >
      <div class="space-y-1">
        <h2 v-if="title || $slots.title" class="text-base font-semibold leading-none tracking-tight">
          <slot name="title">{{ title }}</slot>
        </h2>
        <p v-if="description || $slots.description" class="text-sm text-muted-foreground">
          <slot name="description">{{ description }}</slot>
        </p>
      </div>
      <div v-if="$slots.actions" class="flex flex-wrap items-center gap-2">
        <slot name="actions" />
      </div>
    </header>

    <div :class="cn('p-5', contentClass)">
      <slot />
    </div>

    <footer
      v-if="$slots.footer"
      :class="cn('flex flex-wrap items-center justify-end gap-2 border-t border-border p-5', footerClass)"
    >
      <slot name="footer" />
    </footer>
  </section>
</template>
