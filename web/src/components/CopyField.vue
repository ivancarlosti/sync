<script setup lang="ts">
// CopyField — one value of this instance, ready to paste in a provider console.
//
// The redirect URI and the permission list are the two places where a typo costs
// a failed registration, so they are rendered verbatim (never wrapped mid-token
// where it can be avoided) and copied with one click. `navigator.clipboard` is
// only available on a secure origin, so a legacy `execCommand` path covers an
// instance served over plain HTTP.
import { Check, Copy } from '@lucide/vue';
import { onBeforeUnmount, ref } from 'vue';
import { useI18n } from 'vue-i18n';

import Button from '@/components/ui/Button.vue';
import { cn } from '@/lib/utils';

const props = withDefaults(
  defineProps<{
    value: string;
    /** multiline renders the value as a block (a scope list), not one line. */
    multiline?: boolean;
    class?: string;
  }>(),
  { multiline: false },
);

const { t } = useI18n();
const copied = ref(false);
let reset: number | undefined;

/** copy writes the value to the clipboard and confirms it for two seconds. */
async function copy(): Promise<void> {
  if (!(await write(props.value))) {
    return;
  }
  copied.value = true;
  window.clearTimeout(reset);
  reset = window.setTimeout(() => {
    copied.value = false;
  }, 2000);
}

/** write copies text, falling back to a hidden textarea. */
async function write(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // Falls through to the legacy path (insecure origin, denied permission).
  }
  try {
    const area = document.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', '');
    area.style.position = 'fixed';
    area.style.opacity = '0';
    document.body.appendChild(area);
    area.select();
    const done = document.execCommand('copy');
    document.body.removeChild(area);
    return done;
  } catch {
    return false;
  }
}

onBeforeUnmount(() => window.clearTimeout(reset));
</script>

<template>
  <div :class="cn('flex items-start gap-2', props.class)">
    <code
      :class="
        cn(
          'min-w-0 flex-1 rounded-md border border-border bg-muted/50 px-2 py-1.5 font-mono text-xs',
          multiline ? 'break-all whitespace-pre-wrap' : 'break-all',
        )
      "
      >{{ value }}</code
    >
    <Button variant="outline" size="sm" @click="copy">
      <Check v-if="copied" class="h-4 w-4" aria-hidden="true" />
      <Copy v-else class="h-4 w-4" aria-hidden="true" />
      {{ copied ? t('common.copied') : t('common.copy') }}
    </Button>
  </div>
</template>
