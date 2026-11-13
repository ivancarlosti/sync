package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ivancarlosti/sync/internal/models"
)

// Persistence of the calendar engine. It mirrors the sync-job half of Store: the
// same shape of CRUD, the same schedule helpers and the same run history, so the
// API and the scheduler can treat the two families identically.

// ListCalendarJobs returns every calendar job, newest first.
func (s *Store) ListCalendarJobs(ctx context.Context) ([]models.CalendarJob, error) {
	var rows []models.CalendarJob
	if err := s.db.WithContext(ctx).Order("id DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing calendar jobs: %w", err)
	}
	return rows, nil
}

// GetCalendarJob returns one calendar job by id.
func (s *Store) GetCalendarJob(ctx context.Context, id uint) (*models.CalendarJob, error) {
	var row models.CalendarJob
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("services: loading calendar job %d: %w", id, err)
	}
	return &row, nil
}

// SaveCalendarJob inserts or updates a job.
//
// DeleteMissing defaults to false, so GORM would substitute the default on an
// INSERT and write it back into the struct; the caller's value is captured and
// restored the same way SaveJob does for SyncEmptyFolders.
func (s *Store) SaveCalendarJob(ctx context.Context, job *models.CalendarJob) error {
	deleteMissing := job.DeleteMissing
	if err := s.db.WithContext(ctx).Save(job).Error; err != nil {
		return fmt.Errorf("services: saving calendar job: %w", err)
	}
	if job.DeleteMissing == deleteMissing {
		return nil
	}
	job.DeleteMissing = deleteMissing
	if err := s.db.WithContext(ctx).Model(job).Update("delete_missing", deleteMissing).Error; err != nil {
		return fmt.Errorf("services: saving calendar job: %w", err)
	}
	return nil
}

// DeleteCalendarJob removes a job and its engine state and keeps its run history,
// which is the promise the delete dialog makes: the runs (and their items) are
// the evidence of what happened and stay reachable through the run endpoints.
func (s *Store) DeleteCalendarJob(ctx context.Context, id uint) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("job_id = ?", id).Delete(&models.CalendarEventLink{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.CalendarJob{}, id).Error
	})
	if err != nil {
		return fmt.Errorf("services: deleting calendar job %d: %w", id, err)
	}
	return nil
}

// DueCalendarJobs returns the enabled calendar jobs whose schedule elapsed.
func (s *Store) DueCalendarJobs(ctx context.Context, now time.Time) ([]models.CalendarJob, error) {
	var rows []models.CalendarJob
	err := s.db.WithContext(ctx).
		Where("enabled = ? AND interval_minutes > 0", true).
		Where("next_run_at IS NULL OR next_run_at <= ?", now).
		Order("id ASC").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("services: selecting due calendar jobs: %w", err)
	}
	return rows, nil
}

// ScheduleCalendarJob writes the next execution time of a calendar job alone.
func (s *Store) ScheduleCalendarJob(ctx context.Context, id uint, next *time.Time) error {
	err := s.db.WithContext(ctx).Model(&models.CalendarJob{}).
		Where("id = ?", id).
		Updates(map[string]any{"next_run_at": next}).Error
	if err != nil {
		return fmt.Errorf("services: scheduling calendar job %d: %w", id, err)
	}
	return nil
}

// SaveCalendarFeedState stores the validators of a successful .ics fetch so the
// next run can ask for a 304 instead of downloading the feed again.
func (s *Store) SaveCalendarFeedState(ctx context.Context, id uint, etag, lastModified string, fetchedAt time.Time) error {
	err := s.db.WithContext(ctx).Model(&models.CalendarJob{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"source_etag":          etag,
			"source_last_modified": lastModified,
			"source_fetched_at":    fetchedAt,
		}).Error
	if err != nil {
		return fmt.Errorf("services: saving the feed validators of calendar job %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Calendar runs and engine state
// ---------------------------------------------------------------------------

// RunningCalendarRun returns the run of a job that is still marked running.
func (s *Store) RunningCalendarRun(ctx context.Context, jobID uint) (*models.CalendarRun, error) {
	var row models.CalendarRun
	err := s.db.WithContext(ctx).
		Where("job_id = ? AND status = ?", jobID, string(models.RunRunning)).
		Order("id DESC").First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("services: looking for a running calendar run of job %d: %w", jobID, err)
	}
	return &row, nil
}

// CreateCalendarRun inserts a run.
func (s *Store) CreateCalendarRun(ctx context.Context, run *models.CalendarRun) error {
	if err := s.db.WithContext(ctx).Create(run).Error; err != nil {
		return fmt.Errorf("services: creating calendar run: %w", err)
	}
	return nil
}

// FinishCalendarRun writes the outcome of a run and, in the same transaction, the
// job row it describes: a run that finished but never reached the job would leave
// the scheduler reading a stale status, and a slot that is never advanced would
// make the job due again on the very next tick.
//
// The summary is derived from the run, not from the job, for the same reason
// FinishRun does it: the engine only ever touches the feed validators on the job,
// so copying job.LastRunAt/NextRunAt would persist their pre-run values (empty)
// and never move the schedule forward.
func (s *Store) FinishCalendarRun(ctx context.Context, run *models.CalendarRun, job *models.CalendarJob) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(run).Error; err != nil {
			return err
		}
		if job == nil {
			return nil
		}
		updates := map[string]any{
			"last_run_at": run.StartedAt,
			"last_status": run.Status,
			"last_error":  run.Message,
			"source_etag": job.SourceETag,
			"updated_at":  time.Now().UTC(),
		}
		if job.Enabled && job.IntervalMinutes > 0 {
			next := run.StartedAt.Add(time.Duration(job.IntervalMinutes) * time.Minute)
			updates["next_run_at"] = &next
		} else {
			updates["next_run_at"] = nil
		}
		return tx.Model(&models.CalendarJob{}).Where("id = ?", job.ID).Updates(updates).Error
	})
	if err != nil {
		return fmt.Errorf("services: finishing calendar run %d: %w", run.ID, err)
	}
	return nil
}

// AddCalendarRunItems inserts the per-event detail of a run in one batch.
func (s *Store) AddCalendarRunItems(ctx context.Context, items []models.CalendarRunItem) error {
	if len(items) == 0 {
		return nil
	}
	if err := s.db.WithContext(ctx).CreateInBatches(&items, 100).Error; err != nil {
		return fmt.Errorf("services: recording calendar run items: %w", err)
	}
	return nil
}

// ListCalendarRuns returns the runs of a job, newest first.
func (s *Store) ListCalendarRuns(ctx context.Context, jobID uint, limit int) ([]models.CalendarRun, error) {
	query := s.db.WithContext(ctx).Order("id DESC")
	if jobID != 0 {
		query = query.Where("job_id = ?", jobID)
	}
	if limit > 0 {
		query = query.Limit(limit)
	}
	var rows []models.CalendarRun
	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing calendar runs: %w", err)
	}
	return rows, nil
}

// GetCalendarRun returns one run by id.
func (s *Store) GetCalendarRun(ctx context.Context, id uint) (*models.CalendarRun, error) {
	var row models.CalendarRun
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("services: loading calendar run %d: %w", id, err)
	}
	return &row, nil
}

// ListCalendarRunItems returns the items of a run.
func (s *Store) ListCalendarRunItems(ctx context.Context, runID uint, limit int) ([]models.CalendarRunItem, error) {
	query := s.db.WithContext(ctx).Where("run_id = ?", runID).Order("id ASC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	var rows []models.CalendarRunItem
	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing calendar run items of run %d: %w", runID, err)
	}
	return rows, nil
}

// FailInterruptedCalendarRuns closes the runs a restart left marked running, the
// same way FailInterruptedRuns does for file syncs.
func (s *Store) FailInterruptedCalendarRuns(ctx context.Context, when time.Time) (int64, error) {
	result := s.db.WithContext(ctx).Model(&models.CalendarRun{}).
		Where("status = ?", string(models.RunRunning)).
		Updates(map[string]any{
			"status":      string(models.RunFailed),
			"finished_at": when,
			"message":     "the run was interrupted by a restart",
		})
	if result.Error != nil {
		return 0, fmt.Errorf("services: closing interrupted calendar runs: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// PruneCalendarRuns keeps the newest keep runs of every job and deletes the rest,
// which is what bounds the history table.
func (s *Store) PruneCalendarRuns(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	var jobs []uint
	if err := s.db.WithContext(ctx).Model(&models.CalendarRun{}).Distinct().Pluck("job_id", &jobs).Error; err != nil {
		return 0, fmt.Errorf("services: listing calendar jobs with history: %w", err)
	}
	var removed int64
	for _, jobID := range jobs {
		var ids []uint
		if err := s.db.WithContext(ctx).Model(&models.CalendarRun{}).
			Where("job_id = ?", jobID).
			Order("id DESC").Offset(keep).Limit(500).Pluck("id", &ids).Error; err != nil {
			return removed, fmt.Errorf("services: selecting calendar runs to prune: %w", err)
		}
		if len(ids) == 0 {
			continue
		}
		if err := s.db.WithContext(ctx).Where("run_id IN ?", ids).Delete(&models.CalendarRunItem{}).Error; err != nil {
			return removed, fmt.Errorf("services: pruning calendar run items: %w", err)
		}
		result := s.db.WithContext(ctx).Where("id IN ?", ids).Delete(&models.CalendarRun{})
		if result.Error != nil {
			return removed, fmt.Errorf("services: pruning calendar runs: %w", result.Error)
		}
		removed += result.RowsAffected
	}
	return removed, nil
}

// CalendarLinks returns the engine state of a job keyed by event UID.
func (s *Store) CalendarLinks(ctx context.Context, jobID uint) (map[string]models.CalendarEventLink, error) {
	var rows []models.CalendarEventLink
	if err := s.db.WithContext(ctx).Where("job_id = ?", jobID).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: loading the calendar links of job %d: %w", jobID, err)
	}
	links := make(map[string]models.CalendarEventLink, len(rows))
	for _, row := range rows {
		links[row.UID] = row
	}
	return links, nil
}

// SaveCalendarLinks upserts engine rows on (job_id, uid).
func (s *Store) SaveCalendarLinks(ctx context.Context, rows []models.CalendarEventLink) error {
	if len(rows) == 0 {
		return nil
	}
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "job_id"}, {Name: "uid"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"source_event_id", "destination_event_id",
			"source_fingerprint", "destination_fingerprint",
			"source_start", "destination_start",
			"summary", "last_action", "last_synced_at", "updated_at",
		}),
	}).CreateInBatches(&rows, 100).Error
	if err != nil {
		return fmt.Errorf("services: saving calendar links: %w", err)
	}
	return nil
}

// DeleteCalendarLinks forgets engine rows, which is what a confirmed deletion on
// both sides needs: keeping them would make the next run resurrect an event the
// operator removed.
func (s *Store) DeleteCalendarLinks(ctx context.Context, jobID uint, uids []string) error {
	if len(uids) == 0 {
		return nil
	}
	err := s.db.WithContext(ctx).Where("job_id = ? AND uid IN ?", jobID, uids).Delete(&models.CalendarEventLink{}).Error
	if err != nil {
		return fmt.Errorf("services: forgetting calendar links of job %d: %w", jobID, err)
	}
	return nil
}
