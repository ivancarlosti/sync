package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// Bounds of a directory report.
const (
	// reportKeepCount is how many reports per account survive the pruner.
	reportKeepCount = 20
	// ReportTimeout bounds one pass. A directory of a hundred thousand users is
	// a few dozen pages, so the budget is generous; it exists so a throttled
	// tenant ends as `timeout` instead of holding the slot forever.
	ReportTimeout = 15 * time.Minute
	// ReportMinInterval and ReportMaxInterval bound the schedule interval, in
	// minutes: a quarter of an hour is the shortest sensible cadence for an
	// inventory nobody edits that fast, and 30 days the longest.
	ReportMinInterval = 15
	ReportMaxInterval = 43200
	// DefaultReportInterval is the interval of a newly enabled schedule (one
	// day): an inventory report is read weekly, not hourly.
	DefaultReportInterval = 1440
	// reportProgressEvery throttles the writes of the live counters.
	reportProgressEvery = 2 * time.Second
)

// ReportInput is the request that starts a directory report.
type ReportInput struct {
	AccountID uint
	// Sections is the list of sections to collect; empty means every section
	// the provider supports.
	Sections []string
	// Trigger is models.TriggerManual for an operator request and
	// models.TriggerScheduled for the schedule of the account.
	Trigger string
}

// ReportScheduleInput is the opt-in of one account.
type ReportScheduleInput struct {
	AccountID       uint
	Enabled         bool
	IntervalMinutes int
	Sections        []string
}

// ReportService executes directory reports and drives their schedules.
//
// A report is read-only: it lists the organisation behind an account (users,
// groups, sites, licences, domains) into stored tables. Like AuditService it is
// the async brother of SyncService: Start reserves the account, writes a
// `running` row and collects in the background, so the HTTP handler can answer
// 202 with an id the UI polls. One report per account runs at a time.
type ReportService struct {
	store  *Store
	tokens *TokenManager
	now    func() time.Time

	mu      sync.Mutex
	running map[uint]*reportState
	wg      sync.WaitGroup
}

// reportState is the in-memory handle of a report in flight.
type reportState struct {
	cancel context.CancelFunc
}

// NewReportService builds the service.
func NewReportService(store *Store, tokens *TokenManager) *ReportService {
	return &ReportService{
		store:   store,
		tokens:  tokens,
		now:     func() time.Time { return time.Now().UTC() },
		running: map[uint]*reportState{},
	}
}

// Start launches a report in the background and returns the row immediately. The
// collection is not bound to the request context: finishing the request must not
// abort it.
func (s *ReportService) Start(ctx context.Context, input ReportInput) (*models.DirectoryReport, error) {
	if input.AccountID == 0 {
		return nil, fmt.Errorf("%w: an account is required", ErrValidation)
	}
	trigger := strings.TrimSpace(input.Trigger)
	if trigger == "" {
		trigger = string(models.TriggerManual)
	}
	sections, err := providers.ReportSectionsFrom(input.Sections)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrValidation, err.Error())
	}

	if err := s.reserve(ctx, input.AccountID); err != nil {
		return nil, err
	}
	// Resolving the account before a row is written keeps a missing or
	// unreachable account a clean 404/412 instead of a failed report.
	session, err := s.tokens.Resolve(ctx, input.AccountID)
	if err != nil {
		s.release(input.AccountID)
		return nil, err
	}
	source := providers.ReportSourceOf(session.Provider)
	if source == nil {
		s.release(input.AccountID)
		return nil, fmt.Errorf("%w: %s cannot build directory reports", ErrValidation, session.Account.Provider)
	}
	sections = supportedSections(source, sections)
	if len(sections) == 0 {
		s.release(input.AccountID)
		return nil, fmt.Errorf("%w: %s supports none of the requested report sections", ErrValidation, session.Account.Provider)
	}

	// The pass context is created here, not inside the goroutine, so a Cancel
	// that races the very start of the report is never lost.
	runCtx, cancel := context.WithCancel(context.Background())
	s.attach(input.AccountID, cancel)
	run, err := s.createRun(ctx, session.Account, sections, trigger)
	if err != nil {
		s.release(input.AccountID)
		return nil, err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.release(input.AccountID)
		s.execute(runCtx, session, source, sections, run)
	}()
	return run, nil
}

// supportedSections keeps the requested sections the provider can actually
// collect, in the provider's order: a provider that gains a section later never
// has an older request silently stretched into it.
func supportedSections(source providers.ReportSource, requested []providers.ReportSection) []providers.ReportSection {
	wanted := make(map[providers.ReportSection]bool, len(requested))
	for _, section := range requested {
		wanted[section] = true
	}
	kept := make([]providers.ReportSection, 0, len(requested))
	for _, section := range source.ReportSections() {
		if wanted[section] {
			kept = append(kept, section)
		}
	}
	return kept
}

// Running reports whether an account has a report in flight.
func (s *ReportService) Running(accountID uint) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[accountID]
	return ok
}

// Cancel aborts the report of an account. The pass stops at the next checkpoint
// and the row is marked as cancelled.
func (s *ReportService) Cancel(accountID uint) bool {
	s.mu.Lock()
	state, ok := s.running[accountID]
	var cancel context.CancelFunc
	if ok && state != nil {
		cancel = state.cancel
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return ok
}

// Wait blocks until every background report finished; the server calls it during
// shutdown so a pass in flight is not left half written.
func (s *ReportService) Wait() { s.wg.Wait() }

// reserve marks an account as being reported, refusing a second concurrent pass
// and one left behind by a previous process.
func (s *ReportService) reserve(ctx context.Context, accountID uint) error {
	s.mu.Lock()
	if _, busy := s.running[accountID]; busy {
		s.mu.Unlock()
		return fmt.Errorf("%w: a report of this account is already running", ErrBusy)
	}
	s.running[accountID] = &reportState{}
	s.mu.Unlock()

	if _, err := s.store.RunningReport(ctx, accountID); err == nil {
		s.release(accountID)
		return fmt.Errorf("%w: a report of this account is still marked as running", ErrBusy)
	} else if !IsNotFound(err) {
		s.release(accountID)
		return err
	}
	return nil
}

// attach hands the cancel function of the pass context to the reservation.
func (s *ReportService) attach(accountID uint, cancel context.CancelFunc) {
	s.mu.Lock()
	if state, ok := s.running[accountID]; ok && state != nil {
		state.cancel = cancel
	}
	s.mu.Unlock()
}

// release forgets an account reservation and cancels a context still attached.
func (s *ReportService) release(accountID uint) {
	s.mu.Lock()
	state, ok := s.running[accountID]
	delete(s.running, accountID)
	s.mu.Unlock()
	if ok && state != nil && state.cancel != nil {
		state.cancel()
	}
}

// createRun inserts the report row in the running state.
func (s *ReportService) createRun(ctx context.Context, account *models.ConnectedAccount, sections []providers.ReportSection, trigger string) (*models.DirectoryReport, error) {
	run := &models.DirectoryReport{
		AccountID:    account.ID,
		AccountEmail: account.Email,
		Provider:     account.Provider,
		OrgID:        account.OrgID,
		Status:       string(models.RunRunning),
		Trigger:      trigger,
		StartedAt:    s.now(),
	}
	names := make([]string, 0, len(sections))
	for _, section := range sections {
		names = append(names, string(section))
	}
	run.SetSections(names)
	if err := s.store.CreateReport(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// execute collects the sections, stores the tables and closes the row. passCtx
// is the cancellable context created by Start (Cancel reaches it through the
// reservation) plus the report budget; the persistence below deliberately uses a
// fresh context so a cancelled pass still stores what it managed to collect.
func (s *ReportService) execute(passCtx context.Context, session ProviderSession, source providers.ReportSource, sections []providers.ReportSection, run *models.DirectoryReport) {
	ctx := context.Background()
	passCtx, cancel := context.WithTimeout(passCtx, ReportTimeout)
	defer cancel()

	slog.Info("directory report started",
		"report", run.ID, "account", run.AccountID, "sections", run.Sections, "trigger", run.Trigger)

	// The live counters are an approximation the polling UI reads: the section
	// index and the rows collected so far, written at most every
	// reportProgressEvery.
	var index, done, previous int
	current := providers.ReportSection("")
	lastWrite := s.now().Add(-reportProgressEvery)
	progress := func(event providers.ReportProgress) {
		if event.Section != current {
			current = event.Section
			index++
			done += previous
			previous = 0
		}
		previous = event.Rows
		now := s.now()
		if now.Sub(lastWrite) < reportProgressEvery {
			return
		}
		lastWrite = now
		if err := s.store.UpdateReportProgress(ctx, run.ID, index, done+event.Rows); err != nil {
			slog.Warn("updating the report progress failed", "report", run.ID, "error", err)
		}
	}

	tables, collectErr := source.CollectReport(passCtx, session.Credentials, session.Tokens, sections, progress)
	failed, truncated := 0, false
	rows := 0
	for _, table := range tables {
		if table.Failed() {
			failed++
		}
		if table.Truncated {
			truncated = true
		}
		rows += len(table.Rows)
	}
	if len(tables) > 0 {
		// A cancelled or timed-out pass still stores the sections it managed to
		// collect: they are exact, and the status says the pass did not finish.
		if err := s.storeTables(ctx, run.ID, tables); err != nil {
			slog.Error("storing the report sections failed", "report", run.ID, "error", err)
			if collectErr == nil {
				collectErr = err
			}
		}
	}

	finished := s.now()
	run.FinishedAt = &finished
	run.DurationMS = finished.Sub(run.StartedAt).Milliseconds()
	run.SectionCount = len(tables)
	run.RowCount = rows
	run.Failed = failed
	run.Truncated = truncated
	run.Status = string(reportStatus(passCtx, collectErr, failed))
	if run.Message == "" {
		run.Message = reportMessage(run.Status, failed)
	}
	if err := s.store.FinishReport(ctx, run); err != nil {
		slog.Error("closing the report failed", "report", run.ID, "error", err)
	}
	if err := s.store.PruneReports(ctx, reportKeepCount); err != nil {
		slog.Warn("pruning the report history failed", "error", err)
	}
	if collectErr != nil {
		slog.Error("directory report failed", "report", run.ID, "account", run.AccountID, "error", collectErr)
		return
	}
	slog.Info("directory report finished",
		"report", run.ID, "status", run.Status, "sections", run.SectionCount,
		"rows", run.RowCount, "failed", run.Failed, "truncated", run.Truncated)
}

// storeTables persists the collected tables as JSON documents.
func (s *ReportService) storeTables(ctx context.Context, reportID uint, tables []providers.ReportTable) error {
	records := make([]models.ReportSectionData, 0, len(tables))
	for _, table := range tables {
		columns, err := json.Marshal(table.Columns)
		if err != nil {
			return fmt.Errorf("services: encoding the columns of %s: %w", table.Section, err)
		}
		rows, err := json.Marshal(table.Rows)
		if err != nil {
			return fmt.Errorf("services: encoding the rows of %s: %w", table.Section, err)
		}
		records = append(records, models.ReportSectionData{
			ReportID:  reportID,
			Section:   string(table.Section),
			Columns:   string(columns),
			Rows:      string(rows),
			RowCount:  len(table.Rows),
			Truncated: table.Truncated,
			Error:     table.Error,
		})
	}
	return s.store.AddReportSections(ctx, records)
}

// reportStatus maps the outcome of a pass onto the lifecycle status of the row.
func reportStatus(passCtx context.Context, err error, failed int) models.RunStatus {
	switch {
	case err != nil && errors.Is(passCtx.Err(), context.DeadlineExceeded):
		return models.RunTimeout
	case err != nil && passCtx.Err() != nil:
		return models.RunCancelled
	case err != nil:
		return models.RunFailed
	case failed > 0:
		return models.RunPartial
	default:
		return models.RunSuccess
	}
}

// reportMessage is the human readable summary the history screen shows.
func reportMessage(status string, failed int) string {
	switch models.RunStatus(status) {
	case models.RunSuccess:
		return "completed"
	case models.RunPartial:
		return fmt.Sprintf("completed with %d unusable section(s)", failed)
	case models.RunCancelled:
		return "the report was cancelled"
	case models.RunTimeout:
		return "the report ran out of time"
	default:
		return "the report failed"
	}
}

// SaveSchedule validates and stores the per-account opt-in. A schedule that does
// not exist is what "do not report this account automatically" means, so the
// first call creates the row and every later one edits it in place.
func (s *ReportService) SaveSchedule(ctx context.Context, input ReportScheduleInput) (*models.ReportSchedule, error) {
	if input.AccountID == 0 {
		return nil, fmt.Errorf("%w: an account is required", ErrValidation)
	}
	sections, err := providers.ReportSectionsFrom(input.Sections)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrValidation, err.Error())
	}
	interval := input.IntervalMinutes
	if interval == 0 {
		interval = DefaultReportInterval
	}
	if interval < ReportMinInterval || interval > ReportMaxInterval {
		return nil, fmt.Errorf("%w: the interval must be between %d and %d minutes",
			ErrValidation, ReportMinInterval, ReportMaxInterval)
	}
	schedule := &models.ReportSchedule{
		AccountID:       input.AccountID,
		Enabled:         input.Enabled,
		IntervalMinutes: interval,
	}
	names := make([]string, 0, len(sections))
	for _, section := range sections {
		names = append(names, string(section))
	}
	schedule.SetSections(names)
	schedule.NextRunAt = s.nextRunAt(schedule, s.now())
	if err := s.store.SaveReportSchedule(ctx, schedule); err != nil {
		return nil, err
	}
	return schedule, nil
}

// ListSchedules returns every opt-in of the instance.
func (s *ReportService) ListSchedules(ctx context.Context) ([]models.ReportSchedule, error) {
	return s.store.ListReportSchedules(ctx)
}

// DeleteSchedule removes the opt-in of an account.
func (s *ReportService) DeleteSchedule(ctx context.Context, accountID uint) error {
	if accountID == 0 {
		return fmt.Errorf("%w: an account is required", ErrValidation)
	}
	return s.store.DeleteReportSchedule(ctx, accountID)
}

// RunDue starts every scheduled report whose slot elapsed and answers how many
// reports were launched. It is the body of the reports pass of the scheduler:
// the slot is pushed forward first, so a failing schedule retries once per
// interval instead of once per tick.
func (s *ReportService) RunDue(ctx context.Context) int {
	schedules, err := s.store.DueReportSchedules(ctx, s.now())
	if err != nil {
		slog.Error("listing the due report schedules failed", "error", err)
		return 0
	}
	started := 0
	for i := range schedules {
		schedule := schedules[i]
		if err := s.store.ScheduleReportNext(ctx, schedule.ID, s.nextRunAt(&schedule, s.now())); err != nil {
			slog.Warn("pushing the next report of a schedule failed", "schedule", schedule.ID, "error", err)
		}
		if !schedule.Enabled || s.Running(schedule.AccountID) {
			continue
		}
		report, err := s.Start(ctx, ReportInput{
			AccountID: schedule.AccountID,
			Sections:  schedule.SectionList(),
			Trigger:   string(models.TriggerScheduled),
		})
		if err != nil {
			slog.Error("starting a scheduled report failed", "account", schedule.AccountID, "error", err)
			if recordErr := s.store.RecordReportRun(ctx, schedule.AccountID, nil, s.now(), err.Error()); recordErr != nil {
				slog.Warn("recording the failed report run failed", "account", schedule.AccountID, "error", recordErr)
			}
			continue
		}
		started++
		if err := s.store.RecordReportRun(ctx, schedule.AccountID, &report.ID, s.now(), ""); err != nil {
			slog.Warn("recording the report run failed", "account", schedule.AccountID, "error", err)
		}
		slog.Info("scheduled directory report started", "account", schedule.AccountID, "report", report.ID)
	}
	return started
}

// nextRunAt computes the next slot of a schedule: now plus its interval for an
// enabled schedule and nil for a disabled one.
func (s *ReportService) nextRunAt(schedule *models.ReportSchedule, now time.Time) *time.Time {
	if schedule == nil || !schedule.Enabled || schedule.IntervalMinutes <= 0 {
		return nil
	}
	next := now.Add(time.Duration(schedule.IntervalMinutes) * time.Minute)
	return &next
}
