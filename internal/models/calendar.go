package models

import "time"

// CalendarSourceKind names where the events of a calendar job are read from: one
// of the two providers (through a connected account) or an iCalendar (.ics)
// subscription, which needs no account at all.
type CalendarSourceKind string

const (
	// CalendarSourceGoogle reads a Google Calendar through a connected account.
	CalendarSourceGoogle CalendarSourceKind = "google"
	// CalendarSourceMicrosoft reads a Microsoft 365 / Outlook calendar through a
	// connected account.
	CalendarSourceMicrosoft CalendarSourceKind = "microsoft"
	// CalendarSourceICS reads a public or private iCalendar feed over HTTP(S).
	CalendarSourceICS CalendarSourceKind = "ics"
)

// Valid reports whether the value is a supported source kind.
func (k CalendarSourceKind) Valid() bool {
	switch k {
	case CalendarSourceGoogle, CalendarSourceMicrosoft, CalendarSourceICS:
		return true
	default:
		return false
	}
}

// AccountBacked reports whether the source reads through a connected account.
// An .ics subscription is read over HTTP instead, so its account id stays zero.
func (k CalendarSourceKind) AccountBacked() bool {
	return k == CalendarSourceGoogle || k == CalendarSourceMicrosoft
}

// CalendarDirection is the flow of a calendar job. It is deliberately smaller
// than SyncDirection: calendars are always written to the destination account,
// so the only question is whether changes made there travel back.
type CalendarDirection string

const (
	// CalendarDirectionBidirectional keeps both calendars aligned.
	CalendarDirectionBidirectional CalendarDirection = "bidirectional"
	// CalendarDirectionOneWay mirrors the source into the destination and never
	// writes back (the only direction an .ics subscription can have).
	CalendarDirectionOneWay CalendarDirection = "one_way"
)

// Valid reports whether the value is a supported direction.
func (d CalendarDirection) Valid() bool {
	return d == CalendarDirectionBidirectional || d == CalendarDirectionOneWay
}

// Backward reports whether a change made in the destination calendar may be
// written back to the source.
func (d CalendarDirection) Backward() bool {
	return d == CalendarDirectionBidirectional
}

// CalendarJob is a durable calendar pairing: a source (provider calendar or
// .ics feed) and a writable destination calendar, plus the schedule.
//
// The display names of both calendars are stored next to the ids so the UI can
// render a row without a provider round trip, exactly like SyncJob does for the
// folders.
type CalendarJob struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `gorm:"size:191" json:"name"`

	// SourceKind is google | microsoft | ics (see CalendarSourceKind).
	SourceKind string `gorm:"size:32;default:google" json:"source_kind"`
	// SourceAccountID is zero for an .ics subscription.
	SourceAccountID uint `gorm:"index" json:"source_account_id"`
	// SourceCalendarID is the provider calendar id ("primary" is accepted by
	// Google) or the synthetic "ics" marker of a subscription.
	SourceCalendarID   string `gorm:"size:512" json:"source_calendar_id"`
	SourceCalendarName string `gorm:"size:191" json:"source_calendar_name"`

	// SourceURL, SourceUsername and SourceSecret describe an .ics feed. The
	// secret is an AES-GCM payload (SecretBox) and is never serialised.
	SourceURL      string `gorm:"size:1024" json:"source_url,omitempty"`
	SourceUsername string `gorm:"size:191" json:"source_username,omitempty"`
	SourceSecret   string `gorm:"type:text" json:"-"`
	// SourceETag/SourceLastModified are the validators of the last successful
	// fetch, so the next run can ask for a 304 instead of the whole feed. The
	// column of SourceETag is pinned because the naming strategy would split the
	// initialism into "source_e_tag" and the store updates it by name.
	SourceETag         string     `gorm:"column:source_etag;size:255" json:"source_etag,omitempty"`
	SourceLastModified string     `gorm:"size:64" json:"source_last_modified,omitempty"`
	SourceFetchedAt    *time.Time `json:"source_fetched_at,omitempty"`

	DestinationAccountID    uint   `gorm:"index" json:"destination_account_id"`
	DestinationCalendarID   string `gorm:"size:512" json:"destination_calendar_id"`
	DestinationCalendarName string `gorm:"size:191" json:"destination_calendar_name"`

	Direction      string `gorm:"size:32;default:bidirectional" json:"direction"`
	ConflictPolicy string `gorm:"size:32;default:newest_wins" json:"conflict_policy"`
	// DeleteMissing propagates a deletion from one side to the other instead of
	// recreating the event that disappeared.
	DeleteMissing bool `gorm:"default:false" json:"delete_missing"`
	// PastDays and FutureDays bound the listing window around "now".
	PastDays   int `gorm:"default:30" json:"past_days"`
	FutureDays int `gorm:"default:365" json:"future_days"`

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
func (CalendarJob) TableName() string { return "calendar_jobs" }

// CalendarEventLink is the engine state of a calendar job: one row per event
// both sides know about.
//
// The pairing cannot rely on the provider event id (Google and Microsoft mint
// their own) nor on the provider UID alone (Graph ignores an iCalUId on create),
// so the row carries both ids and both content fingerprints. The fingerprints are
// what make "which side changed since the last run" answerable without keeping a
// copy of the event.
type CalendarEventLink struct {
	ID    uint   `gorm:"primaryKey" json:"id"`
	JobID uint   `gorm:"index:idx_calendarlink_job_uid,unique" json:"job_id"`
	UID   string `gorm:"size:768;index:idx_calendarlink_job_uid,unique" json:"uid"`

	SourceEventID      string `gorm:"size:768" json:"source_event_id"`
	DestinationEventID string `gorm:"size:768" json:"destination_event_id"`
	SourceFingerprint  string `gorm:"size:64" json:"source_fingerprint"`
	// The column is pinned to match the JSON name: the store's upsert updates it
	// by name, and the naming strategy would shorten it to "dest_fingerprint".
	DestFingerprint string `gorm:"column:destination_fingerprint;size:64" json:"destination_fingerprint"`

	// The last known start of both sides, in UTC. They tell an event that moved
	// out of the listing window apart from one that was deleted.
	SourceStart      time.Time `json:"source_start"`
	DestinationStart time.Time `json:"destination_start"`

	Summary      string    `gorm:"size:512" json:"summary"`
	LastAction   string    `gorm:"size:32" json:"last_action"`
	LastSyncedAt time.Time `json:"last_synced_at"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TableName pins the physical table name.
func (CalendarEventLink) TableName() string { return "calendar_event_links" }

// CalendarRun is one execution of a calendar job. It mirrors the shape of
// SyncRun so the UI can reuse its status vocabulary (success, partial, failed,
// cancelled, timeout).
type CalendarRun struct {
	ID      uint   `gorm:"primaryKey" json:"id"`
	JobID   uint   `gorm:"index" json:"job_id"`
	JobName string `gorm:"size:191" json:"job_name"`
	Status  string `gorm:"size:32;index" json:"status"`
	Trigger string `gorm:"size:32" json:"trigger"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`

	EventsScanned int `json:"events_scanned"`
	EventsCreated int `json:"events_created"`
	EventsUpdated int `json:"events_updated"`
	EventsDeleted int `json:"events_deleted"`
	EventsSkipped int `json:"events_skipped"`
	Conflicts     int `json:"conflicts"`
	Errors        int `json:"errors"`

	Message   string    `gorm:"type:text" json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (CalendarRun) TableName() string { return "calendar_runs" }

// CalendarRunItem is the per-event detail of a calendar run. The activity feeds
// of the UI read it, so the evidence of a decision ("destination newer by 42s")
// survives the run that made it.
type CalendarRunItem struct {
	ID     uint   `gorm:"primaryKey" json:"id"`
	RunID  uint   `gorm:"index" json:"run_id"`
	JobID  uint   `gorm:"index" json:"job_id"`
	Action string `gorm:"size:32;index" json:"action"`
	UID    string `gorm:"size:768" json:"uid"`
	// Summary is the event title at the time of the decision.
	Summary  string    `gorm:"size:512" json:"summary"`
	StartAt  time.Time `json:"start_at"`
	Evidence string    `gorm:"type:text" json:"evidence,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (CalendarRunItem) TableName() string { return "calendar_run_items" }
