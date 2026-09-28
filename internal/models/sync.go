package models

import "time"

// SyncJob is a durable source → destination synchronisation definition.
//
// The job stores the ids of both endpoints plus the human readable paths, so
// the UI can render the selection without querying the remote providers.
type SyncJob struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `gorm:"size:191" json:"name"`

	SourceAccountID      uint `gorm:"index" json:"source_account_id"`
	DestinationAccountID uint `gorm:"index" json:"destination_account_id"`

	SourceDriveID         string `gorm:"size:191" json:"source_drive_id"`
	SourceFolderID        string `gorm:"size:191" json:"source_folder_id"`
	SourceFolderPath      string `gorm:"size:1024" json:"source_folder_path"`
	DestinationDriveID    string `gorm:"size:191" json:"destination_drive_id"`
	DestinationFolderID   string `gorm:"size:191" json:"destination_folder_id"`
	DestinationFolderPath string `gorm:"size:1024" json:"destination_folder_path"`

	Direction      string `gorm:"size:32;default:google_to_microsoft" json:"direction"`
	ConflictPolicy string `gorm:"size:32;default:newest_wins" json:"conflict_policy"`
	// ExcludePatterns is a JSON array of shell patterns matched against the
	// relative path (e.g. ["*.tmp","cache/**"]).
	ExcludePatterns string `gorm:"type:text" json:"-"`
	// DeleteMissing propagates deletions instead of re-copying the file back.
	DeleteMissing bool `gorm:"default:false" json:"delete_missing"`
	// IntervalMinutes is the automatic schedule; 0 means "manual only".
	IntervalMinutes int  `gorm:"default:0" json:"interval_minutes"`
	Enabled         bool `gorm:"default:true" json:"enabled"`

	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	NextRunAt  *time.Time `json:"next_run_at,omitempty"`
	LastStatus string     `gorm:"size:32" json:"last_status,omitempty"`
	LastError  string     `gorm:"type:text" json:"last_error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// TableName pins the physical table name.
func (SyncJob) TableName() string { return "sync_jobs" }

// SyncRun is one execution of a job; runs are kept for auditing and feed the
// dashboard statistics.
type SyncRun struct {
	ID      uint   `gorm:"primaryKey" json:"id"`
	JobID   uint   `gorm:"index" json:"job_id"`
	JobName string `gorm:"size:191" json:"job_name"`
	Status  string `gorm:"size:32;index" json:"status"`
	Trigger string `gorm:"size:32" json:"trigger"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`

	FilesScanned     int   `json:"files_scanned"`
	FilesCreated     int   `json:"files_created"`
	FilesUpdated     int   `json:"files_updated"`
	FilesDeleted     int   `json:"files_deleted"`
	FilesSkipped     int   `json:"files_skipped"`
	FoldersCreated   int   `json:"folders_created"`
	Conflicts        int   `json:"conflicts"`
	Errors           int   `json:"errors"`
	BytesTransferred int64 `json:"bytes_transferred"`

	Message   string    `gorm:"type:text" json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (SyncRun) TableName() string { return "sync_runs" }

// SyncItem is the per-file detail of a run. A run over a large tree produces
// many rows, so the API paginates them and the retention pruner keeps the last
// runs per job.
type SyncItem struct {
	ID     uint   `gorm:"primaryKey" json:"id"`
	RunID  uint   `gorm:"index" json:"run_id"`
	JobID  uint   `gorm:"index" json:"job_id"`
	Action string `gorm:"size:32;index" json:"action"`
	Path   string `gorm:"size:1024" json:"path"`
	Size   int64  `json:"size"`
	// Evidence carries the short explanation of the decision (e.g. "destination
	// newer by 42s") so operators can audit a conflict after the fact.
	Evidence  string    `gorm:"type:text" json:"evidence,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (SyncItem) TableName() string { return "sync_items" }

// SyncFile is the state table of the engine: one row per file already in sync
// for a job. It is what makes deletions, renames and conflicts detectable
// without hashing the whole tree on every run.
type SyncFile struct {
	ID    uint   `gorm:"primaryKey" json:"id"`
	JobID uint   `gorm:"index:idx_syncfile_job_path,unique" json:"job_id"`
	Path  string `gorm:"size:1024;index:idx_syncfile_job_path,unique" json:"path"`

	SourceItemID          string    `gorm:"size:191" json:"source_item_id"`
	DestinationItemID     string    `gorm:"size:191" json:"destination_item_id"`
	Size                  int64     `json:"size"`
	Hash                  string    `gorm:"size:128" json:"hash"`
	SourceModifiedAt      time.Time `json:"source_modified_at"`
	DestinationModifiedAt time.Time `json:"destination_modified_at"`
	LastSyncedAt          time.Time `json:"last_synced_at"`
	LastDirection         string    `gorm:"size:32" json:"last_direction"`
	Status                string    `gorm:"size:32" json:"status"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the physical table name.
func (SyncFile) TableName() string { return "sync_files" }
