<script setup lang="ts">
// CapabilityBadges — what a connection can do, in the vocabulary of the API.
//
// `capabilities` are the features a stored grant satisfies; `missing` are the
// ones the provider now asks for and the account does not hold yet, which is what
// makes "reconnect this account" an actionable sentence instead of a guess.
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import Badge from '@/components/ui/Badge.vue';
import { cn } from '@/lib/utils';

const props = withDefaults(
  defineProps<{
    capabilities: string[];
    missing?: string[];
    class?: string;
  }>(),
  { missing: () => [] },
);

const { t, te } = useI18n();

/** label translates a capability, falling back to its identifier. */
function label(capability: string): string {
  const key = `admin.capability.${capability}`;
  return te(key) ? t(key) : capability;
}

const missing = computed(() => new Set(props.missing));

/** all is the badge list: the granted capabilities plus the missing ones. */
const all = computed(() => [...props.capabilities, ...props.missing]);
</script>

<template>
  <div :class="cn('flex flex-wrap gap-1.5', props.class)">
    <Badge
      v-for="capability in all"
      :key="capability"
      :tone="missing.has(capability) ? 'destructive' : 'secondary'"
    >
      {{ label(capability) }}
    </Badge>
  </div>
</template>
