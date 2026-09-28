<script setup lang="ts">
// ThemeToggle — light / dark / system.
//
// The trigger shows the mode that is active *now* (so "system" that resolves to
// dark shows the moon) while the menu shows the stored preference, which is what
// the operator actually chose.
import { Monitor, Moon, Sun } from '@lucide/vue';
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import Button from '@/components/ui/Button.vue';
import DropdownMenu, { type DropdownItem } from '@/components/ui/DropdownMenu.vue';
import { useThemeStore, isTheme } from '@/stores/theme';

const theme = useThemeStore();
const { t } = useI18n();

const icons = { light: Sun, dark: Moon, system: Monitor } as const;

const items = computed<DropdownItem[]>(() =>
  (['light', 'dark', 'system'] as const).map((value) => ({
    value,
    label: t(`theme.${value}`),
    icon: icons[value],
    checked: theme.preference === value,
  })),
);

const triggerIcon = computed(() => icons[theme.dark ? 'dark' : 'light']);

function select(value: string): void {
  theme.set(isTheme(value) ? value : 'system');
}
</script>

<template>
  <DropdownMenu :items="items" :label="t('nav.toggleTheme')" @select="select">
    <template #trigger>
      <Button variant="ghost" size="icon" :aria-label="t('nav.toggleTheme')">
        <component :is="triggerIcon" class="h-4 w-4" aria-hidden="true" />
      </Button>
    </template>
  </DropdownMenu>
</template>
