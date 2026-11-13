package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ivancarlosti/sync/internal/ical"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// Defaults of the calendar engine. The two window sizes bound what one run lists
// around "now": a month of history is what an agenda view shows, and a year ahead
// is what a subscription realistically carries.
const (
	calendarDefaultPastDays   = 30
	calendarDefaultFutureDays = 365
	// calendarRunItemLimit bounds the per-event evidence a single run stores: a
	// large calendar must not turn its history into the biggest table.
	calendarRunItemLimit = 1000
)

// CalendarService executes calendar jobs.
//
// It is the sibling of SyncService, not a mode of it: the file engine
// reconciles trees of items by path, this one reconciles events by iCalendar UID,
// and both share the same lifecycle (reserve, run row, timeout, cancel,
// retention) so the UI can present them side by side.
//
// A run lists the window both sides cover, pairs the events by UID using the
// recorded calendar_event_links rows and applies the plan. Nothing is written
// unless a fingerprint moved, which is what makes a run over an unchanged
// calendar cost two listings and no writes.
type CalendarService struct {
	store    *Store
	tokens   *TokenManager
	settings *SettingsService
	notifier *Notifier
	// box decrypts the feed password of an .ics subscription.
	box *SecretBox
	// allowPrivate is the operator policy for subscription URLs that are not
	// publicly routable (CALENDAR_ALLOW_PRIVATE_TARGETS).
	allowPrivate bool
	now          func() time.Time
	mu           sync.Mutex
	running      map[uint]*runState
	wg           sync.WaitGroup
}

// NewCalendarService builds the service. settings may be nil (the default run
// timeout is then used), notifier may be nil and box may be nil when no job uses
// an .ics feed.
func NewCalendarService(store *Store, tokens *TokenManager, settings *SettingsService, notifier *Notifier, box *SecretBox, allowPrivate bool) *CalendarService {
	return &CalendarService{
		store:        store,
		tokens:       tokens,
		settings:     settings,
		notifier:     notifier,
		box:          box,
		allowPrivate: allowPrivate,
		now:          func() time.Time { return time.Now().UTC() },
		running:      map[uint]*runState{},
	}
}

// Start launches a run in the background and returns its row, so the HTTP handler
// answers 202 with an id the UI can poll. The run is detached from the request
// context: finishing the request must not abort the run.
func (s *CalendarService) Start(ctx context.Context, jobID uint, trigger models.TriggerSource) (*models.CalendarRun, error) {
	job, err := s.store.GetCalendarJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if err := s.reserve(ctx, job); err != nil {
		return nil, err
	}
	run, err := s.createRun(ctx, job, trigger)
	if err != nil {
		s.release(job.ID)
		return nil, err
	}
	started := *run
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.release(job.ID)
		s.execute(context.Background(), job, run, trigger)
	}()
	return &started, nil
}

// Run executes a job synchronously and returns the finished run. Tests and the
// scheduler use it.
func (s *CalendarService) Run(ctx context.Context, job *models.CalendarJob, trigger models.TriggerSource) (*models.CalendarRun, error) {
	if err := s.reserve(ctx, job); err != nil {
		return nil, err
	}
	defer s.release(job.ID)
	run, err := s.createRun(ctx, job, trigger)
	if err != nil {
		return nil, err
	}
	s.execute(ctx, job, run, trigger)
	return run, nil
}

// StartIfIdle starts a job when nothing is running for it and returns nil when
// the job was already busy, so a slow run skips its slot instead of piling up.
func (s *CalendarService) StartIfIdle(ctx context.Context, jobID uint, trigger models.TriggerSource) (*models.CalendarRun, error) {
	run, err := s.Start(ctx, jobID, trigger)
	if errors.Is(err, ErrBusy) {
		slog.Debug("calendar job is already running, skipping this slot", "job", jobID)
		return nil, nil
	}
	return run, err
}

// Running reports whether a job has a run in flight.
func (s *CalendarService) Running(jobID uint) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[jobID]
	return ok
}

// RunningJobs returns the ids of the calendar jobs currently running.
func (s *CalendarService) RunningJobs() []uint {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]uint, 0, len(s.running))
	for id := range s.running {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Cancel aborts the run of a job; the engine stops at the next event.
func (s *CalendarService) Cancel(jobID uint) bool {
	s.mu.Lock()
	state, ok := s.running[jobID]
	var cancel context.CancelFunc
	if ok && state != nil {
		if state.cancel != nil {
			cancel = state.cancel
		} else {
			state.cancelled = true
		}
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return ok
}

// Wait blocks until every background run finished; the server calls it during
// shutdown.
func (s *CalendarService) Wait() {
	s.wg.Wait()
}

// reserve marks a job as running, refusing a second concurrent run and one left
// behind by a previous process.
func (s *CalendarService) reserve(ctx context.Context, job *models.CalendarJob) error {
	if job == nil {
		return fmt.Errorf("%w: no calendar job given", ErrValidation)
	}
	if !job.Enabled {
		return fmt.Errorf("%w: the calendar job is disabled", ErrValidation)
	}
	s.mu.Lock()
	if _, busy := s.running[job.ID]; busy {
		s.mu.Unlock()
		return fmt.Errorf("%w: the calendar job is already running", ErrBusy)
	}
	s.running[job.ID] = &runState{}
	s.mu.Unlock()

	if _, err := s.store.RunningCalendarRun(ctx, job.ID); err == nil {
		s.release(job.ID)
		return fmt.Errorf("%w: a run of this job is still marked as running", ErrBusy)
	} else if !IsNotFound(err) {
		s.release(job.ID)
		return err
	}
	return nil
}

// attach hands the cancel function of the run context to the reservation.
func (s *CalendarService) attach(jobID uint, cancel context.CancelFunc) {
	s.mu.Lock()
	if state, ok := s.running[jobID]; ok && state != nil {
		state.cancel = cancel
		if state.cancelled {
			cancel()
		}
	}
	s.mu.Unlock()
}

// release forgets a job reservation and cancels a context still attached to it.
func (s *CalendarService) release(jobID uint) {
	s.mu.Lock()
	state, ok := s.running[jobID]
	delete(s.running, jobID)
	s.mu.Unlock()
	if ok && state != nil && state.cancel != nil {
		state.cancel()
	}
}

// createRun inserts the run row in the running state.
func (s *CalendarService) createRun(ctx context.Context, job *models.CalendarJob, trigger models.TriggerSource) (*models.CalendarRun, error) {
	run := &models.CalendarRun{
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    string(models.RunRunning),
		Trigger:   string(trigger),
		StartedAt: s.now(),
	}
	if err := s.store.CreateCalendarRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// execute is the body of a run: open the engine, reconcile, store the outcome.
func (s *CalendarService) execute(ctx context.Context, job *models.CalendarJob, run *models.CalendarRun, trigger models.TriggerSource) {
	timeout := s.runTimeout(ctx)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s.attach(job.ID, cancel)

	slog.Info("calendar run started",
		"job", job.ID, "name", job.Name, "run", run.ID, "trigger", trigger, "timeout", timeout)

	engine := &calendarEngine{
		service: s,
		job:     job,
		run:     run,
		links:   map[string]models.CalendarEventLink{},
		pending: map[string]models.CalendarEventLink{},
	}
	if err := engine.reconcile(runCtx); err != nil {
		if run.Message == "" {
			run.Message = err.Error()
		}
	}
	if len(engine.items) > 0 {
		if err := s.store.AddCalendarRunItems(ctx, engine.items); err != nil {
			slog.Error("storing the calendar run items failed", "job", job.ID, "run", run.ID, "error", err)
		}
	}

	finished := s.now()
	run.FinishedAt = &finished
	run.DurationMS = finished.Sub(run.StartedAt).Milliseconds()
	run.Status = string(engine.status(runCtx))
	if run.Message == "" && run.Status == string(models.RunSuccess) {
		run.Message = "completed"
	}
	if err := s.store.FinishCalendarRun(ctx, run, job); err != nil {
		slog.Error("closing the calendar run failed", "job", job.ID, "run", run.ID, "error", err)
	}
	if _, err := s.store.PruneCalendarRuns(ctx, runKeepCount); err != nil {
		slog.Warn("pruning the calendar run history failed", "job", job.ID, "error", err)
	}
	if engine.fatal != nil {
		slog.Error("calendar run failed",
			"job", job.ID, "name", job.Name, "run", run.ID, "error", engine.fatal)
		return
	}
	slog.Info("calendar run finished",
		"job", job.ID, "name", job.Name, "run", run.ID, "status", run.Status,
		"scanned", run.EventsScanned, "created", run.EventsCreated, "updated", run.EventsUpdated,
		"deleted", run.EventsDeleted, "skipped", run.EventsSkipped, "conflicts", run.Conflicts,
		"errors", run.Errors, "duration_ms", run.DurationMS)
}

// runTimeout returns the configured run timeout, falling back to the default.
func (s *CalendarService) runTimeout(ctx context.Context) time.Duration {
	minutes := DefaultRunTimeout
	if s.settings != nil {
		minutes = s.settings.RunTimeout(ctx)
	}
	if minutes <= 0 {
		minutes = DefaultRunTimeout
	}
	return time.Duration(minutes) * time.Minute
}

// ListCalendars returns the calendars of an account, for the destination and
// source pickers. A provider without calendar support is reported as a
// validation problem the UI can explain instead of a 500.
func (s *CalendarService) ListCalendars(ctx context.Context, accountID uint) ([]providers.Calendar, error) {
	session, err := s.tokens.Resolve(ctx, accountID)
	if err != nil {
		return nil, err
	}
	calendars, ok := session.Provider.(providers.CalendarProvider)
	if !ok {
		return nil, fmt.Errorf("%w: %s does not support calendars", ErrValidation, session.Provider.Name())
	}
	return calendars.Calendars(ctx, session.Credentials, session.Tokens)
}

// CalendarPreview is the dry-run answer of the API: the plan a run would apply,
// without writing anything on either side.
type CalendarPreview struct {
	JobID         uint                      `json:"job_id"`
	WindowStart   time.Time                 `json:"window_start"`
	WindowEnd     time.Time                 `json:"window_end"`
	SourceListed  int                       `json:"source_listed"`
	DestListed    int                       `json:"destination_listed"`
	EventsScanned int                       `json:"events_scanned"`
	Plan          []CalendarPreviewDecision `json:"plan"`
}

// CalendarPreviewDecision is one planned action and the evidence for it.
type CalendarPreviewDecision struct {
	UID      string    `json:"uid"`
	Summary  string    `json:"summary"`
	StartAt  time.Time `json:"start_at"`
	Action   string    `json:"action"`
	Evidence string    `json:"evidence"`
}

// Preview lists both sides and returns what a run would do. It holds no
// reservation and writes nothing, so it is safe to call from the editor.
func (s *CalendarService) Preview(ctx context.Context, job *models.CalendarJob) (*CalendarPreview, error) {
	engine := &calendarEngine{
		service: s,
		job:     job,
		run:     &models.CalendarRun{JobID: job.ID, JobName: job.Name},
		links:   map[string]models.CalendarEventLink{},
		pending: map[string]models.CalendarEventLink{},
		dryRun:  true,
	}
	ctx, cancel := context.WithTimeout(ctx, s.runTimeout(ctx))
	defer cancel()
	if err := engine.reconcile(ctx); err != nil {
		return nil, err
	}
	preview := &CalendarPreview{
		JobID:         job.ID,
		WindowStart:   engine.window.Start,
		WindowEnd:     engine.window.End,
		EventsScanned: engine.run.EventsScanned,
		Plan:          make([]CalendarPreviewDecision, 0, len(engine.planned)),
	}
	if engine.source != nil {
		preview.SourceListed = len(engine.source.events)
	}
	if engine.target != nil {
		preview.DestListed = len(engine.target.events)
	}
	for _, decision := range engine.planned {
		preview.Plan = append(preview.Plan, CalendarPreviewDecision{
			UID:      decision.uid,
			Summary:  decision.summary,
			StartAt:  decision.start,
			Action:   string(decision.action),
			Evidence: decision.evidence,
		})
	}
	return preview, nil
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// calendarSide is one side of a job resolved for a run: an account-backed
// calendar or an .ics subscription, plus the events it listed.
type calendarSide struct {
	role     string
	kind     models.CalendarSourceKind
	base     providers.Provider
	provider providers.CalendarProvider
	account  *models.ConnectedAccount
	creds    providers.Credentials
	tokens   *providers.Tokens
	calendar string
	name     string
	feed     ical.Subscription
	// fetched remembers the validators of the download that produced events, so
	// a successful run can store them for the next conditional request.
	fetched  ical.Result
	hasFetch bool
	events   map[string]providers.CalendarEvent
}

// label names the side in evidence strings.
func (s *calendarSide) label() string {
	if s.kind == models.CalendarSourceICS {
		return "the subscription"
	}
	if s.name != "" {
		return s.name
	}
	return s.calendar
}

// writable reports whether the engine may write to the side. An .ics
// subscription is read-only by nature.
func (s *calendarSide) writable() bool {
	return s.provider != nil
}

// calendarEngine is one execution of a calendar job. It is single-use.
type calendarEngine struct {
	service *CalendarService
	job     *models.CalendarJob
	run     *models.CalendarRun
	source  *calendarSide
	target  *calendarSide
	links   map[string]models.CalendarEventLink
	window  providers.CalendarWindow
	items   []models.CalendarRunItem
	planned []plannedDecision
	// pending carries the link rows the run produced, keyed by UID so one event
	// touched twice still answers one row; forgotten carries the UIDs whose row
	// must disappear.
	pending   map[string]models.CalendarEventLink
	forgotten []string
	fatal     error
	// dryRun makes the engine list and plan without writing: it is how the
	// preview endpoint answers with the same code path the run uses.
	dryRun bool
}

// plannedDecision is a decision worth reporting: a write the run made (or would
// make) or a pairing it refused to act on.
type plannedDecision struct {
	uid      string
	summary  string
	start    time.Time
	action   calendarAction
	evidence string
}

// fail records the first fatal problem of a run and returns it.
func (e *calendarEngine) fail(err error) error {
	if err == nil {
		return nil
	}
	if e.fatal == nil {
		e.fatal = err
		e.run.Message = err.Error()
	}
	return err
}

// status classifies the run the way the sync engine does, so both families share
// the status vocabulary of the UI.
func (e *calendarEngine) status(ctx context.Context) models.RunStatus {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return models.RunTimeout
	case errors.Is(ctx.Err(), context.Canceled):
		return models.RunCancelled
	case e.fatal != nil:
		return models.RunFailed
	case e.run.Errors > 0:
		return models.RunPartial
	default:
		return models.RunSuccess
	}
}

// calendarWindow computes the listing range of a job around now.
func calendarWindow(now time.Time, job *models.CalendarJob) providers.CalendarWindow {
	past := job.PastDays
	if past <= 0 {
		past = calendarDefaultPastDays
	}
	future := job.FutureDays
	if future <= 0 {
		future = calendarDefaultFutureDays
	}
	return providers.CalendarWindow{
		Start: now.AddDate(0, 0, -past),
		End:   now.AddDate(0, 0, future),
	}
}

// reconcile is the body of a run: resolve both sides, list both windows, pair the
// events, apply the plan and record the outcome.
func (e *calendarEngine) reconcile(ctx context.Context) error {
	e.window = calendarWindow(e.service.now(), e.job)

	target, err := e.resolveTarget(ctx)
	if err != nil {
		return e.fail(err)
	}
	source, err := e.resolveSource(ctx, target)
	if err != nil {
		return e.fail(err)
	}
	links, err := e.service.store.CalendarLinks(ctx, e.job.ID)
	if err != nil {
		return e.fail(err)
	}
	e.links = links

	if err := e.list(ctx, target); err != nil {
		return e.fail(err)
	}
	if err := e.list(ctx, source); err != nil {
		return e.fail(err)
	}
	if err := e.reconcileEvents(ctx); err != nil {
		return e.fail(err)
	}
	if e.dryRun {
		return nil
	}
	if err := e.persist(ctx); err != nil {
		return e.fail(err)
	}
	now := e.service.now()
	for _, side := range []*calendarSide{source, target} {
		if side.account == nil {
			continue
		}
		if err := e.service.store.TouchAccount(ctx, side.account.ID, now); err != nil {
			slog.Warn("recording the account usage failed", "account", side.account.ID, "error", err)
		}
	}
	return nil
}

// resolveTarget loads the destination: always an account-backed, writable
// calendar.
func (e *calendarEngine) resolveTarget(ctx context.Context) (*calendarSide, error) {
	session, err := e.service.tokens.Resolve(ctx, e.job.DestinationAccountID)
	if err != nil {
		return nil, err
	}
	calendars, ok := session.Provider.(providers.CalendarProvider)
	if !ok {
		return nil, fmt.Errorf("%w: %s cannot write calendars", ErrValidation, session.Provider.Name())
	}
	side := &calendarSide{
		role:     "destination",
		kind:     models.CalendarSourceKind(session.Provider.Name()),
		base:     session.Provider,
		provider: calendars,
		account:  session.Account,
		creds:    session.Credentials,
		tokens:   session.Tokens,
		calendar: strings.TrimSpace(e.job.DestinationCalendarID),
		name:     e.job.DestinationCalendarName,
		events:   map[string]providers.CalendarEvent{},
	}
	if side.calendar == "" {
		if err := e.ensureCalendar(ctx, side); err != nil {
			return nil, err
		}
	}
	e.target = side
	return side, nil
}

// resolveSource loads the source side. An .ics subscription needs no account; a
// provider source needs one whose calendar support is present in this build.
func (e *calendarEngine) resolveSource(ctx context.Context, target *calendarSide) (*calendarSide, error) {
	kind := models.CalendarSourceKind(strings.TrimSpace(e.job.SourceKind))
	if !kind.Valid() {
		kind = models.CalendarSourceGoogle
	}
	side := &calendarSide{
		role:     "source",
		kind:     kind,
		calendar: strings.TrimSpace(e.job.SourceCalendarID),
		name:     e.job.SourceCalendarName,
		events:   map[string]providers.CalendarEvent{},
	}
	if kind == models.CalendarSourceICS {
		side.feed = ical.Subscription{
			URL:          strings.TrimSpace(e.job.SourceURL),
			Username:     strings.TrimSpace(e.job.SourceUsername),
			ETag:         e.job.SourceETag,
			LastModified: e.job.SourceLastModified,
			AllowPrivate: e.service.allowPrivate,
		}
		if e.job.SourceSecret != "" {
			if e.service.box == nil {
				return nil, fmt.Errorf("%w: the feed password cannot be decrypted in this build", ErrValidation)
			}
			password, err := e.service.box.Decrypt(e.job.SourceSecret)
			if err != nil {
				return nil, fmt.Errorf("%w: the feed password cannot be decrypted", ErrValidation)
			}
			side.feed.Password = password
		}
		e.source = side
		return side, nil
	}

	session, err := e.service.tokens.Resolve(ctx, e.job.SourceAccountID)
	if err != nil {
		return nil, err
	}
	calendars, ok := session.Provider.(providers.CalendarProvider)
	if !ok {
		return nil, fmt.Errorf("%w: %s cannot read calendars", ErrValidation, session.Provider.Name())
	}
	side.base = session.Provider
	side.provider = calendars
	side.account = session.Account
	side.creds = session.Credentials
	side.tokens = session.Tokens
	if side.calendar == "" {
		if err := e.ensureCalendar(ctx, side); err != nil {
			return nil, err
		}
	}
	e.source = side
	return side, nil
}

// ensureCalendar fills an empty calendar selection with the account's primary
// calendar, which is what a job saved without a picker selection means.
func (e *calendarEngine) ensureCalendar(ctx context.Context, side *calendarSide) error {
	var calendars []providers.Calendar
	err := e.retry(ctx, side, func() error {
		var callErr error
		calendars, callErr = side.provider.Calendars(ctx, side.creds, side.tokens)
		return callErr
	})
	if err != nil {
		return err
	}
	if len(calendars) == 0 {
		return fmt.Errorf("%w: the account has no calendar", ErrValidation)
	}
	chosen := calendars[0]
	for _, calendar := range calendars {
		if calendar.Primary {
			chosen = calendar
			break
		}
	}
	side.calendar = chosen.ID
	side.name = chosen.Name
	return nil
}

// retry runs a provider call and repeats it once after renewing the access token
// when the provider rejected it as unauthorized. A renewal that fails is the
// token manager's ErrReconnect, which ends the run with the message the UI needs.
func (e *calendarEngine) retry(ctx context.Context, side *calendarSide, call func() error) error {
	err := call()
	if err == nil || side == nil || side.base == nil || side.account == nil {
		return err
	}
	if !side.base.IsUnauthorized(err) {
		return err
	}
	if renewErr := e.reissue(ctx, side); renewErr != nil {
		return renewErr
	}
	return call()
}

// reissue renews the token set of a side in place.
func (e *calendarEngine) reissue(ctx context.Context, side *calendarSide) error {
	if side == nil || side.account == nil {
		return fmt.Errorf("%w: the side has no account", ErrValidation)
	}
	renewed, err := e.service.tokens.Reissue(ctx, side.account.ID)
	if err != nil {
		return err
	}
	side.tokens = renewed
	return nil
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

// list fills the event map of a side.
func (e *calendarEngine) list(ctx context.Context, side *calendarSide) error {
	if side.kind == models.CalendarSourceICS {
		return e.listFeed(ctx, side)
	}
	var events []providers.CalendarEvent
	err := e.retry(ctx, side, func() error {
		var callErr error
		events, callErr = side.provider.CalendarEvents(ctx, side.creds, side.tokens, side.calendar, e.window)
		return callErr
	})
	if err != nil {
		return fmt.Errorf("%w: cannot list %s", err, side.label())
	}
	side.events = indexEvents(events)
	return nil
}

// listFeed downloads and parses the subscription.
//
// A 304 is only the end of the work when the destination still matches what the
// last run recorded: an event the operator deleted there needs its source content
// to be recreated, and a conditional answer carries none. In that case the feed is
// fetched once more without validators, which keeps the cheap path cheap and the
// repair path correct.
func (e *calendarEngine) listFeed(ctx context.Context, side *calendarSide) error {
	result, err := ical.Fetch(ctx, side.feed)
	if err != nil {
		return err
	}
	if result.NotModified && !e.feedDrifted() {
		side.events = map[string]providers.CalendarEvent{}
		if e.run.Message == "" {
			e.run.Message = "the subscription has not changed since the last fetch"
		}
		return nil
	}
	if result.NotModified {
		side.feed.ETag = ""
		side.feed.LastModified = ""
		result, err = ical.Fetch(ctx, side.feed)
		if err != nil {
			return err
		}
	}
	events, err := ical.Parse(result.Body, ical.Options{
		DefaultTimeZone: e.defaultFeedZone(),
		WindowStart:     e.window.Start,
		WindowEnd:       e.window.End,
	})
	if err != nil {
		return err
	}
	side.events = make(map[string]providers.CalendarEvent, len(events))
	for _, event := range events {
		side.events[event.UID] = providers.CalendarEvent{
			ID:          event.UID,
			UID:         event.UID,
			Summary:     event.Summary,
			Description: event.Description,
			Location:    event.Location,
			Start:       event.Start,
			End:         event.End,
			AllDay:      event.AllDay,
			TimeZone:    event.TimeZone,
			UpdatedAt:   event.UpdatedAt,
			Recurring:   event.Recurring,
			Cancelled:   event.Cancelled,
			Organizer:   event.Organizer,
			Attendees:   event.Attendees,
		}
	}
	side.fetched = result
	side.hasFetch = true
	return nil
}

// defaultFeedZone is the zone a floating time of the feed is read in: the zone of
// the destination calendar, because that is where the events are being written.
func (e *calendarEngine) defaultFeedZone() string {
	if e.target == nil {
		return ""
	}
	return e.target.calendarZone()
}

// feedDrifted reports whether the destination still matches the recorded state. A
// link whose destination copy is missing although its last known start is inside
// the window, or whose content changed, is drift the run must look at.
func (e *calendarEngine) feedDrifted() bool {
	if e.target == nil {
		return true
	}
	for uid, link := range e.links {
		event, listed := e.target.events[uid]
		if !listed {
			if withinWindow(link.DestinationStart, e.window) {
				return true
			}
			continue
		}
		if providers.EventFingerprint(event) != link.DestFingerprint {
			return true
		}
	}
	return false
}

// indexEvents keys a listing by the UID the engine pairs on.
func indexEvents(events []providers.CalendarEvent) map[string]providers.CalendarEvent {
	indexed := make(map[string]providers.CalendarEvent, len(events))
	for _, event := range events {
		uid := eventUID(event)
		if uid == "" {
			continue
		}
		event.UID = uid
		indexed[uid] = event
	}
	return indexed
}

// eventUID is the pairing key of a listed event. Both providers publish an
// iCalUID; the fallback keeps an event whose payload lacks one pairable instead of
// silently dropping it.
func eventUID(event providers.CalendarEvent) string {
	if uid := strings.TrimSpace(event.UID); uid != "" {
		return uid
	}
	if id := strings.TrimSpace(event.ID); id != "" {
		return "id:" + id
	}
	return ""
}

// calendarZone answers the zone an all-day write should use: the zone the
// calendar is configured with, when the engine learned it, and UTC otherwise.
func (s *calendarSide) calendarZone() string {
	for _, event := range s.events {
		if event.AllDay && event.TimeZone != "" {
			return event.TimeZone
		}
	}
	return "UTC"
}

// withinWindow reports whether a recorded start falls inside the listing range.
func withinWindow(start time.Time, window providers.CalendarWindow) bool {
	if start.IsZero() {
		// An unknown start cannot prove the event is out of range, so it is
		// treated as in range: that makes the engine look at the event rather
		// than silently ignore it.
		return true
	}
	return !start.Before(window.Start) && start.Before(window.End)
}

// ---------------------------------------------------------------------------
// Pairing and writes
// ---------------------------------------------------------------------------

// reconcileEvents walks the UIDs either side lists or the engine already knows,
// decides one pairing per UID and applies it.
func (e *calendarEngine) reconcileEvents(ctx context.Context) error {
	for _, uid := range e.unionUIDs() {
		if ctx.Err() != nil {
			// A cancelled or timed-out run keeps the work it already did: the
			// loop stops, the outcome is written and the status says why.
			return nil
		}
		e.one(ctx, uid)
	}
	return nil
}

// unionUIDs merges the two listings with the recorded state, sorted so a run is
// reproducible and the activity feed reads in a stable order.
func (e *calendarEngine) unionUIDs() []string {
	seen := make(map[string]struct{}, len(e.source.events)+len(e.target.events)+len(e.links))
	for uid := range e.source.events {
		seen[uid] = struct{}{}
	}
	for uid := range e.target.events {
		seen[uid] = struct{}{}
	}
	for uid := range e.links {
		seen[uid] = struct{}{}
	}
	uids := make([]string, 0, len(seen))
	for uid := range seen {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// one pairs a single event.
func (e *calendarEngine) one(ctx context.Context, uid string) {
	e.run.EventsScanned++

	in := planInput{
		Backward:      e.direction().Backward() && e.source.writable(),
		Policy:        e.policy(),
		DeleteMissing: e.job.DeleteMissing,
	}
	if row, ok := e.links[uid]; ok {
		link := row
		in.Link = &link
		in.SourceInWindow = withinWindow(link.SourceStart, e.window)
		in.DestInWindow = withinWindow(link.DestinationStart, e.window)
	}
	if event, ok := e.source.events[uid]; ok {
		in.Source = &event
		in.SourceFingerprint = providers.EventFingerprint(event)
	}
	if event, ok := e.target.events[uid]; ok {
		in.Destination = &event
		in.DestFingerprint = providers.EventFingerprint(event)
	}

	// A series is never copied: one occurrence cannot be written to the other
	// side without inventing a rule, and copying the master would duplicate the
	// whole series next to the original.
	if (in.Source != nil && in.Source.Recurring) || (in.Destination != nil && in.Destination.Recurring) {
		e.run.EventsSkipped++
		e.record(uid, in.summary(), in.start(), actionSkip, "the event belongs to a recurring series, which the engine does not copy")
		return
	}
	if in.Source != nil && in.Source.Cancelled {
		e.run.EventsSkipped++
		e.record(uid, in.summary(), in.start(), actionSkip, "the source marks the event as cancelled")
		return
	}
	if in.Destination != nil && in.Destination.Cancelled {
		e.run.EventsSkipped++
		e.record(uid, in.summary(), in.start(), actionSkip, "the destination marks the event as cancelled")
		return
	}

	e.apply(ctx, uid, in, decide(in))
}

// apply carries out one decision.
func (e *calendarEngine) apply(ctx context.Context, uid string, in planInput, decision planDecision) {
	if e.dryRun {
		e.simulate(uid, in, decision)
		return
	}
	switch decision.Action {
	case actionUnchanged:
		e.remember(uid, in, nil, "", decision.Action)
	case actionSkip, actionConflict:
		if decision.Action == actionConflict {
			e.run.Conflicts++
		}
		e.record(uid, in.summary(), in.start(), decision.Action, decision.Evidence)
	case actionForgetLink:
		e.forget(uid)
	case actionCreateDest:
		e.createAt(ctx, uid, in, e.target, *in.Source, decision)
	case actionUpdateDest:
		e.updateAt(ctx, uid, in, e.target, in.Destination.ID, *in.Source, decision)
	case actionCreateSource:
		e.createAt(ctx, uid, in, e.source, *in.Destination, decision)
	case actionUpdateSource:
		e.updateAt(ctx, uid, in, e.source, in.Source.ID, *in.Destination, decision)
	case actionDeleteDest:
		e.remove(ctx, uid, in, e.target, in.Destination.ID, decision)
	case actionDeleteSource:
		e.remove(ctx, uid, in, e.source, in.Source.ID, decision)
	}
}

// simulate counts and reports a decision without writing anything. It is the body
// of the preview endpoint, which is why the preview cannot disagree with a run.
func (e *calendarEngine) simulate(uid string, in planInput, decision planDecision) {
	switch decision.Action {
	case actionUnchanged:
		return
	case actionCreateDest, actionCreateSource:
		e.run.EventsCreated++
	case actionUpdateDest, actionUpdateSource:
		e.run.EventsUpdated++
	case actionDeleteDest, actionDeleteSource:
		e.run.EventsDeleted++
	case actionSkip:
		e.run.EventsSkipped++
	case actionConflict:
		e.run.Conflicts++
	}
	e.record(uid, in.summary(), in.start(), decision.Action, decision.Evidence)
}

// direction is the job direction, defaulting to the bidirectional value the model
// documents.
func (e *calendarEngine) direction() models.CalendarDirection {
	direction := models.CalendarDirection(strings.TrimSpace(e.job.Direction))
	if !direction.Valid() {
		return models.CalendarDirectionBidirectional
	}
	return direction
}

// policy is the conflict policy, defaulting to newest wins.
func (e *calendarEngine) policy() models.ConflictPolicy {
	policy := models.ConflictPolicy(strings.TrimSpace(e.job.ConflictPolicy))
	if policy == "" {
		return models.ConflictNewestWins
	}
	return policy
}

// writeResult is what one provider write answered.
type writeResult struct {
	event       providers.CalendarEvent
	fingerprint string
}

// write creates (eventID empty) or updates an event on a side.
func (e *calendarEngine) write(ctx context.Context, side *calendarSide, input providers.CalendarEventInput, eventID string) (writeResult, error) {
	if !side.writable() {
		return writeResult{}, fmt.Errorf("%w: %s is read-only", ErrValidation, side.label())
	}
	var saved *providers.CalendarEvent
	err := e.retry(ctx, side, func() error {
		var callErr error
		if eventID == "" {
			saved, callErr = side.provider.CreateCalendarEvent(ctx, side.creds, side.tokens, side.calendar, input)
			return callErr
		}
		saved, callErr = side.provider.UpdateCalendarEvent(ctx, side.creds, side.tokens, side.calendar, eventID, input)
		return callErr
	})
	if err != nil {
		return writeResult{}, err
	}
	// The created/updated event is preferred over the input fingerprint: a
	// provider that normalises a value would otherwise look like an edit on the
	// next run and be written again forever.
	result := writeResult{fingerprint: providers.InputFingerprint(input)}
	if saved != nil {
		result.event = *saved
		if !saved.Start.IsZero() {
			result.fingerprint = providers.EventFingerprint(*saved)
		}
	}
	return result, nil
}

// createAt writes a new event on side from an event of the other side.
func (e *calendarEngine) createAt(ctx context.Context, uid string, in planInput, side *calendarSide, from providers.CalendarEvent, decision planDecision) {
	input := providers.EventInputFrom(from)
	input.UID = destinationUID(uid)
	written, err := e.write(ctx, side, input, "")
	if err != nil {
		e.failed(uid, in, decision, err)
		return
	}
	e.run.EventsCreated++
	e.remember(uid, in, &written, side.role, decision.Action)
	e.record(uid, in.summary(), writtenStart(written, from.Start), decision.Action, decision.Evidence)
}

// updateAt rewrites an existing event on side from an event of the other side.
func (e *calendarEngine) updateAt(ctx context.Context, uid string, in planInput, side *calendarSide, eventID string, from providers.CalendarEvent, decision planDecision) {
	if strings.TrimSpace(eventID) == "" {
		e.failed(uid, in, decision, errors.New("the event has no provider id to update"))
		return
	}
	input := providers.EventInputFrom(from)
	input.UID = destinationUID(uid)
	written, err := e.write(ctx, side, input, eventID)
	if err != nil {
		e.failed(uid, in, decision, err)
		return
	}
	e.run.EventsUpdated++
	e.remember(uid, in, &written, side.role, decision.Action)
	e.record(uid, in.summary(), writtenStart(written, from.Start), decision.Action, decision.Evidence)
}

// remove deletes an event on side. A provider that answers "not found" is the
// deletion the run wanted, so it is not reported as an error.
func (e *calendarEngine) remove(ctx context.Context, uid string, in planInput, side *calendarSide, eventID string, decision planDecision) {
	if !side.writable() {
		e.failed(uid, in, decision, fmt.Errorf("%w: %s is read-only", ErrValidation, side.label()))
		return
	}
	if strings.TrimSpace(eventID) == "" {
		e.forget(uid)
		e.run.EventsDeleted++
		e.record(uid, in.summary(), in.start(), decision.Action, decision.Evidence)
		return
	}
	err := e.retry(ctx, side, func() error {
		return side.provider.DeleteCalendarEvent(ctx, side.creds, side.tokens, side.calendar, eventID)
	})
	if err != nil && !errors.Is(err, providers.ErrNotFound) {
		e.failed(uid, in, decision, err)
		return
	}
	e.run.EventsDeleted++
	e.forget(uid)
	e.record(uid, in.summary(), in.start(), decision.Action, decision.Evidence)
}

// failed counts one event the engine could not write and keeps its evidence.
func (e *calendarEngine) failed(uid string, in planInput, decision planDecision, err error) {
	e.run.Errors++
	e.record(uid, in.summary(), in.start(), decision.Action, "failed: "+err.Error())
	if e.run.Message == "" && (errors.Is(err, ErrReconnect) || IsConsentRequired(err) || errors.Is(err, ErrValidation)) {
		e.run.Message = err.Error()
	}
	slog.Warn("a calendar event could not be written",
		"job", e.job.ID, "run", e.run.ID, "uid", uid, "action", decision.Action, "error", err)
}

// remember records the state of a pairing after a decision, so the next run can
// tell which side moved. A side that was written keeps the fingerprint of what the
// provider answered; the other keeps the fingerprint of what the run listed.
func (e *calendarEngine) remember(uid string, in planInput, written *writeResult, writtenOn string, action calendarAction) {
	if in.Source == nil && in.Destination == nil {
		return
	}
	row, ok := e.pending[uid]
	if !ok {
		if in.Link != nil {
			row = *in.Link
		} else {
			row = models.CalendarEventLink{JobID: e.job.ID}
		}
	}
	row.JobID = e.job.ID
	row.UID = uid
	if in.Source != nil {
		row.SourceEventID = in.Source.ID
		row.SourceFingerprint = in.SourceFingerprint
		row.SourceStart = in.Source.Start.UTC()
	}
	if in.Destination != nil {
		row.DestinationEventID = in.Destination.ID
		row.DestFingerprint = in.DestFingerprint
		row.DestinationStart = in.Destination.Start.UTC()
	}
	if written != nil {
		start := writtenStart(*written, time.Time{})
		switch writtenOn {
		case "source":
			if written.event.ID != "" {
				row.SourceEventID = written.event.ID
			}
			row.SourceFingerprint = written.fingerprint
			if !start.IsZero() {
				row.SourceStart = start.UTC()
			}
		case "destination":
			if written.event.ID != "" {
				row.DestinationEventID = written.event.ID
			}
			row.DestFingerprint = written.fingerprint
			if !start.IsZero() {
				row.DestinationStart = start.UTC()
			}
		}
	}
	if summary := in.summary(); summary != "" {
		row.Summary = summary
	}
	row.LastAction = string(action)
	row.LastSyncedAt = e.service.now()
	e.pending[uid] = row
}

// forget drops the recorded state of an event that is gone from both sides.
func (e *calendarEngine) forget(uid string) {
	delete(e.pending, uid)
	delete(e.links, uid)
	for _, forgotten := range e.forgotten {
		if forgotten == uid {
			return
		}
	}
	e.forgotten = append(e.forgotten, uid)
}

// record keeps one decision as run evidence.
func (e *calendarEngine) record(uid, summary string, start time.Time, action calendarAction, evidence string) {
	if e.dryRun {
		e.planned = append(e.planned, plannedDecision{
			uid: uid, summary: summary, start: start, action: action, evidence: evidence,
		})
		return
	}
	if len(e.items) >= calendarRunItemLimit {
		return
	}
	e.items = append(e.items, models.CalendarRunItem{
		RunID:    e.run.ID,
		JobID:    e.job.ID,
		Action:   string(action),
		UID:      uid,
		Summary:  summary,
		StartAt:  start,
		Evidence: evidence,
	})
}

// persist stores the engine state the run produced. The context is detached so a
// cancelled or timed-out run still records what it already wrote.
func (e *calendarEngine) persist(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	rows := make([]models.CalendarEventLink, 0, len(e.pending))
	for _, row := range e.pending {
		rows = append(rows, row)
	}
	if err := e.service.store.SaveCalendarLinks(ctx, rows); err != nil {
		return err
	}
	if err := e.service.store.DeleteCalendarLinks(ctx, e.job.ID, e.forgotten); err != nil {
		return err
	}
	if e.source != nil && e.source.hasFetch {
		if err := e.service.store.SaveCalendarFeedState(ctx, e.job.ID, e.source.fetched.ETag, e.source.fetched.LastModified, e.service.now()); err != nil {
			return err
		}
	}
	return nil
}

// destinationUID picks the UID the engine writes on the other side. A source UID
// that is already a valid iCalendar UID is kept, which is what makes a copy
// recognisable by the calendar that reads it; anything else is derived so a
// provider that validates the field does not reject the create.
func destinationUID(uid string) string {
	trimmed := strings.TrimSpace(uid)
	if isCalendarUID(trimmed) {
		return trimmed
	}
	return providers.CalendarEventUID(trimmed)
}

// isCalendarUID reports whether value is shaped like an iCalendar UID: it carries
// an '@', stays printable ASCII and holds none of the characters a provider treats
// as a separator.
func isCalendarUID(value string) bool {
	if len(value) < 3 || len(value) > 400 || !strings.Contains(value, "@") {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
		if strings.ContainsRune(" /\\:;,<>", char) {
			return false
		}
	}
	return true
}

// writtenStart is the start of a written event, falling back to the start that was
// sent when the provider did not read the event back.
func writtenStart(written writeResult, fallback time.Time) time.Time {
	if !written.event.Start.IsZero() {
		return written.event.Start
	}
	return fallback
}

// summary is the best title available for the evidence of a decision.
func (in planInput) summary() string {
	if in.Source != nil && in.Source.Summary != "" {
		return in.Source.Summary
	}
	if in.Destination != nil && in.Destination.Summary != "" {
		return in.Destination.Summary
	}
	if in.Link != nil {
		return in.Link.Summary
	}
	return ""
}

// start is the best start available for the evidence of a decision.
func (in planInput) start() time.Time {
	if in.Source != nil && !in.Source.Start.IsZero() {
		return in.Source.Start
	}
	if in.Destination != nil && !in.Destination.Start.IsZero() {
		return in.Destination.Start
	}
	if in.Link != nil {
		if !in.Link.SourceStart.IsZero() {
			return in.Link.SourceStart
		}
		return in.Link.DestinationStart
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// Subscription helpers used by the API
// ---------------------------------------------------------------------------

// ValidateFeedURL applies the fetch policy to a subscription URL before a job is
// saved, so an operator learns about a blocked or malformed target in the editor
// instead of on the first run.
func (s *CalendarService) ValidateFeedURL(raw string) error {
	_, err := ical.ValidateURL(raw, s.allowPrivate)
	return err
}

// SealFeedSecret encrypts the password of a private subscription. The plaintext
// never reaches the job row or an API answer; an empty password is stored as an
// empty secret, which is what makes clearing one possible.
func (s *CalendarService) SealFeedSecret(password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", nil
	}
	if s.box == nil {
		return "", fmt.Errorf("%w: the feed password cannot be encrypted in this build", ErrValidation)
	}
	return s.box.Encrypt(password)
}

// FeedPreview is what the editor shows after "check this subscription": the
// validators the feed returned, how much it holds and a sample of the events,
// without saving a job.
type FeedPreview struct {
	ETag         string            `json:"etag,omitempty"`
	LastModified string            `json:"last_modified,omitempty"`
	WindowStart  time.Time         `json:"window_start"`
	WindowEnd    time.Time         `json:"window_end"`
	Events       int               `json:"events"`
	Recurring    int               `json:"recurring"`
	Cancelled    int               `json:"cancelled"`
	Sample       []FeedSampleEvent `json:"sample"`
}

// FeedSampleEvent is one event of a feed preview.
type FeedSampleEvent struct {
	UID       string    `json:"uid"`
	Summary   string    `json:"summary"`
	StartAt   time.Time `json:"start_at"`
	EndAt     time.Time `json:"end_at"`
	AllDay    bool      `json:"all_day"`
	Recurring bool      `json:"recurring"`
}

// feedPreviewSample is how many events the preview shows: enough to recognise the
// feed, few enough that the editor stays a form.
const feedPreviewSample = 10

// PreviewFeed downloads and parses a subscription without storing anything. The
// window is the one a job with these bounds would list, so the count the editor
// shows matches what the first run will consider.
func (s *CalendarService) PreviewFeed(ctx context.Context, target, username, password string, pastDays, futureDays int) (*FeedPreview, error) {
	sub := ical.Subscription{
		URL:          strings.TrimSpace(target),
		Username:     strings.TrimSpace(username),
		Password:     password,
		AllowPrivate: s.allowPrivate,
	}
	window := calendarWindow(s.now(), &models.CalendarJob{PastDays: pastDays, FutureDays: futureDays})
	ctx, cancel := context.WithTimeout(ctx, s.runTimeout(ctx))
	defer cancel()

	result, err := ical.Fetch(ctx, sub)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	events, err := ical.Parse(result.Body, ical.Options{WindowStart: window.Start, WindowEnd: window.End})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	preview := &FeedPreview{
		ETag:         result.ETag,
		LastModified: result.LastModified,
		WindowStart:  window.Start,
		WindowEnd:    window.End,
		Events:       len(events),
		Sample:       make([]FeedSampleEvent, 0, feedPreviewSample),
	}
	for _, event := range events {
		switch {
		case event.Cancelled:
			preview.Cancelled++
		case event.Recurring:
			preview.Recurring++
		}
		if len(preview.Sample) >= feedPreviewSample {
			continue
		}
		preview.Sample = append(preview.Sample, FeedSampleEvent{
			UID:       event.UID,
			Summary:   event.Summary,
			StartAt:   event.Start,
			EndAt:     event.End,
			AllDay:    event.AllDay,
			Recurring: event.Recurring,
		})
	}
	return preview, nil
}
