package models

import "time"

// TrafficEntry is the traffic one run moved through one account in one direction
// (see docs/architecture.md and the plan in plans/).
//
// A run keeps absolute counters per (account, direction) and flushes them
// throttled and once at the end, so the row is an upsert on the unique index
// below: a retried flush can never double count and a crash keeps whatever the
// last flush stored, which is the honest answer for traffic that really moved.
//
// Day and Month are the UTC buckets of the run start, stored as text so the
// billing summary is a plain, portable GROUP BY on MySQL/MariaDB and on the
// SQLite the tests run on. Because every row of a run shares its start bucket, a
// run never spans two buckets.
type TrafficEntry struct {
	ID    uint `gorm:"primaryKey" json:"id"`
	RunID uint `gorm:"uniqueIndex:idx_traffic_run_account_direction,priority:1" json:"run_id"`
	// JobID is denormalised so a report by job needs no join.
	JobID     uint `gorm:"index" json:"job_id"`
	AccountID uint `gorm:"uniqueIndex:idx_traffic_run_account_direction,priority:2" json:"account_id"`
	// AccountEmail and Provider let a report read like a bill without a join.
	AccountEmail string `gorm:"size:191" json:"account_email,omitempty"`
	Provider     string `gorm:"size:32" json:"provider,omitempty"`
	// Direction is inbound (read from the source account) or outbound (written to
	// the destination account), see models.TrafficDirection.
	Direction string `gorm:"size:16;uniqueIndex:idx_traffic_run_account_direction,priority:3" json:"direction"`

	// Bytes is the payload moved, Files how many transfers produced it and
	// Operations the payload-free write calls (deletes, folder creations).
	Bytes      int64 `json:"bytes"`
	Files      int   `json:"files"`
	Operations int   `json:"operations"`

	// Day (2006-01-02) and Month (2006-01) are the UTC buckets of StartedAt.
	Day       string    `gorm:"size:10;index" json:"day"`
	Month     string    `gorm:"size:7;index" json:"month"`
	StartedAt time.Time `json:"started_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the physical table name.
func (TrafficEntry) TableName() string { return "traffic_entries" }

// TrafficDayFormat and TrafficMonthFormat render the buckets stored on a traffic
// row. They are the UTC labels the report groups by.
const (
	TrafficDayFormat   = "2006-01-02"
	TrafficMonthFormat = "2006-01"
)
