package models

import "time"

// AuditRun is one execution of a content audit: it lists a provider folder tree
// to a chosen depth and stores the resulting report.
//
// It is the async brother of SyncRun: the row is written in the `running` state
// before the walk begins, the live counters are updated while it runs and the
// summary is written once at the end (see internal/services/audit.go).
type AuditRun struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	AccountID    uint   `gorm:"index" json:"account_id"`
	AccountEmail string `gorm:"size:191" json:"account_email"`
	Provider     string `gorm:"size:32" json:"provider"`

	// The root of the report: the drive and folder the operator picked, plus the
	// human readable path so the UI names it without walking the tree again.
	//
	// DriveName/Kind/Owner are the display parts of the root (see
	// providers.Drive): they are what tells an OneDrive from a SharePoint
	// library, and one library's site from another, on the history screen. They
	// stay empty on runs created before they existed.
	DriveID      string `gorm:"size:191" json:"drive_id"`
	DriveName    string `gorm:"size:191" json:"drive_name"`
	DriveKind    string `gorm:"size:32" json:"drive_kind"`
	DriveOwner   string `gorm:"size:191" json:"drive_owner"`
	RootFolderID string `gorm:"size:191" json:"root_folder_id"`
	RootPath     string `gorm:"size:1024" json:"root_path"`
	// MaxDepth bounds the *report*, not the walk: nodes deeper than it are not
	// stored as entries, but their subtree is still listed and counted so every
	// folder's total is exact. 0 is unlimited, and the selected folder is depth 0.
	MaxDepth int `json:"max_depth"`

	Status     string     `gorm:"size:32;index" json:"status"`
	Trigger    string     `gorm:"size:32" json:"trigger"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`

	// Summary counters. Files and Folders count the nodes below the root; Folders
	// includes the root itself.
	Files           int   `json:"files"`
	Folders         int   `json:"folders"`
	TotalSize       int64 `json:"total_size"`
	MaxDepthReached int   `json:"max_depth_reached"`
	// Truncated is true when the report hit the node cap and stopped early.
	Truncated bool `json:"truncated"`

	Message   string    `gorm:"type:text" json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (AuditRun) TableName() string { return "audit_runs" }

// AuditEntry is one node (folder or file) of an audit report. The rows are
// batch-inserted after the walk, so their id order is the DFS preorder of the
// tree and the table reads top-down.
type AuditEntry struct {
	ID      uint   `gorm:"primaryKey" json:"id"`
	AuditID uint   `gorm:"index" json:"audit_id"`
	Kind    string `gorm:"size:16;index" json:"kind"`
	// Path is relative to the audited root (empty for the root folder itself).
	Path string `gorm:"size:1024" json:"path"`
	Name string `gorm:"size:512" json:"name"`
	// Depth is how many levels below the root the node sits (the root is 0).
	Depth int `json:"depth"`
	// Size is the file size; TotalSize is the subtree size (equal to Size for a
	// file). Files/Folders are the subtree counts, empty/zero for a file.
	Size      int64 `json:"size"`
	TotalSize int64 `json:"total_size"`
	Files     int   `json:"files"`
	Folders   int   `json:"folders"`
	// Expanded is false for a folder the walk could not descend into (it was
	// removed mid-audit, or a budget was hit first); it stays true for a folder
	// whose children are counted but not listed because of the depth limit.
	Expanded bool `json:"expanded"`
	// Listed is whether this folder's children are part of the report. A folder
	// at the depth boundary has exact totals (the subtree was walked and counted)
	// but Listed=false, which is how a reader tells an empty folder from a
	// truncated listing.
	Listed     bool       `json:"listed"`
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
	MimeType   string     `gorm:"size:191" json:"mime_type,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (AuditEntry) TableName() string { return "audit_entries" }
