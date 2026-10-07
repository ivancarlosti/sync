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
	DriveID      string `gorm:"size:191" json:"drive_id"`
	RootFolderID string `gorm:"size:191" json:"root_folder_id"`
	RootPath     string `gorm:"size:1024" json:"root_path"`
	// MaxDepth is how many folder levels below the root are walked; 0 is
	// unlimited. The selected folder is depth 0.
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
	// Expanded is false for a folder that sits at the depth boundary: it is
	// reported but its contents are not.
	Expanded   bool       `json:"expanded"`
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
	MimeType   string     `gorm:"size:191" json:"mime_type,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (AuditEntry) TableName() string { return "audit_entries" }
