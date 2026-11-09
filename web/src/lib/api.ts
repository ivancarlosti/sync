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
  /** purge_auditlogs_days is the age of a stored activity event. */
  purge_auditlogs_days: number;
  /** auditlogs_enabled arms the scheduled pass of the activity feed collector. */
  auditlogs_enabled: boolean;
  /**
   * auditlogs_interval_minutes is how often the collector polls the feeds; the
   * server bounds it to 1–1440 minutes.
   */
  auditlogs_interval_minutes: number;
  /**
   * auditlogs_request_delay_seconds is the pause between two provider calls of
   * one pass, the answer to a tenant that answers 429; 0 leaves it unpaced.
   */
  auditlogs_request_delay_seconds: number;
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

/**
 * ReportSection is one table of a directory report. The vocabulary is shared by
 * every provider; which half an account can collect is what
 * `GET /api/reports/sections` answers, so the start form only offers sections the
 * account's directory actually has (SharePoint sites on a Workspace domain, shared
 * drives on Entra ID).
 *
 * One scope is not part of the grant every connection already holds
 * (`admin.directory.customer.readonly`, for the Google `organization` record); an
 * account connected before it existed still reports, with the row derived from the
 * domain list. Two more (`analytics.readonly`, `youtube.readonly`) carry the two
 * Google-only sections, and those report the scope of the connection rather than
 * the domain: an older grant fails them alone until the account is reconnected.
 */
export type ReportSection =
  | 'organization'
  | 'users'
  | 'guests'
  | 'groups'
  | 'distribution_lists'
  | 'sites'
  | 'licenses'
  | 'domains'
  | 'suspended_users'
  | 'group_members'
  | 'org_units'
  | 'roles'
  | 'role_assignments'
  | 'shared_drives'
  | 'teams'
  | 'team_members'
  | 'planner_plans'
  | 'shared_drive_members'
  | 'analytics'
  | 'youtube';

/** ReportColumn is one column of a section (the CSV header and the UI label). */
export interface ReportColumn {
  key: string;
  title: string;
}

/** ReportRow is one record, aligned with the columns of its section. */
export type ReportRow = string[];

/** ReportSectionSummary is the table of contents entry of one stored section. */
export interface ReportSectionSummary {
  section: ReportSection;
  columns: ReportColumn[];
  row_count: number;
  /** truncated is true when the section stopped at the server row cap. */
  truncated: boolean;
  /** error is the reason the section could not be collected; empty on success. */
  error?: string;
}

/** ReportSectionPage is one page of a stored section. */
export interface ReportSectionPage {
  section: ReportSection;
  columns: ReportColumn[];
  rows: ReportRow[];
  total: number;
  offset: number;
  limit: number;
  truncated: boolean;
  error?: string;
}

/** DirectoryReport is one execution of a directory report. */
export interface DirectoryReport {
  id: number;
  account_id: number;
  account_email: string;
  provider: ProviderName;
  org_id?: string;
  /** sections is the stored comma separated list; section_list is its array. */
  sections: string;
  section_list: ReportSection[];
  status: 'running' | 'success' | 'partial' | 'failed' | 'cancelled' | 'timeout';
  trigger: 'manual' | 'scheduled';
  started_at: string;
  finished_at?: string;
  duration_ms: number;
  section_count: number;
  row_count: number;
  /** failed counts the sections that could not be collected. */
  failed: number;
  truncated: boolean;
  message?: string;
  running?: boolean;
  created_at: string;
}

/** ReportSchedule is the per-account opt-in of the automatic report. */
export interface ReportSchedule {
  id: number;
  account_id: number;
  enabled: boolean;
  interval_minutes: number;
  sections: string;
  section_list: ReportSection[];
  next_run_at?: string;
  last_run_at?: string;
  last_report_id?: number;
  last_error?: string;
  created_at: string;
  updated_at: string;
}

/** ReportPayload is what POST /api/reports accepts. */
export interface ReportPayload {
  account_id: number;
  sections: string[];
}

// -----------------------------------------------------------------------------
// Admin operations (GET/POST /api/management)
// -----------------------------------------------------------------------------

/** ManagementParam is one input an operation accepts. */
export interface ManagementParam {
  key: string;
  required: boolean;
}

/** ManagementUser is one account an Admin operation form can name. */
export interface ManagementUser {
  email: string;
  name: string;
  guest: boolean;
}

/** ManagementGroup is one group an Admin operation form can target. */
export interface ManagementGroup {
  email: string;
  name: string;
}

/** ManagementOptions are the selects the form of one operation reads. */
export interface ManagementOptions {
  users: ManagementUser[];
  groups: ManagementGroup[];
  drives: Drive[];
}

/** ManagementOperation is one entry of the catalogue, decorated per account. */
export interface ManagementOperation {
  kind: string;
  provider: ProviderName;
  capability: Capability;
  danger: boolean;
  /** supported is true when the account can run it now. */
  supported: boolean;
  /** missing_capability marks an operation a reconnect would unlock. */
  missing_capability: boolean;
  params: ManagementParam[];
}

/** ManagementStatus is the lifecycle of a task (see models.RunStatus). */
export type ManagementStatus =
  | 'running'
  | 'success'
  | 'partial'
  | 'failed'
  | 'cancelled'
  | 'timeout';

/** ManagementTask is one execution of an administrative operation. */
export interface ManagementTask {
  id: number;
  account_id: number;
  account_email: string;
  provider: ProviderName;
  kind: string;
  title: string;
  status: ManagementStatus;
  trigger: 'manual' | 'scheduled';
  params: string;
  /** param_map is the stored request as an object. */
  param_map: Record<string, string>;
  result?: string;
  step_count: number;
  failed: number;
  items_processed: number;
  bytes_moved: number;
  started_at: string;
  finished_at?: string;
  duration_ms: number;
  message?: string;
  running: boolean;
  created_at: string;
}

/** ManagementStep is one entry of the journal of a task. */
export interface ManagementStep {
  id: number;
  task_id: number;
  seq: number;
  name: string;
  status: 'success' | 'failed' | 'skipped';
  detail?: string;
  items: number;
  bytes: number;
  created_at: string;
}

/** ManagementTaskDetail is a task with its journal. */
export interface ManagementTaskDetail extends ManagementTask {
  steps: ManagementStep[];
}

/** ManagementStartPayload is what POST /api/management/tasks accepts. */
export interface ManagementStartPayload {
  account_id: number;
  kind: string;
  params: Record<string, string>;
}

/** AuditLogEvent is one stored activity event (GET /api/audit-logs). */
export interface AuditLogEvent {
  id: number;
  provider: ProviderName;
  /** source is the vendor feed the record came from (directory, drive, unified, …). */
  source: string;
  remote_id: string;
  /** org_id is the tenant (Microsoft) or Workspace domain (Google) of the event. */
  org_id: string;
  action: AuditLogAction;
  target: AuditLogTarget;
  /**
   * actor_kind says who acted: a person (`user`, whose address is actor_email), a
   * workload (`application`, whose name is actor_name and whose address column is
   * empty) or nobody at all (`system`). An empty value is an event stored before
   * the column existed and is read as `user` when it carries an address.
   */
  actor_kind: AuditLogActorKind;
  actor_email: string;
  actor_name?: string;
  actor_id?: string;
  /** actor_on_behalf_of names the person a workload acted for, when it is known. */
  actor_on_behalf_of?: string;
  actor_on_behalf_of_name?: string;
  target_name?: string;
  target_id?: string;
  drive_id?: string;
  site_id?: string;
  occurred_at: string;
}

/** AuditLogActorKind is the nature of the principal behind an activity event. */
export type AuditLogActorKind = 'user' | 'application' | 'system';

/** AuditLogAction is the normalised verb of an activity event. */
export type AuditLogAction = 'create' | 'delete' | 'update' | 'move' | 'share' | 'login' | 'other';

/** AuditLogTarget is the normalised kind of object an activity event touched. */
export type AuditLogTarget =
  | 'file'
  | 'folder'
  | 'user'
  | 'group'
  | 'device'
  | 'application'
  | 'site'
  | 'drive'
  | 'other';

/**
 * AuditLogCursor is the position and health of one feed: where the collector
 * stands for one organisation and stream, and why a list may be stale.
 */
export interface AuditLogCursor {
  provider: ProviderName;
  org_id: string;
  stream: string;
  cursor?: string;
  last_event_at?: string;
  last_run_at?: string;
  healthy: boolean;
  /** needs_consent marks a feed waiting for a grant the operator has to make. */
  needs_consent: boolean;
  last_error?: string;
}

/** AuditLogFilters are the query parameters of the list and the export. */
export interface AuditLogFilters {
  provider?: string;
  org_id?: string;
  action?: string;
  target?: string;
  actor?: string;
  search?: string;
  from?: string;
  to?: string;
}

/**
 * AuditLogSchedule is the collector as the operator configured it, answered next
 * to the list: whether the schedule runs, how often it polls and when the next
 * pass is due. It explains a list that stops growing.
 */
export interface AuditLogSchedule {
  enabled: boolean;
  interval_minutes: number;
  request_delay_seconds: number;
  /** last_run_at is when the previous pass finished in this process. */
  last_run_at?: string;
  /** next_run_at is absent while the capture is switched off. */
  next_run_at?: string;
}

/** AuditLogPage is one page of the activity feed. */
export interface AuditLogPage {
  events: AuditLogEvent[];
  total: number;
  limit: number;
  offset: number;
  cursors: AuditLogCursor[];
  schedule: AuditLogSchedule;
}

/** AuditLogCursors is the status of every feed behind the list. */
export interface AuditLogCursors {
  cursors: AuditLogCursor[];
  schedule: AuditLogSchedule;
}

/** AuditLogCollectResult is what POST /api/maintenance/auditlogs/collect answers. */
export interface AuditLogCollectResult {
  organisations: number;
  streams: number;
  failed: number;
  events: number;
  /** repaired is how many stored events the repair pass re-attributed. */
  repaired: number;
  /** pending_repairs is how many stored events still await a repair. */
  pending_repairs: number;
  duration_ms: number;
  errors?: string[];
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
  | 'teams'
  | 'planner'
  | 'domains'
  | 'orgunits'
  | 'roles'
  | 'licenses'
  | 'logs'
  | 'mail'
  | 'calendars'
  | 'delegation'
  | 'sitesadmin'
  | 'analytics'
  | 'youtube';

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
    request<{ run: SyncRun; items: RunItem[]; traffic: TrafficEntry[] }>({
      url: `/runs/${id}`,
      method: 'GET',
    }),
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

/**
 * reports is the directory report behind the Reports screen: a read-only
 * snapshot of an organisation (users, groups, sites, licences, domains) that is
 * collected in the background and read one page of one section at a time.
 */
export const reports = {
  /** sections lists what an account's provider can collect. */
  sections: (accountId?: number) =>
    request<{ sections: ReportSection[]; supported: boolean }>({
      url: '/reports/sections',
      params: { account_id: accountId },
    }),
  list: (limit = 50) => request<{ reports: DirectoryReport[] }>({ url: '/reports', params: { limit } }),
  start: (payload: ReportPayload) =>
    request<{ report: DirectoryReport }>({ url: '/reports', method: 'POST', data: payload }),
  get: (id: number) =>
    request<{ report: DirectoryReport; sections: ReportSectionSummary[] }>({
      url: `/reports/${id}`,
      method: 'GET',
    }),
  /** section reads one page of one stored table. */
  section: (id: number, section: string, offset = 0, limit = 200) =>
    request<ReportSectionPage>({
      url: `/reports/${id}/sections/${section}`,
      params: { offset, limit },
    }),
  cancel: (id: number) =>
    request<{ id: number; cancelled: boolean }>({ url: `/reports/${id}/cancel`, method: 'POST' }),
  /**
   * exportHref is the browser URL of the ZIP download: the server streams one
   * CSV per section, so the client never buffers the workbook.
   */
  exportHref: (id: number) => `/api/reports/${id}/export`,
  schedules: () => request<{ schedules: ReportSchedule[] }>({ url: '/reports/schedules' }),
  saveSchedule: (
    accountId: number,
    payload: { enabled: boolean; interval_minutes: number; sections: string[] },
  ) =>
    request<{ schedule: ReportSchedule }>({
      url: `/reports/schedules/${accountId}`,
      method: 'PUT',
      data: payload,
    }),
  deleteSchedule: (accountId: number) =>
    request<{ account_id: number; deleted: boolean }>({
      url: `/reports/schedules/${accountId}`,
      method: 'DELETE',
    }),
};

/**
 * management is the Admin operations screen: the per-account operation catalogue,
 * the start request and the auditable journal of every task.
 */
export const management = {
  /** operations lists what an account can run right now. */
  operations: (accountId: number) =>
    request<{ operations: ManagementOperation[] }>({
      url: '/management/operations',
      params: { account_id: accountId },
    }),
  /** options reads the users, groups and drives one operation form offers. */
  options: (accountId: number, kind: string) =>
    request<ManagementOptions>({
      url: '/management/options',
      params: { account_id: accountId, kind },
    }),
  list: (accountId?: number, limit = 50) =>
    request<{ tasks: ManagementTask[] }>({
      url: '/management/tasks',
      params: { account_id: accountId, limit },
    }),
  start: (payload: ManagementStartPayload) =>
    request<{ task: ManagementTask }>({ url: '/management/tasks', method: 'POST', data: payload }),
  get: (id: number) => request<{ task: ManagementTaskDetail }>({ url: `/management/tasks/${id}` }),
  cancel: (id: number) =>
    request<{ id: number; cancelled: boolean }>({
      url: `/management/tasks/${id}/cancel`,
      method: 'POST',
    }),
  /** exportHref is the browser URL of the journal CSV download. */
  exportHref: (id: number) => `/api/management/tasks/${id}/export`,
};

/**
 * auditLogs is the activity feed behind the Audit Logs screen: the stored
 * history with its filters, the status of every feed, the CSV export and the
 * on-demand collector pass.
 */
export const auditLogs = {
  list: (filters: AuditLogFilters = {}, limit = 50, offset = 0) =>
    request<AuditLogPage>({
      url: '/audit-logs',
      params: { ...filters, limit, offset },
    }),
  cursors: () => request<AuditLogCursors>({ url: '/audit-logs/cursors' }),
  /**
   * exportHref is the browser URL of the CSV download. It is an absolute path
   * (not a request through the JSON client) because the server answers a file
   * body the browser saves directly.
   */
  exportHref: (filters: AuditLogFilters = {}) => {
    const query = new URLSearchParams();
    for (const [key, value] of Object.entries(filters)) {
      if (typeof value === 'string' && value !== '') {
        query.set(key, value);
      }
    }
    const suffix = query.toString();
    return `/api/audit-logs/export${suffix ? `?${suffix}` : ''}`;
  },
  /** collect asks the server to poll every feed right away. */
  collect: () =>
    request<AuditLogCollectResult>({ url: '/maintenance/auditlogs/collect', method: 'POST' }),
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
export type PurgeTarget = 'all' | 'jobs' | 'audits' | 'billing' | 'auditlogs';

/** PurgeOutcome is what a purge pass erased and the age it applied. */
export interface PurgeOutcome {
  target: string;
  /** days is the age applied to each target, keyed by target name. */
  days: Record<string, number>;
  removed: { jobs: number; audits: number; billing: number; auditlogs: number };
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
