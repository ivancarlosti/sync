package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/ivancarlosti/sync/internal/models"
)

// CreateManagementTask inserts a task row in the running state.
func (s *Store) CreateManagementTask(ctx context.Context, task *models.ManagementTask) error {
	if err := s.db.WithContext(ctx).Create(task).Error; err != nil {
		return fmt.Errorf("services: creating the management task: %w", err)
	}
	return nil
}

// AppendManagementStep writes one journal entry of a task. The caller supplies the
// sequence number (it is the only writer for a task), which keeps the ordering
// explicit instead of relying on insertion order.
func (s *Store) AppendManagementStep(ctx context.Context, step *models.ManagementTaskStep) error {
	if err := s.db.WithContext(ctx).Create(step).Error; err != nil {
		return fmt.Errorf("services: appending a management step: %w", err)
	}
	return nil
}

// UpdateManagementProgress writes the live counters of a running task, which is
// what the polling UI reads.
func (s *Store) UpdateManagementProgress(ctx context.Context, id uint, steps, failed, items int, bytes int64) error {
	err := s.db.WithContext(ctx).Model(&models.ManagementTask{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"step_count":      steps,
			"failed":          failed,
			"items_processed": items,
			"bytes_moved":     bytes,
		}).Error
	if err != nil {
		return fmt.Errorf("services: updating the management progress: %w", err)
	}
	return nil
}

// FinishManagementTask stores the final state of a task.
func (s *Store) FinishManagementTask(ctx context.Context, task *models.ManagementTask) error {
	if err := s.db.WithContext(ctx).Save(task).Error; err != nil {
		return fmt.Errorf("services: finishing management task %d: %w", task.ID, err)
	}
	return nil
}

// GetManagementTask returns one task by id.
func (s *Store) GetManagementTask(ctx context.Context, id uint) (*models.ManagementTask, error) {
	var row models.ManagementTask
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("services: loading the management task: %w", err)
	}
	return &row, nil
}

// ListManagementTasks returns the tasks, newest first, optionally narrowed to one
// account.
func (s *Store) ListManagementTasks(ctx context.Context, accountID uint, limit int) ([]models.ManagementTask, error) {
	query := s.db.WithContext(ctx).Order("id DESC")
	if accountID > 0 {
		query = query.Where("account_id = ?", accountID)
	}
	if limit > 0 {
		query = query.Limit(limit)
	}
	var rows []models.ManagementTask
	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing the management tasks: %w", err)
	}
	return rows, nil
}

// ListManagementSteps returns the journal of a task in order.
func (s *Store) ListManagementSteps(ctx context.Context, taskID uint) ([]models.ManagementTaskStep, error) {
	var rows []models.ManagementTaskStep
	if err := s.db.WithContext(ctx).Where("task_id = ?", taskID).Order("seq ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("services: listing the management steps: %w", err)
	}
	return rows, nil
}

// RunningManagementTask returns the task of an account that is currently
// executing, if any: it is what prevents the same account from running two
// operations at once.
func (s *Store) RunningManagementTask(ctx context.Context, accountID uint) (*models.ManagementTask, error) {
	var row models.ManagementTask
	err := s.db.WithContext(ctx).
		Where("account_id = ? AND status = ?", accountID, string(models.RunRunning)).
		Order("id DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("services: loading the running management task: %w", err)
	}
	return &row, nil
}

// FailInterruptedManagementTasks closes the tasks a crash or a restart left
// behind, so the history never shows a task that stays "running" forever (and an
// account is never blocked by a ghost reservation).
func (s *Store) FailInterruptedManagementTasks(ctx context.Context, now time.Time) (int64, error) {
	result := s.db.WithContext(ctx).Model(&models.ManagementTask{}).
		Where("status = ?", string(models.RunRunning)).
		Updates(map[string]any{
			"status":      string(models.RunFailed),
			"finished_at": &now,
			"message":     "interrupted by a Sync restart",
		})
	if result.Error != nil {
		return 0, fmt.Errorf("services: closing the interrupted management tasks: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// PruneManagementTasks keeps the newest tasks of every account and drops the
// older ones with their journal, which bounds the growth of the history.
func (s *Store) PruneManagementTasks(ctx context.Context, keep int) error {
	if keep <= 0 {
		return nil
	}
	var ids []uint
	err := s.db.WithContext(ctx).Raw(
		"SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY account_id ORDER BY id DESC) AS rn FROM management_tasks) AS ranked WHERE rn > ?",
		keep).Scan(&ids).Error
	if err != nil {
		return fmt.Errorf("services: selecting the prunable management tasks: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := s.db.WithContext(ctx).Where("task_id IN ?", ids).Delete(&models.ManagementTaskStep{}).Error; err != nil {
		return fmt.Errorf("services: pruning the management steps: %w", err)
	}
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Delete(&models.ManagementTask{}).Error; err != nil {
		return fmt.Errorf("services: pruning the management tasks: %w", err)
	}
	return nil
}
