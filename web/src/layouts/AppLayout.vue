<script setup lang="ts">
// AppLayout — the shell every signed-in screen renders inside.
//
// It owns the three things that must not be duplicated per view: the header, the
// single toast stack, and the footer that pins the running version (so a bug
// report can start with "which build?").
import { computed } from 'vue';
import { RouterView } from 'vue-router';
import { useI18n } from 'vue-i18n';

import AppHeader from '@/components/AppHeader.vue';
import AppToaster from '@/components/AppToaster.vue';
import { useAuthStore } from '@/stores/auth';

const auth = useAuthStore();
const { t } = useI18n();

const build = computed(() => auth.version);
const name = computed(() => build.value?.name ?? 'Sync');
</script>

<template>
  <div class="flex min-h-screen flex-col">
    <a
      href="#content"
      class="sr-only focus:not-sr-only focus:absolute focus:start-4 focus:top-3 focus:z-50 focus:rounded-md focus:bg-primary focus:px-3 focus:py-1.5 focus:text-sm focus:text-primary-foreground"
    >
      {{ t('nav.skipToContent') }}
    </a>

    <AppHeader />

    <main id="content" class="container flex-1 py-6">
      <RouterView v-slot="{ Component }">
        <component :is="Component" />
      </RouterView>
    </main>

    <footer class="border-t border-border py-4">
      <div class="container flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
        <span>{{ name }} {{ build?.version ?? '' }}</span>
        <span v-if="build?.commit && build.commit !== 'unknown'" class="numeric">{{ build.commit.slice(0, 7) }}</span>
      </div>
    </footer>

    <AppToaster />
  </div>
</template>
