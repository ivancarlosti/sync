<script setup lang="ts">
// CapabilityBadges — what a connection can do, in the vocabulary of the API.
//
// `capabilities` are the features a stored grant satisfies; `notApplicable` are
// the ones this kind of account can never be granted (a personal Microsoft
// account and the Graph directory permissions), which are neither an error nor
// fixable by reconnecting; `missing` are the ones the provider now asks for and
// the account does not hold yet, which is what makes "reconnect this account" an
// actionable sentence instead of a guess. Only that last group is actionable,
// which is why it alone keeps the destructive tone.
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import Badge from '@/components/ui/Badge.vue';
import { cn } from '@/lib/utils';

const props = withDefaults(
  defineProps<{
    capabilities: string[];
    missing?: string[];
    notApplicable?: string[];
    class?: string;
  }>(),
  { missing: () => [], notApplicable: () => [] },
);

const { t, te } = useI18n();

/** label translates a capability, falling back to its identifier. */
function label(capability: string): string {
  const key = `admin.capability.${capability}`;
  return te(key) ? t(key) : capability;
}

const missing = computed(() => new Set(props.missing));
const notApplicable = computed(() => new Set(props.notApplicable));

/**
 * all is the badge list: the granted capabilities, then the ones that can never
 * apply to this account, then the missing ones — so the actionable red badges
 * come last, where the eye lands.
 */
const all = computed(() => [...props.capabilities, ...props.notApplicable, ...props.missing]);

/** tone greys out what cannot apply, and keeps red for what is really missing. */
function tone(capability: string): 'destructive' | 'muted' | 'secondary' {
  if (missing.value.has(capability)) {
    return 'destructive';
  }
  return notApplicable.value.has(capability) ? 'muted' : 'secondary';
}
</script>

<template>
  <div :class="cn('flex flex-wrap gap-1.5', props.class)">
    <Badge v-for="capability in all" :key="capability" :tone="tone(capability)">
      {{ label(capability) }}
    </Badge>
  </div>
</template>
