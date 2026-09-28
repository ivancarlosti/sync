<script setup lang="ts">
// Select — a styled native `<select>`.
//
// The screens filter by provider, status and locale and the option sets are
// small and known, so the platform control (keyboard behaviour, mobile picker,
// RTL mirroring) is worth more here than a custom listbox.
import { ChevronsUpDown } from '@lucide/vue';

import { cn } from '@/lib/utils';

export interface SelectOption {
  value: string | number;
  label: string;
  disabled?: boolean;
}

withDefaults(
  defineProps<{
    id?: string;
    name?: string;
    options: SelectOption[];
    placeholder?: string;
    required?: boolean;
    disabled?: boolean;
    class?: string;
  }>(),
  {},
);

const model = defineModel<string | number>({ default: '' });
</script>

<template>
  <div class="relative">
    <select
      :id="id"
      v-model="model"
      :name="name"
      :required="required"
      :disabled="disabled"
      :class="
        cn(
          'h-9 w-full appearance-none rounded-md border border-input bg-background ps-3 pe-9 text-sm shadow-sm disabled:cursor-not-allowed disabled:opacity-60',
          $props.class,
        )
      "
    >
      <option v-if="placeholder" value="" disabled>{{ placeholder }}</option>
      <option
        v-for="option in options"
        :key="option.value"
        :value="option.value"
        :disabled="option.disabled"
      >
        {{ option.label }}
      </option>
    </select>
    <ChevronsUpDown
      class="pointer-events-none absolute end-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
      aria-hidden="true"
    />
  </div>
</template>
