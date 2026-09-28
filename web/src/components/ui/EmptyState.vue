<script setup lang="ts">
// EmptyState — what a list shows when it has no rows.
//
// Every list in the app has an empty state with an action ("create the first
// job", "connect an account"), because an empty table without a next step reads
// like a broken screen.
import type { Component } from 'vue';

import { cn } from '@/lib/utils';

withDefaults(
  defineProps<{
    icon?: Component;
    title: string;
    message?: string;
    class?: string;
  }>(),
  {},
);
</script>

<template>
  <div :class="cn('flex flex-col items-center justify-center gap-3 px-6 py-12 text-center', $props.class)">
    <span class="flex h-11 w-11 items-center justify-center rounded-full bg-muted text-muted-foreground">
      <component :is="icon" v-if="icon" class="h-5 w-5" aria-hidden="true" />
    </span>
    <div class="space-y-1">
      <p class="text-sm font-medium">{{ title }}</p>
      <p v-if="message" class="mx-auto max-w-md text-sm text-muted-foreground">{{ message }}</p>
    </div>
    <div v-if="$slots.default" class="flex flex-wrap items-center justify-center gap-2">
      <slot />
    </div>
  </div>
</template>
