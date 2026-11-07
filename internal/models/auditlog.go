package models

import "time"

// AuditLogEvent is one normalised activity event: who did what, to which object,
// when. It is the durable copy of a vendor feed the polling collector reads (see
// plan 2026-11-07-audit-logs.md), which is what makes the two feeds comparable
// and keeps the history past the vendor retention window (Office 365 management
// activity blobs expire after about seven days).
//
// It is deliberately not AuditEntry: an AuditEntry is a node of a *content*
// report (a folder or file with a size), while this is an event of an *activity*
// log. The two are unrelated records that happen to share the word "audit".
//
// The identity of an event is (Provider, Source, RemoteID): the vendor id of the
// record, scoped to the feed it came from, so re-reading an overlapping window
// cannot insert the same event twice.
type AuditLogEvent struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// Provider is the value stored in connected_accounts.provider.
	Provider string `gorm:"size:32;uniqueIndex:idx_auditlog_event_identity" json:"provider"`
	// Source names the vendor feed the record came from (directory, drive,
	// unified, admin, login, token, …): two feeds can use unrelated id spaces.
	Source string `gorm:"size:32;uniqueIndex:idx_auditlog_event_identity" json:"source"`
	// RemoteID is the id the vendor assigned to the record.
	RemoteID string `gorm:"size:191;uniqueIndex:idx_auditlog_event_identity" json:"remote_id"`
	// OrgID is the tenant (Microsoft) or Workspace domain (Google) the event
	// belongs to. It groups the collectors of several administrator accounts
	// under the one organisation-wide feed.
	OrgID string `gorm:"size:191;index:idx_auditlog_event_org_time" json:"org_id"`

	// Action is the normalised verb (AuditLogAction) and Target the normalised
	// kind of object it touched (AuditLogTarget). The vendor-specific spelling
	// stays in Raw.
	Action string `gorm:"size:32;index" json:"action"`
	Target string `gorm:"size:32;index" json:"target"`

	// Actor is who acted. The e-mail is the indexed, human readable key; the id
	// is what stays stable when the address changes.
	ActorEmail string `gorm:"size:191;index" json:"actor_email"`
	ActorName  string `gorm:"size:191" json:"actor_name,omitempty"`
	ActorID    string `gorm:"size:191" json:"actor_id,omitempty"`
	// ActorKind says whether the actor is a person, a workload or nobody at
	// all (AuditLogActorKind). A workload fills ActorName and leaves ActorEmail
	// empty, so an application name never travels in the address column. An
	// empty value is a row stored before the column existed, which is what the
	// repair pass looks for (see plan 2026-11-07-audit-log-actor-attribution.md).
	ActorKind string `gorm:"size:32;index" json:"actor_kind"`
	// ActorOnBehalfOf names the person a workload acted for, when the vendor
	// record allows recovering it: a device registration is made by "Device
	// Registration Service" on behalf of the human who enrolled the device.
	// ActorOnBehalfOf is the address and ActorOnBehalfOfName the label; both
	// stay empty when the vendor does not say (never guessed).
	ActorOnBehalfOf     string `gorm:"size:191;index" json:"actor_on_behalf_of,omitempty"`
	ActorOnBehalfOfName string `gorm:"size:191" json:"actor_on_behalf_of_name,omitempty"`

	// The object the action touched. TargetName is the display label the vendor
	// reported, TargetID its id. DriveID/SiteID locate a file operation on a
	// SharePoint site or a shared drive so a row can be narrowed to a location.
	TargetName string `gorm:"size:512" json:"target_name,omitempty"`
	TargetID   string `gorm:"size:191" json:"target_id,omitempty"`
	DriveID    string `gorm:"size:191" json:"drive_id,omitempty"`
	SiteID     string `gorm:"size:191" json:"site_id,omitempty"`

	// OccurredAt is when the vendor says the event happened (never the time the
	// collector read it), which is what the screen sorts and filters on.
	OccurredAt time.Time `gorm:"index:idx_auditlog_event_org_time" json:"occurred_at"`
	// Raw is the vendor record as JSON, kept so a field the normaliser did not
	// map is still recoverable without a second read (and past the retention
	// window). It is never serialised to the API.
	Raw string `gorm:"type:text" json:"-"`

	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (AuditLogEvent) TableName() string { return "audit_log_events" }

// AuditLogCursor is the collector position of one feed of one organisation. It
// is what makes the polling incremental: the next pass resumes from Cursor
// instead of re-reading the whole window.
//
// The identity is (Provider, OrgID, Stream): one organisation is read once, not
// once per connected administrator account, which is what stops the same
// tenant-wide event from being stored N times.
type AuditLogCursor struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	Provider string `gorm:"size:32;uniqueIndex:idx_auditlog_cursor_identity" json:"provider"`
	OrgID    string `gorm:"size:191;uniqueIndex:idx_auditlog_cursor_identity" json:"org_id"`
	// Stream names one feed of the organisation (directory, drive, admin, …).
	Stream string `gorm:"size:32;uniqueIndex:idx_auditlog_cursor_identity" json:"stream"`
	// Cursor is the opaque resume token the provider handed back (a page token,
	// a watermark id or an ISO timestamp). Empty means "start from the default
	// window".
	Cursor string `gorm:"size:512" json:"cursor,omitempty"`
	// LastEventAt is the OccurredAt of the newest event the cursor covers, which
	// is the human readable form of the position.
	LastEventAt *time.Time `json:"last_event_at,omitempty"`
	// LastRunAt is when the collector last advanced this cursor successfully.
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	// LastError is the last failure of this feed, kept so the screen can explain
	// why a list is stale instead of showing a silently empty history.
	LastError string `gorm:"type:text" json:"last_error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the physical table name.
func (AuditLogCursor) TableName() string { return "audit_log_cursors" }
