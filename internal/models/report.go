package models

import (
	"strings"
	"time"
)

// DirectoryReport is one execution of a directory report: a read-only snapshot
// of the organisation behind an account (users, groups, sites, licences and
// domains), built from the delegated permissions Sync already holds.
//
// It is the async brother of AuditRun: the row is written in the `running` state
// before the collector starts, the live counters are updated while it works and
// the summary is written once at the end (see internal/services/report.go).
type DirectoryReport struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	AccountID    uint   `gorm:"index" json:"account_id"`
	AccountEmail string `gorm:"size:191" json:"account_email"`
	Provider     string `gorm:"size:32" json:"provider"`
	// OrgID is the tenant the report describes (the Entra tenant id), as the
	// provider reported it. It stays empty for an account connected before the
	// column existed and for a personal account, which has no organisation.
	OrgID string `gorm:"size:191" json:"org_id,omitempty"`

	// Sections is the comma separated list of the sections that were requested,
	// in the canonical order of providers.ReportSections. The tables themselves
	// live in ReportSectionData.
	Sections string `gorm:"size:512" json:"sections"`

	Status     string     `gorm:"size:32;index" json:"status"`
	Trigger    string     `gorm:"size:32" json:"trigger"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`

	// Summary counters: how many sections the report holds, how many rows they
	// add up to, how many sections failed and whether any of them was cut at the
	// row cap.
	SectionCount int  `json:"section_count"`
	RowCount     int  `json:"row_count"`
	Failed       int  `json:"failed"`
	Truncated    bool `json:"truncated"`

	Message   string    `gorm:"type:text" json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (DirectoryReport) TableName() string { return "directory_reports" }

// SectionList splits the stored section list into its values.
func (r *DirectoryReport) SectionList() []string {
	return splitSections(r.Sections)
}

// SetSections stores the requested sections in one string.
func (r *DirectoryReport) SetSections(sections []string) {
	r.Sections = joinSections(sections)
}

// splitSections turns the stored comma separated list into its values.
func splitSections(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	sections := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			sections = append(sections, value)
		}
	}
	return sections
}

// joinSections stores a list of sections in one column, dropping empties.
func joinSections(sections []string) string {
	clean := make([]string, 0, len(sections))
	for _, section := range sections {
		if value := strings.TrimSpace(section); value != "" {
			clean = append(clean, value)
		}
	}
	return strings.Join(clean, ",")
}

// ReportSectionData is one stored table of a directory report: the columns and
// the rows as JSON documents, exactly as the provider collected them.
//
// The columns are stored beside the rows (instead of being derived from a
// constant) so a report written before a column existed keeps rendering with the
// header it was collected with. The store never parses the two documents: the
// handler marshals and unmarshals them, which keeps the database layer about
// persistence only.
type ReportSectionData struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// ReportID and Section are unique together: one report holds one table per
	// section.
	ReportID uint   `gorm:"index;uniqueIndex:idx_report_section" json:"report_id"`
	Section  string `gorm:"size:64;uniqueIndex:idx_report_section" json:"section"`

	// Columns is the header of the table: a handful of key/title pairs, which a
	// TEXT column holds many times over.
	Columns string `gorm:"type:text" json:"-"`
	// Rows is the document itself and it is LONGTEXT, not TEXT, because one
	// section holds a whole table of the directory (20 000 rows of the collector
	// budget): the JSON of a large tenant is measured in megabytes, while a TEXT
	// column stops at 65 535 bytes and fails the INSERT with "Error 1406: Data
	// too long for column 'rows'". The document is still bounded
	// (services.reportSectionBytes, 8 MiB) so one value never outgrows what a
	// single statement may carry. Widen the column instead of cutting the table:
	// a report is a document, and a table cut in the middle is worth less than a
	// big one.
	Rows string `gorm:"type:longtext" json:"-"`

	RowCount int `json:"row_count"`
	// Truncated is true when the table stopped at the row cap.
	Truncated bool `json:"truncated"`
	// Error is the reason the section could not be collected; empty on success.
	Error string `gorm:"type:text" json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the physical table name.
func (ReportSectionData) TableName() string { return "report_sections" }

// ReportSchedule is the per-account opt-in of the scheduled directory report.
//
// It is one row per account (a schedule that does not exist means "do not report
// this account automatically"), which is what makes the feature opt-in: nothing
// runs for an account until the operator asks for it on the Reports screen.
type ReportSchedule struct {
	ID        uint `gorm:"primaryKey" json:"id"`
	AccountID uint `gorm:"uniqueIndex" json:"account_id"`
	Enabled   bool `json:"enabled"`
	// IntervalMinutes is how often the report runs; the scheduler picks the
	// schedule up once NextRunAt elapsed.
	IntervalMinutes int        `gorm:"default:0" json:"interval_minutes"`
	Sections        string     `gorm:"size:512" json:"sections"`
	NextRunAt       *time.Time `json:"next_run_at,omitempty"`
	LastRunAt       *time.Time `json:"last_run_at,omitempty"`
	// LastReportID is the report the last scheduled run produced, so the screen
	// can link to it without a search.
	LastReportID *uint     `json:"last_report_id,omitempty"`
	LastError    string    `gorm:"type:text" json:"last_error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TableName pins the physical table name.
func (ReportSchedule) TableName() string { return "report_schedules" }

// SectionList splits the stored section list into its values.
func (s *ReportSchedule) SectionList() []string { return splitSections(s.Sections) }

// SetSections stores the requested sections in one string.
func (s *ReportSchedule) SetSections(sections []string) { s.Sections = joinSections(sections) }
