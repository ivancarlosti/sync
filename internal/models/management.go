package models

import "time"

// Management task kinds. Each kind names one administrative operation the
// Admin operations screen can run against a connected account. The value is
// stored on the row and never leaves this package at runtime: the operation
// catalogue (internal/services/management.go) is what the API exposes, and the
// stored kind is matched back against it when a task is read.
const (
	// Google Drive (files).
	ManagementKindDriveToNewSharedDrive = "google_drive_to_new_shared_drive"
	ManagementKindDriveToSharedDrive    = "google_drive_to_shared_drive"
	// Google Workspace directory, mail and calendar.
	ManagementKindArchiveUserGroup  = "google_archive_user_group"
	ManagementKindGoogleCopyMailbox = "google_copy_mailbox"
	ManagementKindGoogleCalendars   = "google_transfer_calendars"
	ManagementKindGoogleDelegate    = "google_manage_delegate"
	// Microsoft 365 mailbox, sites, calendar and storage.
	ManagementKindMicrosoftCopyMailbox = "microsoft_copy_mailbox"
	ManagementKindOneDriveToSite       = "microsoft_onedrive_to_site"
	ManagementKindMicrosoftCalendars   = "microsoft_transfer_calendars"
	ManagementKindFolderSizes          = "microsoft_folder_sizes"
)

// Step outcomes recorded on ManagementTaskStep.Status. They reuse the sync
// vocabulary where it fits (success/failed) and add `skipped` for a step that the
// operation deliberately did not perform (an item already in place, a group that
// already existed).
const (
	ManagementStepSuccess = "success"
	ManagementStepFailed  = "failed"
	ManagementStepSkipped = "skipped"
)

// ManagementTask is one execution of an administrative operation against a
// connected account: move a Drive into a Shared Drive, archive a departing
// user's mailbox into a group, copy a mailbox, transfer calendars, report folder
// sizes.
//
// It is the async brother of DirectoryReport and AuditRun: the row is written in
// the `running` state before the operation starts, the per-step journal is
// appended while it works and the summary is written once at the end (see
// internal/services/management.go). It is auditable: the request, every step and
// the final counters survive a restart, so an operator can prove what was done
// to whom.
type ManagementTask struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	AccountID    uint   `gorm:"index" json:"account_id"`
	AccountEmail string `gorm:"size:191" json:"account_email"`
	Provider     string `gorm:"size:32" json:"provider"`
	// Kind is one of the ManagementKind values.
	Kind  string `gorm:"size:64;index" json:"kind"`
	Title string `gorm:"size:191" json:"title"`

	Status  string `gorm:"size:32;index" json:"status"`
	Trigger string `gorm:"size:32" json:"trigger"`

	// Params and Result are JSON documents marshalled by the services layer:
	// the request that was run (the operator's confirmation is implied by the
	// row existing) and the summary the provider answered.
	Params string `gorm:"type:text" json:"params"`
	Result string `gorm:"type:text" json:"result,omitempty"`

	// Summary counters: how many journal steps were written, how many of them
	// failed, how many items moved and how many bytes were transferred.
	StepCount      int   `json:"step_count"`
	Failed         int   `json:"failed"`
	ItemsProcessed int   `json:"items_processed"`
	BytesMoved     int64 `json:"bytes_moved"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`

	Message   string    `gorm:"type:text" json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (ManagementTask) TableName() string { return "management_tasks" }

// ManagementTaskStep is one entry of the journal of a task: a single action the
// operation took (or skipped or failed) and the item and byte counters it moved.
// The rows are append-only, which is what makes the history auditable.
type ManagementTaskStep struct {
	ID     uint `gorm:"primaryKey" json:"id"`
	TaskID uint `gorm:"index" json:"task_id"`
	// Seq is the position of the step in the journal (1-based), kept explicit so
	// a repaired or imported row still sorts the way it happened.
	Seq    int    `json:"seq"`
	Name   string `gorm:"size:191" json:"name"`
	Status string `gorm:"size:32" json:"status"`
	Detail string `gorm:"type:text" json:"detail,omitempty"`
	Items  int    `json:"items"`
	Bytes  int64  `json:"bytes"`

	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (ManagementTaskStep) TableName() string { return "management_task_steps" }
