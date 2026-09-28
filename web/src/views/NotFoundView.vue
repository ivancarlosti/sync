<script setup lang="ts">
// NotFoundView — the address that matches no route.
//
// The route is public on purpose: a mistyped link must not land on a sign-in
// screen that then fails to find the page again. The single action leads back to
// the dashboard for a signed-in operator and to the sign-in screen for everybody
// else, and the message names the address that was asked for.
import { Compass } from '@lucide/vue';
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRouter } from 'vue-router';

import Button from '@/components/ui/Button.vue';
import Card from '@/components/ui/Card.vue';
import EmptyState from '@/components/ui/EmptyState.vue';
import { useAuthStore } from '@/stores/auth';

const { t } = useI18n();
const router = useRouter();
const auth = useAuthStore();

/** destination is where the action leads, depending on the session. */
const destination = computed(() => (auth.authenticated ? { name: 'dashboard' } : { name: 'login' }));

/** fullPath is the address the message names, so a bad link stays quotable. */
const fullPath = computed(() => router.currentRoute.value.fullPath);

/** leave returns the visitor to the shell of the application. */
function leave(): void {
  void router.push(destination.value);
}
</script>

<template>
  <div class="flex min-h-screen flex-col bg-muted/40">
    <main class="flex flex-1 items-center justify-center px-4 py-10">
      <Card class="w-full max-w-lg">
        <EmptyState
          :icon="Compass"
          :title="t('errors.notFoundTitle')"
          :message="t('errors.notFoundMessage', { path: fullPath })"
        >
          <Button @click="leave">{{ t('errors.goHome') }}</Button>
        </EmptyState>
      </Card>
    </main>
  </div>
</template>
