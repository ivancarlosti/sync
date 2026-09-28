<script setup lang="ts">
// DropdownMenu — the small menu behind the account, theme and language buttons.
//
// The items are plain data (label + handler value) because all three callers
// build their list from a store; a caller that needs richer rows can still use
// the `item` slot, which receives the item and its own `<button>`.
import {
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuPortal,
  DropdownMenuRoot,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from 'reka-ui';
import { Check } from '@lucide/vue';
import type { Component } from 'vue';

import { cn } from '@/lib/utils';

/** DropdownItem is one row of the menu. */
export interface DropdownItem {
  /** value is what `select` emits; it may also be an i18n key for a label. */
  value: string;
  label: string;
  icon?: Component;
  /** checked renders a leading check mark (language and theme menus). */
  checked?: boolean;
  disabled?: boolean;
  /** tone destructive paints the row in the danger colour (sign out). */
  tone?: 'default' | 'destructive';
  /** separatorBefore draws a rule above the row. */
  separatorBefore?: boolean;
}

withDefaults(
  defineProps<{
    items: DropdownItem[];
    label?: string;
    align?: 'start' | 'center' | 'end';
    class?: string;
    contentClass?: string;
  }>(),
  { align: 'end' },
);

const emit = defineEmits<{ select: [value: string] }>();
</script>

<template>
  <DropdownMenuRoot>
    <DropdownMenuTrigger as-child>
      <slot name="trigger" />
    </DropdownMenuTrigger>

    <DropdownMenuPortal>
      <DropdownMenuContent
        :align="align"
        :side-offset="6"
        :class="
          cn(
            'z-50 min-w-48 overflow-hidden rounded-lg border border-border bg-popover p-1 text-popover-foreground shadow-md',
            'data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 data-[state=closed]:zoom-out-95 data-[state=open]:zoom-in-95',
            contentClass,
          )
        "
      >
        <DropdownMenuLabel v-if="label" class="px-2 py-1.5 text-xs font-medium text-muted-foreground">
          {{ label }}
        </DropdownMenuLabel>

        <template v-for="item in items" :key="item.value">
          <DropdownMenuSeparator v-if="item.separatorBefore" class="-mx-1 my-1 h-px bg-border" />
          <DropdownMenuItem
            :disabled="item.disabled"
            :class="
              cn(
                'flex cursor-pointer select-none items-center gap-2 rounded-md px-2 py-1.5 text-sm outline-none transition-colors data-[highlighted]:bg-accent data-[disabled]:pointer-events-none data-[disabled]:opacity-50',
                item.tone === 'destructive' && 'text-destructive data-[highlighted]:bg-destructive/10',
              )
            "
            @select="emit('select', item.value)"
          >
            <Check v-if="item.checked" class="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
            <span v-else-if="item.icon" class="flex h-3.5 w-3.5 shrink-0 items-center justify-center">
              <component :is="item.icon" class="h-3.5 w-3.5" aria-hidden="true" />
            </span>
            <span class="flex-1 truncate">{{ item.label }}</span>
          </DropdownMenuItem>
        </template>
      </DropdownMenuContent>
    </DropdownMenuPortal>
  </DropdownMenuRoot>
</template>
