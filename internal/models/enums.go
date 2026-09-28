// Package models declares every GORM entity persisted by Sync.
//
// Design rules that every model respects:
//
//   - IDs are auto-increment integers (`BIGINT UNSIGNED`), friendly to MySQL/MariaDB.
//   - Mutable state is always stored as an explicit string enum with a matching
//     constant, so a row stays readable directly in SQL.
//   - Every secret column stores the AES-GCM payload produced by internal/crypto
//     (`v1:...`); plaintext never reaches the database (see docs/database.md).
//   - JSON-ish blobs (channel config, exclude patterns) live in `type:text`
//     columns marshalled by the services layer: the schema stays portable and
//     the SQL readable.
package models

// AuthMode selects how operators authenticate against Sync.
type AuthMode string

const (
	// AuthModeNone leaves the instance open; every visitor is an administrator.
	AuthModeNone AuthMode = "none"
	// AuthModeAccount validates a single credential pair from the environment.
	AuthModeAccount AuthMode = "account"
	// AuthModeKeycloak delegates login to a Keycloak (OIDC) realm.
	AuthModeKeycloak AuthMode = "keycloak"
)

// Valid reports whether the value is a supported authentication mode.
func (a AuthMode) Valid() bool {
	switch a {
	case AuthModeNone, AuthModeAccount, AuthModeKeycloak:
		return true
	default:
		return false
	}
}

// ProviderName identifies a cloud storage provider implementation.
type ProviderName string

const (
	ProviderGoogle    ProviderName = "google"
	ProviderMicrosoft ProviderName = "microsoft"
)

// Valid reports whether the value is a supported provider.
func (p ProviderName) Valid() bool {
	return p == ProviderGoogle || p == ProviderMicrosoft
}

// AccountStatus describes the health of a connected account.
type AccountStatus string

const (
	// AccountConnected means the stored refresh/access token is usable.
	AccountConnected AccountStatus = "connected"
	// AccountError means the last token refresh failed (reconnect required).
	AccountError AccountStatus = "error"
)

// SyncDirection is the requested data flow. The two directional values keep the
// vocabulary of the specification even though source/destination are concrete
// accounts, which also allows intra-provider jobs (Google → Google, Microsoft →
// Microsoft).
type SyncDirection string

const (
	// DirectionGoogleToMicrosoft copies source → destination.
	DirectionGoogleToMicrosoft SyncDirection = "google_to_microsoft"
	// DirectionMicrosoftToGoogle copies destination → source.
	DirectionMicrosoftToGoogle SyncDirection = "microsoft_to_google"
	// DirectionBidirectional keeps both sides aligned.
	DirectionBidirectional SyncDirection = "bidirectional"
)

// Valid reports whether the value is a supported direction.
func (d SyncDirection) Valid() bool {
	switch d {
	case DirectionGoogleToMicrosoft, DirectionMicrosoftToGoogle, DirectionBidirectional:
		return true
	default:
		return false
	}
}

// ConflictPolicy decides what happens when both sides changed a file since the
// last successful synchronisation.
type ConflictPolicy string

const (
	// ConflictNewestWins keeps the file with the most recent modification time.
	ConflictNewestWins ConflictPolicy = "newest_wins"
	// ConflictSourceWins always overwrites the destination with the source.
	ConflictSourceWins ConflictPolicy = "source_wins"
	// ConflictDestinationWins always overwrites the source with the destination.
	ConflictDestinationWins ConflictPolicy = "destination_wins"
	// ConflictSkip leaves both files untouched and records the conflict.
	ConflictSkip ConflictPolicy = "skip"
)

// Valid reports whether the value is a supported conflict policy.
func (c ConflictPolicy) Valid() bool {
	switch c {
	case ConflictNewestWins, ConflictSourceWins, ConflictDestinationWins, ConflictSkip:
		return true
	default:
		return false
	}
}

// RunStatus is the lifecycle state of a sync run.
type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunSuccess   RunStatus = "success"
	RunPartial   RunStatus = "partial"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// ItemAction is the operation performed on a single file inside a run.
type ItemAction string

const (
	ActionCreated       ItemAction = "created"
	ActionUpdated       ItemAction = "updated"
	ActionDeleted       ItemAction = "deleted"
	ActionSkipped       ItemAction = "skipped"
	ActionConflict      ItemAction = "conflict"
	ActionFailed        ItemAction = "failed"
	ActionFolderCreated ItemAction = "folder_created"
	ActionUnsupported   ItemAction = "unsupported"
	ActionRenamed       ItemAction = "renamed"
	ActionInSync        ItemAction = "in_sync"
)

// TriggerSource explains what started a run.
type TriggerSource string

const (
	TriggerManual    TriggerSource = "manual"
	TriggerScheduled TriggerSource = "scheduled"
)

// FlowKind distinguishes the two authorization-code flows Sync implements.
type FlowKind string

const (
	// FlowOAuth is a provider connection (Google Drive / Microsoft Graph).
	FlowOAuth FlowKind = "oauth"
	// FlowKeycloak is an operator login against a Keycloak realm.
	FlowKeycloak FlowKind = "keycloak"
)

// Notification events an operator can subscribe a channel to.
const (
	EventSyncSuccess      = "sync.success"
	EventSyncRunFailed    = "sync.run_failed"
	EventSyncError        = "sync.error"
	EventAccountConnected = "account.connected"
	EventAccountError     = "account.error"
	EventTest             = "test"
)

// AllEvents lists every event in the order the UI displays them.
var AllEvents = []string{
	EventSyncSuccess,
	EventSyncRunFailed,
	EventSyncError,
	EventAccountConnected,
	EventAccountError,
	EventTest,
}

// Settings keys seeded by internal/database and editable in Admin > Settings.
const (
	SettingDefaultLocale = "default_locale"
	SettingDefaultTheme  = "default_theme"
	SettingSyncInterval  = "sync_default_interval_minutes"
	SettingSyncTimeout   = "sync_run_timeout_minutes"
)

// ProviderConfigPrefix is the settings-key prefix holding credentials entered
// in Admin > Providers (they override the environment when present).
const ProviderConfigPrefix = "provider."
