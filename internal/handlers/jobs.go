package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/services"
)

// maxIntervalMinutes caps a schedule at seven days: a longer interval is almost
// always a typo and would hide a job from the dashboard.
const maxIntervalMinutes = 7 * 24 * 60

// jobInput is the create/update payload of a job. Pointers are used where the
// update semantics differ from the create defaults (`enabled`).
type jobInput struct {
	Name                  string   `json:"name"`
	SourceAccountID       uint     `json:"source_account_id"`
	DestinationAccountID  uint     `json:"destination_account_id"`
	SourceDriveID         string   `json:"source_drive_id"`
	SourceFolderID        string   `json:"source_folder_id"`
	SourceFolderPath      string   `json:"source_folder_path"`
	DestinationDriveID    string   `json:"destination_drive_id"`
	DestinationFolderID   string   `json:"destination_folder_id"`
	DestinationFolderPath string   `json:"destination_folder_path"`
	Direction             string   `json:"direction"`
	ConflictPolicy        string   `json:"conflict_policy"`
	ExcludePatterns       []string `json:"exclude_patterns"`
	DeleteMissing         bool     `json:"delete_missing"`
	IntervalMinutes       int      `json:"interval_minutes"`
	Enabled               *bool    `json:"enabled"`
}

// jobView is a job plus the two pieces of state the UI needs but that do not
// live in the table: the decoded exclude patterns and whether a run is in flight.
type jobView struct {
	models.SyncJob
	ExcludePatterns []string `json:"exclude_patterns"`
	Running         bool     `json:"running"`
}

// newJobView decorates a job for the API.
func (s *Server) newJobView(job models.SyncJob) jobView {
	return jobView{
		SyncJob:         job,
		ExcludePatterns: decodePatterns(job.ExcludePatterns),
		Running:         s.deps.Sync.Running(job.ID),
	}
}

// decodePatterns turns the stored JSON array back into a slice. A malformed
// value (written by an older build or by hand) degrades to an empty list rather
// than failing the whole listing.
func decodePatterns(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []string{}
	}
	patterns := []string{}
	if err := json.Unmarshal([]byte(trimmed), &patterns); err != nil || patterns == nil {
		return []string{}
	}
	return patterns
}

// encodePatterns normalises the exclude patterns of a job: blank entries are
// dropped, duplicates removed, and every glob checked so a typo is a validation
// error instead of a pattern that silently matches nothing.
func encodePatterns(patterns []string) (string, error) {
	clean := make([]string, 0, len(patterns))
	seen := map[string]bool{}
	for _, pattern := range patterns {
		value := strings.TrimSpace(strings.ReplaceAll(pattern, "\\", "/"))
		if value == "" || seen[value] {
			continue
		}
		if _, err := path.Match(value, "probe"); err != nil {
			return "", fmt.Errorf("%w: %q is not a valid pattern", services.ErrValidation, value)
		}
		seen[value] = true
		clean = append(clean, value)
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		return "", fmt.Errorf("handlers: encoding the exclude patterns: %w", err)
	}
	return string(encoded), nil
}

// applyJobInput validates a payload and writes it on job. It owns the whole
// contract of a job: required fields, enum values, the existence of both
// accounts, the defaults (root folders, direction, conflict policy) and the
// schedule recomputation.
func (s *Server) applyJobInput(c *gin.Context, input jobInput, job *models.SyncJob) error {
	ctx := c.Request.Context()

	name := strings.TrimSpace(input.Name)
	switch {
	case name == "":
		return fmt.Errorf("%w: the job name is required", services.ErrValidation)
	case len(name) > 190:
		return fmt.Errorf("%w: the job name must be at most 190 characters", services.ErrValidation)
	}

	source, err := s.deps.Store.GetAccount(ctx, input.SourceAccountID)
	if err != nil {
		return err
	}
	destination, err := s.deps.Store.GetAccount(ctx, input.DestinationAccountID)
	if err != nil {
		return err
	}
	if source.ID == destination.ID {
		return fmt.Errorf("%w: the source and the destination must be different accounts", services.ErrValidation)
	}

	direction := models.SyncDirection(strings.ToLower(strings.TrimSpace(input.Direction)))
	if direction == "" {
		// The two constants name the provider pair, so the default is derivable
		// from the accounts the operator picked.
		direction = defaultDirection(source, destination)
	}
	if !direction.Valid() {
		return fmt.Errorf("%w: unknown direction %q", services.ErrValidation, input.Direction)
	}

	policy := models.ConflictPolicy(strings.ToLower(strings.TrimSpace(input.ConflictPolicy)))
	if policy == "" {
		policy = models.ConflictNewestWins
	}
	if !policy.Valid() {
		return fmt.Errorf("%w: unknown conflict policy %q", services.ErrValidation, input.ConflictPolicy)
	}

	if input.IntervalMinutes < 0 || input.IntervalMinutes > maxIntervalMinutes {
		return fmt.Errorf("%w: the interval must be between 0 (manual) and %d minutes",
			services.ErrValidation, maxIntervalMinutes)
	}
	patterns, err := encodePatterns(input.ExcludePatterns)
	if err != nil {
		return err
	}

	enabled := job.Enabled
	if input.Enabled != nil {
		enabled = *input.Enabled
	}

	job.Name = name
	job.SourceAccountID = source.ID
	job.DestinationAccountID = destination.ID
	job.SourceDriveID = driveOrRoot(input.SourceDriveID)
	job.SourceFolderID = driveOrRoot(input.SourceFolderID)
	job.SourceFolderPath = strings.TrimSpace(input.SourceFolderPath)
	job.DestinationDriveID = driveOrRoot(input.DestinationDriveID)
	job.DestinationFolderID = driveOrRoot(input.DestinationFolderID)
	job.DestinationFolderPath = strings.TrimSpace(input.DestinationFolderPath)
	job.Direction = string(direction)
	job.ConflictPolicy = string(policy)
	job.ExcludePatterns = patterns
	job.DeleteMissing = input.DeleteMissing
	job.IntervalMinutes = input.IntervalMinutes
	job.Enabled = enabled
	return nil
}

// defaultDirection picks the direction that matches the provider pair.
func defaultDirection(source, destination *models.ConnectedAccount) models.SyncDirection {
	if source.Provider == string(models.ProviderMicrosoft) && destination.Provider == string(models.ProviderGoogle) {
		return models.DirectionMicrosoftToGoogle
	}
	return models.DirectionGoogleToMicrosoft
}

// driveOrRoot falls back to the root of the drive when the picker did not send
// an explicit id, which is what happens at the top level of the file tree.
func driveOrRoot(value string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return providers.DriveRoot
}

// handleListJobs answers GET /api/jobs.
func (s *Server) handleListJobs(c *gin.Context) {
	jobs, err := s.deps.Store.ListJobs(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]jobView, 0, len(jobs))
	for _, job := range jobs {
		views = append(views, s.newJobView(job))
	}
	c.JSON(http.StatusOK, gin.H{"jobs": views})
}

// handleGetJob answers GET /api/jobs/:id.
func (s *Server) handleGetJob(c *gin.Context) {
	job, ok := s.lookupJob(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, s.newJobView(*job))
}

// lookupJob reads the `:id` parameter and loads the job, answering 404 when it
// does not exist. Every job scoped handler goes through it.
func (s *Server) lookupJob(c *gin.Context) (*models.SyncJob, bool) {
	id, ok := parseID(c, "id")
	if !ok {
		return nil, false
	}
	job, err := s.deps.Store.GetJob(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return nil, false
	}
	return job, true
}

// handleCreateJob answers POST /api/jobs.
func (s *Server) handleCreateJob(c *gin.Context) {
	var input jobInput
	if !decode(c, &input) {
		return
	}
	job := models.SyncJob{Enabled: true}
	if err := s.applyJobInput(c, input, &job); err != nil {
		fail(c, err)
		return
	}
	if err := s.deps.Store.SaveJob(c.Request.Context(), &job); err != nil {
		fail(c, err)
		return
	}
	if err := s.reschedule(c, &job); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, s.newJobView(job))
}

// handleUpdateJob answers PUT /api/jobs/:id. The payload replaces the job as a
// whole (the editor always sends the complete form), and the schedule is
// recomputed so a change of interval takes effect on the next tick.
func (s *Server) handleUpdateJob(c *gin.Context) {
	job, ok := s.lookupJob(c)
	if !ok {
		return
	}
	var input jobInput
	if !decode(c, &input) {
		return
	}
	if err := s.applyJobInput(c, input, job); err != nil {
		fail(c, err)
		return
	}
	if err := s.deps.Store.SaveJob(c.Request.Context(), job); err != nil {
		fail(c, err)
		return
	}
	if err := s.reschedule(c, job); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, s.newJobView(*job))
}

// handleDeleteJob answers DELETE /api/jobs/:id. A run in flight is cancelled
// first: leaving a run alive for a job that no longer exists would keep writing
// to the provider and clutter the history forever.
func (s *Server) handleDeleteJob(c *gin.Context) {
	job, ok := s.lookupJob(c)
	if !ok {
		return
	}
	cancelled := s.deps.Sync.Cancel(job.ID)
	if err := s.deps.Store.DeleteJob(c.Request.Context(), job.ID); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": job.ID, "cancelled": cancelled})
}

// handleRunJob answers POST /api/jobs/:id/run, the "Synchronise now" button. The
// run is started in the background; the answer carries the created run so the
// SPA can follow it (409 while another run of the same job is in flight).
func (s *Server) handleRunJob(c *gin.Context) {
	job, ok := s.lookupJob(c)
	if !ok {
		return
	}
	run, err := s.deps.Sync.Start(c.Request.Context(), job.ID, models.TriggerManual)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job_id": job.ID, "run": run})
}

// handleCancelJob answers POST /api/jobs/:id/cancel. Cancelling a job that is
// not running is not an error, so the endpoint is idempotent and reports what it
// did.
func (s *Server) handleCancelJob(c *gin.Context) {
	job, ok := s.lookupJob(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"job_id": job.ID, "cancelled": s.deps.Sync.Cancel(job.ID)})
}

// handleJobSchedule answers GET /api/jobs/:id/schedule: the next five planned
// executions, which is what the editor shows as a preview so an interval is
// never a guess.
func (s *Server) handleJobSchedule(c *gin.Context) {
	job, ok := s.lookupJob(c)
	if !ok {
		return
	}
	next := services.NextRunAt(s.now(), job)
	preview := []time.Time{}
	if next != nil {
		interval := time.Duration(job.IntervalMinutes) * time.Minute
		cursor := *next
		for i := 0; i < 5; i++ {
			preview = append(preview, cursor)
			cursor = cursor.Add(interval)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"next_run_at": next,
		"scheduled":   next != nil,
		"preview":     preview,
	})
}

// reschedule recomputes next_run_at from the current schedule and persists it,
// so changing the interval or disabling a job takes effect immediately instead
// of on the next bootstrap.
func (s *Server) reschedule(c *gin.Context, job *models.SyncJob) error {
	next := services.NextRunAt(s.now(), job)
	if err := s.deps.Store.ScheduleJob(c.Request.Context(), job.ID, next); err != nil {
		return err
	}
	job.NextRunAt = next
	return nil
}
