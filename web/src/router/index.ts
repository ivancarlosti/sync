// Router.
//
// The route table is the map of the product: everything except the login screen
// lives under the authenticated shell (AppLayout), and the admin screens live
// under a second shell that only adds their sub-navigation. Views are imported
// lazily so a first paint of the dashboard never downloads the admin code.
import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router';

import { useAuthStore } from '@/stores/auth';

/** RouteName is the closed set of names the app navigates to by name. */
export type RouteName =
  | 'login'
  | 'dashboard'
  | 'accounts'
  | 'jobs'
  | 'job-new'
  | 'job-edit'
  | 'runs'
  | 'notifications'
  | 'admin-providers'
  | 'admin-settings'
  | 'admin-about'
  | 'not-found';

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/LoginView.vue'),
    meta: { public: true },
  },
  {
    path: '/',
    component: () => import('@/layouts/AppLayout.vue'),
    children: [
      {
        path: '',
        name: 'dashboard',
        component: () => import('@/views/DashboardView.vue'),
      },
      {
        path: 'accounts',
        name: 'accounts',
        component: () => import('@/views/AccountsView.vue'),
      },
      {
        path: 'jobs',
        name: 'jobs',
        component: () => import('@/views/JobsView.vue'),
      },
      {
        path: 'jobs/new',
        name: 'job-new',
        component: () => import('@/views/JobEditorView.vue'),
      },
      {
        path: 'jobs/:id(\\d+)/edit',
        name: 'job-edit',
        component: () => import('@/views/JobEditorView.vue'),
        props: true,
      },
      {
        path: 'runs',
        name: 'runs',
        component: () => import('@/views/RunsView.vue'),
      },
      {
        path: 'notifications',
        name: 'notifications',
        component: () => import('@/views/NotificationsView.vue'),
      },
      {
        path: 'admin',
        component: () => import('@/layouts/AdminLayout.vue'),
        children: [
          { path: '', redirect: { name: 'admin-providers' } },
          {
            path: 'providers',
            name: 'admin-providers',
            component: () => import('@/views/admin/ProvidersView.vue'),
          },
          {
            path: 'settings',
            name: 'admin-settings',
            component: () => import('@/views/admin/SettingsView.vue'),
          },
          {
            path: 'about',
            name: 'admin-about',
            component: () => import('@/views/admin/AboutView.vue'),
          },
        ],
      },
    ],
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFoundView.vue'),
    meta: { public: true },
  },
];

export const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior(to, _from, saved) {
    if (saved) {
      return saved;
    }
    if (to.hash) {
      return { el: to.hash, behavior: 'smooth' };
    }
    return { top: 0 };
  },
});

/**
 * The guard loads the session once per page and sends an anonymous visitor to
 * the login screen, remembering where they were headed so the sign-in lands on
 * the screen they asked for instead of the dashboard.
 */
router.beforeEach(async (to) => {
  const auth = useAuthStore();
  if (!auth.loaded) {
    await auth.load();
  }
  if (to.meta.public) {
    if (to.name === 'login' && auth.authenticated) {
      return { name: 'dashboard' };
    }
    return true;
  }
  if (!auth.authenticated) {
    return { name: 'login', query: { redirect_to: to.fullPath } };
  }
  return true;
});

export default router;
