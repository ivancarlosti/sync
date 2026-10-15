// This file covers the store-side retention rules: what deleting a job keeps,
// and the age based purge of runs, audits and traffic days (store.go), plus the
// compensation SaveJob makes for a bool column that carries a GORM default.
//
// The count based pruners (PruneRuns, PruneAudits) are exercised in billing_test.go
// and audit_test.go; here it is the calendar that decides.

package services

import (
	"context"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// seedJob stores a job so the foreign rows of a test have something to point at.
func seedJob(t *testing.T, store *Store, name string) *models.SyncJob {
	t.Helper()
	job := &models.SyncJob{
		Name:                 name,
		SourceAccountID:      1,
		DestinationAccountID: 2,
		Direction:            string(models.DirectionGoogleToMicrosoft),
		ConflictPolicy:       string(models.ConflictNewestWins),
		Enabled:              true,
	}
	if err := store.SaveJob(context.Background(), job); err != nil {
		t.Fatalf("saving the job %q: %v", name, err)
	}
	return job
}

// seedRun stores a run in the given state, at the given (back-dated) start
// instant, with one item and one traffic row so a purge can be seen on all three
// tables.
func seedRun(t *testing.T, store *Store, job *models.SyncJob, startedAt time.Time, status models.RunStatus) uint {
	t.Helper()
	ctx := context.Background()
	run := &models.SyncRun{
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    string(status),
		Trigger:   string(models.TriggerManual),
		StartedAt: startedAt,
	}
	if status != models.RunRunning {
		finished := startedAt.Add(time.Minute)
		run.FinishedAt = &finished
		run.DurationMS = 60000
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("creating the run: %v", err)
	}
	if err := store.AddRunItems(ctx, []models.SyncItem{
		{RunID: run.ID, JobID: job.ID, Action: "created", Path: "report.txt", Size: 11},
	}); err != nil {
		t.Fatalf("adding the run item: %v", err)
	}
	entry := &models.TrafficEntry{
		RunID:     run.ID,
		JobID:     job.ID,
		AccountID: job.SourceAccountID,
		Direction: string(models.TrafficInbound),
		Bytes:     11,
		Files:     1,
		Day:       startedAt.UTC().Format(models.TrafficDayFormat),
		Month:     startedAt.UTC().Format(models.TrafficMonthFormat),
		StartedAt: startedAt,
	}
	if err := store.db.WithContext(ctx).Create(entry).Error; err != nil {
		t.Fatalf("creating the traffic row: %v", err)
	}
	return run.ID
}

// seedAudit stores an audit in the given state, at the given start instant, with
// one report entry.
func seedAudit(t *testing.T, store *Store, startedAt time.Time, status models.RunStatus) uint {
	t.Helper()
	ctx := context.Background()
	run := &models.AuditRun{
		AccountID: 1,
		Status:    string(status),
		Trigger:   string(models.TriggerManual),
		StartedAt: startedAt,
		Files:     1,
		Folders:   1,
	}
	if status != models.RunRunning {
		finished := startedAt.Add(time.Minute)
		run.FinishedAt = &finished
		run.DurationMS = 60000
	}
	if err := store.CreateAudit(ctx, run); err != nil {
		t.Fatalf("creating the audit: %v", err)
	}
	if err := store.AddAuditEntries(ctx, []models.AuditEntry{
		{AuditID: run.ID, Kind: "file", Name: "report.txt", Size: 11, TotalSize: 11, Listed: true},
	}); err != nil {
		t.Fatalf("adding the audit entry: %v", err)
	}
	return run.ID
}

// countRows is a small helper that counts the rows of a table.
func countRows(t *testing.T, store *Store, model any, where string, args ...any) int64 {
	t.Helper()
	query := store.db.WithContext(context.Background()).Model(model)
	if where != "" {
		query = query.Where(where, args...)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		t.Fatalf("counting the rows: %v", err)
	}
	return total
}

// TestSaveJobKeepsEmptyFoldersOff pins the compensation the store makes for the
// GORM default of the sync_empty_folders column. A bool column tagged `default`
// makes GORM substitute that default for the zero value on insert, so a job saved
// with the option off would otherwise come back on; the value the caller asked
// for is what a read returns, and an update still follows it.
func TestSaveJobKeepsEmptyFoldersOff(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	job := seedJob(t, store, "quiet job")

	if job.SyncEmptyFolders {
		t.Fatal("saving the job changed the value the caller passed")
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("loading the job: %v", err)
	}
	if stored.SyncEmptyFolders {
		t.Error("the stored column = true, want the false the caller passed")
	}

	// Switching the option back on is an ordinary update.
	job.SyncEmptyFolders = true
	if err := store.SaveJob(ctx, job); err != nil {
		t.Fatalf("saving the job: %v", err)
	}
	again, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("loading the job: %v", err)
	}
	if !again.SyncEmptyFolders {
		t.Error("the update must turn the option back on")
	}

	// The column default is what a row written before the option existed
	// inherits: an insert that does not name the column comes back on, which is
	// the behaviour the migration keeps for the older jobs.
	if err := store.db.WithContext(ctx).Exec(
		"INSERT INTO sync_jobs (name) VALUES (?)", "legacy job").Error; err != nil {
		t.Fatalf("inserting a legacy row: %v", err)
	}
	var legacy models.SyncJob
	if err := store.db.WithContext(ctx).Where("name = ?", "legacy job").First(&legacy).Error; err != nil {
		t.Fatalf("loading the legacy row: %v", err)
	}
	if !legacy.SyncEmptyFolders {
		t.Error("a row written before the column existed must keep mirroring the folders")
	}
}

// TestDeleteJobKeepsRunHistory pins the promise of the delete dialog: removing a
// job erases its definition and the engine file state, never the history an
// operator may still need to read.
func TestDeleteJobKeepsRunHistory(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	job := seedJob(t, store, "google to microsoft")
	runID := seedRun(t, store, job, time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC), models.RunSuccess)

	if err := store.db.WithContext(ctx).Create(&models.SyncFile{
		JobID: job.ID, Path: "report.txt", SourceItemID: "src", DestinationItemID: "dst",
		Size: 11, LastSyncedAt: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC),
	}).Error; err != nil {
		t.Fatalf("creating the file state: %v", err)
	}

	if err := store.DeleteJob(ctx, job.ID); err != nil {
		t.Fatalf("deleting the job: %v", err)
	}
	if jobs, err := store.ListJobs(ctx); err != nil || len(jobs) != 0 {
		t.Fatalf("jobs = %+v (%v), want the job gone", jobs, err)
	}
	if files := countRows(t, store, &models.SyncFile{}, "job_id = ?", job.ID); files != 0 {
		t.Fatalf("sync_files = %d, want the file state gone with the job", files)
	}

	// The history outlives the job: the run, its items and its traffic remain.
	runs, err := store.ListRuns(ctx, 0, 0)
	if err != nil || len(runs) != 1 || runs[0].ID != runID {
		t.Fatalf("runs = %+v (%v), want the run kept", runs, err)
	}
	if runs[0].JobName != job.Name {
		t.Fatalf("job_name = %q, want the name denormalised on the run", runs[0].JobName)
	}
	if items := countRows(t, store, &models.SyncItem{}, "run_id = ?", runID); items != 1 {
		t.Fatalf("sync_items = %d, want the items kept", items)
	}
	if traffic := countRows(t, store, &models.TrafficEntry{}, "run_id = ?", runID); traffic != 1 {
		t.Fatalf("traffic_entries = %d, want the traffic kept", traffic)
	}
	if stats, err := store.Stats(ctx, time.Time{}); err != nil || stats.Runs != 1 || stats.Succeeded != 1 {
		t.Fatalf("stats = %+v (%v), want the run still counted", stats, err)
	}
}

// TestPurgeRunsBeforeErasesFinishedRunsByAge checks the age rule: what started
// before the cutoff and is over goes with its items and its traffic, and nothing
// else moves.
func TestPurgeRunsBeforeErasesFinishedRunsByAge(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	job := seedJob(t, store, "google to microsoft")
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

	old := seedRun(t, store, job, now.AddDate(0, 0, -400), models.RunSuccess)
	recent := seedRun(t, store, job, now.AddDate(0, 0, -2), models.RunSuccess)
	// A run that started long ago but is still going belongs to the engine.
	stuck := seedRun(t, store, job, now.AddDate(0, 0, -400), models.RunRunning)

	removed, err := store.PurgeRunsBefore(ctx, now.AddDate(0, 0, -180))
	if err != nil {
		t.Fatalf("purging the runs: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want the single aged run", removed)
	}
	if runs := countRows(t, store, &models.SyncRun{}, ""); runs != 2 {
		t.Fatalf("sync_runs = %d, want the recent and the running run kept", runs)
	}
	if items := countRows(t, store, &models.SyncItem{}, "run_id = ?", old); items != 0 {
		t.Fatalf("sync_items = %d, want the items of the purged run gone", items)
	}
	if traffic := countRows(t, store, &models.TrafficEntry{}, "run_id = ?", old); traffic != 0 {
		t.Fatalf("traffic_entries = %d, want the traffic of the purged run gone", traffic)
	}
	for _, kept := range []uint{recent, stuck} {
		if items := countRows(t, store, &models.SyncItem{}, "run_id = ?", kept); items != 1 {
			t.Fatalf("sync_items of run %d = %d, want it untouched", kept, items)
		}
		if traffic := countRows(t, store, &models.TrafficEntry{}, "run_id = ?", kept); traffic != 1 {
			t.Fatalf("traffic_entries of run %d = %d, want it untouched", kept, traffic)
		}
	}
	// The job row stays: only the history ages out.
	if jobs := countRows(t, store, &models.SyncJob{}, ""); jobs != 1 {
		t.Fatalf("jobs = %d, want the job kept", jobs)
	}

	// A second pass over the same cutoff has nothing left to erase.
	if again, err := store.PurgeRunsBefore(ctx, now.AddDate(0, 0, -180)); err != nil || again != 0 {
		t.Fatalf("second pass = %d (%v), want a no-op", again, err)
	}
}

// TestPurgeAuditsBeforeErasesFinishedAuditsByAge is the audit counterpart of the
// run rule.
func TestPurgeAuditsBeforeErasesFinishedAuditsByAge(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

	old := seedAudit(t, store, now.AddDate(0, 0, -20), models.RunSuccess)
	recent := seedAudit(t, store, now.AddDate(0, 0, -1), models.RunFailed)
	running := seedAudit(t, store, now.AddDate(0, 0, -20), models.RunRunning)

	removed, err := store.PurgeAuditsBefore(ctx, now.AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("purging the audits: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want the single aged audit", removed)
	}
	if audits := countRows(t, store, &models.AuditRun{}, ""); audits != 2 {
		t.Fatalf("audit_runs = %d, want the recent and the running audit kept", audits)
	}
	if entries := countRows(t, store, &models.AuditEntry{}, "audit_id = ?", old); entries != 0 {
		t.Fatalf("audit_entries = %d, want the report of the purged audit gone", entries)
	}
	for _, kept := range []uint{recent, running} {
		if entries := countRows(t, store, &models.AuditEntry{}, "audit_id = ?", kept); entries != 1 {
			t.Fatalf("audit_entries of audit %d = %d, want it untouched", kept, entries)
		}
	}
}

// TestPurgeTrafficBeforeDropsWholeDays checks that the traffic purge works on
// the indexed day bucket: every row of a day older than the cutoff goes, the day
// of the cutoff and the days after it stay.
func TestPurgeTrafficBeforeDropsWholeDays(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	job := seedJob(t, store, "google to microsoft")

	seedRun(t, store, job, now.AddDate(0, 0, -400), models.RunSuccess)
	seedRun(t, store, job, now.AddDate(0, 0, -30), models.RunSuccess)
	seedRun(t, store, job, now.AddDate(0, 0, -6), models.RunSuccess)

	// A cutoff exactly on a day boundary keeps that whole day.
	cutoff := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	removed, err := store.PurgeTrafficBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("purging the traffic: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want the two rows older than the cutoff day", removed)
	}
	if left := countRows(t, store, &models.TrafficEntry{}, ""); left != 1 {
		t.Fatalf("traffic_entries = %d, want the day of the cutoff kept", left)
	}
	if before := countRows(t, store, &models.TrafficEntry{}, "day < ?", "2026-10-08"); before != 0 {
		t.Fatalf("rows before the cutoff = %d, want none", before)
	}
	// The runs are a different target: the history of a window that lost its
	// traffic still reads.
	if runs := countRows(t, store, &models.SyncRun{}, ""); runs != 3 {
		t.Fatalf("sync_runs = %d, want the runs untouched by the traffic purge", runs)
	}
}
