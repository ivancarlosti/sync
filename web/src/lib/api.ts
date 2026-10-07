// Typed client for the Sync API.
//
// The SPA is served by the same Go binary that owns the API, so every request is
// same-origin and the HttpOnly session cookie travels automatically: no token is
// ever kept in JavaScript. Every failure is normalised into `ApiError`, whose
// `code` is the machine readable value the server sends (see docs/api.md); the
// UI translates that code and shows the server message as technical detail.
import axios, { type AxiosError, type AxiosRequestConfig } from 'axios';

/** errorBody is the JSON shape of every error answer of the API. */
interface ErrorBody {
  code?: string;
  error?: string;
}

/** ApiError carries the status and the machine readable code of a failure. */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

/** defaultCode maps a status onto the code the server would have sent. */
function defaultCode(status: number): string {
  switch (status) {
    case 400:
      return 'validation';
    case 401:
      return 'unauthorized';
    case 403:
      return 'forbidden';
    case 404:
      return 'not_found';
    case 409:
      return 'busy';
    case 412:
      return 'reconnect';
    case 424:
      return 'not_configured';
    case 500:
    case 502:
    case 503:
      return 'internal';
    default:
      return status >= 500 ? 'internal' : 'validation';
  }
}

const client = axios.create({
  // Relative base URL: the SPA talks to whatever origin served it.
  baseURL: '/api',
  timeout: 300_000,
  headers: { Accept: 'application/json' },
});

/** onUnauthorized is called when the session expired, wired to the router. */
let unauthorizedHandler: (() => void) | null = null;

export function setUnauthorizedHandler(handler: () => void): void {
  unauthorizedHandler = handler;
}

/** request performs one call and converts any failure into an `ApiError`. */
async function request<T>(config: AxiosRequestConfig): Promise<T> {
  try {
    const response = await client.request<T>(config);
    return response.data;
  } catch (error) {
    const failure = error as AxiosError<ErrorBody>;
    const status = failure.response?.status ?? 0;
    const body = failure.response?.data ?? {};
    const code = body.code ?? (status === 0 ? 'network' : defaultCode(status));
    const message = body.error ?? failure.message ?? 'unknown error';
    if (status === 401 && unauthorizedHandler) {
      unauthorizedHandler();
    }
    throw new ApiError(status, code, message);
  }
}

/** messageOf renders any thrown value as a string for the UI. */
export function messageOf(error: unknown): string {
  if (error instanceof ApiError) {
    return error.message;
  }
  if (error instanceof Error) {
    return error.message;
  }
  return String(error);
}

/** codeOf renders the machine readable code of any thrown value. */
export function codeOf(error: unknown): string {
  return error instanceof ApiError ? error.code : 'internal';
}

// -----------------------------------------------------------------------------
// Payload types (mirrors of the Go views in internal/handlers and internal/models)
// -----------------------------------------------------------------------------

export type AuthMode = 'none' | 'account' | 'keycloak';
export type ProviderName = 'google' | 'microsoft';
export type ThemePreference = 'light' | 'dark' | 'system';

/** BuildInfo is `version.Info`: what Admin > About shows. */
export interface BuildInfo {
  name: string;
  version: string;
  commit: string;
  go_version: string;
  build_date?: string;
}

/** Identity is the signed-in operator. */
export interface Identity {
  authenticated: boolean;
  subject?: string;
  email?: string;
  name?: string;
  picture?: string;
  method: AuthMode;
  admin: boolean;
  expires_at?: string;
}

/** AppSettings are the defaults an administrator can change in Admin > Settings. */
export interface AppSettings {
  default_locale: string;
  default_theme: string;
  sync_default_interval_minutes: number;
  sync_run_timeout_minutes: number;
}

/** SessionView is the answer of GET /api/auth/session. */
export interface SessionView {
  mode: AuthMode;
  authenticated: boolean;
  open: boolean;
  captcha: { enabled: boolean; site_key?: string };
  keycloak: { enabled: boolean };
  operator: Identity;
  defaults: AppSettings;
  version: BuildInfo;
}

/** ConnectedAccount is the API representation of an OAuth authorisation. */
export interface ConnectedAccount {
  id: number;
  provider: ProviderName;
  provider_account_id: string;
  email: string;
  display_name: string;
  avatar_url: string;
  scopes: string[];
  /** Capabilities the stored grant satisfies, and the ones it misses. */
  capabilities: Capability[];
  missing_capabilities: Capability[];
  /** needs_reconnect is true when the provider now asks for more permissions. */
  needs_reconnect: boolean;
  status: 'connected' | 'error';
  last_error?: string;
  expires_at: string;
  refreshed_at?: string;
  last_synced_at?: string;
  created_at: string;
  updated_at: string;
}

/** Drive is one root that can be synchronised. */
export interface Drive {
  id: string;
  name: string;
  kind?: string;
  owner?: string;
}

/** FolderItem is one entry of a remote folder listing. */
export interface FolderItem {
  id: string;
  name: string;
  is_dir: boolean;
  size: number;
  modified_at: string;
  mime_type?: string;
  hash?: string;
  unsupported: boolean;
}

/** SyncJob is a source to destination synchronisation definition. */
export interface SyncJob {
  id: number;
  name: string;
  source_account_id: number;
  destination_account_id: number;
  source_drive_id: string;
  source_folder_id: string;
  source_folder_path: string;
  destination_drive_id: string;
  destination_folder_id: string;
  destination_folder_path: string;
  direction: 'google_to_microsoft' | 'microsoft_to_google' | 'bidirectional';
  conflict_policy: 'newest_wins' | 'source_wins' | 'destination_wins' | 'skip';
  exclude_patterns: string[];
  delete_missing: boolean;
  interval_minutes: number;
  enabled: boolean;
  running: boolean;
  last_run_at?: string;
  next_run_at?: string;
  last_status?: string;
  last_error?: string;
  created_at: string;
  updated_at: string;
}

/** JobPayload is what POST/PUT /api/jobs accepts. */
export interface JobPayload {
  name: string;
  source_account_id: number;
  destination_account_id: number;
  source_drive_id: string;
  source_folder_id: string;
  source_folder_path: string;
  destination_drive_id: string;
  destination_folder_id: string;
  destination_folder_path: string;
  direction: string;
  conflict_policy: string;
  exclude_patterns: string[];
  delete_missing: boolean;
  interval_minutes: number;
  enabled: boolean;
}

/** SyncRun is one execution of a job. */
export interface SyncRun {
  id: number;
  job_id: number;
  job_name: string;
  status: 'running' | 'success' | 'partial' | 'failed' | 'cancelled' | 'timeout';
  trigger: 'manual' | 'scheduled';
  started_at: string;
  finished_at?: string;
  duration_ms: number;
  files_scanned: number;
  files_created: number;
  files_updated: number;
  files_deleted: number;
  files_skipped: number;
  folders_created: number;
  conflicts: number;
  errors: number;
  bytes_transferred: number;
  message?: string;
  running?: boolean;
  created_at: string;
}

/** RunItem is one file operation of a run. */
export interface RunItem {
  id: number;
  action: string;
  path: string;
  size: number;
  evidence?: string;
  created_at: string;
}

/** AuditRun is one execution of a content audit. */
export interface AuditRun {
  id: number;
  account_id: number;
  account_email: string;
  provider: ProviderName;
  drive_id: string;
  root_folder_id: string;
  root_path: string;
  /** max_depth is how many folder levels below the root are analysed; 0 is unlimited. */
  max_depth: number;
  status: 'running' | 'success' | 'failed' | 'cancelled' | 'timeout';
  trigger: 'manual' | 'scheduled';
  started_at: string;
  finished_at?: string;
  duration_ms: number;
  files: number;
  folders: number;
  total_size: number;
  max_depth_reached: number;
  /** truncated is true when the report stopped at the node cap. */
  truncated: boolean;
  message?: string;
  running?: boolean;
  created_at: string;
}

/** AuditEntry is one node (folder or file) of a report. */
export interface AuditEntry {
  id: number;
  kind: 'folder' | 'file';
  path: string;
  name: string;
  depth: number;
  size: number;
  total_size: number;
  files: number;
  folders: number;
  expanded: boolean;
  modified_at?: string;
  mime_type?: string;
}

/** AuditPayload is what POST /api/audits accepts. */
export interface AuditPayload {
  account_id: number;
  drive_id: string;
  folder_id: string;
  folder_path: string;
  depth: number;
}

/** Stats is the aggregate the dashboard displays. */
export interface Stats {
  accounts: number;
  jobs: number;
  enabled_jobs: number;
  runs: number;
  succeeded: number;
  partial: number;
  failed: number;
  cancelled: number;
  timed_out: number;
  conflicts: number;
  files_created: number;
  files_updated: number;
  files_deleted: number;
  bytes_transferred: number;
}

/** ProviderInfo tells the operator where the credentials come from. */
export interface ProviderInfo {
  provider: ProviderName;
  client_id: string;
  secret_set: boolean;
  redirect_uri: string;
  tenant_id?: string;
  source: 'environment' | 'database' | string;
  configured: boolean;
  /** admin_consent is present for the providers that need a tenant-wide grant. */
  admin_consent?: AdminConsentStatus;
}

/** Capability is a class of provider operations (see internal/providers). */
export type Capability =
  | 'files'
  | 'users'
  | 'groups'
  | 'members'
  | 'domains'
  | 'orgunits'
  | 'roles'
  | 'licenses';

/** AdminConsentStatus is the tenant-wide consent recorded for a provider. */
export interface AdminConsentStatus {
  tenant?: string;
  client_id?: string;
  at?: string;
  granted: boolean;
}

/** GuidePermission is one permission the setup guide asks the operator to grant. */
export interface GuidePermission {
  capability: Capability;
  title: string;
  scope: string;
  admin_consent: boolean;
  /**
   * informational marks a scope requested for the protocol (the OpenID Connect
   * scopes) that never proves a capability: the provider grants it without
   * reporting it back, so it is listed but never gates a capability badge.
   */
  informational?: boolean;
}

/**
 * GuideStep is one step of the walkthrough. The server owns the structure and the
 * console links; the text is a catalog key derived from the provider and the id
 * (`admin.guide.steps.<provider>.<id>.title`), so it is translated here.
 */
export interface GuideStep {
  id: string;
  url?: string;
  /**
   * copy names the value of this instance the step asks to paste in the console:
   * `redirect_uri`, `scopes` (the whole grant, for a console that takes the list
   * in one paste) or `permissions` (one field per permission, for a console that
   * does not).
   */
  copy?: 'redirect_uri' | 'scopes' | 'permissions' | string;
  optional?: boolean;
  /** action names an API action the step offers (`admin_consent`). */
  action?: 'admin_consent' | string;
}

/** ProviderGuide is `GET /api/providers/:provider/guide`. */
export interface ProviderGuide {
  provider: ProviderName;
  configured: boolean;
  redirect_uri: string;
  console_urls: Record<string, string>;
  permissions: GuidePermission[];
  scopes: string[];
  capabilities: Capability[];
  admin_consent_required: boolean;
  admin_consent: AdminConsentStatus;
  steps: GuideStep[];
  warnings: string[];
}

/** ChannelField is one input of the server driven channel form. */
export interface ChannelField {
  key: string;
  label: string;
  type: 'text' | 'password' | 'int' | 'bool' | 'select' | 'json' | 'textarea' | string;
  required?: boolean;
  secret?: boolean;
  hint?: string;
  options?: string[];
  default?: unknown;
}

/** ChannelKind is a channel type plus its form schema. */
export interface ChannelKind {
  kind: string;
  label: string;
  fields: ChannelField[];
}

/** NotificationChannel is the masked channel returned by the API. */
export interface NotificationChannel {
  id: number;
  name: string;
  type: string;
  config: Record<string, unknown>;
  events: string[];
  enabled: boolean;
  last_status?: string;
  last_error?: string;
  last_used_at?: string;
  created_at: string;
  updated_at: string;
}

/** ChannelPayload is what POST/PUT /api/notifications accepts. */
export interface ChannelPayload {
  name: string;
  type: string;
  config: Record<string, unknown>;
  events: string[];
  enabled: boolean;
}

// -----------------------------------------------------------------------------
// Endpoints
// -----------------------------------------------------------------------------

export const auth = {
  session: () => request<SessionView>({ url: '/auth/session', method: 'GET' }),
  login: (payload: {
    login: string;
    password: string;
    captcha_token?: string;
    redirect_to?: string;
  }) => request<SessionView>({ url: '/auth/login', method: 'POST', data: payload }),
  // The API answers 204 on logout and the endpoint is idempotent.
  logout: () => request<void>({ url: '/auth/logout', method: 'POST' }),
  /**
   * keycloak asks the server for the realm authorization URL. The SPA navigates
   * to the returned `url` itself, which keeps a failure (realm misconfigured,
   * session lost) on the login page instead of an unstyled error page.
   */
  keycloak: (redirectTo: string) =>
    request<{ url: string; state?: string }>({
      url: '/auth/keycloak',
      params: { redirect_to: redirectTo },
    }),
};

// oauth lists the providers of this build with the state of their OAuth client
// (ProviderCredentialsInfo): `configured` says which one can start a flow, which
// is what the "Connect an account" screen offers.
export const oauth = {
  providers: () => request<{ providers: ProviderInfo[] }>({ url: '/oauth' }),
};

export const accounts = {
  list: () => request<{ accounts: ConnectedAccount[] }>({ url: '/accounts', method: 'GET' }),
  remove: (id: number) =>
    request<{ id: number; deleted_jobs: number }>({ url: `/accounts/${id}`, method: 'DELETE' }),
  verify: (id: number) => request<ConnectedAccount>({ url: `/accounts/${id}/verify`, method: 'POST' }),
  drives: (id: number) => request<{ drives: Drive[] }>({ url: `/accounts/${id}/drives` }),
  /**
   * sites searches the extra roots of an account (`q` is the keyword; an empty
   * one lists every site the account can reach). It answers an empty list for a
   * provider without the capability, so a personal account is not an error.
   */
  sites: (id: number, q = '') =>
    request<{ sites: Drive[] }>({
      url: `/accounts/${id}/sites`,
      params: q === '' ? undefined : { q },
    }),
  /**
   * resolveSite is the manual fallback of the folder picker: the server turns a
   * SharePoint URL the operator pasted into the default library of that site.
   * It answers the same shape as `sites` so both merge the same way.
   */
  resolveSite: (id: number, url: string) =>
    request<{ sites: Drive[] }>({
      url: `/accounts/${id}/sites/resolve`,
      method: 'POST',
      data: { url },
    }),
  items: (id: number, driveId: string, folderId: string) =>
    request<{ drive_id: string; folder_id: string; items: FolderItem[] }>({
      url: `/accounts/${id}/drives/${encodeURIComponent(driveId)}/items`,
      params: { folder_id: folderId },
    }),
  /** connect starts an OAuth flow; the answer carries the provider URL. */
  connect: (provider: ProviderName, redirectTo: string) =>
    request<{ url: string; state?: string }>({
      url: `/oauth/${provider}/start`,
      method: 'POST',
      data: { redirect_to: redirectTo },
    }),
  /**
   * adminConsent starts the tenant-wide consent flow (Microsoft Entra admin
   * consent); the answer carries the URL the administrator has to follow.
   */
  adminConsent: (provider: ProviderName, redirectTo: string) =>
    request<{ url: string; state?: string }>({
      url: `/oauth/${provider}/admin-consent`,
      method: 'POST',
      data: { redirect_to: redirectTo },
    }),
};

export const jobs = {
  list: () => request<{ jobs: SyncJob[] }>({ url: '/jobs', method: 'GET' }),
  get: (id: number) => request<SyncJob>({ url: `/jobs/${id}`, method: 'GET' }),
  create: (payload: JobPayload) => request<SyncJob>({ url: '/jobs', method: 'POST', data: payload }),
  update: (id: number, payload: JobPayload) =>
    request<SyncJob>({ url: `/jobs/${id}`, method: 'PUT', data: payload }),
  remove: (id: number) =>
    request<{ id: number; cancelled: boolean }>({ url: `/jobs/${id}`, method: 'DELETE' }),
  run: (id: number) =>
    request<{ job_id: number; run: SyncRun }>({ url: `/jobs/${id}/run`, method: 'POST' }),
  cancel: (id: number) =>
    request<{ job_id: number; cancelled: boolean }>({ url: `/jobs/${id}/cancel`, method: 'POST' }),
  schedule: (id: number) =>
    request<{ next_run_at: string | null; scheduled: boolean; preview: string[] }>({
      url: `/jobs/${id}/schedule`,
    }),
};

export const runs = {
  list: (limit = 50, jobId?: number) =>
    request<{ runs: SyncRun[] }>({ url: '/runs', params: { limit, job_id: jobId } }),
  get: (id: number) =>
    request<{ run: SyncRun; items: RunItem[] }>({ url: `/runs/${id}`, method: 'GET' }),
  items: (id: number, limit = 200) =>
    request<{ items: RunItem[]; limit: number }>({ url: `/runs/${id}/items`, params: { limit } }),
};

export const audits = {
  list: (limit = 50) => request<{ audits: AuditRun[] }>({ url: '/audits', params: { limit } }),
  start: (payload: AuditPayload) =>
    request<{ audit: AuditRun }>({ url: '/audits', method: 'POST', data: payload }),
  get: (id: number) =>
    request<{ audit: AuditRun; entries: AuditEntry[] }>({ url: `/audits/${id}`, method: 'GET' }),
  cancel: (id: number) =>
    request<{ id: number; cancelled: boolean }>({ url: `/audits/${id}/cancel`, method: 'POST' }),
  /**
   * exportHref is the browser URL of the CSV download. It is an absolute path
   * (not a request through the JSON client) because the server answers a file
   * body the browser saves directly.
   */
  exportHref: (id: number) => `/api/audits/${id}/export`,
};

export const dashboard = {
  stats: (days = 1) =>
    request<{
      days: number;
      since: string;
      stats: Stats;
      running_jobs: number;
      running_ids: number[];
    }>({ url: '/stats', params: { days } }),
};

export const settings = {
  get: () =>
    request<{ settings: AppSettings; locales: string[]; themes: string[] }>({ url: '/settings' }),
  update: (payload: AppSettings) =>
    request<{ settings: AppSettings }>({ url: '/settings', method: 'PUT', data: payload }),
  raw: () => request<{ values: Record<string, string> }>({ url: '/settings/raw' }),
};

export const providers = {
  list: () =>
    request<{ providers: ProviderInfo[]; redirect_hint: Record<string, string> }>({
      url: '/providers',
    }),
  get: (provider: ProviderName) => request<ProviderInfo>({ url: `/providers/${provider}` }),
  update: (
    provider: ProviderName,
    payload: { client_id: string; client_secret: string; redirect_uri: string; tenant_id?: string },
  ) => request<ProviderInfo>({ url: `/providers/${provider}`, method: 'PUT', data: payload }),
  clear: (provider: ProviderName) =>
    request<ProviderInfo>({ url: `/providers/${provider}`, method: 'DELETE' }),
  /**
   * guide is the app-registration walkthrough of a provider: the steps, the
   * console links, the redirect URI to register and the permission list to grant.
   */
  guide: (provider: ProviderName) => request<ProviderGuide>({ url: `/providers/${provider}/guide` }),
};

export const notifications = {
  list: () =>
    request<{ channels: NotificationChannel[]; kinds: ChannelKind[]; events: string[] }>({
      url: '/notifications',
    }),
  get: (id: number) => request<NotificationChannel>({ url: `/notifications/${id}` }),
  create: (payload: ChannelPayload) =>
    request<NotificationChannel>({ url: '/notifications', method: 'POST', data: payload }),
  update: (id: number, payload: ChannelPayload) =>
    request<NotificationChannel>({ url: `/notifications/${id}`, method: 'PUT', data: payload }),
  remove: (id: number) => request<void>({ url: `/notifications/${id}`, method: 'DELETE' }),
  setEnabled: (id: number, enabled: boolean) =>
    request<NotificationChannel>({
      url: `/notifications/${id}/enabled`,
      method: 'PUT',
      data: { enabled },
    }),
  test: (id: number) =>
    request<{ id: number; delivered: boolean }>({ url: `/notifications/${id}/test`, method: 'POST' }),
};

export const maintenance = {
  runScheduler: () =>
    request<{ started: number[] }>({ url: '/maintenance/schedule/run', method: 'POST' }),
  refreshTokens: () =>
    request<{ refreshed: number }>({ url: '/maintenance/tokens/refresh', method: 'POST' }),
  pruneStates: () =>
    request<{ removed: number }>({ url: '/maintenance/oauth/states/prune', method: 'POST' }),
  pruneRuns: (keep: number) =>
    request<{ keep: number; pruned: boolean }>({
      url: '/maintenance/runs/prune',
      method: 'POST',
      data: { keep },
    }),
};

export const system = {
  health: () =>
    request<{
      status: string;
      database: string;
      version: string;
      auth_mode: AuthMode;
      running_jobs: number;
    }>({ url: '/health', method: 'GET' }),
  version: () =>
    request<{
      info: BuildInfo;
      auth_mode: AuthMode;
      providers: string[];
      locales: string[];
      themes: string[];
    }>({ url: '/version', method: 'GET' }),
};
