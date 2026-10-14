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
	"strings"
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
		// The audits of the account are reports about its data, so they follow
		// it: keeping a report whose endpoint disappeared would only confuse.
		var auditIDs []uint
		if err := tx.Model(&models.AuditRun{}).Where("account_id = ?", id).Pluck("id", &auditIDs).Error; err != nil {
			return err
		}
		if len(auditIDs) > 0 {
			if err := tx.Where("audit_id IN ?", auditIDs).Delete(&models.AuditEntry{}).Error; err != nil {
				return err
			}
			if err := tx.Where("id IN ?", auditIDs).Delete(&models.AuditRun{}).Error; err != nil {
				return err
			}
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

// DeleteJob removes a job definition and its file state, and keeps its run
// history: the runs, their items and their traffic rows outlive the job, which
// is what the delete dialog promises and what an operator needs when a job is
// removed right after a failure. Only sync_files goes, because that state is
// engine bookkeeping (it is what makes a re-created job resume correctly) and
// nothing can ever read it again once the job row is gone.
//
// The history stays reachable: ListRuns(0) returns every run whatever its job,
// and the run row carries job_name so an orphaned run still reads well.
func (s *Store) DeleteJob(ctx context.Context, id uint) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("job_id = ?", id).Delete(&models.SyncFile{}).Error; err != nil {
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
	TimedOut     int64 `json:"timed_out"`
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
		TimedOut     int64
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
			"COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS timed_out, "+
			"COALESCE(SUM(conflicts), 0) AS conflicts, "+
			"COALESCE(SUM(files_created), 0) AS files_created, "+
			"COALESCE(SUM(files_updated), 0) AS files_updated, "+
			"COALESCE(SUM(files_deleted), 0) AS files_deleted, "+
			"COALESCE(SUM(bytes_transferred), 0) AS bytes",
		string(models.RunSuccess),
		string(models.RunPartial),
		string(models.RunFailed),
		string(models.RunCancelled),
		string(models.RunTimeout),
	).Scan(&aggregate).Error
	if err != nil {
		return Stats{}, fmt.Errorf("services: aggregating runs: %w", err)
	}
	out.Runs = aggregate.Runs
	out.Succeeded = aggregate.Succeeded
	out.Partial = aggregate.Partial
	out.Failed = aggregate.Failed
	out.Cancelled = aggregate.Cancelled
	out.TimedOut = aggregate.TimedOut
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
	// A traffic row only exists because of its run, so it is pruned with it.
	if err := s.db.WithContext(ctx).Where("run_id IN ?", ids).Delete(&models.TrafficEntry{}).Error; err != nil {
		return fmt.Errorf("services: pruning run traffic: %w", err)
	}
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Delete(&models.SyncRun{}).Error; err != nil {
		return fmt.Errorf("services: pruning runs: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Audit runs and entries
// ---------------------------------------------------------------------------

// CreateAudit inserts an audit row in the running state.
func (s *Store) CreateAudit(ctx context.Context, run *models.AuditRun) error {
	if err := s.db.WithContext(ctx).Create(run).Error; err != nil {
		return fmt.Errorf("services: creating audit: %w", err)
	}
	return nil
}

// UpdateAuditProgress mirrors the live counters of a running audit onto its row
// (the walk itself is in memory; this is only what the polling UI reads).
func (s *Store) UpdateAuditProgress(ctx context.Context, id uint, files, folders int, total int64, maxDepth int, truncated bool) error {
	err := s.db.WithContext(ctx).Model(&models.AuditRun{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"files":             files,
			"folders":           folders,
			"total_size":        total,
			"max_depth_reached": maxDepth,
			"truncated":         truncated,
		}).Error
	if err != nil {
		return fmt.Errorf("services: updating audit %d: %w", id, err)
	}
	return nil
}

// FinishAudit stores the final state of an audit run.
func (s *Store) FinishAudit(ctx context.Context, run *models.AuditRun) error {
	if err := s.db.WithContext(ctx).Save(run).Error; err != nil {
		return fmt.Errorf("services: finishing audit %d: %w", run.ID, err)
	}
	return nil
}

// AddAuditEntries appends the report entries of an audit in one batch, so a run
// over a large tree is a handful of inserts instead of one per node.
func (s *Store) AddAuditEntries(ctx context.Context, entries []models.AuditEntry) error {
	if len(entries) == 0 {
		return nil
	}
	if err := s.db.WithContext(ctx).CreateInBatches(&entries, 200).Error; err != nil {
		return fmt.Errorf("services: storing the audit entries: %w", err)
	}
	return nil
}

// ListAudits returns the audits, newest first.
func (s *Store) ListAudits(ctx context.Context, limit int) ([]models.AuditRun, error) {
	query := s.db.WithContext(ctx).Order("id DESC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	var rows []models.AuditRun
	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing audits: %w", err)
	}
	return rows, nil
}

// GetAudit returns one audit by id.
func (s *Store) GetAudit(ctx context.Context, id uint) (*models.AuditRun, error) {
	var row models.AuditRun
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("services: loading audit %d: %w", id, err)
	}
	return &row, nil
}

// ListAuditEntries returns the report entries of an audit in DFS preorder.
func (s *Store) ListAuditEntries(ctx context.Context, auditID uint, limit int) ([]models.AuditEntry, error) {
	query := s.db.WithContext(ctx).Where("audit_id = ?", auditID).Order("id ASC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	var rows []models.AuditEntry
	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing the audit entries: %w", err)
	}
	return rows, nil
}

// RunningAudit returns the audit of an account that is currently executing, if
// any: it is what prevents the same account from being audited twice.
func (s *Store) RunningAudit(ctx context.Context, accountID uint) (*models.AuditRun, error) {
	var row models.AuditRun
	err := s.db.WithContext(ctx).
		Where("account_id = ? AND status = ?", accountID, string(models.RunRunning)).
		Order("id DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("services: loading running audit: %w", err)
	}
	return &row, nil
}

// FailInterruptedAudits closes the audits a crash or a restart left behind, so
// the history never shows an audit that stays "running" forever.
func (s *Store) FailInterruptedAudits(ctx context.Context, now time.Time) (int64, error) {
	result := s.db.WithContext(ctx).Model(&models.AuditRun{}).
		Where("status = ?", string(models.RunRunning)).
		Updates(map[string]any{
			"status":      string(models.RunFailed),
			"finished_at": &now,
			"message":     "interrupted by a Sync restart",
		})
	if result.Error != nil {
		return 0, fmt.Errorf("services: closing interrupted audits: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// PruneAudits keeps the newest audits of every account and drops the older ones
// with their entries, which bounds the growth of the report tables.
func (s *Store) PruneAudits(ctx context.Context, keep int) error {
	if keep <= 0 {
		return nil
	}
	var ids []uint
	err := s.db.WithContext(ctx).Raw(
		"SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY account_id ORDER BY id DESC) AS rn FROM audit_runs) AS ranked WHERE rn > ?",
		keep).Scan(&ids).Error
	if err != nil {
		return fmt.Errorf("services: selecting prunable audits: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := s.db.WithContext(ctx).Where("audit_id IN ?", ids).Delete(&models.AuditEntry{}).Error; err != nil {
		return fmt.Errorf("services: pruning the audit entries: %w", err)
	}
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Delete(&models.AuditRun{}).Error; err != nil {
		return fmt.Errorf("services: pruning audits: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Age based retention
// ---------------------------------------------------------------------------

// PurgeRunsBefore deletes every finished run that started before the cutoff,
// with its items and its traffic rows, and answers how many runs went. A run
// still running is never touched: the engine is writing to it.
//
// Unlike PruneRuns (which keeps the newest N runs of every job) this is a plain
// age rule, so it also erases the runs of a job that was deleted in between.
func (s *Store) PurgeRunsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var ids []uint
	err := s.db.WithContext(ctx).Model(&models.SyncRun{}).
		Where("started_at < ? AND status <> ?", cutoff, string(models.RunRunning)).
		Pluck("id", &ids).Error
	if err != nil {
		return 0, fmt.Errorf("services: selecting the runs to purge: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("run_id IN ?", ids).Delete(&models.SyncItem{}).Error; err != nil {
			return err
		}
		// A traffic row only exists because of its run, so it goes with it.
		if err := tx.Where("run_id IN ?", ids).Delete(&models.TrafficEntry{}).Error; err != nil {
			return err
		}
		return tx.Where("id IN ?", ids).Delete(&models.SyncRun{}).Error
	})
	if err != nil {
		return 0, fmt.Errorf("services: purging runs: %w", err)
	}
	return int64(len(ids)), nil
}

// PurgeAuditsBefore deletes every finished audit that started before the
// cutoff, with its entries, and answers how many audits went.
func (s *Store) PurgeAuditsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var ids []uint
	err := s.db.WithContext(ctx).Model(&models.AuditRun{}).
		Where("started_at < ? AND status <> ?", cutoff, string(models.RunRunning)).
		Pluck("id", &ids).Error
	if err != nil {
		return 0, fmt.Errorf("services: selecting the audits to purge: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("audit_id IN ?", ids).Delete(&models.AuditEntry{}).Error; err != nil {
			return err
		}
		return tx.Where("id IN ?", ids).Delete(&models.AuditRun{}).Error
	})
	if err != nil {
		return 0, fmt.Errorf("services: purging audits: %w", err)
	}
	return int64(len(ids)), nil
}

// PurgeTrafficBefore drops the traffic rows of the days before the cutoff and
// answers how many rows went. It bounds the cost history on its own, so a
// deployment that keeps its run history for years can still drop old invoices.
//
// The rows are matched on their indexed Day bucket (a UTC date), which is the
// same column the report filters on; the day in progress is therefore never
// touched, and a run that started just before midnight can leave the traffic of
// the following day behind — one extra day of rows at the very most.
func (s *Store) PurgeTrafficBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	day := cutoff.UTC().Format(models.TrafficDayFormat)
	result := s.db.WithContext(ctx).Where("day < ?", day).Delete(&models.TrafficEntry{})
	if result.Error != nil {
		return 0, fmt.Errorf("services: purging traffic: %w", result.Error)
	}
	return result.RowsAffected, nil
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

// FindOAuthState returns a pending flow without consuming it, which is what the
// provider callback needs to tell a tenant-wide consent answer from an
// authorization code before redeeming anything.
func (s *Store) FindOAuthState(ctx context.Context, value string) (*models.OAuthState, error) {
	var row models.OAuthState
	err := s.db.WithContext(ctx).Where("state = ?", value).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("services: reading oauth state: %w", err)
	}
	return &row, nil
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

// ---------------------------------------------------------------------------
// API tokens
// ---------------------------------------------------------------------------

// ListAPITokens returns every token, newest first. The secret digest travels in
// the row but is never serialised (see models.APIToken).
func (s *Store) ListAPITokens(ctx context.Context) ([]models.APIToken, error) {
	var rows []models.APIToken
	if err := s.db.WithContext(ctx).Order("id DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing api tokens: %w", err)
	}
	return rows, nil
}

// GetAPIToken returns one token by id.
func (s *Store) GetAPIToken(ctx context.Context, id uint) (*models.APIToken, error) {
	var row models.APIToken
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("services: loading api token %d: %w", id, err)
	}
	return &row, nil
}

// FindAPITokenByPrefix is the authentication lookup: the prefix is the indexed,
// non-secret half of a token.
func (s *Store) FindAPITokenByPrefix(ctx context.Context, prefix string) (*models.APIToken, error) {
	var row models.APIToken
	err := s.db.WithContext(ctx).Where("prefix = ?", prefix).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("services: loading api token: %w", err)
	}
	return &row, nil
}

// CreateAPIToken inserts a token.
func (s *Store) CreateAPIToken(ctx context.Context, token *models.APIToken) error {
	if err := s.db.WithContext(ctx).Create(token).Error; err != nil {
		return fmt.Errorf("services: creating api token: %w", err)
	}
	return nil
}

// SaveAPIToken updates a token row (rename, regenerate, revoke).
func (s *Store) SaveAPIToken(ctx context.Context, token *models.APIToken) error {
	if err := s.db.WithContext(ctx).Save(token).Error; err != nil {
		return fmt.Errorf("services: saving api token %d: %w", token.ID, err)
	}
	return nil
}

// DeleteAPIToken removes a token; a delete of a missing row is not an error, so
// the endpoint stays idempotent.
func (s *Store) DeleteAPIToken(ctx context.Context, id uint) error {
	if err := s.db.WithContext(ctx).Delete(&models.APIToken{}, id).Error; err != nil {
		return fmt.Errorf("services: deleting api token %d: %w", id, err)
	}
	return nil
}

// TouchAPIToken records a successful authentication: the counters are what tells
// an operator that a token is still in use (and from where).
func (s *Store) TouchAPIToken(ctx context.Context, id uint, when time.Time, ip string) error {
	err := s.db.WithContext(ctx).Model(&models.APIToken{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"last_used_at": &when,
			"last_used_ip": ip,
			"requests":     gorm.Expr("requests + 1"),
		}).Error
	if err != nil {
		return fmt.Errorf("services: recording the use of api token %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Traffic (billing)
// ---------------------------------------------------------------------------

// The buckets a billing report can be grouped by.
const (
	// BillingByDay is the default: one row per UTC day.
	BillingByDay = "day"
	// BillingByMonth is one row per UTC month, the shape of a cloud invoice.
	BillingByMonth = "month"
	// BillingByAccount is one row per connected account.
	BillingByAccount = "account"
	// BillingByRun is the finest grain: one row per run.
	BillingByRun = "run"
)

// BillingFilter selects the traffic a report covers.
type BillingFilter struct {
	// From and To are inclusive day bounds (UTC); a zero time means "no bound".
	From time.Time
	To   time.Time
	// AccountID and JobID are optional filters (0 means "every one").
	AccountID uint
	JobID     uint
	// GroupBy is one of the BillingBy* constants.
	GroupBy string
	// Limit bounds the rows of a report (0 means "every row of the window").
	Limit int
}

// TrafficTotals are the counters of one direction of a billing row.
type TrafficTotals struct {
	Bytes      int64 `json:"bytes"`
	Files      int64 `json:"files"`
	Operations int64 `json:"operations"`
	// Runs is how many distinct runs contributed to the row.
	Runs int64 `json:"runs"`
}

// BillingRow is one line of a billing report. Which of Day/Month/AccountID/RunID
// is set depends on the grouping the request asked for.
type BillingRow struct {
	Day          string        `json:"day,omitempty"`
	Month        string        `json:"month,omitempty"`
	AccountID    uint          `json:"account_id,omitempty"`
	AccountEmail string        `json:"account_email,omitempty"`
	RunID        uint          `json:"run_id,omitempty"`
	JobID        uint          `json:"job_id,omitempty"`
	Inbound      TrafficTotals `json:"inbound"`
	Outbound     TrafficTotals `json:"outbound"`
	Total        TrafficTotals `json:"total"`
}

// billingAggregate is the raw scan of one GROUP BY row. Summing in SQL is what
// keeps a report over a year of traffic a single query.
type billingAggregate struct {
	Day          string
	Month        string
	AccountID    uint
	AccountEmail string
	RunID        uint
	JobID        uint
	InBytes      int64
	InFiles      int64
	InOps        int64
	InRuns       int64
	OutBytes     int64
	OutFiles     int64
	OutOps       int64
	OutRuns      int64
}

// billingAggregateSelect sums both directions of one bucket. The direction
// literals come from the models constants (never from a request), and
// SUM(CASE WHEN …) with COUNT(DISTINCT CASE …) is the one spelling MySQL/MariaDB
// and SQLite both accept.
var billingAggregateSelect = "MAX(day) AS day, MAX(month) AS month, " +
	"MAX(account_id) AS account_id, MAX(account_email) AS account_email, " +
	"MAX(run_id) AS run_id, MAX(job_id) AS job_id, " +
	"COALESCE(SUM(CASE WHEN direction = '" + string(models.TrafficInbound) + "' THEN bytes ELSE 0 END), 0) AS in_bytes, " +
	"COALESCE(SUM(CASE WHEN direction = '" + string(models.TrafficInbound) + "' THEN files ELSE 0 END), 0) AS in_files, " +
	"COALESCE(SUM(CASE WHEN direction = '" + string(models.TrafficInbound) + "' THEN operations ELSE 0 END), 0) AS in_ops, " +
	"COUNT(DISTINCT CASE WHEN direction = '" + string(models.TrafficInbound) + "' THEN run_id END) AS in_runs, " +
	"COALESCE(SUM(CASE WHEN direction = '" + string(models.TrafficOutbound) + "' THEN bytes ELSE 0 END), 0) AS out_bytes, " +
	"COALESCE(SUM(CASE WHEN direction = '" + string(models.TrafficOutbound) + "' THEN files ELSE 0 END), 0) AS out_files, " +
	"COALESCE(SUM(CASE WHEN direction = '" + string(models.TrafficOutbound) + "' THEN operations ELSE 0 END), 0) AS out_ops, " +
	"COUNT(DISTINCT CASE WHEN direction = '" + string(models.TrafficOutbound) + "' THEN run_id END) AS out_runs"

// SaveTraffic upserts the absolute counters of a run. The uniqueness of
// (run, account, direction) is what makes a retried or throttled flush harmless:
// the counters are the current totals, never an increment.
func (s *Store) SaveTraffic(ctx context.Context, rows []models.TrafficEntry) error {
	if len(rows) == 0 {
		return nil
	}
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "run_id"}, {Name: "account_id"}, {Name: "direction"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"job_id", "account_email", "provider", "bytes", "files", "operations",
			"day", "month", "started_at", "updated_at",
		}),
	}).Create(&rows).Error
	if err != nil {
		return fmt.Errorf("services: saving traffic: %w", err)
	}
	return nil
}

// BillingSummary aggregates a window of traffic by day, month, account or run.
func (s *Store) BillingSummary(ctx context.Context, filter BillingFilter) ([]BillingRow, error) {
	group, err := billingGroupBy(filter.GroupBy)
	if err != nil {
		return nil, err
	}
	query := s.db.WithContext(ctx).Model(&models.TrafficEntry{}).Select(billingAggregateSelect)
	query = billingWindow(query, filter).Group(group).Order(group + " DESC")
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}

	var aggregates []billingAggregate
	if err := query.Scan(&aggregates).Error; err != nil {
		return nil, fmt.Errorf("services: aggregating traffic: %w", err)
	}
	rows := make([]BillingRow, 0, len(aggregates))
	for _, aggregate := range aggregates {
		rows = append(rows, billingRow(filter.GroupBy, aggregate))
	}
	return rows, nil
}

// ListTrafficRuns returns the individual traffic rows of a window, newest run
// first: the per-run breakdown behind the aggregate report.
func (s *Store) ListTrafficRuns(ctx context.Context, filter BillingFilter) ([]models.TrafficEntry, error) {
	query := billingWindow(s.db.WithContext(ctx).Model(&models.TrafficEntry{}), filter).
		Order("run_id DESC, account_id ASC, direction ASC")
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	var rows []models.TrafficEntry
	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing traffic: %w", err)
	}
	return rows, nil
}

// GetRunTraffic returns the traffic rows of one run, which is what the run detail
// shows beside the per-file items.
func (s *Store) GetRunTraffic(ctx context.Context, runID uint) ([]models.TrafficEntry, error) {
	var rows []models.TrafficEntry
	err := s.db.WithContext(ctx).
		Where("run_id = ?", runID).
		Order("direction ASC, account_id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("services: loading the traffic of run %d: %w", runID, err)
	}
	return rows, nil
}

// billingWindow applies the day bounds and the optional filters of a report. The
// comparison is on the stored day bucket, so it stays a portable string range.
func billingWindow(query *gorm.DB, filter BillingFilter) *gorm.DB {
	if !filter.From.IsZero() {
		query = query.Where("day >= ?", filter.From.UTC().Format(models.TrafficDayFormat))
	}
	if !filter.To.IsZero() {
		query = query.Where("day <= ?", filter.To.UTC().Format(models.TrafficDayFormat))
	}
	if filter.AccountID > 0 {
		query = query.Where("account_id = ?", filter.AccountID)
	}
	if filter.JobID > 0 {
		query = query.Where("job_id = ?", filter.JobID)
	}
	return query
}

// billingGroupBy whitelists the grouping of a report: the returned expression is
// the only thing ever interpolated into the GROUP BY.
func billingGroupBy(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", BillingByDay:
		return "day", nil
	case BillingByMonth:
		return "month", nil
	case BillingByAccount:
		return "account_id", nil
	case BillingByRun:
		return "run_id", nil
	default:
		return "", fmt.Errorf("%w: unknown grouping %q", ErrValidation, value)
	}
}

// billingRow maps one aggregate onto the line the request asked for, so a day
// report never carries the arbitrary account id of a MAX() over the whole window.
func billingRow(groupBy string, aggregate billingAggregate) BillingRow {
	row := BillingRow{
		Inbound:  TrafficTotals{Bytes: aggregate.InBytes, Files: aggregate.InFiles, Operations: aggregate.InOps, Runs: aggregate.InRuns},
		Outbound: TrafficTotals{Bytes: aggregate.OutBytes, Files: aggregate.OutFiles, Operations: aggregate.OutOps, Runs: aggregate.OutRuns},
	}
	row.Total = TrafficTotals{
		Bytes:      row.Inbound.Bytes + row.Outbound.Bytes,
		Files:      row.Inbound.Files + row.Outbound.Files,
		Operations: row.Inbound.Operations + row.Outbound.Operations,
		Runs:       row.Inbound.Runs + row.Outbound.Runs,
	}
	switch strings.ToLower(strings.TrimSpace(groupBy)) {
	case BillingByMonth:
		row.Month = aggregate.Month
	case BillingByAccount:
		row.AccountID, row.AccountEmail = aggregate.AccountID, aggregate.AccountEmail
	case BillingByRun:
		row.RunID, row.JobID = aggregate.RunID, aggregate.JobID
	default:
		row.Day = aggregate.Day
	}
	return row
}
