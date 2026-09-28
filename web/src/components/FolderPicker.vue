<script setup lang="ts">
// FolderPicker — the folder browser of the job editor.
//
// It walks the remote tree through the API (`/accounts/:id/drives` and
// `/accounts/:id/drives/:drive/items`), which keeps the OAuth token on the server
// and shows the operator the same drives the engine will see. A file that cannot
// be synchronised is listed but marked, because hiding it would make a folder
// look incomplete (see the `unsupported` flag of FolderItem).
import { ArrowUp, Folder, FolderOpen, HardDrive, RefreshCw } from '@lucide/vue';
import { computed, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';

import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Dialog from '@/components/ui/Dialog.vue';
import Label from '@/components/ui/Label.vue';
import Select, { type SelectOption } from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import { accounts, type Drive, type FolderItem, messageOf } from '@/lib/api';
import { pathFrom, type FolderSelection } from '@/lib/folders';
import { formatBytes, formatDateTime } from '@/lib/format';

const props = withDefaults(
  defineProps<{
    accountId: number;
    /** purpose titles the dialog: the source and the destination differ. */
    purpose?: 'source' | 'destination';
    /** start* reopens the browser where the job already points. */
    startDriveId?: string;
    startFolderId?: string;
    startFolderPath?: string;
  }>(),
  { purpose: 'source', startDriveId: '', startFolderId: '', startFolderPath: '' },
);

const emit = defineEmits<{ select: [selection: FolderSelection] }>();

const open = defineModel<boolean>('open', { default: false });
const { t, locale } = useI18n();

const drives = ref<Drive[]>([]);
const sites = ref<Drive[]>([]);
const driveId = ref('');
const folderId = ref('');
/** trail is the breadcrumb; the first entry is always the root of the drive. */
const trail = ref<Array<{ id: string; name: string }>>([]);
const items = ref<FolderItem[]>([]);

const loadingDrives = ref(false);
const loadingItems = ref(false);
const failure = ref('');

const title = computed(() =>
  props.purpose === 'destination' ? t('browser.destination') : t('browser.source'),
);

/** driveOptions merges the drives of the account with its SharePoint sites. */
const driveOptions = computed<SelectOption[]>(() => {
  const options: SelectOption[] = drives.value.map((drive) => ({
    value: drive.id,
    label: drive.name,
  }));
  for (const site of sites.value) {
    options.push({ value: site.id, label: site.kind ? `${site.name} · ${site.kind}` : site.name });
  }
  return options;
});

const directories = computed(() => items.value.filter((item) => item.is_dir));
const files = computed(() => items.value.filter((item) => !item.is_dir));
const canGoUp = computed(() => trail.value.length > 1);
const currentPath = computed(() => pathFrom(trail.value.slice(1).map((step) => step.name)));

/** reset clears the browsing state left over from a previous account. */
function reset(): void {
  failure.value = '';
  items.value = [];
  trail.value = [];
  folderId.value = '';
  driveId.value = '';
  drives.value = [];
  sites.value = [];
}

/** loadSites lists the SharePoint sites of a Microsoft account, best effort. */
async function loadSites(): Promise<void> {
  try {
    const answer = await accounts.sites(props.accountId);
    sites.value = answer.sites ?? [];
  } catch {
    // A personal account has no sites; the drives are the whole story.
    sites.value = [];
  }
}

/** listItems loads the folder the breadcrumb points at. */
async function listItems(): Promise<void> {
  loadingItems.value = true;
  failure.value = '';
  try {
    const answer = await accounts.items(props.accountId, driveId.value, folderId.value);
    // Directories first: the picker is about folders, files are context.
    items.value = [...answer.items].sort((left, right) => {
      if (left.is_dir !== right.is_dir) {
        return left.is_dir ? -1 : 1;
      }
      return left.name.localeCompare(right.name);
    });
  } catch (error) {
    items.value = [];
    failure.value = messageOf(error);
  } finally {
    loadingItems.value = false;
  }
}

/** openDrive selects a drive (or a site) and lists its root. */
async function openDrive(id: string): Promise<void> {
  if (id === driveId.value) {
    return;
  }
  driveId.value = id;
  folderId.value = '';
  trail.value = [{ id: '', name: '' }];
  await listItems();
}

/** enterFolder descends into one directory. */
async function enterFolder(item: FolderItem): Promise<void> {
  if (!item.is_dir) {
    return;
  }
  trail.value = [...trail.value, { id: item.id, name: item.name }];
  folderId.value = item.id;
  await listItems();
}

/** goTo jumps to one segment of the breadcrumb. */
async function goTo(index: number): Promise<void> {
  trail.value = trail.value.slice(0, index + 1);
  folderId.value = trail.value[trail.value.length - 1]?.id ?? '';
  await listItems();
}

/** goUp moves back to the parent folder. */
async function goUp(): Promise<void> {
  if (!canGoUp.value) {
    return;
  }
  await goTo(trail.value.length - 2);
}

/** start loads the drives and reopens the folder the job already points at. */
async function start(): Promise<void> {
  reset();
  loadingDrives.value = true;
  try {
    const answer = await accounts.drives(props.accountId);
    drives.value = answer.drives ?? [];
    void loadSites();
    if (drives.value.length === 0) {
      failure.value = t('browser.drivesError');
      return;
    }
    const requested = drives.value.find((drive) => drive.id === props.startDriveId);
    const selected = requested ?? drives.value[0];
    driveId.value = selected.id;
    trail.value = [{ id: '', name: '' }];
    folderId.value = '';
    if (requested && props.startFolderId) {
      // The stored ids are authoritative; the stored path only labels the crumb.
      const segments = props.startFolderPath.split('/').filter((part) => part !== '');
      trail.value.push({
        id: props.startFolderId,
        name: segments[segments.length - 1] ?? props.startFolderId,
      });
      folderId.value = props.startFolderId;
    }
    await listItems();
  } catch (error) {
    failure.value = messageOf(error);
  } finally {
    loadingDrives.value = false;
  }
}

/** confirm hands the selection back to the job editor and closes. */
function confirm(): void {
  emit('select', {
    driveId: driveId.value,
    folderId: folderId.value,
    folderPath: currentPath.value,
  });
  open.value = false;
}

watch(open, (value) => {
  if (value && props.accountId > 0) {
    void start();
  }
});
</script>

<template>
  <Dialog v-model:open="open" :title="title" size="lg" :close-label="t('common.closeDialog')">
    <div class="space-y-4">
      <Alert v-if="failure" tone="destructive" :title="t('browser.loadError')" :message="failure" />

      <div class="flex flex-wrap items-end gap-2">
        <div class="min-w-56 flex-1 space-y-1.5">
          <Label for="browser-drive">{{ t('browser.drive') }}</Label>
          <Select
            id="browser-drive"
            :options="driveOptions"
            :disabled="loadingDrives || driveOptions.length === 0"
            @update:model-value="openDrive(String($event))"
          />
        </div>
        <Button
          variant="outline"
          size="icon"
          :disabled="!canGoUp || loadingItems"
          :title="t('browser.up')"
          @click="goUp"
        >
          <ArrowUp class="h-4 w-4" aria-hidden="true" />
        </Button>
        <Button
          variant="outline"
          size="icon"
          :disabled="loadingItems"
          :title="t('common.refresh')"
          @click="listItems"
        >
          <RefreshCw class="h-4 w-4" aria-hidden="true" />
        </Button>
      </div>

      <div class="space-y-1">
        <p class="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {{ t('browser.path') }} · <span class="font-mono">{{ currentPath }}</span>
        </p>
        <nav class="flex flex-wrap items-center gap-1 text-sm">
          <template v-for="(step, index) in trail" :key="`${step.id}-${index}`">
            <span v-if="index > 0" class="text-muted-foreground">/</span>
            <button
              type="button"
              class="rounded px-1.5 py-0.5 hover:bg-accent hover:text-accent-foreground"
              @click="goTo(index)"
            >
              {{ step.name || t('browser.root') }}
            </button>
          </template>
        </nav>
      </div>

      <div class="rounded-lg border border-border">
        <div v-if="loadingItems" class="flex items-center justify-center gap-2 p-6">
          <Spinner size="sm" />
          <span class="text-sm text-muted-foreground">{{ t('browser.loading') }}</span>
        </div>

        <p v-else-if="items.length === 0" class="p-6 text-center text-sm text-muted-foreground">
          {{ t('browser.empty') }}
        </p>

        <ul v-else class="divide-y divide-border">
          <li v-for="item in [...directories, ...files]" :key="item.id">
            <button
              v-if="item.is_dir"
              type="button"
              class="flex w-full items-center gap-3 px-3 py-2 text-start text-sm hover:bg-accent hover:text-accent-foreground"
              @click="enterFolder(item)"
            >
              <Folder class="h-4 w-4 shrink-0 text-primary" aria-hidden="true" />
              <span class="flex-1 truncate">{{ item.name }}</span>
              <span class="text-xs text-muted-foreground">
                {{ formatDateTime(item.modified_at, locale) }}
              </span>
            </button>
            <div v-else class="flex items-center gap-3 px-3 py-2 text-sm text-muted-foreground">
              <FolderOpen class="h-4 w-4 shrink-0" aria-hidden="true" />
              <span class="flex-1 truncate">{{ item.name }}</span>
              <span v-if="item.unsupported" class="text-xs text-warning">
                {{ t('browser.unsupported') }}
              </span>
              <span class="text-xs tabular-nums">{{ formatBytes(item.size, locale) }}</span>
            </div>
          </li>
        </ul>
      </div>
    </div>

    <template #footer>
      <Button variant="outline" @click="open = false">{{ t('common.cancel') }}</Button>
      <Button :disabled="driveId === ''" @click="confirm">
        <HardDrive class="h-4 w-4" aria-hidden="true" />
        {{ t('browser.select') }}
      </Button>
    </template>
  </Dialog>
</template>
