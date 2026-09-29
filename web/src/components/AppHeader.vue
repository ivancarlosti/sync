<script setup lang="ts">
// AppHeader — the persistent navigation of the shell.
//
// Every screen is one click away, the current section is marked for both sighted
// users (background) and assistive technology (`aria-current`), and the language,
// theme and account controls sit together on the trailing edge. Below `md` the
// links collapse into a panel behind the menu button: six destinations do not fit
// a phone width, and hiding them behind "more" would be worse than one tap.
import { Menu, X } from '@lucide/vue';
import { computed, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute } from 'vue-router';

import LanguageSwitcher from '@/components/LanguageSwitcher.vue';
import ThemeToggle from '@/components/ThemeToggle.vue';
import UserMenu from '@/components/UserMenu.vue';
import Button from '@/components/ui/Button.vue';
import { cn } from '@/lib/utils';
import { useAuthStore } from '@/stores/auth';

interface NavLink {
  /** route is the route name the link navigates to. */
  route: string;
  /** label is the i18n key under `nav`. */
  label: string;
  /** matches lists the route names that keep this link active. */
  matches: string[];
}

const LINKS: NavLink[] = [
  { route: 'dashboard', label: 'nav.dashboard', matches: ['dashboard'] },
  { route: 'accounts', label: 'nav.accounts', matches: ['accounts'] },
  { route: 'jobs', label: 'nav.jobs', matches: ['jobs', 'job-new', 'job-edit'] },
  { route: 'runs', label: 'nav.runs', matches: ['runs'] },
  { route: 'notifications', label: 'nav.notifications', matches: ['notifications'] },
  {
    route: 'admin-providers',
    label: 'nav.admin',
    matches: ['admin-providers', 'admin-guide', 'admin-settings', 'admin-about'],
  },
];

const auth = useAuthStore();
const route = useRoute();
const { t } = useI18n();
const mobileOpen = ref(false);

const name = computed(() => auth.version?.name ?? 'Sync');

function isActive(link: NavLink): boolean {
  return link.matches.includes(String(route.name ?? ''));
}

// The mobile panel closes on navigation, like a drawer should.
watch(
  () => route.fullPath,
  () => {
    mobileOpen.value = false;
  },
);
</script>

<template>
  <header class="sticky top-0 z-40 w-full border-b border-border bg-background/95 backdrop-blur">
    <div class="container flex h-14 items-center gap-2">
      <RouterLink :to="{ name: 'dashboard' }" class="flex items-center gap-2 rounded-md pe-2 font-semibold">
        <img src="/logo.svg" alt="" class="h-7 w-7" />
        <span class="text-base tracking-tight">{{ name }}</span>
      </RouterLink>

      <nav class="hidden items-center gap-1 md:flex" :aria-label="t('nav.primaryNavigation')">
        <RouterLink
          v-for="link in LINKS"
          :key="link.route"
          :to="{ name: link.route }"
          :aria-current="isActive(link) ? 'page' : undefined"
          :class="
            cn(
              'rounded-md px-3 py-1.5 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground',
              isActive(link) && 'bg-accent text-accent-foreground',
            )
          "
        >
          {{ t(link.label) }}
        </RouterLink>
      </nav>

      <div class="ms-auto flex items-center gap-1">
        <LanguageSwitcher />
        <ThemeToggle />
        <UserMenu />
        <Button
          class="md:hidden"
          variant="ghost"
          size="icon"
          :aria-label="t('nav.openMenu')"
          :aria-expanded="mobileOpen"
          aria-controls="mobile-navigation"
          @click="mobileOpen = !mobileOpen"
        >
          <X v-if="mobileOpen" class="h-4 w-4" aria-hidden="true" />
          <Menu v-else class="h-4 w-4" aria-hidden="true" />
        </Button>
      </div>
    </div>

    <nav
      v-if="mobileOpen"
      id="mobile-navigation"
      class="border-t border-border bg-background px-4 py-2 md:hidden"
      :aria-label="t('nav.primaryNavigation')"
    >
      <ul class="flex flex-col">
        <li v-for="link in LINKS" :key="link.route">
          <RouterLink
            :to="{ name: link.route }"
            :aria-current="isActive(link) ? 'page' : undefined"
            :class="
              cn(
                'flex items-center gap-2 rounded-md px-3 py-2 text-sm font-medium text-muted-foreground hover:bg-accent hover:text-accent-foreground',
                isActive(link) && 'bg-accent text-accent-foreground',
              )
            "
          >
            {{ t(link.label) }}
          </RouterLink>
        </li>
      </ul>
    </nav>
  </header>
</template>
