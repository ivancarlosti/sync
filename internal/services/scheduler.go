package services

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// Periodic behaviour of the process. These are constants and not settings
// because they describe how quickly Sync reacts, not what the operator wants to
// synchronise: no external scheduler or queue is required.
const (
	// DefaultScheduleInterval is how often due sync jobs are picked up.
	DefaultScheduleInterval = 30 * time.Second
	// DefaultTokenInterval is how often the stored provider tokens are checked.
	DefaultTokenInterval = 60 * time.Second
	// DefaultRefreshWindow refreshes an access token before it actually expires,
	// so a scheduled run never has to refresh on its critical path.
	DefaultRefreshWindow = 5 * time.Minute
	// DefaultAuditLogInterval is how often the activity feeds of the connected
	// organisations are polled. Ten minutes is the finest interval Microsoft
	// documents for the management activity API (its content becomes available
	// with a delay of up to fifteen minutes) and comfortably inside every vendor
	// quota, so a shorter one would only repeat the same window.
	DefaultAuditLogInterval = 10 * time.Minute
	// DefaultPurgeInterval is how often the retention policy of Admin > Settings
	// > Purge is applied. Age is measured in days, so an hourly pass would only
	// churn through the same rows: six hours keeps the tables bounded without
	// turning the retention into a hot path.
	DefaultPurgeInterval = 6 * time.Hour
)

// Scheduler is the in-process time source of Sync. It owns four loops:
//
//   - the schedule loop starts every enabled job whose next_run_at elapsed and
//     every opted-in directory report whose slot elapsed;
//   - the token loop renews the access tokens that expire inside the refresh
//     window of every connected account;
//   - the retention loop applies the purge policy (runs, audits, traffic days and
//     activity events older than the configured age) once at start-up and every
//     DefaultPurgeInterval afterwards;
//   - the logs loop polls the activity feeds of the connected organisations once
//     at start-up and every DefaultAuditLogInterval afterwards.
//
// The loops are plain goroutines with a ticker, so running the binary twice
// against the same database only causes jobs to be attempted twice (each
// attempt is guarded by the per-job lease of SyncService).
type Scheduler struct {
	store    *Store
	engine   *SyncService
	tokens   *TokenManager
	notifier *Notifier
	purge    *PurgeService
	logs     *AuditLogService
	reports  *ReportService
	now      func() time.Time

	scheduleEvery time.Duration
	tokenEvery    time.Duration
	refreshWindow time.Duration
	purgeEvery    time.Duration
	logsEvery     time.Duration

	mu      sync.Mutex
	started bool
	stop    chan struct{}
	wg      sync.WaitGroup
}

// NewScheduler builds the scheduler with the default intervals. reports may be
// nil (a test that only exercises the sync schedule), in which case the reports
// pass is a no-op.
func NewScheduler(store *Store, engine *SyncService, tokens *TokenManager, notifier *Notifier, reports *ReportService) *Scheduler {
	return &Scheduler{
		store:         store,
		engine:        engine,
		tokens:        tokens,
		notifier:      notifier,
		purge:         NewPurgeService(store),
		logs:          NewAuditLogService(store, tokens),
		reports:       reports,
		now:           func() time.Time { return time.Now().UTC() },
		scheduleEvery: DefaultScheduleInterval,
		tokenEvery:    DefaultTokenInterval,
		refreshWindow: DefaultRefreshWindow,
		purgeEvery:    DefaultPurgeInterval,
		logsEvery:     DefaultAuditLogInterval,
		stop:          make(chan struct{}),
	}
}

// Bootstrap prepares the periodic work at start-up: it closes the runs a
// restart interrupted and gives every scheduled job a next_run_at, so a
// container that restarts often still keeps its schedule.
func (s *Scheduler) Bootstrap(ctx context.Context) error {
	if closed, err := s.store.FailInterruptedRuns(ctx, s.now()); err != nil {
		return err
	} else if closed > 0 {
		slog.Warn("closed the runs interrupted by a restart", "runs", closed)
	}
	if closed, err := s.store.FailInterruptedAudits(ctx, s.now()); err != nil {
		return err
	} else if closed > 0 {
		slog.Warn("closed the audits interrupted by a restart", "audits", closed)
	}
	if closed, err := s.store.FailInterruptedReports(ctx, s.now()); err != nil {
		return err
	} else if closed > 0 {
		slog.Warn("closed the reports interrupted by a restart", "reports", closed)
	}
	if closed, err := s.store.FailInterruptedManagementTasks(ctx, s.now()); err != nil {
		return err
	} else if closed > 0 {
		slog.Warn("closed the management tasks interrupted by a restart", "tasks", closed)
	}

	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		return err
	}
	for i := range jobs {
		job := jobs[i]
		next := NextRunAt(s.now(), &job)
		if next == nil {
			continue
		}
		if job.NextRunAt != nil && !job.NextRunAt.Before(*next) {
			continue
		}
		if err := s.store.ScheduleJob(ctx, job.ID, next); err != nil {
			slog.Warn("scheduling a job failed", "job", job.ID, "name", job.Name, "error", err)
		}
	}
	return nil
}

// Start launches both loops. It can only be called once per scheduler; the
// returned error is ErrBusy when a previous call is still running.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("%w: the scheduler is already running", ErrBusy)
	}
	s.started = true
	s.mu.Unlock()

	s.wg.Add(4)
	go func() {
		defer s.wg.Done()
		// The reports pass shares the schedule loop: it ticks at the same rate
		// and only looks at the opt-in schedules of ReportService, so a fifth
		// goroutine would buy nothing but another timer.
		s.loop(ctx, "schedule", s.scheduleEvery, func(ctx context.Context) {
			s.RunDue(ctx)
			s.RunDueReports(ctx)
		})
	}()
	go func() {
		defer s.wg.Done()
		s.loop(ctx, "tokens", s.tokenEvery, func(ctx context.Context) { s.RefreshTokens(ctx) })
	}()
	go func() {
		defer s.wg.Done()
		// The retention pass runs once at start-up as well: a container that is
		// restarted more often than the interval would otherwise never purge.
		s.RunRetention(ctx)
		s.loop(ctx, "retention", s.purgeEvery, func(ctx context.Context) { s.RunRetention(ctx) })
	}()
	go func() {
		defer s.wg.Done()
		// The first pass runs immediately: a fresh instance shows the activity
		// of the last week without waiting for the first tick.
		s.RunAuditLogs(ctx)
		s.loop(ctx, "logs", s.logsEvery, func(ctx context.Context) { s.RunAuditLogs(ctx) })
	}()
	return nil
}

// Stop ends both loops and waits for the pass in flight. It is safe to call it
// on a scheduler that was never started.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	started := s.started
	s.started = false
	s.mu.Unlock()
	if !started {
		return
	}
	close(s.stop)
	s.wg.Wait()
}

// loop runs task on a ticker until the context is cancelled or Stop is called.
func (s *Scheduler) loop(ctx context.Context, name string, every time.Duration, task func(context.Context)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	slog.Info("scheduler loop started", "loop", name, "interval", every.String())
	for {
		select {
		case <-ctx.Done():
			slog.Info("scheduler loop stopped", "loop", name, "reason", "context cancelled")
			return
		case <-s.stop:
			slog.Info("scheduler loop stopped", "loop", name, "reason", "shutdown")
			return
		case <-ticker.C:
			task(ctx)
		}
	}
}

// RunDue starts every job whose schedule elapsed and returns how many runs were
// launched. It is the body of the schedule loop, exported so the API can force
// a pass and so tests can drive the scheduler deterministically.
func (s *Scheduler) RunDue(ctx context.Context) int {
	jobs, err := s.store.DueJobs(ctx, s.now())
	if err != nil {
		slog.Error("listing the due sync jobs failed", "error", err)
		return 0
	}
	started := 0
	for i := range jobs {
		job := jobs[i]
		run, err := s.engine.StartIfIdle(ctx, job.ID, models.TriggerScheduled)
		if err != nil {
			slog.Error("starting a scheduled run failed", "job", job.ID, "name", job.Name, "error", err)
			s.skipSlot(ctx, &job)
			continue
		}
		if run == nil {
			s.skipSlot(ctx, &job)
			continue
		}
		started++
		slog.Info("scheduled sync run started", "job", job.ID, "name", job.Name, "run", run.ID)
	}
	return started
}

// skipSlot pushes the next execution of a job by one interval. It is used when
// the job is busy or cannot be started, so a failing job retries once per
// interval instead of once per tick.
func (s *Scheduler) skipSlot(ctx context.Context, job *models.SyncJob) {
	next := s.now().Add(time.Duration(job.IntervalMinutes) * time.Minute)
	if err := s.store.ScheduleJob(ctx, job.ID, &next); err != nil {
		slog.Warn("pushing the next run of a job failed", "job", job.ID, "error", err)
	}
}

// RefreshTokens renews the access tokens that expire inside the refresh window
// and returns how many accounts were refreshed.
func (s *Scheduler) RefreshTokens(ctx context.Context) int {
	refreshed := s.tokens.RefreshExpiring(ctx, s.refreshWindow)
	if refreshed > 0 {
		slog.Debug("refreshed expiring provider tokens", "accounts", refreshed)
	}
	return refreshed
}

// RunRetention applies the retention policy of Admin > Settings > Purge and
// answers what the pass erased. It is the body of the retention loop, exported
// so the API can force a pass and so tests can drive it deterministically; a
// failure is logged and reported as an empty pass, because a retention problem
// must never take the scheduler down.
func (s *Scheduler) RunRetention(ctx context.Context) PurgeResult {
	outcome, err := s.purge.Run(ctx)
	if err != nil {
		slog.Error("the retention pass failed", "error", err)
		return PurgeResult{}
	}
	return outcome.Removed
}

// RunDueReports starts every directory report whose schedule elapsed and answers
// how many were launched. It runs inside the schedule loop, is a no-op when the
// scheduler was built without a ReportService, and never panics the loop: a
// broken schedule is logged and the next tick retries.
func (s *Scheduler) RunDueReports(ctx context.Context) int {
	if s.reports == nil {
		return 0
	}
	return s.reports.RunDue(ctx)
}

// RunAuditLogs polls every activity feed once and answers what the pass did. It
// is the body of the logs loop, exported so the API can force a pass and so tests
// can drive the collector deterministically. A failure is logged and reported as
// an empty pass: an unreachable provider must never take the scheduler down.
func (s *Scheduler) RunAuditLogs(ctx context.Context) AuditLogCollectResult {
	outcome := s.logs.Collect(ctx)
	if outcome.Failed > 0 {
		slog.Warn("the activity feed pass finished with failures",
			"streams", outcome.Streams,
			"failed", outcome.Failed,
			"errors", len(outcome.Errors))
	}
	return outcome
}

// RunAuditLogRepair re-reads the history of the activity feed and re-attributes
// the events a release before the actor kind mapped with a single actor slot. It
// is the body of POST /api/maintenance/auditlogs/renormalise; the same pass runs
// at the end of every collect (see AuditLogService.Collect).
func (s *Scheduler) RunAuditLogRepair(ctx context.Context) AuditLogRepairResult {
	return s.logs.RepairActors(ctx)
}

// AuditLogOrganisationNames answers the display name of every organisation the
// collector has resolved so far (see AuditLogService.OrganisationNames). The API
// joins it onto the feed cursors so the Audit Logs screen names a tenant instead
// of showing its id alone.
func (s *Scheduler) AuditLogOrganisationNames() map[string]string {
	return s.logs.OrganisationNames()
}

// NextRunAt computes the next execution of a job: now plus its interval for an
// enabled scheduled job and nil for a job that is disabled or manual only. The
// job handlers use it whenever an interval or the enabled flag changes.
func NextRunAt(now time.Time, job *models.SyncJob) *time.Time {
	if job == nil || !job.Enabled || job.IntervalMinutes <= 0 {
		return nil
	}
	next := now.Add(time.Duration(job.IntervalMinutes) * time.Minute)
	return &next
}
