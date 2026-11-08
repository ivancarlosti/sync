<script setup lang="ts">
// ManagementLayout — the section shell of Admin operations.
//
// The catalogue of provider-backed administrative tasks and the auditable journal
// of every task share one title block and a sub-navigation instead of repeating
// the same header twice (see views/management/*).
import { ScrollText, Wrench } from '@lucide/vue';
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { RouterView, useRoute } from 'vue-router';

import PageHeader from '@/components/PageHeader.vue';
import { cn } from '@/lib/utils';

const { t } = useI18n();
const route = useRoute();

const tabs = computed(() => [
  { name: 'management-operations', label: t('management.operations'), icon: Wrench },
  { name: 'management-activity', label: t('management.activity'), icon: ScrollText },
]);
</script>

<template>
  <div class="space-y-6">
    <PageHeader :title="t('management.title')" :subtitle="t('management.subtitle')" />

    <nav class="flex flex-wrap gap-1 rounded-lg bg-muted p-1" :aria-label="t('management.title')">
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
