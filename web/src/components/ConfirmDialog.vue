<script setup lang="ts">
// ConfirmDialog — the "are you sure?" of destructive actions.
//
// Deletions in Sync are irreversible (a job takes its run history with it, an
// account takes its jobs), so the confirmation is a shared component rather than
// a `window.confirm`: it inherits the theme, it is translated, and the handler
// stays busy until the request finishes.
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import Button from '@/components/ui/Button.vue';
import Dialog from '@/components/ui/Dialog.vue';

const props = withDefaults(
  defineProps<{
    title: string;
    message?: string;
    confirmLabel?: string;
    cancelLabel?: string;
    tone?: 'default' | 'destructive';
    loading?: boolean;
    size?: 'sm' | 'md';
  }>(),
  { tone: 'destructive', size: 'sm' },
);

const emit = defineEmits<{ confirm: [] }>();

const open = defineModel<boolean>('open', { default: false });
const { t } = useI18n();

const confirmLabel = computed(() => props.confirmLabel ?? t('common.confirm'));
const cancelLabel = computed(() => props.cancelLabel ?? t('common.cancel'));
</script>

<template>
  <Dialog v-model:open="open" :title="title" :description="message" :size="size" :close-label="t('common.close')">
    <p v-if="$slots.default" class="text-sm text-muted-foreground"><slot /></p>
    <template #footer>
      <Button variant="outline" :disabled="loading" @click="open = false">{{ cancelLabel }}</Button>
      <Button :variant="tone === 'destructive' ? 'destructive' : 'default'" :loading="loading" @click="emit('confirm')">
        {{ confirmLabel }}
      </Button>
    </template>
  </Dialog>
</template>
