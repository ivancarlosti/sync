<script setup lang="ts">
// UserMenu — who is signed in, and the way out.
//
// The identity comes from the cached session, so the header never flashes an
// empty name; the avatar falls back to the initials of the operator when the
// provider gave no picture.
import { ChevronsUpDown, LogOut, ShieldCheck } from '@lucide/vue';
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRouter } from 'vue-router';

import Button from '@/components/ui/Button.vue';
import DropdownMenu, { type DropdownItem } from '@/components/ui/DropdownMenu.vue';
import { useAction } from '@/composables/useAction';
import { useAuthStore } from '@/stores/auth';

const auth = useAuthStore();
const router = useRouter();
const { t } = useI18n();
const { run } = useAction();

const items = computed<DropdownItem[]>(() => [
  {
    value: 'sign-out',
    label: t('nav.signOut'),
    icon: LogOut,
    tone: 'destructive',
    separatorBefore: true,
  },
]);

const initials = computed(() => {
  const name = auth.displayName;
  if (!name) {
    return '?';
  }
  return name
    .split(/[\s@.]+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]?.toUpperCase() ?? '')
    .join('');
});

async function select(value: string): Promise<void> {
  if (value !== 'sign-out') {
    return;
  }
  // signOut clears the cookie and the local session even when the request fails
  // (the operator asked to leave), so the navigation below always runs.
  await run(() => auth.signOut(), { failure: t('auth.logoutFailed') });
  await router.push({ name: 'login' });
}
</script>

<template>
  <DropdownMenu :items="items" align="end" @select="select">
    <template #trigger>
      <Button variant="ghost" class="gap-2 px-2" :aria-label="auth.displayName || t('nav.openMenu')">
        <img
          v-if="auth.operator?.picture"
          :src="auth.operator.picture"
          alt=""
          class="h-6 w-6 rounded-full object-cover"
          referrerpolicy="no-referrer"
        />
        <span
          v-else
          class="flex h-6 w-6 items-center justify-center rounded-full bg-primary/10 text-xs font-semibold text-primary"
          aria-hidden="true"
        >
          {{ initials }}
        </span>
        <span class="hidden max-w-40 truncate text-sm sm:inline">{{ auth.displayName }}</span>
        <ShieldCheck v-if="auth.operator?.admin" class="h-3.5 w-3.5 text-success" aria-hidden="true" />
        <ChevronsUpDown class="h-3.5 w-3.5 text-muted-foreground" aria-hidden="true" />
      </Button>
    </template>
  </DropdownMenu>
</template>
