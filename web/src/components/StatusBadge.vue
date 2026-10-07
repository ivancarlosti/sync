<script setup lang="ts">
// StatusBadge — one status string, one colour, one translation.
//
// The API encodes states as stable identifiers (run status, account status,
// notification health). Mapping them here means a new status added to the Go
// side shows up as its raw identifier instead of a blank chip, and the tone of
// "failed" is never accidentally cheerful.
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import Badge from '@/components/ui/Badge.vue';

type BadgeTone = 'default' | 'secondary' | 'outline' | 'success' | 'warning' | 'destructive' | 'muted';

const TONES: Record<string, BadgeTone> = {
  // Healthy / done.
  connected: 'success',
  success: 'success',
  ok: 'success',
  created: 'success',
  updated: 'success',
  inSync: 'success',
  folderCreated: 'success',
  tokensRefreshed: 'success',
  configured: 'success',
  environment: 'success',
  database: 'success',
  // In flight.
  running: 'default',
  // Needs attention but is not a failure.
  partial: 'warning',
  timeout: 'warning',
  conflict: 'warning',
  degraded: 'warning',
  unsupported: 'warning',
  notConfigured: 'warning',
  // Broken.
  failed: 'destructive',
  error: 'destructive',
  // Inert.
  cancelled: 'muted',
  skipped: 'muted',
  deleted: 'muted',
  anonymous: 'muted',
  // Informational labels that are not really states.
  manual: 'secondary',
  scheduled: 'secondary',
  renamed: 'secondary',
};

const props = defineProps<{
  status: string;
  class?: string;
}>();

const { t, te } = useI18n();

const tone = computed<BadgeTone>(() => TONES[props.status] ?? 'outline');
const key = computed(() => `status.${props.status}`);
const text = computed(() => (te(key.value) ? t(key.value) : props.status));
</script>

<template>
  <Badge :tone="tone" :class="$props.class">{{ text }}</Badge>
</template>
