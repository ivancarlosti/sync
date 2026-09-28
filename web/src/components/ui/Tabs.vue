<script setup lang="ts">
// Tabs — wraps the reka-ui tabs primitives.
//
// The job editor is the reason it exists: details / source / destination /
// options stay one screen (so the folder browser never loses the form) and each
// panel is a named slot matching the tab value, which keeps the markup flat.
import { TabsContent, TabsList, TabsRoot, TabsTrigger } from 'reka-ui';
import type { Component } from 'vue';

import { cn } from '@/lib/utils';

/** TabItem is one tab: its value doubles as the slot name. */
export interface TabItem {
  value: string;
  label: string;
  icon?: Component;
}

withDefaults(
  defineProps<{
    items: TabItem[];
    listClass?: string;
    contentClass?: string;
    class?: string;
  }>(),
  {},
);

const model = defineModel<string>({ required: true });
</script>

<template>
  <TabsRoot v-model="model" :class="cn('flex flex-col gap-4', $props.class)">
    <TabsList
      :class="
        cn(
          'inline-flex h-9 w-full flex-wrap items-center justify-start gap-1 rounded-lg bg-muted p-1 text-muted-foreground',
          listClass,
        )
      "
    >
      <TabsTrigger
        v-for="item in items"
        :key="item.value"
        :value="item.value"
        :class="
          cn(
            'inline-flex items-center gap-2 whitespace-nowrap rounded-md px-3 py-1 text-sm font-medium transition-colors data-[state=active]:bg-background data-[state=active]:text-foreground data-[state=active]:shadow-sm',
          )
        "
      >
        <component :is="item.icon" v-if="item.icon" class="h-4 w-4" aria-hidden="true" />
        {{ item.label }}
      </TabsTrigger>
    </TabsList>

    <TabsContent
      v-for="item in items"
      :key="item.value"
      :value="item.value"
      :class="cn('focus-visible:outline-none', contentClass)"
    >
      <slot :name="item.value" />
    </TabsContent>
  </TabsRoot>
</template>
