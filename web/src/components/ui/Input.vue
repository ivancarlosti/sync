<script setup lang="ts">
// Input — text and number field.
//
// A `type="number"` field keeps its empty state as an empty string (instead of
// turning into 0) so an untouched optional bound is not silently submitted as
// zero; `asNumber()` in `src/lib/forms.ts` is what converts it at submit time.
import { computed } from 'vue';

import { cn } from '@/lib/utils';

const props = withDefaults(
  defineProps<{
    type?: 'text' | 'password' | 'email' | 'number' | 'url' | 'search' | 'tel' | 'datetime-local';
    id?: string;
    name?: string;
    placeholder?: string;
    autocomplete?: string;
    required?: boolean;
    disabled?: boolean;
    readonly?: boolean;
    min?: number | string;
    max?: number | string;
    step?: number | string;
    /** numeric aligns digits left-to-right under RTL locales. */
    numeric?: boolean;
    class?: string;
  }>(),
  { type: 'text' },
);

const model = defineModel<string | number>({ default: '' });

const value = computed<string | number>({
  get: () => model.value,
  set: (next) => {
    if (props.type === 'number' && next !== '') {
      model.value = Number(next);
      return;
    }
    model.value = next;
  },
});
</script>

<template>
  <input
    :id="id"
    v-model="value"
    :type="type"
    :name="name"
    :placeholder="placeholder"
    :autocomplete="autocomplete"
    :required="required"
    :disabled="disabled"
    :readonly="readonly"
    :min="min"
    :max="max"
    :step="step"
    :class="
      cn(
        'flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm shadow-sm transition-colors placeholder:text-muted-foreground disabled:cursor-not-allowed disabled:opacity-60',
        numeric && 'numeric',
        $props.class,
      )
    "
  />
</template>
