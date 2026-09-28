<script setup lang="ts">
// LanguageSwitcher — the seven catalogs of the build.
//
// Each language is listed in its own script (so an Arabic reader sees العربية,
// not "Arabic"), the active one carries a check mark, and choosing one persists
// the value and flips `<html lang dir>` through `setLocale`.
import { Languages } from '@lucide/vue';
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import Button from '@/components/ui/Button.vue';
import DropdownMenu, { type DropdownItem } from '@/components/ui/DropdownMenu.vue';
import { LOCALES, setLocale } from '@/i18n';

const { t, locale } = useI18n();

const items = computed<DropdownItem[]>(() =>
  LOCALES.map((descriptor) => ({
    value: descriptor.code,
    label: descriptor.label,
    checked: descriptor.code === locale.value,
  })),
);

function select(code: string): void {
  setLocale(code);
}
</script>

<template>
  <DropdownMenu :items="items" :label="t('nav.changeLanguage')" @select="select">
    <template #trigger>
      <Button variant="ghost" size="icon" :aria-label="t('nav.changeLanguage')">
        <Languages class="h-4 w-4" aria-hidden="true" />
      </Button>
    </template>
  </DropdownMenu>
</template>
