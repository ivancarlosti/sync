package services

import (
	"context"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// newTestCalendarJob inserts a minimal, valid calendar job and returns it with
// the id the store assigned.
func newTestCalendarJob(t *testing.T, store *Store, interval int, enabled bool) *models.CalendarJob {
	t.Helper()
	job := &models.CalendarJob{
		Name:                    "Google to Outlook",
		SourceKind:              string(models.CalendarSourceGoogle),
		SourceAccountID:         1,
		SourceCalendarID:        "primary",
		SourceCalendarName:      "Work",
		DestinationAccountID:    2,
		DestinationCalendarID:   "AQMkADIt",
		DestinationCalendarName: "Calendar",
		Direction:               string(models.CalendarDirectionOneWay),
		ConflictPolicy:          string(models.ConflictSourceWins),
		PastDays:                30,
		FutureDays:              365,
		IntervalMinutes:         interval,
		Enabled:                 enabled,
	}
	if err := store.SaveCalendarJob(context.Background(), job); err != nil {
		t.Fatalf("saving the calendar job: %v", err)
	}
	return job
}

// timePtr returns a pointer to t, for the store calls that take *time.Time.
func timePtr(t time.Time) *time.Time { return &t }

// TestFinishCalendarRunMirrorsTheRunOnTheJob pins the contract the calendar list
// and the scheduler read: a finished run updates the job summary and, for an
// enabled job with a schedule, moves next_run_at one interval past the start.
//
// Copying the job's own (empty) summary instead of the run's would leave
// last_status blank forever and never advance the slot, so the job would stay due
// and run again on the next tick.
func TestFinishCalendarRunMirrorsTheRunOnTheJob(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	job := newTestCalendarJob(t, store, 60, true)

	started := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	run := &models.CalendarRun{
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    string(models.RunSuccess),
		Trigger:   string(models.TriggerScheduled),
		StartedAt: started,
		Message:   "completed",
	}
	if err := store.CreateCalendarRun(ctx, run); err != nil {
		t.Fatalf("creating the calendar run: %v", err)
	}

	// The engine only ever writes the feed validators on the job.
	job.SourceETag = `W/"abc"`
	if err := store.FinishCalendarRun(ctx, run, job); err != nil {
		t.Fatalf("finishing the calendar run: %v", err)
	}

	loaded, err := store.GetCalendarJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("reloading the calendar job: %v", err)
	}
	if loaded.LastStatus != string(models.RunSuccess) {
		t.Errorf("last_status = %q, want %q", loaded.LastStatus, models.RunSuccess)
	}
	if loaded.LastError != "completed" {
		t.Errorf("last_error = %q, want %q", loaded.LastError, "completed")
	}
	if loaded.LastRunAt == nil || !loaded.LastRunAt.Equal(started) {
		t.Errorf("last_run_at = %v, want %v", loaded.LastRunAt, started)
	}
	if want := started.Add(time.Hour); loaded.NextRunAt == nil || !loaded.NextRunAt.Equal(want) {
		t.Errorf("next_run_at = %v, want %v", loaded.NextRunAt, want)
	}
	if loaded.SourceETag != `W/"abc"` {
		t.Errorf("source_etag = %q, want the validator the run stored", loaded.SourceETag)
	}

	// A job whose slot just moved must not be due again straight away, which is
	// what a stale next_run_at would cause.
	due, err := store.DueCalendarJobs(ctx, started.Add(time.Minute))
	if err != nil {
		t.Fatalf("listing due calendar jobs: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("due calendar jobs = %d, want 0 right after a run", len(due))
	}
}

// TestFinishCalendarRunClearsTheSlotOfAManualJob covers the other arm: a job that
// is manual-only or disabled has no next slot, so the row must say so instead of
// keeping the timestamp a previous schedule left behind.
func TestFinishCalendarRunClearsTheSlotOfAManualJob(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	job := newTestCalendarJob(t, store, 0, true)

	if err := store.ScheduleCalendarJob(ctx, job.ID, timePtr(time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC))); err != nil {
		t.Fatalf("seeding the next slot: %v", err)
	}

	started := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	run := &models.CalendarRun{
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    string(models.RunPartial),
		Trigger:   string(models.TriggerManual),
		StartedAt: started,
		Message:   "one event could not be written",
	}
	if err := store.CreateCalendarRun(ctx, run); err != nil {
		t.Fatalf("creating the calendar run: %v", err)
	}
	if err := store.FinishCalendarRun(ctx, run, job); err != nil {
		t.Fatalf("finishing the calendar run: %v", err)
	}

	loaded, err := store.GetCalendarJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("reloading the calendar job: %v", err)
	}
	if loaded.NextRunAt != nil {
		t.Errorf("next_run_at = %v, want nil for a manual job", loaded.NextRunAt)
	}
	if loaded.LastStatus != string(models.RunPartial) {
		t.Errorf("last_status = %q, want %q", loaded.LastStatus, models.RunPartial)
	}
	if loaded.LastError != "one event could not be written" {
		t.Errorf("last_error = %q, want the run message", loaded.LastError)
	}
}
