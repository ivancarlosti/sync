<script setup lang="ts">
// FolderPicker — the folder browser of the job editor.
//
// It walks the remote tree through the API (`/accounts/:id/drives` and
// `/accounts/:id/drives/:drive/items`), which keeps the OAuth token on the server
// and shows the operator the same drives the engine will see. A file that cannot
// be synchronised is listed but marked, because hiding it would make a folder
// look incomplete (see the `unsupported` flag of FolderItem).
//
// A Microsoft account also owns SharePoint libraries, which are not part of
// `/me/drives`. They are listed through `/accounts/:id/sites`: the search field
// above the SharePoint select chooses one of the two delegated calls — a keyword
// searches the tenant (`$search`), a pasted SharePoint URL resolves that exact
// site (`/sites/resolve`) — so a library the search does not surface stays
// reachable. The select itself lists the libraries, while the account's own
// OneDrive (personal, or OneDrive for Business on a work account) gets its own
// select next to it: the `provider` prop tells the picker which of the two
// layouts the account needs.
import { ArrowUp, Folder, FolderOpen, HardDrive, RefreshCw, Search } from '@lucide/vue';
import { computed, onBeforeUnmount, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';

import Alert from '@/components/ui/Alert.vue';
import Button from '@/components/ui/Button.vue';
import Dialog from '@/components/ui/Dialog.vue';
import Input from '@/components/ui/Input.vue';
import Label from '@/components/ui/Label.vue';
import Select, { type SelectOption } from '@/components/ui/Select.vue';
import Spinner from '@/components/ui/Spinner.vue';
import { accounts, type Drive, type FolderItem, type ProviderName, messageOf } from '@/lib/api';
import { pathFrom, type FolderSelection } from '@/lib/folders';
import { formatBytes, formatDateTime } from '@/lib/format';

const props = withDefaults(
  defineProps<{
    accountId: number;
    /** provider owns the roots: it decides which selects the picker shows. */
    provider?: ProviderName | '';
    /** purpose titles the dialog: the source and the destination differ. */
    purpose?: 'source' | 'destination';
    /** start* reopens the browser where the job already points. */
    startDriveId?: string;
    startFolderId?: string;
    startFolderPath?: string;
  }>(),
  { provider: '', purpose: 'source', startDriveId: '', startFolderId: '', startFolderPath: '' },
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

/** siteQuery holds the keyword or the SharePoint URL the operator typed. */
const siteQuery = ref('');
const searchingSites = ref(false);
const siteFailure = ref('');
/** siteTimer debounces the site field so typing issues one request per pause. */
let siteTimer: ReturnType<typeof setTimeout> | undefined;

const loadingDrives = ref(false);
const loadingItems = ref(false);
const failure = ref('');

const title = computed(() =>
  props.purpose === 'destination' ? t('browser.destination') : t('browser.source'),
);

/**
 * isMicrosoft gates the SharePoint controls: only Microsoft 365 owns libraries,
 * and only it answers the `/sites` calls, so a Google account must not show a
 * dead search field.
 */
const isMicrosoft = computed(() => props.provider === 'microsoft');

/**
 * locationOf names the place a library lives in, which is what tells two of them
 * apart: every SharePoint site ships a default library literally called
 * "Documents", so the name alone would show one indistinguishable row per site.
 * The place is the site segment of the drive URL (/sites/marketing,
 * /teams/hr); a URL that carries none — the account's own OneDrive under
 * /personal — contributes nothing, because that drive is already unique. The
 * site name is skipped when it merely repeats the library name, so a searched
 * site does not end up labelled twice.
 */
function locationOf(drive: Drive): string {
  if (!drive.owner) {
    return '';
  }
  try {
    const segments = new URL(drive.owner).pathname.split('/').filter((part) => part !== '');
    const marker = segments.findIndex((part) => ['sites', 'teams'].includes(part.toLowerCase()));
    const place = marker >= 0 ? segments[marker + 1] : undefined;
    if (!place || place.toLowerCase() === drive.name.toLowerCase()) {
      return '';
    }
    return place;
  } catch {
    return '';
  }
}

/** optionOf renders one root; the kind (or the library's site) tells two apart. */
function optionOf(drive: Drive, withKind: boolean): SelectOption {
  const suffix = [withKind && drive.kind ? drive.kind : '', locationOf(drive)]
    .filter((part) => part !== '')
    .join(' · ');
  return {
    value: drive.id,
    label: suffix ? `${drive.name} · ${suffix}` : drive.name,
  };
}

/**
 * dedupe keeps the first entry of every root and drops an empty id: `/me/drives`
 * already returns the libraries the user was given access to, and a search adds
 * the rest, so the picker never lists one root twice.
 */
function dedupe(candidates: Drive[], withKind: boolean): SelectOption[] {
  const seen = new Set<string>();
  const options: SelectOption[] = [];
  for (const drive of candidates) {
    if (drive.id === '' || seen.has(drive.id)) {
      continue;
    }
    seen.add(drive.id);
    options.push(optionOf(drive, withKind));
  }
  return options;
}

/**
 * isOneDriveKind marks the account's own drive: "personal" on a personal
 * account, "business" on a work/school one. Every other kind is a SharePoint
 * library (document_library, site), which is what keeps the work OneDrive out of
 * the library select.
 */
function isOneDriveKind(kind: string | undefined): boolean {
  return kind === 'personal' || kind === 'business';
}

/**
 * driveOptions is the single select of every other provider: a Google account
 * lists My Drive and its Shared Drives together, told apart by their kind.
 */
const driveOptions = computed<SelectOption[]>(() => dedupe([...drives.value, ...sites.value], true));

/** oneDriveOptions is the account's own OneDrive, personal or for Business. */
const oneDriveOptions = computed<SelectOption[]>(() =>
  dedupe(
    drives.value.filter((drive) => isOneDriveKind(drive.kind)),
    false,
  ),
);

/**
 * sharePointOptions holds the libraries: the ones `/me/drives` returned plus the
 * ones the search above found, deduped so the two collections do not repeat.
 */
const sharePointOptions = computed<SelectOption[]>(() =>
  dedupe([...drives.value.filter((drive) => !isOneDriveKind(drive.kind)), ...sites.value], false),
);

/**
 * driveModel, oneDriveModel and sharePointModel drive the selects from the one
 * state that matters, `driveId`: the two Microsoft selects each answer in the
 * group the current root belongs to and fall back to their placeholder
 * otherwise, so neither can disagree with the listing below.
 */
const driveModel = computed<string | number>({
  get: () => driveId.value,
  set: (value) => {
    void openDrive(String(value));
  },
});

const oneDriveModel = computed<string | number>({
  get: () => (oneDriveOptions.value.some((option) => option.value === driveId.value) ? driveId.value : ''),
  set: (value) => {
    void openDrive(String(value));
  },
});

const sharePointModel = computed<string | number>({
  get: () =>
    sharePointOptions.value.some((option) => option.value === driveId.value) ? driveId.value : '',
  set: (value) => {
    void openDrive(String(value));
  },
});

const directories = computed(() => items.value.filter((item) => item.is_dir));
const files = computed(() => items.value.filter((item) => !item.is_dir));
const canGoUp = computed(() => trail.value.length > 1);
const currentPath = computed(() => pathFrom(trail.value.slice(1).map((step) => step.name)));

/** reset clears the browsing state left over from a previous account. */
function reset(): void {
  failure.value = '';
  siteFailure.value = '';
  siteQuery.value = '';
  if (siteTimer !== undefined) {
    clearTimeout(siteTimer);
    siteTimer = undefined;
  }
  items.value = [];
  trail.value = [];
  folderId.value = '';
  driveId.value = '';
  drives.value = [];
  sites.value = [];
}

/**
 * looksLikeSiteUrl tells the two uses of the site field apart: a full SharePoint
 * URL (or a Graph site id) is resolved as-is, anything else is a search keyword.
 */
function looksLikeSiteUrl(value: string): boolean {
  const raw = value.trim();
  if (raw === '') {
    return false;
  }
  if (/^https?:\/\//i.test(raw) || /(?:sharepoint|onmicrosoft)\.com/i.test(raw)) {
    return true;
  }
  // A Graph site id (host,g1,g2) or the "host:/path" form SharePoint shows.
  return /^[a-z0-9.-]+,[a-z0-9-]+/i.test(raw) || /^[a-z0-9.-]+:\//i.test(raw);
}

/**
 * loadSites lists every library the account can reach (`$search=*` on the
 * server), so the picker is complete before the operator types anything. A
 * personal account legitimately has none; a real failure is reported instead of
 * being swallowed, which is what made a missing library look like a missing
 * feature.
 */
async function loadSites(): Promise<void> {
  try {
    const answer = await accounts.sites(props.accountId);
    sites.value = answer.sites ?? [];
    siteFailure.value = '';
  } catch (error) {
    sites.value = [];
    siteFailure.value = messageOf(error);
  }
}

/**
 * runSiteQuery runs the site field: a keyword searches the sites of the tenant
 * and replaces the list, a pasted SharePoint URL is resolved and added to it, so
 * a library the search does not know about stays selectable next to the results.
 */
async function runSiteQuery(): Promise<void> {
  const value = siteQuery.value.trim();
  const byUrl = looksLikeSiteUrl(value);
  searchingSites.value = true;
  siteFailure.value = '';
  try {
    const answer = byUrl
      ? await accounts.resolveSite(props.accountId, value)
      : await accounts.sites(props.accountId, value);
    const found = answer.sites ?? [];
    sites.value = byUrl ? [...found, ...sites.value] : found;
  } catch (error) {
    siteFailure.value = messageOf(error);
  } finally {
    searchingSites.value = false;
  }
}

/** submitSiteQuery runs the field now, cancelling the pending debounce. */
function submitSiteQuery(): void {
  if (siteTimer !== undefined) {
    clearTimeout(siteTimer);
    siteTimer = undefined;
  }
  void runSiteQuery();
}

/** runSiteQuerySoon debounces typing, so a search issues one request per pause. */
function runSiteQuerySoon(): void {
  if (siteTimer !== undefined) {
    clearTimeout(siteTimer);
  }
  siteTimer = setTimeout(() => {
    siteTimer = undefined;
    void runSiteQuery();
  }, 300);
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

/**
 * openDrive selects a drive (or a site) and lists its root. An empty id is the
 * placeholder of a Microsoft select, which only shows while the other select
 * holds the selection: it is not a request.
 */
async function openDrive(id: string): Promise<void> {
  if (id === '' || id === driveId.value) {
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

/**
 * start loads the drives and reopens the folder the job already points at.
 */
async function start(): Promise<void> {
  reset();
  loadingDrives.value = true;
  try {
    const answer = await accounts.drives(props.accountId);
    drives.value = answer.drives ?? [];
    if (isMicrosoft.value) {
      // Only Microsoft owns libraries; the call answers 424 for a provider
      // without the capability, so it is not worth making.
      void loadSites();
    }
    if (drives.value.length === 0) {
      failure.value = t('browser.drivesError');
      return;
    }
    const requested = drives.value.find((drive) => drive.id === props.startDriveId);
    // A Microsoft account opens on its OneDrive: that root always exists, while
    // a library only shows up once `/me/drives` or a search returned it.
    const fallback = isMicrosoft.value
      ? (drives.value.find((drive) => isOneDriveKind(drive.kind)) ?? drives.value[0])
      : drives.value[0];
    const selected = requested ?? fallback;
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

// Typing searches after a pause; the button and Enter run the field at once.
watch(siteQuery, runSiteQuerySoon);

onBeforeUnmount(() => {
  if (siteTimer !== undefined) {
    clearTimeout(siteTimer);
  }
});
</script>

<template>
  <Dialog v-model:open="open" :title="title" size="lg" :close-label="t('common.closeDialog')">
    <div class="space-y-4">
      <Alert v-if="failure" tone="destructive" :title="t('browser.loadError')" :message="failure" />

      <!-- Microsoft only: one field, two calls — a keyword searches the sites of
           the tenant, a pasted SharePoint URL resolves that exact library. -->
      <div v-if="isMicrosoft" class="space-y-1.5">
        <Label for="browser-sites">{{ t('browser.siteSearch') }}</Label>
        <div class="flex flex-wrap items-center gap-2">
          <Input
            id="browser-sites"
            v-model="siteQuery"
            type="search"
            class="min-w-56 flex-1"
            :placeholder="t('browser.sitePlaceholder')"
            :disabled="loadingDrives"
            @keydown.enter="submitSiteQuery"
          />
          <Button variant="outline" :loading="searchingSites" @click="submitSiteQuery">
            <Search v-if="!searchingSites" class="h-4 w-4" aria-hidden="true" />
            {{ t('browser.siteFind') }}
          </Button>
        </div>
        <p class="text-xs text-muted-foreground">{{ t('browser.siteHint') }}</p>
        <Alert
          v-if="siteFailure"
          tone="destructive"
          :title="t('browser.siteError')"
          :message="siteFailure"
        />
        <p v-else-if="sites.length > 0" class="text-xs text-muted-foreground">
          {{ t('browser.siteResults', { count: sites.length }) }}
        </p>
      </div>

      <div class="flex flex-wrap items-end gap-2">
        <!-- Every other provider has a single kind of root: Google lists My
             Drive and its Shared Drives in one select. -->
        <div v-if="!isMicrosoft" class="min-w-56 flex-1 space-y-1.5">
          <Label for="browser-drive">{{ t('browser.drive') }}</Label>
          <Select
            id="browser-drive"
            v-model="driveModel"
            :options="driveOptions"
            :disabled="loadingDrives || driveOptions.length === 0"
          />
        </div>

        <!-- Microsoft 365 keeps its two kinds of root apart — the account's own
             OneDrive (personal or for Business) and the SharePoint libraries —
             so the libraries sit in their own select, under the search field
             that fills it. -->
        <template v-else>
          <div class="min-w-56 flex-1 space-y-1.5">
            <Label for="browser-onedrive">{{ t('browser.oneDrive') }}</Label>
            <Select
              id="browser-onedrive"
              v-model="oneDriveModel"
              :options="oneDriveOptions"
              :placeholder="t('browser.selectRoot')"
              :disabled="loadingDrives || oneDriveOptions.length === 0"
            />
          </div>
          <div class="min-w-56 flex-1 space-y-1.5">
            <Label for="browser-sharepoint">{{ t('browser.sharePointLibrary') }}</Label>
            <Select
              id="browser-sharepoint"
              v-model="sharePointModel"
              :options="sharePointOptions"
              :placeholder="t('browser.selectRoot')"
              :disabled="loadingDrives || sharePointOptions.length === 0"
            />
          </div>
        </template>
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
