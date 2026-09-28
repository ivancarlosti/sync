<script setup lang="ts">
// PageHeader — the title block of a screen: what it is, what it is for, and the
// primary action on the trailing edge.
defineProps<{
  title: string;
  subtitle?: string;
}>();
</script>

<template>
  <div class="flex flex-wrap items-start justify-between gap-4">
    <div class="space-y-1">
      <h1 class="text-xl font-semibold tracking-tight">{{ title }}</h1>
      <p v-if="subtitle" class="max-w-2xl text-sm text-muted-foreground">{{ subtitle }}</p>
    </div>
    <div v-if="$slots.actions" class="flex flex-wrap items-center gap-2">
      <slot name="actions" />
    </div>
  </div>
</template>
