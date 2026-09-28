<script setup lang="ts">
// AdminLayout — the section shell of Admin.
//
// Providers, Settings and About are three screens of one task ("configure this
// instance"), so they share a title block and a sub-navigation instead of
// repeating the same header three times.
import { Info, KeyRound, Settings } from '@lucide/vue';
import { computed } from 'vue';
import { RouterView, useRoute } from 'vue-router';
import { useI18n } from 'vue-i18n';

import PageHeader from '@/components/PageHeader.vue';
import { cn } from '@/lib/utils';

const { t } = useI18n();
const route = useRoute();

const tabs = computed(() => [
  { name: 'admin-providers', label: t('nav.adminProviders'), icon: KeyRound },
  { name: 'admin-settings', label: t('nav.adminSettings'), icon: Settings },
  { name: 'admin-about', label: t('nav.adminAbout'), icon: Info },
]);
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('admin.title')" :subtitle="t('admin.subtitle')" />

    <nav class="flex flex-wrap gap-1 rounded-lg bg-muted p-1" :aria-label="t('nav.admin')">
      <RouterLink
        v-for="tab in tabs"
        :key="tab.name"
        :to="{ name: tab.name }"
        :aria-current="route.name === tab.name ? 'page' : undefined"
        :class="
          cn(
            'inline-flex items-center gap-2 rounded-md px-3 py-1.5 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground',
            route.name === tab.name && 'bg-background text-foreground shadow-sm',
          )
        "
      >
        <component :is="tab.icon" class="h-4 w-4" aria-hidden="true" />
        {{ tab.label }}
      </RouterLink>
    </nav>

    <RouterView v-slot="{ Component }">
      <component :is="Component" />
    </RouterView>
  </div>
</template>
