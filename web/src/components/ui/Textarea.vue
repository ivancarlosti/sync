<script setup lang="ts">
// Textarea — free text field, used by the exclusion patterns and the JSON
// configuration of a notification channel (a monospace variant is opt-in
// through `mono`).
import { cn } from '@/lib/utils';

withDefaults(
  defineProps<{
    id?: string;
    name?: string;
    placeholder?: string;
    rows?: number;
    required?: boolean;
    disabled?: boolean;
    readonly?: boolean;
    spellcheck?: boolean;
    mono?: boolean;
    class?: string;
  }>(),
  { rows: 4, spellcheck: false },
);

const model = defineModel<string>({ default: '' });
</script>

<template>
  <textarea
    :id="id"
    v-model="model"
    :name="name"
    :rows="rows"
    :placeholder="placeholder"
    :required="required"
    :disabled="disabled"
    :readonly="readonly"
    :spellcheck="spellcheck"
    :class="
      cn(
        'flex min-h-[80px] w-full rounded-md border border-input bg-background px-3 py-2 text-sm shadow-sm placeholder:text-muted-foreground disabled:cursor-not-allowed disabled:opacity-60',
        mono && 'font-mono text-xs',
        $props.class,
      )
    "
  />
</template>
