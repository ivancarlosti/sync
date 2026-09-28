<script setup lang="ts">
// StatCard — one number of the dashboard.
//
// A counter without a unit is noise, so every card carries a label and an
// optional hint (the window it covers, the accounts it describes). The tone is
// what keeps "failed" red and "succeeded" green on every screen.
import type { Component } from 'vue';

import { cn } from '@/lib/utils';

withDefaults(
  defineProps<{
    label: string;
    value: string | number;
    hint?: string;
    icon?: Component;
    tone?: 'default' | 'success' | 'warning' | 'destructive' | 'muted';
    class?: string;
  }>(),
  { tone: 'default' },
);

const TONES = {
  default: 'text-foreground',
  success: 'text-success',
  warning: 'text-warning',
  destructive: 'text-destructive',
  muted: 'text-muted-foreground',
} as const;
</script>

<template>
  <div
    :class="
      cn('rounded-xl border border-border bg-card p-4 text-card-foreground shadow-sm', $props.class)
    "
  >
    <div class="flex items-center justify-between gap-2">
      <p class="text-xs font-medium uppercase tracking-wide text-muted-foreground">{{ label }}</p>
      <component
        :is="icon"
        v-if="icon"
        class="h-4 w-4 shrink-0 text-muted-foreground"
        aria-hidden="true"
      />
    </div>
    <p :class="cn('mt-2 text-2xl font-semibold tabular-nums', TONES[tone])">{{ value }}</p>
    <p v-if="hint" class="mt-1 text-xs text-muted-foreground">{{ hint }}</p>
  </div>
</template>
