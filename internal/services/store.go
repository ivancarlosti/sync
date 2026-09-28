// Package services holds the application logic between the HTTP handlers and
// the persistence/provider layers: credential resolution, encrypted token
// handling, sessions, the OAuth flows, the sync engine and its scheduler.
//
// The package never imports Gin: handlers translate HTTP in and out, services
// stay reusable (and testable) without a server.
package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ivancarlosti/sync/internal/database"
	"github.com/ivancarlosti/sync/internal/models"
)

// Store is the persistence facade used by every service. It keeps the GORM
// queries in one place so handlers never build SQL and services never guess at
// a table name.
type Store struct {
	db       *gorm.DB
	settings *database.Settings
}

// NewStore wraps a GORM handle and its settings repository.
func NewStore(db *gorm.DB, settings *database.Settings) *Store {
	return &Store{db: db, settings: settings}
}

// DB exposes the raw handle (used by the boot sequence for migrations).
func (s *Store) DB() *gorm.DB { return s.db }

// Settings exposes the key/value repository.
func (s *Store) Settings() *database.Settings { return s.settings }

// ---------------------------------------------------------------------------
// Connected accounts
// ---------------------------------------------------------------------------

// ListAccounts returns every connected account, ordered by provider then mail.
func (s *Store) ListAccounts(ctx context.Context) ([]models.ConnectedAccount, error) {
	var rows []models.ConnectedAccount
	if err := s.db.WithContext(ctx).Order("provider ASC, email ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing accounts: %w", err)
	}
	return rows, nil
}

// GetAccount returns one account by id.
func (s *Store) GetAccount(ctx context.Context, id uint) (*models.ConnectedAccount, error) {
	var row models.ConnectedAccount
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("services: loading account %d: %w", id, err)
	}
	return &row, nil
}

// FindAccount returns the account previously connected for a remote identity,
// which is how a reconnection rotates the stored tokens instead of duplicating
// the row.
func (s *Store) FindAccount(ctx context.Context, provider models.ProviderName, providerAccountID string) (*models.ConnectedAccount, error) {
	var row models.ConnectedAccount
	err := s.db.WithContext(ctx).
		Where("provider = ? AND provider_account_id = ?", string(provider), providerAccountID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("services: loading %s account: %w", provider, err)
	}
	return &row, nil
}

// SaveAccount inserts or updates an account row.
func (s *Store) SaveAccount(ctx context.Context, account *models.ConnectedAccount) error {
	if err := s.db.WithContext(ctx).Save(account).Error; err != nil {
		return fmt.Errorf("services: saving account: %w", err)
	}
	return nil
}

// DeleteAccount removes an account together with the jobs that referenced it:
// keeping a job whose endpoint disappeared would only produce failing runs.
func (s *Store) DeleteAccount(ctx context.Context, id uint) (int64, error) {
	var jobs int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// A missing account is reported instead of silently succeeding, so the
		// handler can answer 404 the way every other delete does.
		var account models.ConnectedAccount
		if err := tx.First(&account, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		where := "source_account_id = ? OR destination_account_id = ?"
		if err := tx.Model(&models.SyncJob{}).Where(where, id, id).Count(&jobs).Error; err != nil {
			return err
		}
		if err := tx.Where(where, id, id).Delete(&models.SyncJob{}).Error; err != nil {
			return err
		}
		remaining := tx.Model(&models.SyncJob{}).Select("id")
		if err := tx.Where("job_id NOT IN (?)", remaining).Delete(&models.SyncFile{}).Error; err != nil {
			return err
		}
		if err := tx.Where("job_id NOT IN (?)", remaining).Delete(&models.SyncItem{}).Error; err != nil {
			return err
		}
		if err := tx.Where("job_id NOT IN (?)", remaining).Delete(&models.SyncRun{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.ConnectedAccount{}, id).Error
	})
	if err != nil {
		return 0, fmt.Errorf("services: deleting account %d: %w", id, err)
	}
	return jobs, nil
}

// MarkAccountError records a token failure so the UI flags the account and the
// operator is notified.
func (s *Store) MarkAccountError(ctx context.Context, id uint, message string) error {
	err := s.db.WithContext(ctx).Model(&models.ConnectedAccount{}).
		Where("id = ?", id).
		Updates(map[string]any{"status": string(models.AccountError), "last_error": message}).
		Error
	if err != nil {
		return fmt.Errorf("services: flagging account %d: %w", id, err)
	}
	return nil
}

// TouchAccount records a successful provider interaction.
func (s *Store) TouchAccount(ctx context.Context, id uint, when time.Time) error {
	err := s.db.WithContext(ctx).Model(&models.ConnectedAccount{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":         string(models.AccountConnected),
			"last_error":     "",
			"last_synced_at": when,
		}).Error
	if err != nil {
		return fmt.Errorf("services: touching account %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sync jobs
// ---------------------------------------------------------------------------

// ListJobs returns every job, newest first.
func (s *Store) ListJobs(ctx context.Context) ([]models.SyncJob, error) {
	var rows []models.SyncJob
	if err := s.db.WithContext(ctx).Order("id DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing jobs: %w", err)
	}
	return rows, nil
}

// GetJob returns one job by id.
func (s *Store) GetJob(ctx context.Context, id uint) (*models.SyncJob, error) {
	var row models.SyncJob
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("services: loading job %d: %w", id, err)
	}
	return &row, nil
}

// SaveJob inserts or updates a job.
func (s *Store) SaveJob(ctx context.Context, job *models.SyncJob) error {
	if err := s.db.WithContext(ctx).Save(job).Error; err != nil {
		return fmt.Errorf("services: saving job: %w", err)
	}
	return nil
}

// DeleteJob removes a job with its run history, its items and its file state.
func (s *Store) DeleteJob(ctx context.Context, id uint) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("job_id = ?", id).Delete(&models.SyncFile{}).Error; err != nil {
			return err
		}
		if err := tx.Where("job_id = ?", id).Delete(&models.SyncItem{}).Error; err != nil {
			return err
		}
		if err := tx.Where("job_id = ?", id).Delete(&models.SyncRun{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.SyncJob{}, id).Error
	})
	if err != nil {
		return fmt.Errorf("services: deleting job %d: %w", id, err)
	}
	return nil
}

// DueJobs returns the enabled jobs whose schedule elapsed, oldest first.
func (s *Store) DueJobs(ctx context.Context, now time.Time) ([]models.SyncJob, error) {
	var rows []models.SyncJob
	err := s.db.WithContext(ctx).
		Where("enabled = ? AND interval_minutes > 0", true).
		Where("next_run_at IS NULL OR next_run_at <= ?", now).
		Order("id ASC").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("services: selecting due jobs: %w", err)
	}
	return rows, nil
}

// ScheduleJob writes the next execution time of a job without touching anything
// else on the row, so the scheduler can push a slot forward on its own.
func (s *Store) ScheduleJob(ctx context.Context, id uint, next *time.Time) error {
	err := s.db.WithContext(ctx).Model(&models.SyncJob{}).
		Where("id = ?", id).
		Updates(map[string]any{"next_run_at": next}).Error
	if err != nil {
		return fmt.Errorf("services: scheduling job %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sync runs and items
// ---------------------------------------------------------------------------

// CreateRun inserts a run row in the running state.
func (s *Store) CreateRun(ctx context.Context, run *models.SyncRun) error {
	if err := s.db.WithContext(ctx).Create(run).Error; err != nil {
		return fmt.Errorf("services: creating run: %w", err)
	}
	return nil
}

// FinishRun stores the outcome of a run and mirrors the summary on the job
// (last status, last run time and the next scheduled slot).
func (s *Store) FinishRun(ctx context.Context, run *models.SyncRun, job *models.SyncJob) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(run).Error; err != nil {
			return err
		}
		updates := map[string]any{
			"last_run_at": run.StartedAt,
			"last_status": run.Status,
			"last_error":  run.Message,
		}
		if job != nil && job.IntervalMinutes > 0 {
			next := run.StartedAt.Add(time.Duration(job.IntervalMinutes) * time.Minute)
			updates["next_run_at"] = &next
		} else {
			updates["next_run_at"] = nil
		}
		return tx.Model(&models.SyncJob{}).Where("id = ?", run.JobID).Updates(updates).Error
	})
}

// AddRunItems appends the per-file details of a run in one batch.
func (s *Store) AddRunItems(ctx context.Context, items []models.SyncItem) error {
	if len(items) == 0 {
		return nil
	}
	if err := s.db.WithContext(ctx).CreateInBatches(&items, 200).Error; err != nil {
		return fmt.Errorf("services: storing run items: %w", err)
	}
	return nil
}

// ListRuns returns the runs of a job (every job when jobID is 0), newest first.
func (s *Store) ListRuns(ctx context.Context, jobID uint, limit int) ([]models.SyncRun, error) {
	query := s.db.WithContext(ctx).Order("id DESC")
	if jobID > 0 {
		query = query.Where("job_id = ?", jobID)
	}
	if limit > 0 {
		query = query.Limit(limit)
	}
	var rows []models.SyncRun
	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing runs: %w", err)
	}
	return rows, nil
}

// Stats aggregates the numbers the dashboard displays. The run counters are the
// totals over the counted window: `since` in the zero value means "every run
// ever recorded", a non-zero instant limits the sums to the runs started after
// it (the dashboard asks for the last 24 hours and for everything).
type Stats struct {
	Accounts     int64 `json:"accounts"`
	Jobs         int64 `json:"jobs"`
	EnabledJobs  int64 `json:"enabled_jobs"`
	Runs         int64 `json:"runs"`
	Succeeded    int64 `json:"succeeded"`
	Partial      int64 `json:"partial"`
	Failed       int64 `json:"failed"`
	Cancelled    int64 `json:"cancelled"`
	Conflicts    int64 `json:"conflicts"`
	FilesCreated int64 `json:"files_created"`
	FilesUpdated int64 `json:"files_updated"`
	FilesDeleted int64 `json:"files_deleted"`
	Bytes        int64 `json:"bytes_transferred"`
}

// Stats counts the rows the dashboard shows. Everything is computed in SQL so a
// long run history never has to be loaded into the process.
func (s *Store) Stats(ctx context.Context, since time.Time) (Stats, error) {
	var out Stats
	db := s.db.WithContext(ctx)

	if err := db.Model(&models.ConnectedAccount{}).Count(&out.Accounts).Error; err != nil {
		return Stats{}, fmt.Errorf("services: counting accounts: %w", err)
	}
	if err := db.Model(&models.SyncJob{}).Count(&out.Jobs).Error; err != nil {
		return Stats{}, fmt.Errorf("services: counting jobs: %w", err)
	}
	if err := db.Model(&models.SyncJob{}).Where("enabled = ?", true).Count(&out.EnabledJobs).Error; err != nil {
		return Stats{}, fmt.Errorf("services: counting enabled jobs: %w", err)
	}

	// One aggregate query keeps the dashboard cheap even with a large history.
	aggregate := struct {
		Runs         int64
		Succeeded    int64
		Partial      int64
		Failed       int64
		Cancelled    int64
		Conflicts    int64
		FilesCreated int64
		FilesUpdated int64
		FilesDeleted int64
		Bytes        int64
	}{}
	query := db.Model(&models.SyncRun{})
	if !since.IsZero() {
		query = query.Where("started_at >= ?", since)
	}
	err := query.Select(
		"COUNT(*) AS runs, "+
			"COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS succeeded, "+
			"COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS partial, "+
			"COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS failed, "+
			"COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS cancelled, "+
			"COALESCE(SUM(conflicts), 0) AS conflicts, "+
			"COALESCE(SUM(files_created), 0) AS files_created, "+
			"COALESCE(SUM(files_updated), 0) AS files_updated, "+
			"COALESCE(SUM(files_deleted), 0) AS files_deleted, "+
			"COALESCE(SUM(bytes_transferred), 0) AS bytes",
		string(models.RunSuccess),
		string(models.RunPartial),
		string(models.RunFailed),
		string(models.RunCancelled),
	).Scan(&aggregate).Error
	if err != nil {
		return Stats{}, fmt.Errorf("services: aggregating runs: %w", err)
	}
	out.Runs = aggregate.Runs
	out.Succeeded = aggregate.Succeeded
	out.Partial = aggregate.Partial
	out.Failed = aggregate.Failed
	out.Cancelled = aggregate.Cancelled
	out.Conflicts = aggregate.Conflicts
	out.FilesCreated = aggregate.FilesCreated
	out.FilesUpdated = aggregate.FilesUpdated
	out.FilesDeleted = aggregate.FilesDeleted
	out.Bytes = aggregate.Bytes
	return out, nil
}

func (s *Store) GetRun(ctx context.Context, id uint) (*models.SyncRun, error) {
	var row models.SyncRun
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("services: loading run %d: %w", id, err)
	}
	return &row, nil
}

// ListRunItems returns the per-file detail of a run.
func (s *Store) ListRunItems(ctx context.Context, runID uint, limit int) ([]models.SyncItem, error) {
	query := s.db.WithContext(ctx).Where("run_id = ?", runID).Order("id ASC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	var rows []models.SyncItem
	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing run items: %w", err)
	}
	return rows, nil
}

// RunningRun returns the run of a job that is currently executing, if any: it is
// what prevents a job from being started twice.
func (s *Store) RunningRun(ctx context.Context, jobID uint) (*models.SyncRun, error) {
	var row models.SyncRun
	err := s.db.WithContext(ctx).
		Where("job_id = ? AND status = ?", jobID, string(models.RunRunning)).
		Order("id DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("services: loading running run: %w", err)
	}
	return &row, nil
}

// FailInterruptedRuns closes the runs a crash or a restart left behind, so the
// dashboard never shows a run that stays "running" forever.
func (s *Store) FailInterruptedRuns(ctx context.Context, now time.Time) (int64, error) {
	result := s.db.WithContext(ctx).Model(&models.SyncRun{}).
		Where("status = ?", string(models.RunRunning)).
		Updates(map[string]any{
			"status":      string(models.RunFailed),
			"finished_at": &now,
			"message":     "interrupted by a Sync restart",
		})
	if result.Error != nil {
		return 0, fmt.Errorf("services: closing interrupted runs: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// PruneRuns keeps the newest runs of every job and drops the older ones with
// their items, which bounds the growth of the audit tables.
func (s *Store) PruneRuns(ctx context.Context, keep int) error {
	if keep <= 0 {
		return nil
	}
	var ids []uint
	err := s.db.WithContext(ctx).Raw(
		"SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY job_id ORDER BY id DESC) AS rn FROM sync_runs) AS ranked WHERE rn > ?",
		keep).Scan(&ids).Error
	if err != nil {
		return fmt.Errorf("services: selecting prunable runs: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := s.db.WithContext(ctx).Where("run_id IN ?", ids).Delete(&models.SyncItem{}).Error; err != nil {
		return fmt.Errorf("services: pruning run items: %w", err)
	}
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Delete(&models.SyncRun{}).Error; err != nil {
		return fmt.Errorf("services: pruning runs: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// File state
// ---------------------------------------------------------------------------

// FileState returns the state rows of a job keyed by relative path.
func (s *Store) FileState(ctx context.Context, jobID uint) (map[string]models.SyncFile, error) {
	var rows []models.SyncFile
	if err := s.db.WithContext(ctx).Where("job_id = ?", jobID).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: loading file state: %w", err)
	}
	state := make(map[string]models.SyncFile, len(rows))
	for _, row := range rows {
		state[row.Path] = row
	}
	return state, nil
}

// SaveFileState upserts one state row (job + path is the unique key).
func (s *Store) SaveFileState(ctx context.Context, row *models.SyncFile) error {
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "job_id"}, {Name: "path"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"source_item_id", "destination_item_id", "size", "hash",
			"source_modified_at", "destination_modified_at", "last_synced_at",
			"last_direction", "status", "updated_at",
		}),
	}).Create(row).Error
	if err != nil {
		return fmt.Errorf("services: saving file state %q: %w", row.Path, err)
	}
	return nil
}

// DeleteFileState forgets the tracked file at path.
func (s *Store) DeleteFileState(ctx context.Context, jobID uint, path string) error {
	err := s.db.WithContext(ctx).
		Where("job_id = ? AND path = ?", jobID, path).
		Delete(&models.SyncFile{}).Error
	if err != nil {
		return fmt.Errorf("services: deleting file state %q: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// OAuth states
// ---------------------------------------------------------------------------

// SaveOAuthState stores the server side of an authorization-code flow.
func (s *Store) SaveOAuthState(ctx context.Context, state *models.OAuthState) error {
	if err := s.db.WithContext(ctx).Create(state).Error; err != nil {
		return fmt.Errorf("services: storing oauth state: %w", err)
	}
	return nil
}

// ConsumeOAuthState returns a pending flow and deletes it in the same
// transaction, which makes a replayed callback impossible.
func (s *Store) ConsumeOAuthState(ctx context.Context, value string) (*models.OAuthState, error) {
	var row models.OAuthState
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("state = ?", value).First(&row).Error; err != nil {
			return err
		}
		return tx.Delete(&models.OAuthState{}, row.ID).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("services: consuming oauth state: %w", err)
	}
	return &row, nil
}

// ---------------------------------------------------------------------------
// Notification channels
// ---------------------------------------------------------------------------

// ListChannels returns the configured notification channels.
func (s *Store) ListChannels(ctx context.Context) ([]models.NotificationChannel, error) {
	var rows []models.NotificationChannel
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing channels: %w", err)
	}
	return rows, nil
}

// GetChannel returns one channel by id.
func (s *Store) GetChannel(ctx context.Context, id uint) (*models.NotificationChannel, error) {
	var row models.NotificationChannel
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("services: loading channel %d: %w", id, err)
	}
	return &row, nil
}

// SaveChannel inserts or updates a channel.
func (s *Store) SaveChannel(ctx context.Context, channel *models.NotificationChannel) error {
	if err := s.db.WithContext(ctx).Save(channel).Error; err != nil {
		return fmt.Errorf("services: saving channel: %w", err)
	}
	return nil
}

// DeleteChannel removes a channel.
func (s *Store) DeleteChannel(ctx context.Context, id uint) error {
	if err := s.db.WithContext(ctx).Delete(&models.NotificationChannel{}, id).Error; err != nil {
		return fmt.Errorf("services: deleting channel %d: %w", id, err)
	}
	return nil
}

// TouchChannel records the outcome of a delivery attempt.
func (s *Store) TouchChannel(ctx context.Context, id uint, status, message string, when time.Time) error {
	err := s.db.WithContext(ctx).Model(&models.NotificationChannel{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"last_status":  status,
			"last_error":   message,
			"last_used_at": &when,
		}).Error
	if err != nil {
		return fmt.Errorf("services: updating channel %d: %w", id, err)
	}
	return nil
}
