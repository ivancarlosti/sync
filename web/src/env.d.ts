/// <reference types="vite/client" />

// TypeScript cannot resolve `.vue` files on its own; this shim lets `vue-tsc`
// type-check imports such as `import AppHeader from '@/components/AppHeader.vue'`.
declare module '*.vue' {
  import type { DefineComponent } from 'vue';

  const component: DefineComponent<Record<string, unknown>, Record<string, unknown>, unknown>;
  export default component;
}
