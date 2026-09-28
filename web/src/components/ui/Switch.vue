<script setup lang="ts">
// Switch — the accessible on/off toggle of the reka-ui primitives, styled with
// the app tokens. The thumb translates to the logical `end` position, so the
// switch mirrors with `dir="rtl"` like the rest of the layout.
import { SwitchRoot, SwitchThumb } from 'reka-ui';

import { cn } from '@/lib/utils';

const props = defineProps<{
  id?: string;
  name?: string;
  disabled?: boolean;
  label?: string;
  class?: string;
}>();

const model = defineModel<boolean>({ default: false });
</script>

<template>
  <SwitchRoot
    :id="props.id"
    v-model="model"
    :name="props.name"
    :disabled="props.disabled"
    :aria-label="props.label"
    :class="
      cn(
        'peer inline-flex h-5 w-9 shrink-0 items-center rounded-full border-2 border-transparent transition-colors focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background disabled:cursor-not-allowed disabled:opacity-50',
        'data-[state=checked]:bg-primary data-[state=unchecked]:bg-muted',
        props.class,
      )
    "
  >
    <SwitchThumb
      class="pointer-events-none block h-4 w-4 rounded-full bg-background shadow-lg ring-0 transition-transform rtl:-translate-x-4 data-[state=unchecked]:translate-x-0 data-[state=checked]:translate-x-4"
    />
  </SwitchRoot>
</template>
