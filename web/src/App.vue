<script setup lang="ts">
// App root.
//
// The shell of the authenticated app lives in layouts/AppLayout.vue; this
// component only mounts the view so the login and "not found" screens can render
// without the navigation chrome (there is nothing to navigate to before signing
// in).
import { RouterView } from 'vue-router';
</script>

<template>
  <RouterView />
</template>
