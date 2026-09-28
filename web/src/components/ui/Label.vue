<script setup lang="ts">
// Label — a form label that reflects the disabled state of its control.
import { cn } from '@/lib/utils';

defineProps<{
  for?: string;
  /** hint renders a muted sentence under the label. */
  hint?: string;
  required?: boolean;
  class?: string;
}>();
</script>

<template>
  <div :class="cn('space-y-1.5', $props.class)">
    <label
      :for="$props.for"
      class="text-sm font-medium leading-none text-foreground peer-disabled:cursor-not-allowed peer-disabled:opacity-70"
    >
      <slot />
      <span v-if="required" class="text-destructive" aria-hidden="true">*</span>
    </label>
    <p v-if="hint" class="text-xs text-muted-foreground">{{ hint }}</p>
  </div>
</template>
