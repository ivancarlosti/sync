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
  method: AuthMode | 'token';
  admin: boolean;
  expires_at?: string;
  /** token is the name of the API token that authenticated the request. */
  token?: string;
  /** scopes is what an API token may do; a session carries none (it may do all). */
  scopes?: string[];
}

/** AppSettings are the defaults an administrator can change in Admin > Settings. */
export interface AppSettings {
  default_locale: string;
  default_theme: string;
  sync_default_interval_minutes: number;
  sync_run_timeout_minutes: number;
  /** billing_schema is the line-item vocabulary of the billing report. */
  billing_schema: string;
  /** billing_schema_custom is the JSON document of the `custom` schema. */
  billing_schema_custom: string;
  /**
   * billing_rate_in_per_gib prices one GiB read from a source account; 0 is the
   * common case (ingress is free) and leaves the inbound cost at zero.
   */
  billing_rate_in_per_gib: number;
  /** billing_rate_out_per_gib prices one GiB written to a destination account. */
  billing_rate_out_per_gib: number;
  billing_currency: string;
  /** purge_enabled arms the automatic retention pass of the scheduler. */
  purge_enabled: boolean;
  /** purge_jobs_days is the age, in days, past which finished runs are erased. */
  purge_jobs_days: number;
  /** purge_audits_days is the age of a finished content-audit report. */
  purge_audits_days: number;
  /** purge_billing_days is the age of a traffic day of the billing report. */
  purge_billing_days: number;
}

/**
 * BillingSchema is the line-item vocabulary of the billing report. The two rates
 * price the report: inbound is what was read from a source account (0 is the
 * common case, ingress is free) and outbound what was written to a destination
 * account. With both at 0 the report stays in bytes and carries no cost.
 */
export interface BillingSchema {
  id: string;
  label: string;
  hint?: string;
  inbound_label: string;
  outbound_label: string;
  operations_label: string;
  cost_label: string;
  unit: string;
  currency?: string;
  rate_in_per_gib: number;
  rate_out_per_gib: number;
  /** default_rate_in_per_gib is the indicative list price the preset ships. */
  default_rate_in_per_gib: number;
  /** default_rate_out_per_gib is the indicative list price the preset ships. */
  default_rate_out_per_gib: number;
  custom: boolean;
}

/** TrafficTotals are the counters of one direction of a billing row. */
export interface TrafficTotals {
  bytes: number;
  files: number;
  operations: number;
  runs: number;
}

/** BillingRow is one line of a billing report. */
export interface BillingRow {
  day?: string;
  month?: string;
  account_id?: number;
  account_email?: string;
  run_id?: number;
  job_id?: number;
  inbound: TrafficTotals;
  outbound: TrafficTotals;
  total: TrafficTotals;
}

/** TrafficEntry is the traffic one run moved through one account. */
export interface TrafficEntry {
  id: number;
  run_id: number;
  job_id: number;
  account_id: number;
  account_email?: string;
  provider?: string;
  direction: 'inbound' | 'outbound';
  bytes: number;
  files: number;
  operations: number;
  day: string;
  month: string;
  started_at: string;
}

/** APITokenView is the public shape of an API token (never the secret). */
export interface APITokenView {
  id: number;
  name: string;
  prefix: string;
  scopes: string[];
  created_by?: string;
  expires_at?: string;
  revoked_at?: string;
  last_used_at?: string;
  last_used_ip?: string;
  requests: number;
  created_at: string;
  state: 'active' | 'revoked' | 'expired';
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
  /**
   * account_kind is the family of the account when the provider can tell them
   * apart ("personal" or "work"), and is absent when it cannot. A personal
   * Microsoft account can never hold the directory permissions, which the API
   * then reports as not applicable instead of missing.
   */
  account_kind?: 'personal' | 'work';
  /**
   * org_id is the organisation the account belongs to (the Entra tenant, the
   * Workspace domain) and is what the Audit Logs screen filters by: the feed is
   * read once per organisation, not once per account. It is absent when no
   * source could name one (a personal account, whose organisation has no feed),
   * which the screen offers as an account without activity to filter by.
   */
  org_id?: string;
  scopes: string[];
  /** Capabilities the stored grant satisfies, and the ones it misses. */
  capabilities: Capability[];
  missing_capabilities: Capability[];
  /**
   * not_applicable_capabilities cannot be granted to this account at all: no
   * reconnection changes that, so they are not missing and must not read as an
   * error.
   */
  not_applicable_capabilities: Capability[];
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
  /** source_drive_name/kind/owner label the root (see lib/drives.ts). */
  source_drive_name: string;
  source_drive_kind: string;
  source_drive_owner: string;
  source_folder_id: string;
  source_folder_path: string;
  destination_drive_id: string;
  destination_drive_name: string;
  destination_drive_kind: string;
  destination_drive_owner: string;
  destination_folder_id: string;
  destination_folder_path: string;
  direction: 'google_to_microsoft' | 'microsoft_to_google' | 'bidirectional';
  conflict_policy: 'newest_wins' | 'source_wins' | 'destination_wins' | 'skip';
  exclude_patterns: string[];
  delete_missing: boolean;
  sync_empty_folders: boolean;
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
  source_drive_name: string;
  source_drive_kind: string;
  source_drive_owner: string;
  source_folder_id: string;
  source_folder_path: string;
  destination_drive_id: string;
  destination_drive_name: string;
  destination_drive_kind: string;
  destination_drive_owner: string;
  destination_folder_id: string;
  destination_folder_path: string;
  direction: string;
  conflict_policy: string;
  exclude_patterns: string[];
  delete_missing: boolean;
  sync_empty_folders: boolean;
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
  folders_deleted: number;
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

/** CalendarSourceKind names where the events of a calendar job are read from. */
export type CalendarSourceKind = 'google' | 'microsoft' | 'ics';

/**
 * CalendarJob is a pairing of two calendars plus its schedule: a source (a
 * provider calendar or an .ics subscription) and a writable destination. The
 * display names of both ends travel with the row, so the list renders without a
 * provider round trip.
 */
export interface CalendarJob {
  id: number;
  name: string;
  source_kind: CalendarSourceKind;
  /** source_account_id is zero for an .ics subscription. */
  source_account_id: number;
  source_calendar_id: string;
  source_calendar_name: string;
  /** The subscription half; the password is never returned by the API. */
  source_url?: string;
  source_username?: string;
  source_etag?: string;
  source_last_modified?: string;
  source_fetched_at?: string;
  destination_account_id: number;
  destination_calendar_id: string;
  destination_calendar_name: string;
  direction: 'bidirectional' | 'one_way';
  conflict_policy: 'newest_wins' | 'source_wins' | 'destination_wins' | 'skip';
  delete_missing: boolean;
  past_days: number;
  future_days: number;
  interval_minutes: number;
  enabled: boolean;
  /** running is the decoration the API adds when a run is in flight. */
  running: boolean;
  last_run_at?: string;
  next_run_at?: string;
  last_status?: string;
  last_error?: string;
  created_at: string;
  updated_at: string;
}

/**
 * CalendarPayload is what POST/PUT /api/calendars accepts. `source_password` is
 * write-only: an empty value keeps the stored secret and
 * `clear_source_password` erases it.
 */
export interface CalendarPayload {
  name: string;
  source_kind: string;
  source_account_id: number;
  source_calendar_id: string;
  source_calendar_name: string;
  source_url: string;
  source_username: string;
  source_password: string;
  clear_source_password: boolean;
  destination_account_id: number;
  destination_calendar_id: string;
  destination_calendar_name: string;
  direction: string;
  conflict_policy: string;
  delete_missing: boolean;
  past_days: number;
  future_days: number;
  interval_minutes: number;
  enabled: boolean;
}

/** CalendarOption is one entry of the calendar picker of an account. */
export interface CalendarOption {
  id: string;
  name: string;
  primary: boolean;
  time_zone?: string;
  read_only: boolean;
}

/** CalendarRun is one execution of a calendar job. */
export interface CalendarRun {
  id: number;
  job_id: number;
  job_name: string;
  status: 'running' | 'success' | 'partial' | 'failed' | 'cancelled' | 'timeout';
  trigger: 'manual' | 'scheduled';
  started_at: string;
  finished_at?: string;
  duration_ms: number;
  events_scanned: number;
  events_created: number;
  events_updated: number;
  events_deleted: number;
  events_skipped: number;
  conflicts: number;
  errors: number;
  message?: string;
  created_at: string;
}

/** CalendarRunItem is the evidence of one decision of a calendar run. */
export interface CalendarRunItem {
  id: number;
  run_id: number;
  job_id: number;
  /** action is the plan vocabulary: create_destination, update_source, … */
  action: string;
  uid: string;
  summary: string;
  start_at: string;
  evidence?: string;
  created_at: string;
}

/** CalendarPreviewDecision is one planned action and why it was planned. */
export interface CalendarPreviewDecision {
  uid: string;
  summary: string;
  start_at: string;
  action: string;
  evidence: string;
}

/** CalendarPreview is what a run would do right now, without writing anything. */
export interface CalendarPreview {
  job_id: number;
  window_start: string;
  window_end: string;
  source_listed: number;
  destination_listed: number;
  events_scanned: number;
  plan: CalendarPreviewDecision[];
}

/** FeedSampleEvent is one event of an .ics preview. */
export interface FeedSampleEvent {
  uid: string;
  summary: string;
  start_at: string;
  end_at: string;
  all_day: boolean;
  recurring: boolean;
}

/** FeedPreview is the answer of "check this subscription" before a job exists. */
export interface FeedPreview {
  etag?: string;
  last_modified?: string;
  window_start: string;
  window_end: string;
  events: number;
  recurring: number;
  cancelled: number;
  sample: FeedSampleEvent[];
}

/** AuditRun is one execution of a content audit. */
export interface AuditRun {
  id: number;
  account_id: number;
  account_email: string;
  provider: ProviderName;
  drive_id: string;
  /** drive_name/kind/owner label the root of the report (see lib/drives.ts). */
  drive_name: string;
  drive_kind: string;
  drive_owner: string;
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
  /**
   * listed is whether the folder's children are part of the report. A folder at
   * the depth boundary still has exact totals (its subtree was walked and
   * counted) but says listed=false, which tells an empty folder from a listing
   * that stops at the requested depth.
   */
  listed: boolean;
  modified_at?: string;
  mime_type?: string;
}

/** AuditPayload is what POST /api/audits accepts. */
export interface AuditPayload {
  account_id: number;
  drive_id: string;
  drive_name: string;
  drive_kind: string;
  drive_owner: string;
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
export type Capability = 'files';

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
  /**
   * keycloakLogout asks the server for the realm end-session URL. Following it is
   * what ends the *single sign-on* session: without it the realm hands the same
   * account back on the next attempt, which traps an operator the allow-list
   * refused. The client id and APP_URL stay server side; the SPA only navigates.
   */
  keycloakLogout: () => request<{ url: string }>({ url: '/auth/keycloak/logout', method: 'GET' }),
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
   * calendarOptions lists the calendars of an account for the source and the
   * destination pickers. A provider without calendars in this build answers a
   * validation error, which is what the editor shows next to the picker.
   */
  calendarOptions: (id: number) =>
    request<{ calendars: CalendarOption[] }>({ url: `/accounts/${id}/calendars` }),
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
    request<{ run: SyncRun; items: RunItem[]; traffic: TrafficEntry[] }>({
      url: `/runs/${id}`,
      method: 'GET',
    }),
  items: (id: number, limit = 200) =>
    request<{ items: RunItem[]; limit: number }>({ url: `/runs/${id}/items`, params: { limit } }),
};

// calendars is the calendar half of the engine: the pairings of this instance
// (see internal/handlers/calendars.go), their history and the two dry runs the
// editor offers. `feedPreview` validates an .ics URL before a job exists, so a
// typo costs one request instead of a failed first run.
export const calendars = {
  list: () => request<{ calendars: CalendarJob[] }>({ url: '/calendars', method: 'GET' }),
  get: (id: number) => request<CalendarJob>({ url: `/calendars/${id}`, method: 'GET' }),
  create: (payload: CalendarPayload) =>
    request<CalendarJob>({ url: '/calendars', method: 'POST', data: payload }),
  update: (id: number, payload: CalendarPayload) =>
    request<CalendarJob>({ url: `/calendars/${id}`, method: 'PUT', data: payload }),
  remove: (id: number) =>
    request<{ id: number; cancelled: boolean }>({ url: `/calendars/${id}`, method: 'DELETE' }),
  run: (id: number) =>
    request<{ job_id: number; run: CalendarRun }>({ url: `/calendars/${id}/run`, method: 'POST' }),
  cancel: (id: number) =>
    request<{ job_id: number; cancelled: boolean }>({
      url: `/calendars/${id}/cancel`,
      method: 'POST',
    }),
  runs: (id: number, limit = 20) =>
    request<{ runs: CalendarRun[] }>({ url: `/calendars/${id}/runs`, params: { limit } }),
  runDetail: (id: number, runId: number) =>
    request<{ run: CalendarRun; items: CalendarRunItem[] }>({
      url: `/calendars/${id}/runs/${runId}`,
      method: 'GET',
    }),
  preview: (id: number) => request<CalendarPreview>({ url: `/calendars/${id}/preview` }),
  feedPreview: (payload: {
    url: string;
    username?: string;
    password?: string;
    past_days?: number;
    future_days?: number;
  }) => request<FeedPreview>({ url: '/calendars/feed-preview', method: 'POST', data: payload }),
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
    request<{
      settings: AppSettings;
      locales: string[];
      themes: string[];
      billing_schemas: BillingSchema[];
      billing_schema_default: string;
    }>({ url: '/settings' }),
  update: (payload: AppSettings) =>
    request<{ settings: AppSettings }>({ url: '/settings', method: 'PUT', data: payload }),
  raw: () => request<{ values: Record<string, string> }>({ url: '/settings/raw' }),
};

/**
 * tokens manages the programmatic credentials of the API (Admin > API). The
 * secret is returned exactly once, by `create` and `regenerate`: every other
 * answer, including `list`, only ever carries the public prefix.
 */
export const tokens = {
  list: () => request<{ tokens: APITokenView[]; scopes: string[] }>({ url: '/tokens' }),
  create: (payload: { name: string; scopes?: string[]; expires_in_days?: number }) =>
    request<{ token: APITokenView; secret: string }>({ url: '/tokens', method: 'POST', data: payload }),
  regenerate: (id: number) =>
    request<{ token: APITokenView; secret: string }>({
      url: `/tokens/${id}/regenerate`,
      method: 'POST',
    }),
  revoke: (id: number) =>
    request<{ token: APITokenView }>({ url: `/tokens/${id}/revoke`, method: 'POST' }),
  remove: (id: number) => request<void>({ url: `/tokens/${id}`, method: 'DELETE' }),
};

/** BillingGroupBy is the bucket a billing report is grouped by. */
export type BillingGroupBy = 'day' | 'month' | 'account' | 'run';

/** BillingQuery is the window and the grouping of a billing report. */
export interface BillingQuery {
  from?: string;
  to?: string;
  group_by?: BillingGroupBy;
  account_id?: number;
  job_id?: number;
  limit?: number;
}

export const billing = {
  summary: (query: BillingQuery = {}) =>
    request<{
      group_by: BillingGroupBy;
      from: string;
      to: string;
      schema: BillingSchema;
      rows: BillingRow[];
      total: TrafficTotals;
    }>({ url: '/billing', params: query }),
  runs: (query: BillingQuery = {}) =>
    request<{ from: string; to: string; traffic: TrafficEntry[] }>({
      url: '/billing/runs',
      params: query,
    }),
  /** exportHref is the browser URL of the CSV download (a file, not JSON). */
  exportHref: (query: BillingQuery = {}) => {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined && value !== '') {
        params.set(key, String(value));
      }
    }
    const encoded = params.toString();
    return `/api/billing/export${encoded === '' ? '' : `?${encoded}`}`;
  },
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

/** PurgeTarget is what a purge pass erases. */
export type PurgeTarget = 'all' | 'jobs' | 'audits' | 'billing';

/** PurgeOutcome is what a purge pass erased and the age it applied. */
export interface PurgeOutcome {
  target: string;
  /** days is the age applied to each target, keyed by target name. */
  days: Record<string, number>;
  removed: { jobs: number; audits: number; billing: number };
}

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
  /**
   * purge erases the records older than `days`; 0 applies the retention saved in
   * Admin > Settings, which is what a "purge now" button sends. The pass is
   * age-based (unlike pruneRuns, which counts): it never touches a run or an
   * audit that is still going.
   */
  purge: (target: PurgeTarget = 'all', days = 0) =>
    request<PurgeOutcome>({
      url: '/maintenance/purge',
      method: 'POST',
      data: { target, days },
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
